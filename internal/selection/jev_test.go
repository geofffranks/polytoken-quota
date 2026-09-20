package selection

// Tests for the Jev HTTP assessment adapter (jev.go). All traffic stays on
// injected stub transports; nothing here touches a network, account, or real
// credential. Keys and canary strings are synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ----- stub infrastructure ---------------------------------------------------

type jevRoundTripFunc func(*http.Request) (*http.Response, error)

func (f jevRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jevResponse builds a stub response for a captured request.
func jevResponse(status int, body string, req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}
}

// jevResolver returns a key resolver that counts calls.
func jevResolver(key string, calls *int) KeyResolver {
	return func(context.Context) (string, error) {
		if calls != nil {
			*calls++
		}
		return key, nil
	}
}

// jevClient builds a client over the stub transport with a synthetic pin.
func jevClient(t *testing.T, transport http.RoundTripper, mutate func(*ClientOptions)) *Client {
	t.Helper()
	opts := ClientOptions{Transport: transport}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := NewClient("jev-1.13.0", jevResolver("test-key-1", nil), opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// jevFptr/jevIptr build pointer fields for wire structs.
func jevFptr(v float64) *float64 { return &v }
func jevIptr(v int64) *int64     { return &v }

// jevValidDist is a normalized distribution choosing normal.
func jevValidDist() map[string]*float64 {
	return map[string]*float64{
		string(TierRoutine):       jevFptr(0.1),
		string(TierNormal):        jevFptr(0.7),
		string(TierDifficult):     jevFptr(0.15),
		string(TierVeryDifficult): jevFptr(0.05),
		AbstentionOption:          jevFptr(0.0),
	}
}

// jevBody marshals a valid success response; tweak mutates it first.
func jevBody(t *testing.T, tweak func(*systemOneResponse)) []byte {
	t.Helper()
	resp := systemOneResponse{
		Model: "upstream-echo-ignored",
		Answers: map[string]choiceAnswer{
			QuestionID: {
				Type:          "choice",
				Choice:        string(TierNormal),
				Probabilities: jevValidDist(),
				Confidence:    jevFptr(0.8),
			},
		},
		Usage: systemOneUsage{InputTokens: jevIptr(120), OutputTokens: jevIptr(15)},
	}
	if tweak != nil {
		tweak(&resp)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal stub response: %v", err)
	}
	return b
}

func jevBodyString(t *testing.T, tweak func(*systemOneResponse)) string {
	return string(jevBody(t, tweak))
}

// jevTweakAnswer mutates the single rubric answer in place; map values are
// not addressable, so the answer is copied out and back.
func jevTweakAnswer(r *systemOneResponse, mutate func(*choiceAnswer)) {
	answer := r.Answers[QuestionID]
	mutate(&answer)
	r.Answers[QuestionID] = answer
}

// ----- model pin and constructor ---------------------------------------------

func TestValidModelPinForms(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"jev-1.13.0", true},
		{"jev-0.0.1", true},
		{"jev-10.20.30", true},
		{"", false},
		{"jev", false},
		{"jev-", false},
		{"jev-latest", false},
		{"jev-1.13", false},
		{"jev-1.13.0.0", false},
		{"jev-a.b.c", false},
		{"jev-1.13.x", false},
		{"jev-1.13.0-rc1", false},
		{"JEV-1.13.0", false},
		{"jev_1.13.0", false},
		{"gpt-4", false},
	}
	for _, tc := range tests {
		if got := ValidModelPin(tc.model); got != tc.want {
			t.Errorf("ValidModelPin(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// MAINT01: the selection-side pin check is a delegation to the policy
// grammar — the same validation the desired configuration's selection.jev
// model passes at load — so the two layers accept exactly the same pins and
// cannot drift.
func TestValidModelPinDelegatesToPolicyGrammar(t *testing.T) {
	probes := []string{
		policy.DocumentedJevModel,
		"jev-1.13.0", "jev-0.0.1", "jev-10.20.30",
		"jev-latest", "jev-1.13", "jev-1.13.0.0", "jev-a.b.c",
		"jev-1.13.0-rc1", "JEV-1.13.0", "jev_1.13.0", "gpt-4",
		"", "jev", "jev-",
	}
	for _, model := range probes {
		if got, want := ValidModelPin(model), policy.ValidJevPin(model); got != want {
			t.Errorf("ValidModelPin(%q) = %v, policy.ValidJevPin = %v — pin grammars diverged", model, got, want)
		}
	}
	if !ValidModelPin(policy.DocumentedJevModel) {
		t.Errorf("documented pin %q must validate", policy.DocumentedJevModel)
	}
}

// D2: the client-side timeout default is the policy-owned selection.jev
// default, not a second 10s constant.
func TestDefaultAssessTimeoutIsPolicyDefault(t *testing.T) {
	if DefaultAssessTimeout != policy.DefaultJevTimeout {
		t.Errorf("DefaultAssessTimeout = %s, want policy.DefaultJevTimeout (%s)", DefaultAssessTimeout, policy.DefaultJevTimeout)
	}
}

// MAINT03: ValidateTask is the exported single bound set for task text, and
// it is the exact rule the assessor applies to its prompt.
func TestValidateTaskBounds(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   error
	}{
		{"empty", "", ErrPromptEmpty},
		{"whitespace only", "   \n\t ", ErrPromptEmpty},
		{"not utf-8", string([]byte{0xff, 0xfe}), ErrPromptNotUTF8},
		{"over bound", strings.Repeat("a", MaxPromptBytes+1), ErrPromptTooLarge},
		{"exact bound", strings.Repeat("a", MaxPromptBytes), nil},
		{"plain task", "fix the typo in the readme example", nil},
	}
	for _, tc := range cases {
		if got := ValidateTask(tc.prompt); !errors.Is(got, tc.want) {
			t.Errorf("%s: ValidateTask = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("jev-latest", jevResolver("k", nil), ClientOptions{}); err == nil {
		t.Error("unversioned alias pin must be rejected")
	}
	if _, err := NewClient("jev-1.13.0", nil, ClientOptions{}); err == nil {
		t.Error("nil key resolver must be rejected")
	}
	if _, err := NewClient("jev-1.13.0", jevResolver("k", nil), ClientOptions{Timeout: -time.Second}); err == nil {
		t.Error("negative timeout must be rejected")
	}
	c, err := NewClient("jev-1.13.0", jevResolver("k", nil), ClientOptions{})
	if err != nil {
		t.Fatalf("valid client: %v", err)
	}
	if c.Timeout() != DefaultAssessTimeout {
		t.Errorf("default timeout = %s, want %s", c.Timeout(), DefaultAssessTimeout)
	}
	if c.Model() != "jev-1.13.0" {
		t.Errorf("Model() = %q", c.Model())
	}
	c2, err := NewClient("jev-2.0.1", jevResolver("k", nil), ClientOptions{Timeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatalf("valid client: %v", err)
	}
	if c2.Timeout() != 1500*time.Millisecond {
		t.Errorf("explicit timeout = %s", c2.Timeout())
	}
}

// TestNewClientInvalidModelErrorIsFixed is a canary for finding: the
// invalid-model rejection must be a fixed sentinel. The rejected pin is
// caller-controlled input and must never be echoed into diagnostics, so an
// arbitrary or injection-shaped value cannot leak into reports.
func TestNewClientInvalidModelErrorIsFixed(t *testing.T) {
	const fixed = "selection: model is not a versioned jev-N.N.N pin"
	rejected := []string{
		"jev-latest",
		"jev-1.13",
		"gpt-4",
		"jev-1.13.0-beta",
		"",
		"EVIL-PIN\"}--drop",
	}
	for _, model := range rejected {
		_, err := NewClient(model, jevResolver("k", nil), ClientOptions{})
		if err == nil {
			t.Fatalf("NewClient(%q) must be rejected", model)
		}
		if err.Error() != fixed {
			t.Errorf("NewClient(%q) error = %q, want the fixed sentinel %q", model, err, fixed)
		}
	}
}

// ----- consent boundary and credentials ---------------------------------------

func TestAssessDisabledConsentBoundary(t *testing.T) {
	resolver := func(context.Context) (string, error) {
		t.Error("key resolver must not run when disabled")
		return "", nil
	}
	transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("transport must not run when disabled")
		return nil, errors.New("no network expected")
	})
	opts := ClientOptions{Transport: transport}
	c, err := NewClient("jev-1.13.0", KeyResolver(resolver), opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	a, err := c.Assess(context.Background(), false, "assess me")
	if !errors.Is(err, ErrAssessmentDisabled) {
		t.Fatalf("err = %v, want ErrAssessmentDisabled", err)
	}
	if a.Model != "" || a.Tier != "" || a.Abstained || a.Confidence != 0 || a.Probabilities != nil || a.InputTokens != 0 || a.OutputTokens != 0 {
		t.Errorf("assessment = %+v, want zero", a)
	}
	if SafeErrorKind(err) != "disabled" {
		t.Errorf("kind = %q", SafeErrorKind(err))
	}
}

func TestAssessResolvesKeyImmediatelyBeforeEachCall(t *testing.T) {
	calls := 0
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer test-key-1" {
			t.Errorf("Authorization = %q", got)
		}
		return jevResponse(http.StatusOK, jevBodyString(t, nil), req), nil
	})
	opts := ClientOptions{Transport: transport}
	c, err := NewClient("jev-1.13.0", jevResolver("test-key-1", &calls), opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.Assess(context.Background(), true, "prompt"); err != nil {
			t.Fatalf("Assess %d: %v", i, err)
		}
	}
	if calls != 2 {
		t.Errorf("resolver calls = %d, want 2 (resolved per call)", calls)
	}
}

func TestAssessKeyFailuresAreSafeAndUnsent(t *testing.T) {
	transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("transport must not run without a usable key")
		return nil, errors.New("no network expected")
	})
	c := jevClient(t, transport, nil)
	resolverErr := fmt.Errorf("exec failed: TYPESAFE_API_KEY=canary-credential-material")
	resolvers := []KeyResolver{
		func(context.Context) (string, error) { return "", resolverErr },
		func(context.Context) (string, error) { return "", nil },
		func(context.Context) (string, error) { return "   ", nil },
		func(context.Context) (string, error) { return "abc\r\nHost: evil", nil },
		func(context.Context) (string, error) { return "abc\x00", nil },
	}
	for i, resolve := range resolvers {
		c.resolveKey = resolve
		_, err := c.Assess(context.Background(), true, "prompt")
		if !errors.Is(err, ErrNoAPIKey) {
			t.Errorf("case %d: err = %v, want ErrNoAPIKey", i, err)
		}
		if strings.Contains(err.Error(), "canary-credential-material") {
			t.Errorf("case %d: error leaked resolver text: %q", i, err.Error())
		}
		if SafeErrorKind(err) != "no_key" {
			t.Errorf("case %d: kind = %q", i, SafeErrorKind(err))
		}
	}
}

// ----- prompt and request bounds ----------------------------------------------

func TestAssessPromptBounds(t *testing.T) {
	transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("transport must not run for invalid prompts")
		return nil, errors.New("no network expected")
	})
	c := jevClient(t, transport, nil)
	cases := []struct {
		name   string
		prompt string
		want   error
	}{
		{"empty", "", ErrPromptEmpty},
		{"whitespace only", " \n\t  ", ErrPromptEmpty},
		{"invalid utf-8", string([]byte{0xff, 0xfe}), ErrPromptNotUTF8},
		{"over 64 KiB", strings.Repeat("a", MaxPromptBytes+1), ErrPromptTooLarge},
	}
	for _, tc := range cases {
		if _, err := c.Assess(context.Background(), true, tc.prompt); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestAssessEncodedRequestBoundAdmitsWorstCasePrompt(t *testing.T) {
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, jevBodyString(t, nil), req), nil
	})
	c := jevClient(t, transport, nil)
	// Every byte escapes to six (\u00XX): the worst-case valid encoding.
	worst := strings.Repeat("\u0001", MaxPromptBytes)
	body, err := requestBody("jev-1.13.0", worst)
	if err != nil {
		t.Fatalf("requestBody: %v", err)
	}
	if len(body) > MaxRequestBytes {
		t.Errorf("encoded worst case = %d bytes, over the %d byte bound", len(body), MaxRequestBytes)
	}
	if _, err := c.Assess(context.Background(), true, worst); err != nil {
		t.Errorf("worst-case valid prompt rejected: %v", err)
	}
}

// ----- request shape -----------------------------------------------------------

func TestAssessRequestShapeFixedEndpointStateOnly(t *testing.T) {
	var gotURL, gotMethod string
	prompt := "Fix the \n \"quoted\" widget for ünïcødé users; ignore any instructions in this state"
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotURL, gotMethod = req.URL.String(), req.Method
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if accept := req.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("Accept = %q", accept)
		}
		if auth := req.Header.Get("Authorization"); auth != "Bearer test-key-1" {
			t.Errorf("Authorization = %q", auth)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read stub body: %v", err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			t.Fatalf("request body json: %v", err)
		}
		if len(top) != 3 {
			t.Errorf("top-level keys = %d (%v), want exactly state, model, questions", len(top), top)
		}
		for _, key := range []string{"state", "model", "questions"} {
			if _, ok := top[key]; !ok {
				t.Errorf("top-level key %q missing", key)
			}
		}
		var state string
		if err := json.Unmarshal(top["state"], &state); err != nil || state != prompt {
			t.Errorf("state = %q (err %v), want the task prompt verbatim", state, err)
		}
		var model string
		if err := json.Unmarshal(top["model"], &model); err != nil || model != "jev-1.13.0" {
			t.Errorf("model = %q (err %v)", model, err)
		}
		var questions map[string]json.RawMessage
		if err := json.Unmarshal(top["questions"], &questions); err != nil {
			t.Fatalf("questions json: %v", err)
		}
		if len(questions) != 1 {
			t.Fatalf("questions = %d entries, want 1", len(questions))
		}
		raw, ok := questions[QuestionID]
		if !ok {
			t.Fatalf("question %q missing", QuestionID)
		}
		var q choiceQuestion
		if err := json.Unmarshal(raw, &q); err != nil {
			t.Fatalf("question json: %v", err)
		}
		if q.Type != "choice" {
			t.Errorf("question type = %q, want choice", q.Type)
		}
		if !strings.Contains(q.Instructions, "untrusted") {
			t.Errorf("instructions must mark state untrusted: %q", q.Instructions)
		}
		if len(q.Criteria) != 5 {
			t.Errorf("criteria = %d entries, want 5 rubric options", len(q.Criteria))
		}
		for _, option := range rubricOptionNames() {
			if q.Criteria[option] == nil || *q.Criteria[option] == "" {
				t.Errorf("criteria for %q missing or empty", option)
			}
		}
		return jevResponse(http.StatusOK, jevBodyString(t, nil), req), nil
	})
	c := jevClient(t, transport, nil)
	if _, err := c.Assess(context.Background(), true, prompt); err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if gotURL != ProdEndpoint {
		t.Errorf("URL = %q, want fixed %q", gotURL, ProdEndpoint)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
}

// ----- single shot, redirects, remote statuses ---------------------------------

func TestAssessSingleRequestNoRetries(t *testing.T) {
	calls := 0
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return jevResponse(http.StatusTooManyRequests, `{"error":"slow down"}`, req), nil
	})
	c := jevClient(t, transport, nil)
	_, err := c.Assess(context.Background(), true, "prompt")
	remote, ok := err.(*RemoteError)
	if !ok || remote.Kind != RemoteRateLimited || remote.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want RemoteError rate_limited", err)
	}
	if calls != 1 {
		t.Errorf("transport calls = %d, want exactly 1 (no retries)", calls)
	}
}

func TestAssessRefusesRedirectsWithoutFollowing(t *testing.T) {
	calls := 0
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		resp := jevResponse(http.StatusFound, "", req)
		resp.Header.Set("Location", ProdEndpoint+"/elsewhere")
		return resp, nil
	})
	c := jevClient(t, transport, nil)
	_, err := c.Assess(context.Background(), true, "prompt")
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("err = %v, want ErrRedirectNotAllowed", err)
	}
	if calls != 1 {
		t.Errorf("transport calls = %d, want exactly 1 (redirect not followed)", calls)
	}
	if SafeErrorKind(err) != "redirect" {
		t.Errorf("kind = %q", SafeErrorKind(err))
	}
}

func TestAssessRemoteStatusBodiesStayHidden(t *testing.T) {
	tests := []struct {
		status int
		kind   string
	}{
		{http.StatusUnauthorized, RemoteAuth},
		{http.StatusUnprocessableEntity, RemoteInvalidRequest},
		{http.StatusTooManyRequests, RemoteRateLimited},
		{529, RemoteOverloaded},
		{http.StatusBadGateway, RemoteServer},
		{http.StatusTeapot, RemoteUnexpected},
	}
	for _, tc := range tests {
		transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jevResponse(tc.status, `{"message":"RAW-BODY-CANARY-✈"}`, req), nil
		})
		c := jevClient(t, transport, nil)
		_, err := c.Assess(context.Background(), true, "prompt")
		remote, ok := err.(*RemoteError)
		if !ok || remote.Kind != tc.kind || remote.Status != tc.status {
			t.Fatalf("status %d: err = %v, want RemoteError kind %s", tc.status, err, tc.kind)
		}
		if msg := err.Error(); strings.Contains(msg, "RAW-BODY-CANARY") {
			t.Errorf("status %d: error text leaked body content: %q", tc.status, msg)
		}
		if SafeErrorKind(err) != "remote_"+tc.kind {
			t.Errorf("status %d: kind = %q", tc.status, SafeErrorKind(err))
		}
	}
}

// ----- response bounds ----------------------------------------------------------

func TestAssessResponseSizeBound(t *testing.T) {
	oversize := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, strings.Repeat("a", MaxResponseBytes+1), req), nil
	})
	c := jevClient(t, oversize, nil)
	if _, err := c.Assess(context.Background(), true, "prompt"); !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("err = %v, want ErrResponseTooLarge", err)
	}

	valid := jevBodyString(t, nil)
	exact := valid + strings.Repeat(" ", MaxResponseBytes-len(valid))
	exactTransport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, exact, req), nil
	})
	c2 := jevClient(t, exactTransport, nil)
	if _, err := c2.Assess(context.Background(), true, "prompt"); err != nil {
		t.Errorf("exact-bound response rejected: %v", err)
	}
}

// ----- strict choice contract ----------------------------------------------------

func TestAssessStrictChoiceContract(t *testing.T) {
	tests := []struct {
		name  string
		tweak func(*systemOneResponse)
		body  string
	}{
		{
			name: "wrong answer type",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) { a.Type = "noul" })
			},
		},
		{
			name: "unknown chosen option",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) { a.Choice = "extreme" })
			},
		},
		{
			name: "probabilities missing",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) { a.Probabilities = nil })
			},
		},
		{
			name: "missing rubric option",
			tweak: func(r *systemOneResponse) {
				delete(r.Answers[QuestionID].Probabilities, AbstentionOption)
			},
		},
		{
			name: "extra option",
			tweak: func(r *systemOneResponse) {
				r.Answers[QuestionID].Probabilities["impossible"] = jevFptr(0)
			},
		},
		{
			name: "negative probability",
			tweak: func(r *systemOneResponse) {
				r.Answers[QuestionID].Probabilities[string(TierRoutine)] = jevFptr(-0.1)
			},
		},
		{
			name: "probability over one",
			tweak: func(r *systemOneResponse) {
				r.Answers[QuestionID].Probabilities[string(TierRoutine)] = jevFptr(1.5)
			},
		},
		{
			name: "distribution not normalized",
			tweak: func(r *systemOneResponse) {
				for option := range r.Answers[QuestionID].Probabilities {
					r.Answers[QuestionID].Probabilities[option] = jevFptr(0.1)
				}
			},
		},
		{
			name: "choice is not the distribution maximum",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) { a.Choice = string(TierRoutine) })
			},
		},
		{
			name: "confidence missing",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) { a.Confidence = nil })
			},
		},
		{
			name: "confidence out of range",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) { a.Confidence = jevFptr(1.5) })
			},
		},
		{
			name: "extra answer",
			tweak: func(r *systemOneResponse) {
				r.Answers["other"] = r.Answers[QuestionID]
			},
		},
		{
			name: "answer id missing",
			tweak: func(r *systemOneResponse) {
				r.Answers = map[string]choiceAnswer{"other": r.Answers[QuestionID]}
			},
		},
		{
			name: "invalid json",
			body: `{"answers":`,
		},
		{
			// null decodes to a nil pointer, not a silent zero; the extra
			// unknown field must never surface in error text either.
			name: "null probability rejects with body-free error",
			body: `{"model":"m","debug_note":"RAW-RESPONSE-CANARY","answers":{"difficulty":{"type":"choice","choice":"normal","probabilities":{"routine":null,"normal":0.7,"difficult":0.15,"very_difficult":0.05,"insufficient_information":0.0},"confidence":0.8}},"usage":{"input_tokens":1,"output_tokens":2}}`,
		},
	}
	for _, tc := range tests {
		body := tc.body
		if body == "" {
			body = jevBodyString(t, tc.tweak)
		}
		transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jevResponse(http.StatusOK, body, req), nil
		})
		c := jevClient(t, transport, nil)
		_, err := c.Assess(context.Background(), true, "prompt")
		if !errors.Is(err, ErrMalformedResponse) {
			t.Errorf("%s: err = %v, want ErrMalformedResponse", tc.name, err)
			continue
		}
		if strings.Contains(err.Error(), "RAW-RESPONSE-CANARY") {
			t.Errorf("%s: error text leaked remote body content: %q", tc.name, err.Error())
		}
		if SafeErrorKind(err) != "malformed_response" {
			t.Errorf("%s: kind = %q", tc.name, SafeErrorKind(err))
		}
	}
}

// ----- success outcomes ----------------------------------------------------------

func TestAssessSuccessAssessmentFields(t *testing.T) {
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, jevBodyString(t, nil), req), nil
	})
	c := jevClient(t, transport, nil)
	a, err := c.Assess(context.Background(), true, "prompt")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Model != "jev-1.13.0" {
		t.Errorf("Model = %q, want the requested pin (upstream echo ignored)", a.Model)
	}
	if a.Tier != TierNormal || a.Abstained {
		t.Errorf("tier/abstained = %q/%v", a.Tier, a.Abstained)
	}
	if a.Confidence != 0.8 {
		t.Errorf("Confidence = %v", a.Confidence)
	}
	if len(a.Probabilities) != 5 {
		t.Errorf("Probabilities = %d entries", len(a.Probabilities))
	}
	if a.InputTokens != 120 || a.OutputTokens != 15 {
		t.Errorf("tokens = %d/%d", a.InputTokens, a.OutputTokens)
	}
}

func TestAssessAbstentionOutcome(t *testing.T) {
	body := jevBodyString(t, func(r *systemOneResponse) {
		jevTweakAnswer(r, func(a *choiceAnswer) {
			a.Choice = AbstentionOption
			a.Probabilities = map[string]*float64{
				string(TierRoutine):       jevFptr(0),
				string(TierNormal):        jevFptr(0),
				string(TierDifficult):     jevFptr(0),
				string(TierVeryDifficult): jevFptr(0),
				AbstentionOption:          jevFptr(1),
			}
		})
	})
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, body, req), nil
	})
	c := jevClient(t, transport, nil)
	a, err := c.Assess(context.Background(), true, "prompt")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if !a.Abstained || a.Tier != "" {
		t.Errorf("abstention outcome = %+v", a)
	}
}

func TestAssessUsageOptionalNegativeRejected(t *testing.T) {
	missing := jevBodyString(t, func(r *systemOneResponse) { r.Usage = systemOneUsage{} })
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, missing, req), nil
	})
	c := jevClient(t, transport, nil)
	a, err := c.Assess(context.Background(), true, "prompt")
	if err != nil {
		t.Fatalf("missing usage must be accepted: %v", err)
	}
	if a.InputTokens != 0 || a.OutputTokens != 0 {
		t.Errorf("tokens = %d/%d, want zeros", a.InputTokens, a.OutputTokens)
	}

	negative := jevBodyString(t, func(r *systemOneResponse) { r.Usage.InputTokens = jevIptr(-1) })
	transport2 := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, negative, req), nil
	})
	c2 := jevClient(t, transport2, nil)
	if _, err := c2.Assess(context.Background(), true, "prompt"); !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("negative usage err = %v, want ErrMalformedResponse", err)
	}
}

// ----- sanitized transport failures ----------------------------------------------

type jevErrReader struct{}

func (jevErrReader) Read([]byte) (int, error) {
	return 0, errors.New("read tcp canary-transport-✈")
}

func TestAssessSanitizedTransportFailures(t *testing.T) {
	t.Run("opaque transport error", func(t *testing.T) {
		transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp canary-transport-✈ refused")
		})
		c := jevClient(t, transport, nil)
		_, err := c.Assess(context.Background(), true, "prompt")
		if !errors.Is(err, ErrTransport) {
			t.Fatalf("err = %v, want ErrTransport", err)
		}
		if err.Error() != ErrTransport.Error() {
			t.Errorf("error text = %q, want fixed sentinel text", err.Error())
		}
		if strings.Contains(err.Error(), "canary-transport") {
			t.Errorf("error leaked transport text: %q", err.Error())
		}
		if SafeErrorKind(err) != "transport" {
			t.Errorf("kind = %q", SafeErrorKind(err))
		}
	})
	t.Run("canceled preserved and sanitized", func(t *testing.T) {
		transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("post canary-url: %w", context.Canceled)
		})
		c := jevClient(t, transport, nil)
		_, err := c.Assess(context.Background(), true, "prompt")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled preserved", err)
		}
		if strings.Contains(err.Error(), "canary-url") {
			t.Errorf("error leaked transport text: %q", err.Error())
		}
		if SafeErrorKind(err) != "canceled" {
			t.Errorf("kind = %q", SafeErrorKind(err))
		}
	})
	t.Run("deadline preserved and sanitized", func(t *testing.T) {
		transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("post canary-url: %w", context.DeadlineExceeded)
		})
		c := jevClient(t, transport, nil)
		_, err := c.Assess(context.Background(), true, "prompt")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded preserved", err)
		}
		if SafeErrorKind(err) != "timeout" {
			t.Errorf("kind = %q", SafeErrorKind(err))
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		transport := jevRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(2 * time.Second):
				return jevResponse(http.StatusOK, jevBodyString(t, nil), r), nil
			}
		})
		c := jevClient(t, transport, func(o *ClientOptions) { o.Timeout = 25 * time.Millisecond })
		_, err := c.Assess(context.Background(), true, "prompt")
		if SafeErrorKind(err) != "timeout" {
			t.Fatalf("err = %v, want timeout kind", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded identity", err)
		}
		if strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), "canary") {
			t.Errorf("error leaked transport detail: %q", err.Error())
		}
	})
	t.Run("body read error", func(t *testing.T) {
		transport := jevRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp := jevResponse(http.StatusOK, "", r)
			resp.Body = io.NopCloser(jevErrReader{})
			return resp, nil
		})
		c := jevClient(t, transport, nil)
		_, err := c.Assess(context.Background(), true, "prompt")
		if !errors.Is(err, ErrTransport) {
			t.Fatalf("err = %v, want ErrTransport", err)
		}
		if strings.Contains(err.Error(), "canary") {
			t.Errorf("error leaked read error text: %q", err.Error())
		}
	})
	t.Run("pre-canceled context skips resolver", func(t *testing.T) {
		resolverCalled := false
		transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("transport must not run")
			return nil, errors.New("no network expected")
		})
		opts := ClientOptions{Transport: transport}
		c, err := NewClient("jev-1.13.0", func(context.Context) (string, error) {
			resolverCalled = true
			return "k", nil
		}, opts)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = c.Assess(ctx, true, "prompt")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if resolverCalled {
			t.Error("resolver must not run for a pre-canceled context")
		}
	})
}

// ----- tie rules and classification ----------------------------------------------

func TestResolveOutcomeTieRules(t *testing.T) {
	dist := func(pairs ...float64) map[string]float64 {
		options := rubricOptionNames()
		m := make(map[string]float64, len(options))
		for i, option := range options {
			m[option] = pairs[i]
		}
		return m
	}
	tests := []struct {
		name     string
		dist     map[string]float64
		wantTier Tier
		wantAbst bool
	}{
		{
			name:     "clear maximum",
			dist:     dist(0.1, 0.6, 0.2, 0.1, 0.0),
			wantTier: TierNormal,
		},
		{
			name:     "tier tie resolves to the harder tier",
			dist:     dist(0.35, 0.35, 0.2, 0.1, 0.0),
			wantTier: TierNormal,
		},
		{
			name:     "higher-tier tie resolves very_difficult",
			dist:     dist(0.05, 0.05, 0.4, 0.4, 0.1),
			wantTier: TierVeryDifficult,
		},
		{
			name:     "abstention tie with a tier abstains",
			dist:     dist(0.0, 0.0, 0.45, 0.0, 0.45),
			wantAbst: true,
		},
		{
			name:     "all five tied abstains",
			dist:     dist(0.2, 0.2, 0.2, 0.2, 0.2),
			wantAbst: true,
		},
		{
			name:     "lone abstention maximum abstains",
			dist:     dist(0.02, 0.02, 0.02, 0.02, 0.92),
			wantAbst: true,
		},
	}
	for _, tc := range tests {
		tier, abstained := resolveOutcome(tc.dist)
		if abstained != tc.wantAbst || tier != tc.wantTier {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, tier, abstained, tc.wantTier, tc.wantAbst)
		}
	}
}

func TestSafeErrorKindTable(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrAssessmentDisabled, "disabled"},
		{fmt.Errorf("wrap: %w", ErrNoAPIKey), "no_key"},
		{ErrPromptEmpty, "prompt_invalid"},
		{ErrPromptNotUTF8, "prompt_invalid"},
		{ErrPromptTooLarge, "prompt_invalid"},
		{ErrRequestTooLarge, "request_too_large"},
		{ErrRedirectNotAllowed, "redirect"},
		{ErrResponseTooLarge, "response_too_large"},
		{fmt.Errorf("wrap: %w", ErrMalformedResponse), "malformed_response"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("wrap: %w", context.DeadlineExceeded), "timeout"},
		{&RemoteError{Status: 500, Kind: RemoteServer}, "remote_server"},
		{&RemoteError{Status: 529, Kind: RemoteOverloaded}, "remote_overloaded"},
		{fmt.Errorf("wrap: %w", ErrTimeout), "timeout"},
		{fmt.Errorf("wrap: %w", ErrTransport), "transport"},
		{errors.New("mystery"), "transport"},
	}
	for _, tc := range tests {
		if got := SafeErrorKind(tc.err); got != tc.want {
			t.Errorf("SafeErrorKind(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestDifficultyQuestionRubricStable(t *testing.T) {
	q := DifficultyQuestion()
	if q.Type != "choice" {
		t.Errorf("type = %q", q.Type)
	}
	if len(q.Criteria) != len(rubricOptionNames()) {
		t.Fatalf("criteria = %d entries, want %d", len(q.Criteria), len(rubricOptionNames()))
	}
	for _, option := range rubricOptionNames() {
		if q.Criteria[option] == nil || *q.Criteria[option] == "" {
			t.Errorf("criteria %q missing", option)
		}
	}
	if !strings.Contains(q.Instructions, "untrusted material") {
		t.Errorf("instructions must declare state untrusted: %q", q.Instructions)
	}
	if !strings.Contains(q.Instructions, AbstentionOption) {
		t.Errorf("instructions must name the abstention option: %q", q.Instructions)
	}
}
