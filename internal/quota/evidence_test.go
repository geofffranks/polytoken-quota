package quota

import (
	"strings"
	"testing"
	"time"
)

// fixedNow is a stable reference time for deterministic evidence evaluation.
var fixedNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// freshEvidence returns a complete, current Evidence record for the named
// provider. All required fields are populated; ReviewBy is well in the future.
func freshEvidence(provider string) Evidence {
	return Evidence{
		Provider:   provider,
		Endpoint:   "https://api.example.com/v1/quota",
		Method:     "GET",
		AuthType:   "oauth-bearer",
		SchemaNote: "usage and limit fields",
		RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ReviewBy:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// --- EvaluateEvidence -----------------------------------------------------

func TestEvaluateEvidenceFresh(t *testing.T) {
	e := freshEvidence("codex")
	st := EvaluateEvidence(&e, fixedNow)
	if st.State != EvidenceFresh {
		t.Fatalf("state = %s, want fresh", st.State)
	}
	if st.Reason != "" {
		t.Fatalf("fresh reason should be empty, got %q", st.Reason)
	}
}

func TestEvaluateEvidenceAbsent(t *testing.T) {
	st := EvaluateEvidence(nil, fixedNow)
	if st.State != EvidenceAbsent {
		t.Fatalf("state = %s, want absent", st.State)
	}
}

func TestEvaluateEvidenceExpired(t *testing.T) {
	e := freshEvidence("codex")
	e.ReviewBy = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // well before fixedNow
	st := EvaluateEvidence(&e, fixedNow)
	if st.State != EvidenceExpired {
		t.Fatalf("state = %s, want expired", st.State)
	}
	if !strings.Contains(st.Reason, "expired") {
		t.Fatalf("reason should mention expired, got %q", st.Reason)
	}
	if !strings.Contains(st.Reason, "codex") {
		t.Fatalf("reason should mention provider codex, got %q", st.Reason)
	}
	if !strings.Contains(st.Reason, "2026-01-01") {
		t.Fatalf("reason should mention the review date, got %q", st.Reason)
	}
}

func TestEvaluateEvidenceIncomplete(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*Evidence)
		wantField string // expected to appear in the missing-fields list
	}{
		{"missing provider", func(e *Evidence) { e.Provider = "" }, "provider"},
		{"missing endpoint", func(e *Evidence) { e.Endpoint = "" }, "endpoint"},
		{"missing method", func(e *Evidence) { e.Method = "" }, "method"},
		{"missing auth_type", func(e *Evidence) { e.AuthType = "" }, "auth_type"},
		{"zero recorded_at", func(e *Evidence) { e.RecordedAt = time.Time{} }, "recorded_at"},
		{"zero review_by", func(e *Evidence) { e.ReviewBy = time.Time{} }, "review_by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := freshEvidence("codex")
			tc.mutate(&e)
			st := EvaluateEvidence(&e, fixedNow)
			if st.State != EvidenceIncomplete {
				t.Fatalf("state = %s, want incomplete", st.State)
			}
			if !strings.Contains(st.Reason, "incomplete") {
				t.Fatalf("reason should mention incomplete, got %q", st.Reason)
			}
			if !strings.Contains(st.Reason, tc.wantField) {
				t.Fatalf("reason should mention missing %q, got %q", tc.wantField, st.Reason)
			}
		})
	}
}

// A zero ReviewBy must classify as incomplete, not expired (the incomplete
// check precedes the expiry check so a missing date is never mistaken for a
// stale one).
func TestEvaluateEvidenceZeroReviewByIsIncomplete(t *testing.T) {
	e := freshEvidence("codex")
	e.ReviewBy = time.Time{}
	st := EvaluateEvidence(&e, fixedNow)
	if st.State != EvidenceIncomplete {
		t.Fatalf("zero ReviewBy should be incomplete, got %s", st.State)
	}
}

// --- EvidenceRegistry -----------------------------------------------------

func TestEvidenceRegistryRegisterGet(t *testing.T) {
	r := NewEvidenceRegistry()
	e := freshEvidence("codex")
	r.Register(e)

	got, ok := r.Get("codex")
	if !ok {
		t.Fatal("expected to find codex evidence")
	}
	if got.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", got.Provider)
	}
	if got.Endpoint != e.Endpoint {
		t.Fatalf("endpoint = %q, want %q", got.Endpoint, e.Endpoint)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get(missing) should be false")
	}
}

func TestEvidenceRegistryRegisterReplaces(t *testing.T) {
	r := NewEvidenceRegistry()
	r.Register(freshEvidence("codex"))

	updated := freshEvidence("codex")
	updated.Endpoint = "https://api.new.example.com/v2"
	r.Register(updated)

	got, ok := r.Get("codex")
	if !ok {
		t.Fatal("expected to find codex evidence")
	}
	if got.Endpoint != "https://api.new.example.com/v2" {
		t.Fatalf("endpoint = %q, want updated value", got.Endpoint)
	}
	if len(r.Providers()) != 1 {
		t.Fatalf("providers = %v, want exactly 1 after replace", r.Providers())
	}
}

func TestEvidenceRegistryProvidersSorted(t *testing.T) {
	r := NewEvidenceRegistry()
	r.Register(freshEvidence("zai"))
	r.Register(freshEvidence("codex"))
	r.Register(freshEvidence("beta"))

	got := r.Providers()
	want := []string{"beta", "codex", "zai"}
	if len(got) != len(want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("providers[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestEvidenceRegistryStatusAbsent(t *testing.T) {
	r := NewEvidenceRegistry()
	st := r.Status("codex", fixedNow)
	if st.State != EvidenceAbsent {
		t.Fatalf("state = %s, want absent", st.State)
	}
	if !strings.Contains(st.Reason, "codex") {
		t.Fatalf("absent reason should mention provider codex, got %q", st.Reason)
	}
	if !strings.Contains(st.Reason, "no recorded contract evidence") {
		t.Fatalf("absent reason should explain the problem, got %q", st.Reason)
	}
}

func TestEvidenceRegistryStatusFresh(t *testing.T) {
	r := NewEvidenceRegistry()
	r.Register(freshEvidence("codex"))
	st := r.Status("codex", fixedNow)
	if st.State != EvidenceFresh {
		t.Fatalf("state = %s, want fresh", st.State)
	}
}

func TestEvidenceRegistryStatusExpired(t *testing.T) {
	r := NewEvidenceRegistry()
	e := freshEvidence("codex")
	e.ReviewBy = fixedNow.Add(-24 * time.Hour)
	r.Register(e)
	st := r.Status("codex", fixedNow)
	if st.State != EvidenceExpired {
		t.Fatalf("state = %s, want expired", st.State)
	}
}

func TestEvidenceRegistryStatusIncomplete(t *testing.T) {
	r := NewEvidenceRegistry()
	e := freshEvidence("codex")
	e.Endpoint = ""
	r.Register(e)
	st := r.Status("codex", fixedNow)
	if st.State != EvidenceIncomplete {
		t.Fatalf("state = %s, want incomplete", st.State)
	}
}

// --- SupportFromEvidence --------------------------------------------------

func TestSupportFromEvidenceFresh(t *testing.T) {
	ss := SupportFromEvidence(EvidenceStatus{State: EvidenceFresh})
	if !ss.Supported {
		t.Fatal("expected supported for fresh evidence")
	}
	if ss.Reason != "" {
		t.Fatalf("fresh support reason should be empty, got %q", ss.Reason)
	}
}

func TestSupportFromEvidenceNotFresh(t *testing.T) {
	for _, state := range []EvidenceState{EvidenceAbsent, EvidenceExpired, EvidenceIncomplete} {
		t.Run(string(state), func(t *testing.T) {
			reason := "test remediation reason"
			ss := SupportFromEvidence(EvidenceStatus{State: state, Reason: reason})
			if ss.Supported {
				t.Fatal("expected unsupported")
			}
			if ss.Reason != reason {
				t.Fatalf("reason = %q, want %q", ss.Reason, reason)
			}
		})
	}
}

// --- ValidateRelease ------------------------------------------------------

func TestValidateRelease(t *testing.T) {
	r := NewEvidenceRegistry()

	// Empty configured list → empty result.
	if got := ValidateRelease(r, nil, fixedNow); len(got) != 0 {
		t.Fatalf("empty configured should return empty, got %v", got)
	}

	r.Register(freshEvidence("codex")) // codex is fresh; zai is absent
	statuses := ValidateRelease(r, []string{"codex", "zai"}, fixedNow)
	if len(statuses) != 2 {
		t.Fatalf("got %d statuses, want 2", len(statuses))
	}
	if statuses[0].State != EvidenceFresh {
		t.Fatalf("codex should be fresh, got %s: %s", statuses[0].State, statuses[0].Reason)
	}
	if statuses[1].State != EvidenceAbsent {
		t.Fatalf("zai should be absent, got %s", statuses[1].State)
	}
}

// --- Sanitization of evidence reasons -------------------------------------

// containsSecretPattern reports whether s contains a bearer token, URL with
// embedded credentials, or key/value secret pattern. It reuses the package's
// own redaction regexes so the guard matches production sanitization.
func containsSecretPattern(s string) bool {
	return reBearer.MatchString(s) || reSecretKV.MatchString(s) || reURLCreds.MatchString(s)
}

func TestEvidenceReasonsAreSanitized(t *testing.T) {
	var reasons []string

	// Absent via registry (carries provider name).
	r := NewEvidenceRegistry()
	reasons = append(reasons, r.Status("codex", fixedNow).Reason)
	reasons = append(reasons, r.Status("zai", fixedNow).Reason)

	// Expired.
	exp := freshEvidence("codex")
	exp.ReviewBy = fixedNow.Add(-24 * time.Hour)
	reasons = append(reasons, EvaluateEvidence(&exp, fixedNow).Reason)

	// Incomplete — each missing required field.
	mutators := []func(*Evidence){
		func(e *Evidence) { e.Provider = "" },
		func(e *Evidence) { e.Endpoint = "" },
		func(e *Evidence) { e.Method = "" },
		func(e *Evidence) { e.AuthType = "" },
		func(e *Evidence) { e.RecordedAt = time.Time{} },
		func(e *Evidence) { e.ReviewBy = time.Time{} },
	}
	for _, m := range mutators {
		e := freshEvidence("codex")
		m(&e)
		reasons = append(reasons, EvaluateEvidence(&e, fixedNow).Reason)
	}

	for i, reason := range reasons {
		if containsSecretPattern(reason) {
			t.Fatalf("reason %d contains a secret pattern: %q", i, reason)
		}
		if reason == "" {
			t.Fatalf("reason %d is unexpectedly empty", i)
		}
	}
}
