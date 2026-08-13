package quota

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuiltInAdapterRegistry(t *testing.T) {
	definitions := AdapterDefinitions()
	want := []string{"anthropic", "codex", "neuralwatt", "zai"}
	if len(definitions) != len(want) {
		t.Fatalf("definitions=%d want %d", len(definitions), len(want))
	}
	for i, name := range want {
		if definitions[i].Name != name || definitions[i].Evidence == nil || definitions[i].New == nil {
			t.Fatalf("definition[%d]=%+v", i, definitions[i])
		}
		got, ok := AdapterDefinitionFor(name)
		if !ok || got.Name != name || !KnownAdapter(name) {
			t.Fatalf("lookup %q: got=%+v ok=%v known=%v", name, got, ok, KnownAdapter(name))
		}
	}
	if _, ok := AdapterDefinitionFor("unknown"); ok || KnownAdapter("unknown") {
		t.Fatal("unknown adapter reported as built in")
	}
}

// --- HTTP transport fakes -------------------------------------------------

// recordingDoer captures requests (cloned, so inspection survives body close)
// and returns a canned response or error.
type recordingDoer struct {
	calls []*http.Request
	resp  *http.Response
	err   error
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls = append(d.calls, req.Clone(context.Background()))
	if d.err != nil {
		return nil, d.err
	}
	if d.resp == nil {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
	}
	return d.resp, nil
}

func (d *recordingDoer) lastCall() *http.Request {
	if len(d.calls) == 0 {
		return nil
	}
	return d.calls[len(d.calls)-1]
}

// hangingDoer blocks until the request context is done, then returns ctx.Err().
// It cooperates with the bounded client's context deadline.
type hangingDoer struct{ calls int }

func (h *hangingDoer) Do(req *http.Request) (*http.Response, error) {
	h.calls++
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func newRequest(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatalf("newRequest %q: %v", rawurl, err)
	}
	return req
}

func bodyResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"X-Test": []string{"yes"}},
	}
}

// --- BoundedClient: HTTPS / URL policy -----------------------------------

func TestBoundedClientHTTPSPolicy(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https proceeds", "https://quota.example.com/v1", false},
		{"http rejected", "http://quota.example.com/v1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &recordingDoer{resp: bodyResponse(200, []byte("ok"))}
			bc := &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 10}
			_, err := bc.Do(newRequest(t, tc.url))
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
			if tc.wantErr && len(doer.calls) != 0 {
				t.Fatalf("transport must not be called on rejected URL; got %d calls", len(doer.calls))
			}
		})
	}
}

func TestBoundedClientRejectsCrossOriginRedirects(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			leaked.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer first.Close()

	client := first.Client()
	bc := &BoundedClient{Transport: client, Timeout: time.Second}
	req := newRequest(t, first.URL)
	req.Header.Set("x-api-key", "synthetic-admin-key")
	_, err := bc.Do(req)
	if err == nil {
		t.Fatal("expected cross-origin redirect to be rejected")
	}
	if leaked.Load() != 0 {
		t.Fatal("credential was forwarded to redirect origin")
	}
}

func TestBoundedClientRequiresNonEmptyHost(t *testing.T) {
	doer := &recordingDoer{}
	bc := &BoundedClient{Transport: doer}
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: ""},
	}
	_, err := bc.Do(req)
	if err == nil {
		t.Fatal("expected error for empty host")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Fatalf("error should mention host, got: %v", err)
	}
	if len(doer.calls) != 0 {
		t.Fatalf("transport must not be called; got %d calls", len(doer.calls))
	}
}

func TestBoundedClientNilRequest(t *testing.T) {
	bc := &BoundedClient{Transport: &recordingDoer{}, Timeout: time.Second}
	if _, err := bc.Do(nil); err == nil {
		t.Fatal("expected error for nil request")
	}
}

// --- BoundedClient: response size limit ----------------------------------

func TestBoundedClientMaxBodyLimit(t *testing.T) {
	const limit = int64(16)
	cases := []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{"under limit", limit - 1, false},
		{"exactly limit", limit, false},
		{"over by one", limit + 1, true},
		{"far over limit", limit * 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), int(tc.size))
			doer := &recordingDoer{resp: bodyResponse(200, payload)}
			bc := &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: limit}
			br, err := bc.Do(newRequest(t, "https://quota.example.com/v1"))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected oversize error for size %d", tc.size)
				}
				if br != nil {
					t.Fatalf("oversized response must not be returned; got %+v", br)
				}
				if !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("error should mention exceeds, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for size %d: %v", tc.size, err)
			}
			if int64(len(br.Body)) != tc.size {
				t.Fatalf("body size = %d, want %d", len(br.Body), tc.size)
			}
		})
	}
}

// --- BoundedClient: timeout ----------------------------------------------

func TestBoundedClientTimeoutDoesNotBlock(t *testing.T) {
	h := &hangingDoer{}
	bc := &BoundedClient{Transport: h, Timeout: 50 * time.Millisecond}
	start := time.Now()
	_, err := bc.Do(newRequest(t, "https://quota.example.com/v1"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Safety bound only: the real mechanism is the 50ms context deadline.
	if elapsed > time.Second {
		t.Fatalf("Do did not return promptly: elapsed=%v", elapsed)
	}
	if h.calls != 1 {
		t.Fatalf("hanging transport should be called once, got %d", h.calls)
	}
}

// --- BoundedClient: injectable transport + response shape ----------------

func TestBoundedClientInjectableTransport(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(202, []byte("hello"))}
	bc := &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 10}

	req := newRequest(t, "https://quota.example.com/v1/quota")
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("X-Trace", "t-123")

	br, err := bc.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if br.StatusCode != 202 {
		t.Fatalf("status = %d, want 202", br.StatusCode)
	}
	if string(br.Body) != "hello" {
		t.Fatalf("body = %q, want %q", br.Body, "hello")
	}
	if br.Headers.Get("X-Test") != "yes" {
		t.Fatalf("header X-Test = %q, want yes", br.Headers.Get("X-Test"))
	}
	got := doer.lastCall()
	if got.Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", got.Method)
	}
	if got.URL.String() != "https://quota.example.com/v1/quota" {
		t.Fatalf("url = %q", got.URL.String())
	}
	if got.Header.Get("Authorization") != "Bearer abc" {
		t.Fatalf("auth header not forwarded to transport")
	}
}

// --- BoundedClient: error sanitization -----------------------------------

func TestBoundedClientErrorSanitization(t *testing.T) {
	const secret = "supersecrettoken"
	// A transport error that echoes a URL with embedded credentials.
	leaky := fmt.Errorf(`Get "https://%s:hunter2@quota.example.com/v1": connection refused`, secret)
	doer := &recordingDoer{err: leaky}
	bc := &BoundedClient{Transport: doer, Timeout: time.Second}

	_, err := bc.Do(newRequest(t, "https://quota.example.com/v1"))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Fatalf("error leaks secret token: %s", msg)
	}
	if strings.Contains(msg, "hunter2") {
		t.Fatalf("error leaks URL password: %s", msg)
	}
	if strings.Contains(SanitizeError(err), secret) {
		t.Fatalf("SanitizeError still leaks token")
	}
}

// --- CredentialResolver ---------------------------------------------------

func TestDefaultResolverEnv(t *testing.T) {
	r := DefaultCredentialResolver()
	t.Run("set resolves", func(t *testing.T) {
		t.Setenv("PQ_TEST_TOKEN", "env-value-123")
		got, err := r.Resolve(CredentialRef{Kind: CredentialEnv, Locator: "PQ_TEST_TOKEN"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "env-value-123" {
			t.Fatalf("got %q, want env-value-123", got)
		}
	})
	t.Run("unset errors", func(t *testing.T) {
		_, err := r.Resolve(CredentialRef{Kind: CredentialEnv, Locator: "PQ_TEST_DEFINITELY_UNSET"})
		if err == nil {
			t.Fatal("expected error for unset env var")
		}
	})
	t.Run("empty errors", func(t *testing.T) {
		t.Setenv("PQ_TEST_EMPTY", "")
		_, err := r.Resolve(CredentialRef{Kind: CredentialEnv, Locator: "PQ_TEST_EMPTY"})
		if err == nil {
			t.Fatal("expected error for empty env var")
		}
	})
}

func TestDefaultResolverFile(t *testing.T) {
	r := DefaultCredentialResolver()
	t.Run("file resolves trimmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("  file-secret-456  \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := r.Resolve(CredentialRef{Kind: CredentialFile, Locator: path})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "file-secret-456" {
			t.Fatalf("got %q, want trimmed file-secret-456", got)
		}
	})
	t.Run("missing file errors", func(t *testing.T) {
		_, err := r.Resolve(CredentialRef{Kind: CredentialFile, Locator: filepath.Join(t.TempDir(), "nope")})
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}

func TestDefaultResolverLiteral(t *testing.T) {
	r := DefaultCredentialResolver()
	got, err := r.Resolve(CredentialRef{Kind: CredentialLiteral, Locator: "literal-789"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "literal-789" {
		t.Fatalf("got %q, want literal-789", got)
	}
}

func TestDefaultResolverUnsupportedKind(t *testing.T) {
	r := DefaultCredentialResolver()
	_, err := r.Resolve(CredentialRef{Kind: CredentialKind("vault"), Locator: "x"})
	if err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}

// Resolved values must never appear in any error path.
func TestDefaultResolverSecretFreeErrors(t *testing.T) {
	r := DefaultCredentialResolver()
	const secret = "topsecretvalue"
	t.Setenv("PQ_TEST_SECRET", secret)

	if _, err := r.Resolve(CredentialRef{Kind: CredentialEnv, Locator: "PQ_TEST_DEFINITELY_UNSET"}); err == nil {
		t.Fatal("expected error")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("env error leaks secret: %s", err)
	}
	if _, err := r.Resolve(CredentialRef{Kind: CredentialFile, Locator: "PQ_TEST_SECRET"}); err == nil {
		t.Fatal("expected error for nonexistent file")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("file error leaks secret: %s", err)
	}
}

func TestCredentialResolverInjectable(t *testing.T) {
	fake := &fakeResolver{val: "injected-value"}
	got, err := fake.Resolve(CredentialRef{Kind: CredentialEnv, Locator: "anything"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "injected-value" {
		t.Fatalf("got %q, want injected-value", got)
	}
}

type fakeResolver struct {
	val string
	err error
}

func (f *fakeResolver) Resolve(_ CredentialRef) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.val, nil
}

// --- QuotaSource seam -----------------------------------------------------

// supportedFakeSource routes Fetch through a real BoundedClient with an injected
// transport, demonstrating the seam adapters (Task 5) will compose.
type supportedFakeSource struct {
	id string
	bc *BoundedClient
}

func (s *supportedFakeSource) MappingID() string     { return s.id }
func (s *supportedFakeSource) Status() SupportStatus { return SupportStatus{Supported: true} }
func (s *supportedFakeSource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://quota.example.com/v1", nil)
	if _, err := s.bc.Do(req); err != nil {
		return QuotaSnapshot{
			MappingID:    s.id,
			Availability: QuotaUnknown,
			Status:       SourceFailed,
			Error:        SanitizeError(err),
		}, err
	}
	// No provider parsing in this task; return a sanitized marker snapshot.
	return QuotaSnapshot{
		MappingID:    s.id,
		Availability: QuotaAvailable,
		Status:       SourceFresh,
		CheckedAt:    time.Now(),
	}, nil
}

// unsupportedFakeSource fails closed: Fetch returns an error without touching
// the transport, as the QuotaSource contract requires.
type unsupportedFakeSource struct {
	id     string
	reason string
	doer   *recordingDoer
}

func (s *unsupportedFakeSource) MappingID() string { return s.id }
func (s *unsupportedFakeSource) Status() SupportStatus {
	return SupportStatus{Supported: false, Reason: s.reason}
}
func (s *unsupportedFakeSource) Fetch(_ context.Context) (QuotaSnapshot, error) {
	if !s.Status().Supported {
		return QuotaSnapshot{
			MappingID: s.id,
			Status:    SourceFailed,
			Error:     s.reason,
		}, errors.New(s.reason)
	}
	return QuotaSnapshot{}, nil
}

func TestSupportedSourceCallsTransport(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte("{}"))}
	src := &supportedFakeSource{
		id: "codex-acct1",
		bc: &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 10},
	}
	if !src.Status().Supported {
		t.Fatal("expected supported")
	}
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.MappingID != "codex-acct1" {
		t.Fatalf("mapping id = %q", snap.MappingID)
	}
	if snap.Status != SourceFresh {
		t.Fatalf("status = %s, want fresh", snap.Status)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("transport should be called once, got %d", len(doer.calls))
	}
}

func TestUnsupportedSourceFailsClosed(t *testing.T) {
	doer := &recordingDoer{}
	src := &unsupportedFakeSource{id: "zai-x", reason: "contract evidence expired", doer: doer}
	if src.Status().Supported {
		t.Fatal("expected unsupported")
	}
	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error from unsupported Fetch")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("snapshot status = %s, want failed", snap.Status)
	}
	if len(doer.calls) != 0 {
		t.Fatalf("transport must not be called when unsupported; got %d calls", len(doer.calls))
	}
}

// --- Registry -------------------------------------------------------------

func TestRegistryRoundTrip(t *testing.T) {
	r := NewRegistry()
	if got := r.All(); len(got) != 0 {
		t.Fatalf("empty registry All = %d, want 0", len(got))
	}
	a := &supportedFakeSource{id: "a"}
	b := &unsupportedFakeSource{id: "b"}
	r.Register(a)
	r.Register(b)

	if s, ok := r.Get("a"); !ok || s.MappingID() != "a" {
		t.Fatalf("Get(a) = %+v ok=%v", s, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get(missing) should be false")
	}
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All = %d, want 2", len(all))
	}
	// Mutating All() must not affect the registry.
	all[0] = b
	if s, ok := r.Get("a"); !ok || s.MappingID() != "a" {
		t.Fatalf("registry mutated via All(); Get(a) = %+v ok=%v", s, ok)
	}

	// Re-registering a mapping ID replaces the entry (no stale duplicates).
	c := &supportedFakeSource{id: "a"}
	r.Register(c)
	if len(r.All()) != 2 {
		t.Fatalf("after replace All = %d, want 2", len(r.All()))
	}
	if s, ok := r.Get("a"); !ok || s != c {
		t.Fatal("replace did not update source a")
	}
}
