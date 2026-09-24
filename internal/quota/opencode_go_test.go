package quota

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

var opencodeGoTestNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

const opencodeGoTestKey = "synthetic-opencode-key-AbCd1234"

type opencodeGoResolver struct {
	ref   CredentialRef
	value string
	err   error
}

func (r *opencodeGoResolver) Resolve(ref CredentialRef) (string, error) {
	r.ref = ref
	if r.value != "" || r.err != nil {
		return r.value, r.err
	}
	return opencodeGoTestKey, nil
}

func opencodeGoTestSource(t *testing.T, body string, status int, evidence bool) (*OpenCodeGoSource, *recordingDoer) {
	t.Helper()
	reg := NewEvidenceRegistry()
	if evidence {
		reg.Register(OpenCodeGoEvidence(opencodeGoTestNow))
	}
	doer := &recordingDoer{resp: bodyResponse(status, []byte(body))}
	return &OpenCodeGoSource{
		mappingID: "opencode-go-test", Client: &BoundedClient{Transport: doer},
		Credentials: &opencodeGoResolver{}, Evidence: reg,
		Now: func() time.Time { return opencodeGoTestNow },
	}, doer
}

func TestOpenCodeGoKnownAdapterContract(t *testing.T) {
	if !KnownAdapter("opencode-go") {
		t.Fatal("opencode-go is not a known built-in adapter")
	}
	def, ok := AdapterDefinitionFor("opencode-go")
	if !ok {
		t.Fatal("AdapterDefinitionFor(opencode-go) not found")
	}
	if def.Evidence == nil || def.New == nil {
		t.Fatalf("definition=%+v", def)
	}
	src := def.New("m", &BoundedClient{}, nil, 0, nil, opencodeGoTestNow)
	if _, ok := src.(QuotaSource); !ok {
		t.Fatalf("factory returned %T, want a QuotaSource", src)
	}
	if def.New("m", &BoundedClient{}, nil, 0, nil, opencodeGoTestNow).MappingID() != "m" {
		t.Fatal("factory dropped the mapping id")
	}
}

func TestOpenCodeGoEvidenceContract(t *testing.T) {
	ev := OpenCodeGoEvidence(opencodeGoTestNow)
	if ev.Endpoint != opencodeGoUsageEndpoint || ev.Method != http.MethodGet || ev.AuthType != "bearer-api-key" {
		t.Fatalf("evidence=%+v", ev)
	}
	if ev.ContractID != "" {
		t.Fatalf("contract id = %q, want the single legacy empty contract", ev.ContractID)
	}
	if ev.FixturePath == "" {
		t.Fatal("fixture path must be non-empty for the release gate")
	}
	if ev.Provider != opencodeGoProviderName {
		t.Fatalf("provider=%q", ev.Provider)
	}
	if !ev.RecordedAt.Equal(evidenceRecordedAt()) || !ev.ReviewBy.Equal(evidenceRecordedAt().AddDate(0, 3, 0)) {
		t.Fatalf("evidence dates=%v/%v", ev.RecordedAt, ev.ReviewBy)
	}
	if !strings.Contains(ev.SchemaNote, "percent USED") || !strings.Contains(ev.SchemaNote, "not officially documented") {
		t.Fatalf("schema note=%q", ev.SchemaNote)
	}
}

func TestOpenCodeGoEvidenceDoesNotRenewOnConstruction(t *testing.T) {
	first := OpenCodeGoEvidence(time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC))
	later := OpenCodeGoEvidence(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if !first.RecordedAt.Equal(later.RecordedAt) || !first.ReviewBy.Equal(later.ReviewBy) {
		t.Fatalf("evidence dates changed on construction: first=%+v later=%+v", first, later)
	}
	status := EvaluateEvidence(&later, later.ReviewBy.Add(time.Minute))
	if status.State != EvidenceExpired {
		t.Fatalf("stale evidence state=%s want expired", status.State)
	}
}

// TestOpenCodeGoEvidenceGateFailsClosedWithoutRequest pins the fail-closed
// guarantee: absent or expired evidence yields an error and zero HTTP calls.
func TestOpenCodeGoEvidenceGateFailsClosedWithoutRequest(t *testing.T) {
	for name, evidence := range map[string]bool{
		"absent":   false,
		"expired":  true,
	} {
		t.Run(name, func(t *testing.T) {
			src, doer := opencodeGoTestSource(t, "{}", http.StatusOK, evidence)
			if name == "expired" {
				stale := OpenCodeGoEvidence(opencodeGoTestNow)
				stale.ReviewBy = opencodeGoTestNow.AddDate(0, 0, -1)
				reg := NewEvidenceRegistry()
				reg.Register(stale)
				src.Evidence = reg
			}
			if st := src.Status(); st.Supported {
				t.Fatalf("%s evidence reported supported", name)
			}
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown || snap.Error == "" {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if len(doer.calls) != 0 {
				t.Fatalf("fail-closed path made %d HTTP calls, want 0", len(doer.calls))
			}
		})
	}
}

func TestOpenCodeGoNilConfigurationFailsClosed(t *testing.T) {
	reg := NewEvidenceRegistry()
	reg.Register(OpenCodeGoEvidence(opencodeGoTestNow))
	for name, src := range map[string]*OpenCodeGoSource{
		"nil client":      {mappingID: "test", Credentials: &opencodeGoResolver{}, Evidence: reg, Now: func() time.Time { return opencodeGoTestNow }},
		"nil credentials": {mappingID: "test", Client: &BoundedClient{}, Evidence: reg, Now: func() time.Time { return opencodeGoTestNow }},
	} {
		t.Run(name, func(t *testing.T) {
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown || snap.Error != "opencode-go: adapter is not configured" {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if err.Error() != "opencode-go: adapter is not configured" {
				t.Fatalf("err=%v", err)
			}
		})
	}
	// The nil-credentials path has a live client: the counting transport must
	// record zero calls.
	doer := &recordingDoer{}
	src := &OpenCodeGoSource{mappingID: "test", Client: &BoundedClient{Transport: doer}, Evidence: reg, Now: func() time.Time { return opencodeGoTestNow }}
	if _, err := src.Fetch(context.Background()); err == nil || len(doer.calls) != 0 {
		t.Fatalf("nil-credentials fetch err=%v calls=%d", err, len(doer.calls))
	}
}

// TestOpenCodeGoUnresolvedCredentialFailsClosedWithoutRequest covers blank and
// erroring credential resolution: fail closed with zero HTTP calls.
func TestOpenCodeGoUnresolvedCredentialFailsClosedWithoutRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		err   error
	}{
		"blank":          {value: "   "},
		"resolver error": {err: errors.New("missing synthetic key")},
	} {
		t.Run(name, func(t *testing.T) {
			src, doer := opencodeGoTestSource(t, "{}", http.StatusOK, true)
			r := src.Credentials.(*opencodeGoResolver)
			r.value, r.err = tc.value, tc.err
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if err.Error() != "opencode-go: could not resolve OPENCODE_API_KEY" {
				t.Fatalf("err=%v", err)
			}
			if len(doer.calls) != 0 {
				t.Fatalf("fail-closed path made %d HTTP calls, want 0", len(doer.calls))
			}
		})
	}
}

func TestOpenCodeGoQuotedKeyIsTrimmed(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain":            {in: " k0 ", want: "k0"},
		"double quotes":    {in: `"k1"`, want: "k1"},
		"single quotes":    {in: "'k2'", want: "k2"},
		"quoted + spaces":  {in: ` "k3" `, want: "k3"},
		"mismatched":       {in: `"k4'`, want: `"k4'`},
		"inner only":       {in: `"k5", "k6"`, want: `k5", "k6`},
		"nested":           {in: `""k7""`, want: `"k7"`},
	} {
		if got := cleanOpenCodeGoKey(tc.in); got != tc.want {
			t.Fatalf("%s: cleanOpenCodeGoKey(%q)=%q want %q", name, tc.in, got, tc.want)
		}
	}
	// The cleaned key, not the raw value, reaches the Authorization header.
	src, doer := opencodeGoTestSource(t, "{}", http.StatusOK, true)
	resolver := src.Credentials.(*opencodeGoResolver)
	resolver.value = `"` + opencodeGoTestKey + `"`
	_ = doer
	if cleanOpenCodeGoKey(resolver.value) != opencodeGoTestKey {
		t.Fatalf("cleaned=%q", cleanOpenCodeGoKey(resolver.value))
	}
}

// TestOpenCodeGoTwoHundredWithStubDecoderFailsClosed documents the skeleton
// boundary: before the decode task lands, any 2xx body fails closed rather
// than inventing a fresh snapshot.
func TestOpenCodeGoTwoHundredWithStubDecoderFailsClosed(t *testing.T) {
	src, doer := opencodeGoTestSource(t, `{"usage":{"rolling":{"status":"ok","percent":10}}}`, http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("calls=%d, want exactly 1", len(doer.calls))
	}
}
