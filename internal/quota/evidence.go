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
	Provider    string    // built-in adapter name, or a contract-specific provider key
	ContractID  string    // endpoint contract within the provider; empty for legacy single-endpoint adapters
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
type evidenceKey struct {
	provider   string
	contractID string
}

type EvidenceRegistry struct {
	records map[evidenceKey]Evidence
}

// NewEvidenceRegistry returns an empty EvidenceRegistry.
func NewEvidenceRegistry() *EvidenceRegistry {
	return &EvidenceRegistry{records: make(map[evidenceKey]Evidence)}
}

// Register adds or replaces exactly one provider endpoint contract.
func (r *EvidenceRegistry) Register(e Evidence) {
	if r.records == nil {
		r.records = make(map[evidenceKey]Evidence)
	}
	r.records[evidenceKey{provider: e.Provider, contractID: e.ContractID}] = e
}

// Get returns the provider's default contract. Codex usage is its compatibility
// default; other providers use their legacy empty contract id.
func (r *EvidenceRegistry) Get(provider string) (*Evidence, bool) {
	if e, ok := r.GetContract(provider, ""); ok {
		return e, true
	}
	if provider == codexProviderName {
		return r.GetContract(provider, CodexUsageContract)
	}
	return nil, false
}

// GetContract returns one endpoint contract without affecting sibling records.
func (r *EvidenceRegistry) GetContract(provider, contractID string) (*Evidence, bool) {
	e, ok := r.records[evidenceKey{provider: provider, contractID: contractID}]
	if !ok {
		return nil, false
	}
	return &e, true
}

// Status returns the evaluated freshness of evidence for provider. A missing
// provider yields EvidenceAbsent with a provider-specific remediation reason.
func (r *EvidenceRegistry) Status(provider string, now time.Time) EvidenceStatus {
	contractID := ""
	if provider == codexProviderName {
		contractID = CodexUsageContract
	}
	return r.StatusContract(provider, contractID, now)
}

// StatusContract evaluates one endpoint contract independently.
func (r *EvidenceRegistry) StatusContract(provider, contractID string, now time.Time) EvidenceStatus {
	e, ok := r.records[evidenceKey{provider: provider, contractID: contractID}]
	// Preserve compatibility with pre-endpoint generic Codex usage evidence.
	// Optional reset-credit evidence never falls back to this record.
	if !ok && provider == codexProviderName && contractID == CodexUsageContract {
		e, ok = r.records[evidenceKey{provider: provider}]
	}
	if !ok {
		contract := ""
		if contractID != "" {
			contract = "/" + contractID
		}
		return EvidenceStatus{
			State: EvidenceAbsent,
			Reason: fmt.Sprintf(
				"provider %s%s has no recorded contract evidence; record evidence before enabling",
				provider, contract),
		}
	}
	return EvaluateEvidence(&e, now)
}

// Providers returns all registered provider names in sorted order.
func (r *EvidenceRegistry) Providers() []string {
	seen := make(map[string]bool)
	for k := range r.records {
		seen[k.provider] = true
	}
	out := make([]string, 0, len(seen))
	for provider := range seen {
		out = append(out, provider)
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

// ValidateRelease returns evaluated evidence statuses in configured-provider
// order. Each configured Codex provider expands in contract order to usage then
// reset credits; other providers contribute one status. A release is valid only
// when every returned status is EvidenceFresh. Callers (the release test) assert that;
// this function performs no assertions itself so it can be reused in doctor
// diagnostics and dry runs.
func ValidateRelease(registry *EvidenceRegistry, configured []string, now time.Time) []EvidenceStatus {
	var statuses []EvidenceStatus
	for _, provider := range configured {
		contracts := []string{""}
		if provider == codexProviderName {
			contracts = []string{CodexUsageContract, CodexResetCreditsContract}
		}
		for _, contractID := range contracts {
			status := registry.StatusContract(provider, contractID, now)
			if status.State == EvidenceFresh {
				evidence, _ := registry.GetContract(provider, contractID)
				if evidence == nil || evidence.FixturePath == "" {
					status = EvidenceStatus{State: EvidenceIncomplete, Reason: fmt.Sprintf("provider %s/%s release evidence is incomplete (missing fixture_path); record complete evidence", provider, contractID)}
				}
			}
			statuses = append(statuses, status)
		}
	}
	return statuses
}
