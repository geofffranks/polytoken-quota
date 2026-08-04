// evidence.go defines sanitized, machine-checkable contract evidence metadata
// for provider adapters and the evaluation logic that gates provider requests.
//
// An Evidence record contains NO secrets — only public endpoint paths, auth
// type categories, schema notes, and dates. The evaluation rule is fail-closed:
// absent, expired, or incomplete evidence yields an unsupported SupportStatus so
// that no provider request is made and a sanitized remediation diagnostic is
// reported.
//
// Adapters (added in a later task) register their evidence in an
// EvidenceRegistry and translate its evaluated status into a SupportStatus via
// SupportFromEvidence. The coordinator and doctor consult the registry before
// polling; the release gate (ValidateRelease) asserts every configured adapter
// has current evidence.

package quota

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Evidence is the sanitized, machine-checkable contract record for a provider
// adapter. It contains NO secrets — only public endpoint paths, auth type
// categories, and metadata.
type Evidence struct {
	Provider    string    // adapter name: "codex", "zai"
	Endpoint    string    // sanitized base URL (no credentials, no query secrets)
	Method      string    // HTTP method: "GET", "POST"
	AuthType    string    // credential reference category: "oauth-bearer", "api-key"
	SchemaNote  string    // brief response schema summary (for human review)
	FixturePath string    // path to a sanitized test fixture (relative to repo root)
	RecordedAt  time.Time // when this evidence was recorded/verified
	ReviewBy    time.Time // review/expiry date — evidence is stale after this
}

// EvidenceState classifies the freshness of an evidence record.
type EvidenceState string

const (
	EvidenceFresh      EvidenceState = "fresh"
	EvidenceExpired    EvidenceState = "expired"    // past ReviewBy
	EvidenceIncomplete EvidenceState = "incomplete" // missing required fields
	EvidenceAbsent     EvidenceState = "absent"     // no record at all
)

// EvidenceStatus is the evaluated state of evidence for a provider.
type EvidenceStatus struct {
	State  EvidenceState
	Reason string // sanitized remediation diagnostic (empty when fresh)
}

// missingEvidenceFields returns the names of required fields absent from e.
// Required: Provider, Endpoint, Method, AuthType, and non-zero RecordedAt and
// ReviewBy. SchemaNote and FixturePath are optional metadata, not required.
func missingEvidenceFields(e *Evidence) []string {
	var missing []string
	if e.Provider == "" {
		missing = append(missing, "provider")
	}
	if e.Endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if e.Method == "" {
		missing = append(missing, "method")
	}
	if e.AuthType == "" {
		missing = append(missing, "auth_type")
	}
	if e.RecordedAt.IsZero() {
		missing = append(missing, "recorded_at")
	}
	if e.ReviewBy.IsZero() {
		missing = append(missing, "review_by")
	}
	return missing
}

// EvaluateEvidence returns the freshness status of evidence for a provider.
// A nil evidence pointer yields EvidenceAbsent. A record missing required
// fields (Provider, Endpoint, Method, AuthType, or zero RecordedAt/ReviewBy)
// yields EvidenceIncomplete. A record past its ReviewBy date yields
// EvidenceExpired. Otherwise EvidenceFresh.
//
// Incomplete is checked before expired so that a missing ReviewBy date is never
// mistaken for a stale one (a zero time is technically "in the past").
func EvaluateEvidence(e *Evidence, now time.Time) EvidenceStatus {
	if e == nil {
		return EvidenceStatus{State: EvidenceAbsent}
	}
	if missing := missingEvidenceFields(e); len(missing) > 0 {
		return EvidenceStatus{
			State: EvidenceIncomplete,
			Reason: fmt.Sprintf(
				"provider %s contract evidence is incomplete (missing %s); record complete evidence",
				e.Provider, strings.Join(missing, ", ")),
		}
	}
	if now.After(e.ReviewBy) {
		return EvidenceStatus{
			State: EvidenceExpired,
			Reason: fmt.Sprintf(
				"provider %s contract evidence expired on %s; re-verify and update",
				e.Provider, e.ReviewBy.Format("2006-01-02")),
		}
	}
	return EvidenceStatus{State: EvidenceFresh}
}

// --- Evidence registry ----------------------------------------------------

// EvidenceRegistry holds sanitized contract evidence for provider adapters.
// It is not safe for concurrent use; the coordinator (a later task) serializes
// access.
type EvidenceRegistry struct {
	records map[string]Evidence
}

// NewEvidenceRegistry returns an empty EvidenceRegistry.
func NewEvidenceRegistry() *EvidenceRegistry {
	return &EvidenceRegistry{records: make(map[string]Evidence)}
}

// Register adds or replaces the evidence record for a provider (keyed by
// Evidence.Provider).
func (r *EvidenceRegistry) Register(e Evidence) {
	if r.records == nil {
		r.records = make(map[string]Evidence)
	}
	r.records[e.Provider] = e
}

// Get returns the raw evidence record for provider, or false when none is
// registered. The returned pointer is safe to use independently of the registry.
func (r *EvidenceRegistry) Get(provider string) (*Evidence, bool) {
	e, ok := r.records[provider]
	if !ok {
		return nil, false
	}
	return &e, ok
}

// Status returns the evaluated freshness of evidence for provider. A missing
// provider yields EvidenceAbsent with a provider-specific remediation reason.
func (r *EvidenceRegistry) Status(provider string, now time.Time) EvidenceStatus {
	e, ok := r.records[provider]
	if !ok {
		return EvidenceStatus{
			State: EvidenceAbsent,
			Reason: fmt.Sprintf(
				"provider %s has no recorded contract evidence; record evidence before enabling",
				provider),
		}
	}
	return EvaluateEvidence(&e, now)
}

// Providers returns all registered provider names in sorted order.
func (r *EvidenceRegistry) Providers() []string {
	out := make([]string, 0, len(r.records))
	for k := range r.records {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- Evidence gate integration -------------------------------------------

// SupportFromEvidence maps an EvidenceStatus to a QuotaSource SupportStatus.
// Fresh evidence → supported. Absent/expired/incomplete → unsupported with the
// sanitized remediation reason from the EvidenceStatus (no request should be
// made).
func SupportFromEvidence(es EvidenceStatus) SupportStatus {
	if es.State == EvidenceFresh {
		return SupportStatus{Supported: true}
	}
	return SupportStatus{Supported: false, Reason: es.Reason}
}

// --- Release gate ---------------------------------------------------------

// ValidateRelease returns the evaluated evidence status for each configured
// provider, in the same order as configured. A release is valid only when every
// returned status is EvidenceFresh. Callers (the release test) assert that;
// this function performs no assertions itself so it can be reused in doctor
// diagnostics and dry runs.
func ValidateRelease(registry *EvidenceRegistry, configured []string, now time.Time) []EvidenceStatus {
	statuses := make([]EvidenceStatus, len(configured))
	for i, p := range configured {
		statuses[i] = registry.Status(p, now)
	}
	return statuses
}
