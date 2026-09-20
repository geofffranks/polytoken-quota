package selection

// runner.go — orchestration for the `select` and `select-eval` commands: the
// sequenced business flow around the pure Select function. The runner owns
// only ordering, local validation, and classification:
//
//	optional one-shot quota refresh (no reconciliation)
//	→ one narrow business snapshot (desired/state/as-of)
//	→ candidate-policy parse (bounded read) and local phase/tier
//	  validation, including consent and task bounds, all before any
//	  disclosure
//	→ remote assessment (only when no explicit tier was given; the
//	  credential is resolved by the client immediately before the request)
//	→ floor applied to a valid assessment only
//	→ pure selection over the snapshot
//
// It never reads credentials, never logs or returns task text, never launches
// work or reserves quota, and never writes state.
//
// Errors are FatalError values carrying a fixed kind. Their Error() text is a
// fixed safe sentence per kind — never the raw wrapped cause — so callers can
// print them without leaking paths or configuration details; Unwrap preserves
// the cause for errors.Is internally. The safe fallback statuses — uncertain,
// no_selection, assessment_unavailable — are outcomes, not errors.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// Snapshot is the narrow business snapshot a selection runs against.
type Snapshot struct {
	Desired policy.Desired
	State   state.State
	AsOf    time.Time
}

// SnapshotSource supplies the business snapshot exactly once per invocation.
// service.Coordinator satisfies it directly with its SelectionSnapshot read —
// the runner's own snapshot type, no adapter or mirrored copy — and that read
// resolves no targets, so target diagnostic failures never reach a selection.
type SnapshotSource interface {
	SelectionSnapshot(ctx context.Context) (Snapshot, error)
}

// QuotaRefresher performs one quota check without reconciliation and finishes
// its transaction before returning. It backs the opt-in --refresh flag.
type QuotaRefresher interface {
	RefreshQuota(ctx context.Context) error
}

// Assessor performs one remote difficulty assessment. enabled is the consent
// boundary: implementations must not resolve credentials or touch the network
// while it is false. *Client satisfies this interface.
type Assessor interface {
	Assess(ctx context.Context, enabled bool, prompt string) (Assessment, error)
}

// SelectorFunc is the narrow injected pure selector. nil selects Select.
type SelectorFunc func(p Policy, desired policy.Desired, st state.State, asOf time.Time, phase string, tier Tier, excludedFamilies []string) (Result, error)

// SelectRunner orchestrates one select invocation over its injected
// dependencies. NewAssessor is required only for requests without an
// explicit tier.
type SelectRunner struct {
	Snapshot SnapshotSource
	// Refresh is required only when a request asks for --refresh.
	Refresh QuotaRefresher
	// NewAssessor constructs the assessor for the desired configuration's
	// model pin and timeout. It is invoked only after the desired-config
	// consent check passes, and the returned assessor resolves the runtime
	// credential itself, immediately before its enabled request.
	NewAssessor func(model string, timeout time.Duration) (Assessor, error)
	// Selector is the pure selection function; nil uses Select.
	Selector SelectorFunc
	// ReadPolicyFile reads the candidate policy document; nil uses the
	// package bounded reader. Injected for tests.
	ReadPolicyFile func(path string) ([]byte, error)
}

// FatalKind classifies a fatal orchestration failure with a fixed, safe
// label. The CLI maps kinds to fixed sentences; raw causes never surface.
type FatalKind string

// Fatal failure kinds.
const (
	FatalRequest    FatalKind = "request"    // invalid invocation or conflicting flags
	FatalPolicy     FatalKind = "policy"     // candidate policy or fixtures missing/unreadable/invalid
	FatalSnapshot   FatalKind = "snapshot"   // desired configuration or state unreadable/invalid
	FatalRefresh    FatalKind = "refresh"    // the opt-in quota refresh failed
	FatalConsent    FatalKind = "consent"    // disclosure not enabled in the desired configuration
	FatalCredential FatalKind = "credential" // runtime credential missing or unusable
	FatalTask       FatalKind = "task"       // task empty, not UTF-8, or over the byte bound
	FatalAssessor   FatalKind = "assessor"   // assessment client unavailable
	FatalCanceled   FatalKind = "canceled"   // the run's context ended before completion
)

// fatalSentences are the fixed, safe Error() sentences per kind. They never
// embed paths, configuration values, or causes.
var fatalSentences = map[FatalKind]string{
	FatalRequest:    "selection: invalid invocation",
	FatalPolicy:     "selection: candidate policy or fixtures are missing, unreadable, or invalid",
	FatalSnapshot:   "selection: desired configuration or state is unreadable or invalid",
	FatalRefresh:    "selection: quota refresh failed; selection stopped",
	FatalConsent:    "selection: disclosure requires selection.jev.enabled in the desired configuration",
	FatalCredential: "selection: assessment credential is unavailable",
	FatalTask:       "selection: task is empty, not valid UTF-8, or exceeds the 64 KiB limit",
	FatalAssessor:   "selection: assessment is unavailable",
	FatalCanceled:   "selection: run was canceled before completion",
}

// FatalError is a fatal orchestration failure. Error() is the fixed safe
// sentence for its kind; the raw cause stays reachable through Unwrap for
// internal errors.Is checks only.
type FatalError struct {
	Kind FatalKind
	err  error
}

// Error returns the fixed safe sentence for the failure kind. It never
// includes the raw cause.
func (e *FatalError) Error() string {
	if msg, ok := fatalSentences[e.Kind]; ok {
		return msg
	}
	return fatalSentences[FatalRequest]
}

// Unwrap exposes the wrapped cause for errors.Is classification. The cause is
// not safe to print.
func (e *FatalError) Unwrap() error { return e.err }

func fatal(kind FatalKind, cause error) *FatalError {
	return &FatalError{Kind: kind, err: cause}
}

// Fatal builds a fatal failure for a kind with an internal cause. It exists
// so collaborators and tests outside the package can construct classified
// failures; Error() always returns the fixed safe sentence for the kind.
func Fatal(kind FatalKind, cause error) *FatalError {
	return fatal(kind, cause)
}

// SelectStatus is the safe result classification of one select invocation.
type SelectStatus string

// Selection statuses. confirmed exits 0; every other status is an accepted
// result the caller must handle (exit 2); fatal failures are errors, not
// statuses (exit 1).
const (
	SelectConfirmed             SelectStatus = "confirmed"
	SelectUncertain             SelectStatus = "uncertain"
	SelectNoSelection           SelectStatus = "no_selection"
	SelectAssessmentUnavailable SelectStatus = "assessment_unavailable"
)

// Fixed reason codes describing why the outcome was reached. They are stable
// values callers can switch on; the uncertain evidence category travels
// separately in SelectOutcome.Result.Evidence.
const (
	ReasonFreshQuotaEvidence     = "fresh_quota_evidence"     // confirmed on fresh, complete, positive evidence
	ReasonUncertainQuotaEvidence = "uncertain_quota_evidence" // uncertain fallback (see Evidence)
	ReasonNoEligibleCandidate    = "no_eligible_candidate"    // every candidate excluded
	ReasonAssessmentAbstained    = "assessment_abstained"     // assessment answered insufficient_information
	ReasonAssessmentFailed       = "assessment_failed"        // assessment error, timeout, or unusable response
)

// SelectRequest is one select invocation. The CLI layer parses flags and
// stdin into it; conflict and tier-name validation belongs to the caller and
// is re-checked here.
type SelectRequest struct {
	// PolicyPath names the candidate policy document. It never names the
	// operator's desired configuration.
	PolicyPath string
	Phase      string
	// Tier is the explicit difficulty; empty means assess remotely. It
	// conflicts with MinTier.
	Tier Tier
	// MinTier is the difficulty floor: it raises a valid assessment and
	// never rescues an abstention or failed assessment.
	MinTier Tier
	// ExcludedFamilies names exact provider families (the prefix before
	// "/") to exclude from the reviewed selection.
	ExcludedFamilies []string
	// RefreshFirst performs one quota check (no reconciliation) before the
	// snapshot. Default selection reads the saved snapshot without polling.
	RefreshFirst bool
	// Prompt is the task text for remote assessment. It is ignored — and
	// the caller must not have read it — when Tier is explicit.
	Prompt string
}

// SelectOutcome is one safe selection result. It carries no task text, no
// credential material, and no raw remote response content.
type SelectOutcome struct {
	Status SelectStatus
	// Reason is the fixed reason code for the outcome.
	Reason string
	Phase  string
	// Tier is the difficulty the selection ran against; empty when no
	// usable assessment existed.
	Tier Tier
	// AssessedTier is the raw remotely assessed tier; empty when the tier
	// was explicit or the assessment produced none.
	AssessedTier Tier
	// AssessedModel is the classifier model pin that produced the
	// assessment; empty when the tier was explicit.
	AssessedModel string
	// Confidence is the validated [0,1] confidence reported by the
	// assessment; zero when the tier was explicit.
	Confidence float64
	// Probabilities is the validated normalized distribution over the
	// trusted rubric options from the assessment; nil when the tier was
	// explicit.
	Probabilities map[string]float64
	// Abstained records that the assessment answered with the documented
	// abstention rather than failing.
	Abstained bool
	// Refreshed records that a quota refresh ran before the snapshot.
	Refreshed bool
	// AsOf is the snapshot's clock sample the selection ran against.
	AsOf time.Time
	// EvidenceCheckedAt is the selected mapping's last-good quota snapshot
	// timestamp, when one exists; it dates the quota evidence behind a
	// selection.
	EvidenceCheckedAt *time.Time
	// Result is the selection; zero when Status is no_selection or
	// assessment_unavailable.
	Result Result
}

// readBoundedPolicy reads a candidate policy document with the ParsePolicy
// oversize bound enforced before full buffering: at most MaxPolicyBytes are
// ever read, and anything larger is rejected without truncation.
func readBoundedPolicy(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxPolicyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxPolicyBytes {
		return nil, fmt.Errorf("candidate policy is %d bytes, over the %d byte limit", len(data), MaxPolicyBytes)
	}
	return data, nil
}

// Run executes one select invocation. The returned error is always a
// *FatalError (CLI exit 1): invalid invocation, unreadable or invalid policy
// or configuration, unreadable state, missing consent or credential, task
// bound violations, or a failed opt-in refresh. Every other result —
// including abstentions, unusable assessments, and empty candidate sets — is
// a safe outcome with a status.
func (r *SelectRunner) Run(ctx context.Context, req SelectRequest) (SelectOutcome, error) {
	out := SelectOutcome{Phase: req.Phase}
	if req.PolicyPath == "" {
		return SelectOutcome{}, fatal(FatalRequest, errors.New("candidate policy path is required"))
	}
	if req.Phase == "" {
		return SelectOutcome{}, fatal(FatalRequest, errors.New("phase is required"))
	}
	if req.Tier != "" && req.MinTier != "" {
		return SelectOutcome{}, fatal(FatalRequest, errors.New("explicit difficulty and a difficulty floor conflict"))
	}
	if req.Tier != "" && !ValidTier(req.Tier) {
		return SelectOutcome{}, fatal(FatalRequest, fmt.Errorf("unknown difficulty %q", req.Tier))
	}
	if req.MinTier != "" && !ValidTier(req.MinTier) {
		return SelectOutcome{}, fatal(FatalRequest, fmt.Errorf("unknown difficulty floor %q", req.MinTier))
	}
	// The explicit-tier path never assesses: no task read, no credential,
	// no consent, no network.
	if req.Tier == "" {
		if r.NewAssessor == nil {
			return SelectOutcome{}, fatal(FatalAssessor, errors.New("no assessor factory configured"))
		}
		// Local task validation before anything remote can see it, using the
		// exported shared bound set (the same rule the assessor and the
		// fixture parser apply).
		if err := ValidateTask(req.Prompt); err != nil {
			return SelectOutcome{}, fatal(FatalTask, err)
		}
	}

	if req.RefreshFirst {
		if r.Refresh == nil {
			return SelectOutcome{}, fatal(FatalRefresh, errors.New("no refresher configured"))
		}
		if err := r.Refresh.RefreshQuota(ctx); err != nil {
			return SelectOutcome{}, fatal(FatalRefresh, err)
		}
		out.Refreshed = true
	}

	if r.Snapshot == nil {
		return SelectOutcome{}, fatal(FatalSnapshot, errors.New("no snapshot source configured"))
	}
	snap, err := r.Snapshot.SelectionSnapshot(ctx)
	if err != nil {
		return SelectOutcome{}, fatal(FatalSnapshot, err)
	}
	out.AsOf = snap.AsOf

	read := r.ReadPolicyFile
	if read == nil {
		read = readBoundedPolicy
	}
	data, err := read(req.PolicyPath)
	if err != nil {
		return SelectOutcome{}, fatal(FatalPolicy, err)
	}
	p, err := ParsePolicy(data, snap.Desired)
	if err != nil {
		return SelectOutcome{}, fatal(FatalPolicy, err)
	}
	// Local validation before any disclosure: the phase must exist, and
	// every locally requested tier (explicit difficulty, or the floor that
	// can become the final tier) must be covered by it. A policy may
	// define a subset of tiers per phase; missing requested tiers are
	// errors, not permission to borrow another tier.
	pp, ok := p.Phases[req.Phase]
	if !ok {
		return SelectOutcome{}, fatal(FatalPolicy, fmt.Errorf("phase %q is not in the candidate policy", req.Phase))
	}
	if req.Tier != "" {
		if _, ok := pp[req.Tier]; !ok {
			return SelectOutcome{}, fatal(FatalPolicy, fmt.Errorf("tier %q is not covered for phase %q", req.Tier, req.Phase))
		}
	}
	if req.MinTier != "" {
		if _, ok := pp[req.MinTier]; !ok {
			return SelectOutcome{}, fatal(FatalPolicy, fmt.Errorf("floor tier %q is not covered for phase %q", req.MinTier, req.Phase))
		}
	}

	tier := req.Tier
	if req.Tier == "" {
		// Consent is enforced here, before any assessor is constructed or
		// invoked: the desired configuration alone permits disclosure.
		if !snap.Desired.Selection.Jev.Enabled {
			return SelectOutcome{}, fatal(FatalConsent, errors.New("selection.jev.enabled is false"))
		}
		assessor, aerr := r.NewAssessor(snap.Desired.Selection.Jev.Model, snap.Desired.Selection.Jev.Timeout)
		if aerr != nil {
			return SelectOutcome{}, fatal(FatalAssessor, aerr)
		}
		assessment, err := assessor.Assess(ctx, true, req.Prompt)
		if err != nil {
			switch {
			case errors.Is(err, ErrNoAPIKey):
				return SelectOutcome{}, fatal(FatalCredential, err)
			case errors.Is(err, ErrPromptEmpty), errors.Is(err, ErrPromptNotUTF8), errors.Is(err, ErrPromptTooLarge):
				return SelectOutcome{}, fatal(FatalTask, err)
			case errors.Is(err, ErrAssessmentDisabled):
				return SelectOutcome{}, fatal(FatalConsent, err)
			default:
				// Timeout, cancellation, redirects, HTTP errors, malformed
				// or contradictory responses: safe unavailable result, no
				// retry.
				out.Status = SelectAssessmentUnavailable
				out.Reason = ReasonAssessmentFailed
				return out, nil
			}
		}
		// Safe assessment telemetry: the pinned classifier model, the
		// validated confidence, and the validated rubric distribution.
		out.AssessedModel = assessment.Model
		out.Confidence = assessment.Confidence
		out.Probabilities = assessment.Probabilities
		if assessment.Abstained {
			// Abstention is assessment-unavailable, not a tier; the floor
			// cannot rescue it.
			out.Status = SelectAssessmentUnavailable
			out.Reason = ReasonAssessmentAbstained
			out.Abstained = true
			return out, nil
		}
		if !ValidTier(assessment.Tier) {
			out.Status = SelectAssessmentUnavailable
			out.Reason = ReasonAssessmentFailed
			return out, nil
		}
		out.AssessedTier = assessment.Tier
		tier = assessment.Tier
		if req.MinTier != "" {
			// A floor raises a valid assessment only.
			if TierIndex(assessment.Tier) < TierIndex(req.MinTier) {
				tier = req.MinTier
			}
		}
		// An assessed tier outside the phase's tier subset is a missing
		// tier, and missing tiers are errors.
		if _, ok := pp[tier]; !ok {
			return SelectOutcome{}, fatal(FatalPolicy, fmt.Errorf("assessed tier %q is not covered for phase %q", tier, req.Phase))
		}
	}
	out.Tier = tier

	selectFn := r.Selector
	if selectFn == nil {
		selectFn = Select
	}
	result, err := selectFn(p, snap.Desired, snap.State, snap.AsOf, req.Phase, tier, req.ExcludedFamilies)
	switch {
	case err == nil:
		out.Result = result
		if result.Kind == KindConfirmed {
			out.Status = SelectConfirmed
			out.Reason = ReasonFreshQuotaEvidence
		} else {
			out.Status = SelectUncertain
			out.Reason = ReasonUncertainQuotaEvidence
		}
		// Nullable provenance: a quota snapshot without a timestamp carries
		// no evidence date — leave the pointer nil rather than publishing a
		// zero time.
		if ps, ok := snap.State.Providers[result.Mapping]; ok && ps.QuotaSnapshot != nil && !ps.QuotaSnapshot.CheckedAt.IsZero() {
			checkedAt := ps.QuotaSnapshot.CheckedAt
			out.EvidenceCheckedAt = &checkedAt
		}
	case errors.Is(err, ErrNoCandidate):
		out.Status = SelectNoSelection
		out.Reason = ReasonNoEligibleCandidate
	default:
		// An uncovered requested tier was validated above; any residual
		// pure-selector error is an orchestration inconsistency.
		return SelectOutcome{}, fatal(FatalPolicy, err)
	}
	return out, nil
}

// EvalInvocation is one select-eval invocation. Both paths are required; the
// caller gates --live before invoking a live assessor.
type EvalInvocation struct {
	// PolicyPath names the candidate policy document. It never names the
	// operator's desired configuration.
	PolicyPath string
	// FixturesPath names the synthetic fixture document.
	FixturesPath string
}

// EvaluationRunner orchestrates one select-eval invocation: local validation
// (candidate policy parse, fixture parse, coverage) before any disclosure,
// the desired-config consent boundary enforced before any assessor use, and
// one Evaluate pass over the injected assessor factory. The report it returns
// is safe by construction: case IDs, classifications, classified error
// kinds, and numerics — never task text, credentials, or raw remote
// responses.
type EvaluationRunner struct {
	Snapshot SnapshotSource
	// NewJev constructs the live assessor for the configured model pin and
	// timeout. The credential is resolved inside, immediately before each
	// enabled request. Required; the offline FakeEvalRunner is for package
	// tests, not this command.
	NewJev func(model string, timeout time.Duration) (EvalRunner, error)
	// ReadPolicyFile reads the candidate policy document; nil uses the
	// package bounded reader. Injected for tests.
	ReadPolicyFile func(path string) ([]byte, error)
}

// RunEval executes one select-eval invocation. The returned error is always a
// *FatalError (CLI exit 1): invalid local input or configuration. A returned
// report classifies exit 0 (every case matched, none policy-rejected) versus
// exit 2 (mismatches, unavailable assessments, or policy-rejected cases).
func (r *EvaluationRunner) RunEval(ctx context.Context, req EvalInvocation) (*Report, error) {
	if req.PolicyPath == "" || req.FixturesPath == "" {
		return nil, fatal(FatalRequest, errors.New("policy and fixtures paths are required"))
	}
	if r.NewJev == nil {
		return nil, fatal(FatalAssessor, errors.New("no live assessor configured"))
	}
	if r.Snapshot == nil {
		return nil, fatal(FatalSnapshot, errors.New("no snapshot source configured"))
	}
	snap, err := r.Snapshot.SelectionSnapshot(ctx)
	if err != nil {
		return nil, fatal(FatalSnapshot, err)
	}
	// Local validation before any disclosure: policy parse, fixture parse,
	// and phase/tier coverage all complete before the first remote call.
	read := r.ReadPolicyFile
	if read == nil {
		read = readBoundedPolicy
	}
	data, err := read(req.PolicyPath)
	if err != nil {
		return nil, fatal(FatalPolicy, err)
	}
	p, err := ParsePolicy(data, snap.Desired)
	if err != nil {
		return nil, fatal(FatalPolicy, err)
	}
	set, err := LoadFixtureSet(req.FixturesPath)
	if err != nil {
		return nil, fatal(FatalPolicy, err)
	}
	if err := PolicyCoverage(p, set); err != nil {
		return nil, fatal(FatalPolicy, err)
	}
	// Persistent consent is the desired configuration alone, enforced here
	// before any assessor is constructed.
	if !snap.Desired.Selection.Jev.Enabled {
		return nil, fatal(FatalConsent, errors.New("selection.jev.enabled is false"))
	}
	assessor, err := r.NewJev(snap.Desired.Selection.Jev.Model, snap.Desired.Selection.Jev.Timeout)
	if err != nil {
		return nil, fatal(FatalAssessor, err)
	}
	// Per-case policy check: an actual tier outside the candidate policy is
	// counted (policy_rejected), never fatal. OutcomeCoverage is the same
	// phase/tier encoding PolicyCoverage applies to expected outcomes — one
	// shared coverage rule for both.
	report, err := Evaluate(ctx, assessor, set, EvalOptions{ValidatePolicy: OutcomeCoverage(p)})
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The run stopped because its context ended — caller
			// cancellation or deadline — not because any policy or fixture
			// was invalid. The canceled kind carries the fixed safe
			// cancellation sentence; the context identity stays wrapped for
			// errors.Is classification.
			return nil, fatal(FatalCanceled, err)
		case errors.Is(err, ErrNoAPIKey):
			// The credential failed at assessment time: fatal for the whole
			// invocation, matching the select path — never a per-case
			// unavailable report the CLI would treat as exit 2.
			return nil, fatal(FatalCredential, err)
		}
		return nil, fatal(FatalPolicy, err)
	}
	return report, nil
}
