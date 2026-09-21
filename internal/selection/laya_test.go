package selection

// Tests for the laya HTTP assessment adapter (laya.go). All traffic stays on
// injected stub transports; nothing here touches a network, a daemon, or a
// real checkpoint. Checkpoint names are synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// layaClient builds a laya client over the stub transport.
func layaClient(t *testing.T, transport http.RoundTripper, mutate func(*LayaClientOptions)) *LayaClient {
	t.Helper()
	opts := LayaClientOptions{Transport: transport}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := NewLayaClient(opts)
	if err != nil {
		t.Fatalf("NewLayaClient: %v", err)
	}
	return c
}

// layaBodyString marshals a valid daemon success response reporting a
// synthetic checkpoint; tweak mutates it first.
func layaBodyString(t *testing.T, tweak func(*systemOneResponse)) string {
	t.Helper()
	return string(jevBody(t, func(r *systemOneResponse) {
		r.Model = "laya-8b-checkpoint"
		if tweak != nil {
			tweak(r)
		}
	}))
}

// layaNetTimeout is a net.Error that always reports a timeout.
type layaNetTimeout struct{}

func (layaNetTimeout) Error() string   { return "synthetic i/o timeout" }
func (layaNetTimeout) Timeout() bool   { return true }
func (layaNetTimeout) Temporary() bool { return false }

func TestNewLayaClientValidation(t *testing.T) {
	if _, err := NewLayaClient(LayaClientOptions{Timeout: -time.Second}); err == nil {
		t.Fatal("negative timeout should be rejected")
	}
	c, err := NewLayaClient(LayaClientOptions{})
	if err != nil {
		t.Fatalf("NewLayaClient: %v", err)
	}
	if c.Timeout() != DefaultLayaAssessTimeout {
		t.Errorf("Timeout = %s, want %s", c.Timeout(), DefaultLayaAssessTimeout)
	}
	if c.Model() != "laya-daemon" {
		t.Errorf("Model = %q, want the fixed backend label", c.Model())
	}
}

func TestLayaDefaultTimeoutIsPolicyDefault(t *testing.T) {
	if DefaultLayaAssessTimeout != policy.DefaultLayaTimeout {
		t.Fatalf("DefaultLayaAssessTimeout = %s, want the policy default %s", DefaultLayaAssessTimeout, policy.DefaultLayaTimeout)
	}
}

func TestLayaAssessDisabledConsentBoundary(t *testing.T) {
	calls := 0
	transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be reached")
	})
	c := layaClient(t, transport, nil)
	if _, err := c.Assess(context.Background(), false, "prompt"); !errors.Is(err, ErrAssessmentDisabled) {
		t.Fatalf("err = %v, want ErrAssessmentDisabled", err)
	}
	if calls != 0 {
		t.Fatalf("transport called %d times while disabled", calls)
	}
}

func TestLayaAssessPromptBounds(t *testing.T) {
	c := layaClient(t, nil, nil)
	if _, err := c.Assess(context.Background(), true, "   "); !errors.Is(err, ErrPromptEmpty) {
		t.Fatalf("err = %v, want ErrPromptEmpty", err)
	}
}

func TestLayaAssessRequestShape(t *testing.T) {
	var captured *http.Request
	var raw []byte
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req
		var err error
		raw, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read stub request body: %v", err)
		}
		return jevResponse(http.StatusOK, layaBodyString(t, nil), req), nil
	})
	c := layaClient(t, transport, nil)
	if _, err := c.Assess(context.Background(), true, "refactor the reconciler lock"); err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if captured.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.Method)
	}
	if captured.URL.String() != LayaEndpoint {
		t.Errorf("url = %q, want the fixed loopback endpoint %q", captured.URL.String(), LayaEndpoint)
	}
	if got := captured.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want none (loopback daemon needs no credential)", got)
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	// No model pin: the daemon owns the checkpoint, so the request must not
	// carry a model key at all.
	if strings.Contains(string(raw), `"model"`) {
		t.Errorf("request carries a model key; the daemon owns the pin:\n%s", raw)
	}
	var decoded systemOneRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode stub request: %v", err)
	}
	if decoded.State != "refactor the reconciler lock" {
		t.Errorf("state = %q, want the task prompt only", decoded.State)
	}
	q, ok := decoded.Questions[QuestionID]
	if !ok {
		t.Fatalf("questions missing the rubric id %q:\n%s", QuestionID, raw)
	}
	if q.Type != "choice" || len(q.Criteria) != len(rubricDescriptions) {
		t.Errorf("rubric question malformed: %+v", q)
	}
	if _, ok := q.Criteria[AbstentionOption]; !ok {
		t.Errorf("rubric criteria missing the abstention option")
	}
}

func TestLayaAssessSuccessReportsDaemonCheckpoint(t *testing.T) {
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, layaBodyString(t, nil), req), nil
	})
	c := layaClient(t, transport, nil)
	a, err := c.Assess(context.Background(), true, "prompt")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Model != "laya-8b-checkpoint" {
		t.Errorf("Model = %q, want the daemon-reported checkpoint", a.Model)
	}
	if a.Tier != TierNormal || a.Abstained {
		t.Errorf("tier/abstained = %q/%v", a.Tier, a.Abstained)
	}
	if a.Confidence != 0.8 {
		t.Errorf("Confidence = %v", a.Confidence)
	}
	if a.InputTokens != 120 || a.OutputTokens != 15 {
		t.Errorf("tokens = %d/%d", a.InputTokens, a.OutputTokens)
	}
}

func TestLayaTransportFailuresAreSanitized(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
		kind string
	}{
		{name: "transport", err: errors.New("dial tcp 127.0.0.1:8742: connect: connection refused"), want: ErrTransport, kind: "transport"},
		{name: "timeout", err: layaNetTimeout{}, want: ErrTimeout, kind: "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.err
			})
			c := layaClient(t, transport, nil)
			_, err := c.Assess(context.Background(), true, "prompt")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := SafeErrorKind(err); got != tc.kind {
				t.Errorf("SafeErrorKind = %q, want %q", got, tc.kind)
			}
			if tc.name == "transport" && strings.Contains(err.Error(), "connection refused") {
				t.Errorf("error echoes transport detail: %v", err)
			}
		})
	}
}

func TestLayaRefusesRedirects(t *testing.T) {
	calls := 0
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		resp := jevResponse(http.StatusFound, "", req)
		resp.Header.Set("Location", LayaEndpoint+"/elsewhere")
		return resp, nil
	})
	c := layaClient(t, transport, nil)
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

func TestLayaRemoteStatusClassifiedAndHidden(t *testing.T) {
	cases := []struct {
		name   string
		status int
		kind   string
	}{
		{name: "daemon-loading-503", status: http.StatusServiceUnavailable, kind: RemoteServer},
		{name: "server-500", status: http.StatusInternalServerError, kind: RemoteServer},
		{name: "bad-request-400", status: http.StatusBadRequest, kind: RemoteUnexpected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return jevResponse(tc.status, `{"error":"DAEMON-BODY-CANARY"}`, req), nil
			})
			c := layaClient(t, transport, nil)
			_, err := c.Assess(context.Background(), true, "prompt")
			var remote *RemoteError
			if !errors.As(err, &remote) {
				t.Fatalf("err = %v, want a RemoteError", err)
			}
			if remote.Status != tc.status || remote.Kind != tc.kind {
				t.Errorf("remote = %d/%s, want %d/%s", remote.Status, remote.Kind, tc.status, tc.kind)
			}
			if strings.Contains(err.Error(), "DAEMON-BODY-CANARY") {
				t.Errorf("error echoes daemon body: %v", err)
			}
			if got := SafeErrorKind(err); got != "remote_"+tc.kind {
				t.Errorf("SafeErrorKind = %q", got)
			}
		})
	}
}

func TestLayaStrictChoiceContractShared(t *testing.T) {
	cases := []struct {
		name   string
		tweak  func(*systemOneResponse)
		wantFx string
	}{
		{
			name:   "probabilities missing",
			wantFx: "probabilities missing",
			tweak:  func(r *systemOneResponse) { jevTweakAnswer(r, func(a *choiceAnswer) { a.Probabilities = nil }) },
		},
		{
			name:   "choice not maximum",
			wantFx: "not the maximum-probability",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) { a.Choice = string(TierRoutine) })
			},
		},
		{
			name:   "not normalized",
			wantFx: "normalized",
			tweak: func(r *systemOneResponse) {
				jevTweakAnswer(r, func(a *choiceAnswer) {
					dist := jevValidDist()
					dist[string(TierNormal)] = jevFptr(0.5) // now sums to 0.8
					a.Probabilities = dist
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return jevResponse(http.StatusOK, layaBodyString(t, tc.tweak), req), nil
			})
			c := layaClient(t, transport, nil)
			_, err := c.Assess(context.Background(), true, "prompt")
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
			if !strings.Contains(err.Error(), tc.wantFx) {
				t.Errorf("error %q does not mention %q", err, tc.wantFx)
			}
		})
	}
}

func TestLayaDaemonModelValidation(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		wantFx string
	}{
		{name: "empty", model: "", wantFx: "daemon model missing or oversized"},
		{name: "control-characters", model: "laya\x01", wantFx: "control characters"},
		{name: "oversized", model: strings.Repeat("l", maxLayaModelLen+1), wantFx: "daemon model missing or oversized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := layaBodyString(t, func(r *systemOneResponse) { r.Model = tc.model })
			transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return jevResponse(http.StatusOK, body, req), nil
			})
			c := layaClient(t, transport, nil)
			_, err := c.Assess(context.Background(), true, "prompt")
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
			if !strings.Contains(err.Error(), tc.wantFx) {
				t.Errorf("error %q does not mention %q", err, tc.wantFx)
			}
		})
	}
}

func TestLayaEvalRunnerConsentAndLabel(t *testing.T) {
	transport := jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, layaBodyString(t, nil), req), nil
	})
	client := layaClient(t, transport, nil)

	disabled := &LayaEvalRunner{Client: client, Enabled: false}
	if _, err := disabled.Assess(context.Background(), EvalRequest{ID: "case-1", Prompt: "p"}); !errors.Is(err, ErrAssessmentDisabled) {
		t.Fatalf("err = %v, want ErrAssessmentDisabled", err)
	}
	enabled := &LayaEvalRunner{Client: client, Enabled: true}
	a, err := enabled.Assess(context.Background(), EvalRequest{ID: "case-1", Prompt: "p"})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Model != "laya-8b-checkpoint" {
		t.Errorf("Model = %q", a.Model)
	}
	if enabled.Model() != "laya-daemon" {
		t.Errorf("Model() = %q, want the fixed backend label", enabled.Model())
	}
}
