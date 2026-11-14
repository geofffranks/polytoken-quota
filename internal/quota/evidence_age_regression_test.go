package quota

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Every built-in endpoint must still be requested and yield a valid result at
// its former exact review deadline and far beyond it. Provider snapshot parsing
// and freshness remain governed by the provider response, not evidence age.
func TestBuiltInEvidenceReviewDateDoesNotBlockValidRequests(t *testing.T) {
	deadlines := map[string]time.Time{
		"codex":                  CodexUsageEvidence(time.Time{}).ReviewBy,
		"zai":                    ZaiEvidence(time.Time{}).ReviewBy,
		"anthropic":              AnthropicEvidence(time.Time{}).ReviewBy,
		"neuralwatt":             NeuralwattEvidence(time.Time{}).ReviewBy,
		"opencode-go":            OpenCodeGoEvidence(time.Time{}).ReviewBy,
		"anthropic-subscription": AnthropicSubscriptionEvidence(time.Time{}).ReviewBy,
		"codex-reset-credits":    CodexResetCreditsEvidence(time.Time{}).ReviewBy,
	}
	for provider, deadline := range deadlines {
		for _, now := range []time.Time{deadline, deadline.AddDate(100, 0, 0)} {
			t.Run(provider+"/"+now.Format("2006"), func(t *testing.T) {
				switch provider {
				case "codex":
					e := CodexUsageEvidence(now)
					d := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
					s := codexTestSource(t, d, &fakeResolver{val: codexTestAuthJSON}, e)
					s.Now = func() time.Time { return now }
					if snap, err := s.Fetch(context.Background()); err != nil || snap.Status != SourceFresh || len(d.calls) != 1 {
						t.Fatalf("snapshot=%+v calls=%d err=%v", snap, len(d.calls), err)
					}
				case "codex-reset-credits":
					usage := CodexUsageEvidence(now)
					reset := CodexResetCreditsEvidence(now)
					d := &sequenceDoer{responses: []*http.Response{bodyResponse(200, []byte(codexMinimalBody)), bodyResponse(200, []byte(`{"available_count":1,"credits":[{"status":"available"}]}`))}}
					s := codexSourceWithEvidence(t, d, usage, reset)
					s.Now = func() time.Time { return now }
					snap, err := s.Fetch(context.Background())
					if err != nil || snap.Status != SourceFresh || len(d.calls) != 2 || snap.ResetCredits == nil || snap.ResetCredits.Status != CreditAttemptSuccess {
						t.Fatalf("snapshot=%+v calls=%d err=%v", snap, len(d.calls), err)
					}
				case "zai":
					e := ZaiEvidence(now)
					d := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
					s := zaiTestSource(t, d, &fakeResolver{val: zaiTestKey}, e)
					s.Now = func() time.Time { return now }
					if snap, err := s.Fetch(context.Background()); err != nil || snap.Status != SourceFresh || len(d.calls) != 1 {
						t.Fatalf("snapshot=%+v calls=%d err=%v", snap, len(d.calls), err)
					}
				case "anthropic":
					e := AnthropicEvidence(now)
					d := &recordingDoer{resp: costResponse(200, costBody("12050"))}
					s := anthropicTestSource(t, d, &fakeResolver{val: anthropicTestAdminKey}, e)
					s.Now = func() time.Time { return now }
					if snap, err := s.Fetch(context.Background()); err != nil || snap.Status != SourceFresh || len(d.calls) != 1 {
						t.Fatalf("snapshot=%+v calls=%d err=%v", snap, len(d.calls), err)
					}
				case "anthropic-subscription":
					d := &subStubTransport{body: []byte(`{"five_hour":{"used_percentage":12}}`), code: http.StatusOK}
					reg := NewEvidenceRegistry()
					reg.Register(AnthropicSubscriptionEvidence(now))
					s := NewAnthropicSubscriptionSource("anthropic", &BoundedClient{Transport: d}, subLoader(subValidCreds, nil), reg, now)
					if snap, err := s.Fetch(context.Background()); err != nil || snap.Status == SourceFailed || d.req == nil {
						t.Fatalf("snapshot=%+v request=%v err=%v", snap, d.req, err)
					}
				case "neuralwatt":
					e := NeuralwattEvidence(now)
					body := fmt.Sprintf(`{"snapshot_at":%q,"balance":{"credits_remaining_usd":72.5,"total_credits_usd":100,"credits_used_usd":27.5},"subscription":null}`, now.Format(time.RFC3339))
					d := &recordingDoer{resp: bodyResponse(http.StatusOK, []byte(body))}
					reg := NewEvidenceRegistry()
					reg.Register(e)
					s := NewNeuralwattSource("test", &BoundedClient{Transport: d}, &neuralwattResolver{}, reg, now)
					if snap, err := s.Fetch(context.Background()); err != nil || snap.Status != SourceFresh || len(d.calls) != 1 {
						t.Fatalf("snapshot=%+v calls=%d err=%v", snap, len(d.calls), err)
					}
				case "opencode-go":
					d := &recordingDoer{resp: bodyResponse(200, []byte(`{"usage":{"rolling":{"status":"ok","percent":20}}}`))}
					reg := NewEvidenceRegistry()
					reg.Register(OpenCodeGoEvidence(now))
					s := NewOpenCodeGoSource("test", &BoundedClient{Transport: d}, &opencodeGoResolver{}, reg, now)
					if snap, err := s.Fetch(context.Background()); err != nil || snap.Status == SourceFailed || len(snap.Windows) == 0 || len(d.calls) != 1 {
						t.Fatalf("snapshot=%+v calls=%d err=%v", snap, len(d.calls), err)
					}
				}
			})
		}
	}
}
