// Local laya-daemon assessment adapter.
//
// This file implements the opt-in local task-difficulty assessment over a
// laya-mcp daemon's /predict endpoint. It evaluates the same trusted,
// code-defined difficulty rubric — QuestionID, RubricID, and
// DifficultyQuestion are shared with the Jev adapter — against the task
// prompt and nothing else.
//
// Differences from the Jev adapter are deliberate and minimal:
//   - The endpoint is fixed to the documented loopback daemon address;
//     there is deliberately no configuration surface for it, matching the
//     Jev posture. An operator needing a different port runs the daemon on
//     the documented one.
//   - There is no credential: the daemon listens on loopback and requires
//     no authentication, so no key resolver exists and ErrNoAPIKey can
//     never surface from this backend.
//   - There is no per-request model pin: the daemon pins its checkpoint at
//     startup (LAYA_MCP_MODEL and friends). The checkpoint the daemon
//     reports in its response is validated and surfaced as the assessment
//     model so reports name the checkpoint that actually judged the task.
//
// Security posture (docs/selection.md, AGENTS.md), shared with jev.go:
//   - Exactly one HTTP request per assessment; redirects are refused and
//     there are no retries.
//   - The prompt is bounded (nonempty after trimming, valid UTF-8, at most
//     64 KiB) and the encoded request and response bodies are capped.
//   - All transport and daemon failures surface as fixed, sanitized
//     sentinels: no underlying error text, URL details, or body content is
//     ever propagated. context.Canceled and context.DeadlineExceeded are
//     preserved as themselves.
//   - A daemon that is not running, or answers 503 because its model is
//     still loading, is an assessment-unavailable outcome, never a fatal
//     configuration error: callers fall back exactly as they do for Jev.
package selection

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// LayaEndpoint is the fixed laya-mcp daemon prediction endpoint. Loopback
// only: the daemon is an operator-run local service, and fixing the address
// keeps the disclosure surface reviewable in one place.
const LayaEndpoint = "http://127.0.0.1:8742/predict"

// DefaultLayaAssessTimeout is the positive per-attempt timeout applied when
// the caller does not configure one. It delegates to the policy package's
// DefaultLayaTimeout — the documented selection.laya default — so the
// desired-config default and the client fallback are one bound, not two.
const DefaultLayaAssessTimeout = policy.DefaultLayaTimeout

// LayaClientOptions configures a LayaClient. The endpoint is not
// configurable.
type LayaClientOptions struct {
	// Transport is the HTTP transport. nil uses http.DefaultTransport.
	// Tests inject a stub here; production leaves it nil.
	Transport http.RoundTripper

	// Timeout is the positive per-attempt timeout covering the whole
	// exchange including body read. Zero selects DefaultLayaAssessTimeout.
	Timeout time.Duration
}

// LayaClient performs single-shot laya difficulty assessments against the
// fixed loopback daemon endpoint. It is safe for concurrent use. The zero
// value is not usable; construct with NewLayaClient. It satisfies the
// Assessor interface.
type LayaClient struct {
	timeout time.Duration
	http    *http.Client
}

// maxLayaModelLen bounds the daemon-reported checkpoint name before it is
// surfaced in an Assessment.
const maxLayaModelLen = 128

// NewLayaClient constructs a local assessment client. Timeout must not be
// negative; zero selects DefaultLayaAssessTimeout.
func NewLayaClient(opts LayaClientOptions) (*LayaClient, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultLayaAssessTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("selection: laya timeout must be positive, got %s", opts.Timeout)
	}
	return &LayaClient{
		timeout: timeout,
		http: &http.Client{
			Transport: opts.Transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return ErrRedirectNotAllowed
			},
		},
	}, nil
}

// Model returns the fixed backend label. The per-assessment checkpoint is
// reported by the daemon in each response and surfaced in the Assessment;
// there is no client-side pin to return here.
func (c *LayaClient) Model() string { return "laya-daemon" }

// Timeout returns the effective positive per-attempt timeout.
func (c *LayaClient) Timeout() time.Duration { return c.timeout }

// Assess evaluates the task prompt's difficulty tier against the local laya
// daemon. It performs work only when enabled is true — that explicit flag is
// the consent boundary. The returned error is nil on success,
// ErrAssessmentDisabled when consent is withheld, or a sanitized classified
// error otherwise (see SafeErrorKind). Underlying transport error text is
// never propagated; context.Canceled and context.DeadlineExceeded remain
// recognizable.
func (c *LayaClient) Assess(ctx context.Context, enabled bool, prompt string) (Assessment, error) {
	if !enabled {
		return Assessment{}, ErrAssessmentDisabled
	}
	if err := ValidateTask(prompt); err != nil {
		return Assessment{}, err
	}
	if err := ctx.Err(); err != nil {
		return Assessment{}, fmt.Errorf("selection: assessment not started: %w", err)
	}

	// The rubric question is shared with the Jev backend; the model field is
	// omitted because the daemon owns the checkpoint pin.
	body, err := json.Marshal(systemOneRequest{
		State:     prompt,
		Questions: map[string]choiceQuestion{QuestionID: DifficultyQuestion()},
	})
	if err != nil {
		// Unreachable: every field is JSON-safe by construction.
		return Assessment{}, fmt.Errorf("selection: encode request: %w", err)
	}
	if len(body) > MaxRequestBytes {
		return Assessment{}, ErrRequestTooLarge
	}

	callCtx := ctx
	cancel := context.CancelFunc(nil)
	if c.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, LayaEndpoint, bytes.NewReader(body))
	if err != nil {
		// Unreachable with the fixed endpoint; kept for completeness.
		return Assessment{}, ErrTransport
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Same sanitization contract as the Jev client: the loopback address
		// is fixed and public, but transport chatter still carries nothing
		// worth propagating.
		return Assessment{}, sanitizeTransportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Bounded drain to release the connection; content is never read
		// into the error. A 503 (daemon model still loading) classifies as a
		// remote server error: assessment-unavailable, never fatal.
		drain(resp.Body)
		return Assessment{}, remoteErrorFor(resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return Assessment{}, sanitizeTransportError(err)
	}
	if len(payload) > MaxResponseBytes {
		return Assessment{}, ErrResponseTooLarge
	}
	return parseLayaAssessment(payload)
}

// parseLayaAssessment decodes and strictly validates a daemon response using
// the shared response contract, and surfaces the daemon-reported checkpoint
// as the assessment model after bounding it. A daemon that reports no model,
// an oversized model, or a model containing control characters yields a
// malformed-response error: reports never carry unbounded remote-controlled
// strings.
func parseLayaAssessment(payload []byte) (Assessment, error) {
	var parsed systemOneResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return Assessment{}, fmt.Errorf("%w: invalid json", ErrMalformedResponse)
	}
	model := parsed.Model
	if model == "" || len(model) > maxLayaModelLen {
		return Assessment{}, fmt.Errorf("%w: daemon model missing or oversized", ErrMalformedResponse)
	}
	for _, r := range model {
		if r < ' ' || r == 0x7f {
			return Assessment{}, fmt.Errorf("%w: daemon model contains control characters", ErrMalformedResponse)
		}
	}
	if !utf8.ValidString(model) {
		return Assessment{}, fmt.Errorf("%w: daemon model is not valid UTF-8", ErrMalformedResponse)
	}
	return buildAssessment(parsed, model)
}

// LayaEvalRunner adapts a LayaClient to the EvalRunner interface, mirroring
// JevEvalRunner. Enabled is the consent boundary; when false every
// assessment reports disabled.
type LayaEvalRunner struct {
	Client  *LayaClient
	Enabled bool
}

// Model returns the backend label; per-assessment checkpoints appear in each
// Assessment.
func (r *LayaEvalRunner) Model() string { return r.Client.Model() }

// Assess performs the local assessment for one case, sending only the
// prompt as state.
func (r *LayaEvalRunner) Assess(ctx context.Context, req EvalRequest) (Assessment, error) {
	return r.Client.Assess(ctx, r.Enabled, req.Prompt)
}

// Compile-time interface checks.
var (
	_ Assessor   = (*LayaClient)(nil)
	_ EvalRunner = (*LayaEvalRunner)(nil)
)
