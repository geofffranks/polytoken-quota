package selection

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Offline evaluation harness for Jev difficulty assessment.
//
// Evaluate runs a strict, synthetic fixture set (id, phase, prompt, expected
// tier/abstention) through a EvalRunner and produces a Report containing only
// safe data: case IDs, expected/actual outcomes, classified error kinds,
// confusion counts, under/over-classification and abstention counts/rates,
// the classifier model and rubric version, latency, and token usage. Task
// text and raw remote responses never enter the report.
//
// The offline path (FakeEvalRunner) validates plumbing and report arithmetic
// only — never Jev accuracy or real latency (docs/selection.md).

// fixtureSchemaVersion is the only fixture schema this build parses.
const fixtureSchemaVersion = 1

// Outcome is the classification side of an assessment: a difficulty tier or
// an abstention. Abstention is not a fifth tier.
type Outcome struct {
	Tier      Tier `json:"tier"`
	Abstained bool `json:"abstained"`
}

// Equal reports whether two outcomes classify identically.
func (o Outcome) Equal(other Outcome) bool {
	if o.Abstained || other.Abstained {
		return o.Abstained == other.Abstained
	}
	return o.Tier == other.Tier
}

// ExpectedOutcome is the fixture-file form of an expected classification:
// exactly one of a tier or an abstention.
type ExpectedOutcome struct {
	Tier       Tier `yaml:"tier"`
	Abstention bool `yaml:"abstention"`
}

func (e ExpectedOutcome) outcome() Outcome {
	return Outcome{Tier: e.Tier, Abstained: e.Abstention}
}

// Fixture is one synthetic evaluation case.
type Fixture struct {
	ID       string          `yaml:"id"`
	Phase    string          `yaml:"phase"`
	Prompt   string          `yaml:"prompt"`
	Expected ExpectedOutcome `yaml:"expected"`
}

// Outcome returns the fixture's expected classification.
func (f *Fixture) Outcome() Outcome { return f.Expected.outcome() }

// FixtureSet is a parsed, validated fixture document.
type FixtureSet struct {
	Rubric   string
	Fixtures []Fixture
}

// fixtureDoc mirrors the YAML document shape.
type fixtureDoc struct {
	Version  int       `yaml:"version"`
	Rubric   string    `yaml:"rubric"`
	Fixtures []Fixture `yaml:"fixtures"`
}

// MaxFixtureBytes bounds a fixture document. Fixture sets are synthetic and
// small — a handful of prompts, each already capped at MaxPromptBytes, plus
// schema metadata — so this generous ceiling is never a working limit; it
// exists only so a stray or hostile file cannot be buffered whole before
// validation.
const MaxFixtureBytes = 256 << 10

// MaxFixtureCases conservatively bounds how many cases one fixture document
// may declare. A live evaluation performs one real, paid remote request per
// case, and the shipped synthetic set (docs/selection-fixtures.yaml, 15
// cases) is the intended scale; 64 admits legitimate growth and rubric
// coverage experiments while turning an accidentally or hostilely inflated
// document into a parse rejection before any request can be sent. Raise the
// bound deliberately, with a documented reason, when a larger shipped set is
// actually wanted.
const MaxFixtureCases = 64

// LoadFixtureSet reads and parses a fixture document from disk. At most
// MaxFixtureBytes+1 bytes are ever read: anything larger is rejected with a
// fixed error before an unbounded allocation.
func LoadFixtureSet(path string) (*FixtureSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("selection: fixtures: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFixtureBytes+1))
	if err != nil {
		return nil, fmt.Errorf("selection: fixtures: %w", err)
	}
	if len(data) > MaxFixtureBytes {
		return nil, fmt.Errorf("selection: fixtures: document is %d bytes, over the %d byte limit", len(data), MaxFixtureBytes)
	}
	return ParseFixtureSet(data)
}

// ParseFixtureSet strictly parses a fixture document: known fields only,
// schema version and rubric pinned, unique non-empty IDs, non-empty phases,
// prompts satisfying the assessment bounds, and expected outcomes that are
// exactly one of a known tier or an abstention.
func ParseFixtureSet(data []byte) (*FixtureSet, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("selection: fixtures: document is not valid UTF-8")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc fixtureDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("selection: fixtures: %w", err)
	}
	var extra struct{}
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("selection: fixtures: unexpected trailing document")
	}
	if doc.Version != fixtureSchemaVersion {
		return nil, fmt.Errorf("selection: fixtures: unsupported schema version %d (want %d)", doc.Version, fixtureSchemaVersion)
	}
	if doc.Rubric != RubricID {
		return nil, fmt.Errorf("selection: fixtures: rubric %q is not %q", doc.Rubric, RubricID)
	}
	if len(doc.Fixtures) == 0 {
		return nil, errors.New("selection: fixtures: document contains no fixtures")
	}
	if len(doc.Fixtures) > MaxFixtureCases {
		return nil, fmt.Errorf("selection: fixtures: document declares %d fixtures, over the %d case limit", len(doc.Fixtures), MaxFixtureCases)
	}
	seen := make(map[string]bool, len(doc.Fixtures))
	for i := range doc.Fixtures {
		f := &doc.Fixtures[i]
		if err := validateFixture(f); err != nil {
			return nil, fmt.Errorf("selection: fixtures: fixture %d: %w", i+1, err)
		}
		if seen[f.ID] {
			return nil, fmt.Errorf("selection: fixtures: duplicate fixture id %q", f.ID)
		}
		seen[f.ID] = true
	}
	return &FixtureSet{Rubric: doc.Rubric, Fixtures: doc.Fixtures}, nil
}

// validateFixture enforces one fixture's structural contract.
func validateFixture(f *Fixture) error {
	if strings.TrimSpace(f.ID) == "" {
		return errors.New("id must be nonempty")
	}
	if strings.TrimSpace(f.Phase) == "" {
		return errors.New("phase must be nonempty")
	}
	if err := ValidateTask(f.Prompt); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	hasTier := f.Expected.Tier != ""
	if hasTier && !ValidTier(f.Expected.Tier) {
		return fmt.Errorf("unknown expected tier %q", f.Expected.Tier)
	}
	if hasTier && f.Expected.Abstention {
		return errors.New("expected must be a tier or an abstention, not both")
	}
	if !hasTier && !f.Expected.Abstention {
		return errors.New("expected must declare exactly one of tier or abstention")
	}
	return nil
}

// coveredOutcome is the single phase/tier coverage encoding: phase must
// exist in the policy and, unless the outcome is an abstention (abstention
// is not a tier and references only the phase), the outcome's tier must be
// covered by that phase. Missing coverage is an error, never a borrow from
// another tier (docs/selection.md). PolicyCoverage applies it to expected
// outcomes and OutcomeCoverage to actual outcomes.
func coveredOutcome(p Policy, phase string, o Outcome) error {
	pp, ok := p.Phases[phase]
	if !ok {
		return fmt.Errorf("phase %q is not covered by the candidate policy", phase)
	}
	if o.Abstained {
		return nil
	}
	if _, ok := pp[o.Tier]; !ok {
		return fmt.Errorf("tier %q is not covered for phase %q", o.Tier, phase)
	}
	return nil
}

// PolicyCoverage verifies that every fixture's phase — and, for tier
// expectations, its tier — exists in the supplied candidate policy, using
// the shared coveredOutcome encoding. Per docs/selection.md, missing
// phase/tier coverage is an error; coverage is never borrowed from another
// tier. Abstention fixtures reference only the phase.
func PolicyCoverage(p Policy, set *FixtureSet) error {
	if set == nil {
		return errors.New("selection: fixtures: nil fixture set")
	}
	for i := range set.Fixtures {
		f := &set.Fixtures[i]
		if err := coveredOutcome(p, f.Phase, f.Outcome()); err != nil {
			return fmt.Errorf("selection: fixtures: fixture %q: %w", f.ID, err)
		}
	}
	return nil
}

// OutcomeCoverage returns an EvalOptions.ValidatePolicy function that marks
// an actual outcome policy-rejected when its phase is not in the candidate
// policy or its tier is not covered by that phase. Abstentions are never
// rejected: abstention is an outcome, not a tier. It is the per-case
// counterpart of PolicyCoverage and shares its coverage encoding, so
// expected-outcome preflight and actual-outcome reporting can never drift.
func OutcomeCoverage(p Policy) func(phase string, actual Outcome) error {
	return func(phase string, actual Outcome) error {
		return coveredOutcome(p, phase, actual)
	}
}

// EvalRequest is one evaluation case handed to a EvalRunner. Only Prompt reaches the
// remote API; ID and Phase are harness bookkeeping.
type EvalRequest struct {
	ID     string
	Phase  string
	Prompt string
}

// EvalRunner performs one assessment per evaluation case. Implementations must
// be offline-safe or explicitly gated by their caller.
type EvalRunner interface {
	Model() string
	Assess(ctx context.Context, req EvalRequest) (Assessment, error)
}

// JevEvalRunner adapts an assessment Client to the EvalRunner interface. Enabled is
// the consent boundary; when false every assessment reports disabled.
type JevEvalRunner struct {
	Client  *Client
	Enabled bool
}

// Model returns the pinned classifier model.
func (r *JevEvalRunner) Model() string { return r.Client.Model() }

// Assess performs the remote assessment for one case, sending only the
// prompt as state.
func (r *JevEvalRunner) Assess(ctx context.Context, req EvalRequest) (Assessment, error) {
	return r.Client.Assess(ctx, r.Enabled, req.Prompt)
}

// FakeEvalRunner is an offline, scripted EvalRunner for plumbing and report
// arithmetic tests. Responses are keyed by fixture ID; Errors take
// precedence.
type FakeEvalRunner struct {
	ModelName string
	Responses map[string]Assessment
	Errors    map[string]error
}

// Model returns the scripted model name, or "fake-offline" when unset.
func (f *FakeEvalRunner) Model() string {
	if f.ModelName == "" {
		return "fake-offline"
	}
	return f.ModelName
}

// Assess returns the scripted response for the fixture ID.
func (f *FakeEvalRunner) Assess(_ context.Context, req EvalRequest) (Assessment, error) {
	if err := f.Errors[req.ID]; err != nil {
		return Assessment{}, err
	}
	if a, ok := f.Responses[req.ID]; ok {
		return a, nil
	}
	return Assessment{}, fmt.Errorf("fake runner: no scripted response for fixture %q", req.ID)
}

// EvalOptions tunes an evaluation run.
type EvalOptions struct {
	// Now measures latency; tests inject a deterministic clock. nil uses
	// time.Now.
	Now func() time.Time

	// ValidatePolicy, when set, is called for each successfully assessed
	// case with the case's phase and actual outcome. A returned error marks
	// the record policy-rejected (counted, never fatal). Wire
	// selection.OutcomeCoverage(policy) here to enforce candidate-policy
	// coverage per case; PolicyCoverage remains the expected-outcome
	// preflight over a whole fixture set.
	ValidatePolicy func(phase string, actual Outcome) error
}

// Record is the per-case outcome row of a report. Every field is safe to
// log: IDs, classifications, classified error kinds, and numerics.
type Record struct {
	ID             string  `json:"id"`
	Phase          string  `json:"phase"`
	Expected       Outcome `json:"expected"`
	Actual         Outcome `json:"actual"`
	Match          bool    `json:"match"`
	Err            string  `json:"error,omitempty"`
	PolicyRejected bool    `json:"policy_rejected,omitempty"`
	LatencyMS      float64 `json:"latency_ms"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
}

// LatencyStats summarizes per-attempt latency in milliseconds.
type LatencyStats struct {
	Samples int     `json:"samples"`
	MeanMS  float64 `json:"mean_ms"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	MaxMS   float64 `json:"max_ms"`
}

// ConfusionMatrix counts expected-tier rows against actual-tier columns.
type ConfusionMatrix struct {
	Tiers  []Tier  `json:"tiers"`
	Counts [][]int `json:"counts"`
}

// NewConfusionMatrix returns an empty square matrix over the four tiers in
// increasing difficulty order.
func NewConfusionMatrix() ConfusionMatrix {
	tiers := make([]Tier, len(Tiers))
	copy(tiers, Tiers)
	counts := make([][]int, len(tiers))
	for i := range counts {
		counts[i] = make([]int, len(tiers))
	}
	return ConfusionMatrix{Tiers: tiers, Counts: counts}
}

func (m *ConfusionMatrix) add(expected, actual Tier) {
	e, a := TierIndex(expected), TierIndex(actual)
	if e < 0 || a < 0 {
		return
	}
	m.Counts[e][a]++
}

// Report aggregates one evaluation run. It contains no task text, no
// credentials, and no raw remote responses — only safe numeric and
// classification data.
type Report struct {
	Model  string `json:"model"`
	Rubric string `json:"rubric"`

	Total    int `json:"total"`
	Assessed int `json:"assessed"`
	Matches  int `json:"matches"`
	Errors   int `json:"errors"`

	PolicyFailures int `json:"policy_failures"`

	AbstainedCount int `json:"abstained_count"`
	TierComparable int `json:"tier_comparable"`
	UnderCount     int `json:"under_count"`
	OverCount      int `json:"over_count"`

	UnderRate                float64          `json:"under_rate"`
	OverRate                 float64          `json:"over_rate"`
	AbstentionRate           float64          `json:"abstention_rate"`
	AbstentionByExpectedTier map[Tier]float64 `json:"abstention_by_expected_tier"`

	TotalInputTokens  int64 `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`

	Latency   LatencyStats    `json:"latency"`
	Confusion ConfusionMatrix `json:"confusion"`
	Records   []Record        `json:"records"`
}

// Evaluate runs every fixture in the set through the runner and aggregates
// the report. Context cancellation aborts the run; all-fixture runs are
// deterministic given a deterministic runner and clock.
//
// A missing assessment credential (ErrNoAPIKey) is fatal for the whole run:
// Evaluate stops immediately — without assessing the remaining cases — and
// returns no report. Callers map it to a fatal credential failure. Every
// other runner error (remote HTTP failures, timeouts, malformed responses)
// stays a per-case record in the report.
//
// Rate conventions:
//   - assessed: cases whose runner call returned an assessment (no error)
//   - abstention_rate: abstained assessments over assessed
//   - tier-comparable: cases where expected and actual are both tiers
//   - under/over rates: misclassifications below/above the expected tier
//     over tier-comparable cases
//   - abstention_by_expected_tier: per expected tier, abstained over assessed
func Evaluate(ctx context.Context, runner EvalRunner, set *FixtureSet, opts EvalOptions) (*Report, error) {
	if runner == nil {
		return nil, errors.New("selection: evaluate requires a runner")
	}
	if set == nil || len(set.Fixtures) == 0 {
		return nil, errors.New("selection: evaluate requires a fixture set")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	report := &Report{
		Model:                    runner.Model(),
		Rubric:                   set.Rubric,
		Total:                    len(set.Fixtures),
		AbstentionByExpectedTier: make(map[Tier]float64),
		Records:                  make([]Record, 0, len(set.Fixtures)),
		Confusion:                NewConfusionMatrix(),
	}
	var latencies []float64
	abstainTotalByTier := make(map[Tier]int)
	abstainedByTier := make(map[Tier]int)

	for i := range set.Fixtures {
		f := &set.Fixtures[i]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec := Record{
			ID:       f.ID,
			Phase:    f.Phase,
			Expected: f.Outcome(),
		}
		start := now()
		assessment, err := runner.Assess(ctx, EvalRequest{ID: f.ID, Phase: f.Phase, Prompt: f.Prompt})
		rec.LatencyMS = float64(now().Sub(start)) / float64(time.Millisecond)
		latencies = append(latencies, rec.LatencyMS)

		if err != nil {
			if errors.Is(err, ErrNoAPIKey) {
				// A missing credential fails every remaining case
				// identically: stop the run now and report nothing. This is
				// a fatal condition, not a per-case unavailable. The
				// context-cancellation path below stays separate.
				return nil, fmt.Errorf("selection: evaluation stopped: %w", err)
			}
			rec.Err = SafeErrorKind(err)
			report.Errors++
		} else {
			rec.Actual = Outcome{Tier: assessment.Tier, Abstained: assessment.Abstained}
			rec.Match = rec.Actual.Equal(rec.Expected)
			rec.InputTokens = assessment.InputTokens
			rec.OutputTokens = assessment.OutputTokens
			report.Assessed++
			report.TotalInputTokens += assessment.InputTokens
			report.TotalOutputTokens += assessment.OutputTokens
			if rec.Match {
				report.Matches++
			}
			if assessment.Abstained {
				report.AbstainedCount++
			}
			if !rec.Expected.Abstained && !rec.Actual.Abstained &&
				ValidTier(rec.Expected.Tier) && ValidTier(rec.Actual.Tier) {
				report.TierComparable++
				report.Confusion.add(rec.Expected.Tier, rec.Actual.Tier)
				expectedRank, actualRank := TierIndex(rec.Expected.Tier), TierIndex(rec.Actual.Tier)
				switch {
				case actualRank < expectedRank:
					report.UnderCount++
				case actualRank > expectedRank:
					report.OverCount++
				}
			}
			if ValidTier(rec.Expected.Tier) {
				abstainTotalByTier[rec.Expected.Tier]++
				if assessment.Abstained {
					abstainedByTier[rec.Expected.Tier]++
				}
			}
			if opts.ValidatePolicy != nil {
				if perr := opts.ValidatePolicy(f.Phase, rec.Actual); perr != nil {
					rec.PolicyRejected = true
					report.PolicyFailures++
				}
			}
		}
		report.Records = append(report.Records, rec)
	}

	if report.Assessed > 0 {
		report.AbstentionRate = float64(report.AbstainedCount) / float64(report.Assessed)
	}
	if report.TierComparable > 0 {
		report.UnderRate = float64(report.UnderCount) / float64(report.TierComparable)
		report.OverRate = float64(report.OverCount) / float64(report.TierComparable)
	}
	for _, tier := range Tiers {
		if n := abstainTotalByTier[tier]; n > 0 {
			report.AbstentionByExpectedTier[tier] = float64(abstainedByTier[tier]) / float64(n)
		}
	}
	report.Latency = latencyStats(latencies)
	return report, nil
}

// latencyStats summarizes latencies with nearest-rank percentiles.
func latencyStats(ms []float64) LatencyStats {
	if len(ms) == 0 {
		return LatencyStats{}
	}
	sorted := append([]float64(nil), ms...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	return LatencyStats{
		Samples: len(sorted),
		MeanMS:  sum / float64(len(sorted)),
		P50MS:   nearestRank(sorted, 0.50),
		P95MS:   nearestRank(sorted, 0.95),
		MaxMS:   sorted[len(sorted)-1],
	}
}

// nearestRank returns the p-quantile of a sorted slice (nearest-rank method).
func nearestRank(sorted []float64, p float64) float64 {
	n := int(math.Ceil(p * float64(len(sorted))))
	if n < 1 {
		n = 1
	}
	return sorted[n-1]
}
