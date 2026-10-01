package contract

// Shared stub-provider test helper. It was previously defined in the
// model-groups feasibility suite; the provider-notice contract still uses it.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubProviderReq struct {
	Method     string
	Path       string
	AuthHeader string
	Model      string
	Stream     bool
}

// stubProvider is a minimal OpenAI chat-completions compatible provider bound
// to the loopback interface. It answers streaming requests with a short SSE
// completion and non-streaming requests with plain JSON. It never performs an
// outbound request of its own.
type stubProvider struct {
	srv  *httptest.Server
	URL  string // base URL up to and including /v1
	mu   chan struct{}
	reqs []stubProviderReq
}

func newStubProvider(t *testing.T) *stubProvider {
	t.Helper()
	sp := &stubProvider{mu: make(chan struct{}, 1)}
	sp.mu <- struct{}{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		<-sp.mu
		sp.reqs = append(sp.reqs, stubProviderReq{
			Method:     r.Method,
			Path:       r.URL.Path,
			AuthHeader: r.Header.Get("Authorization"),
			Model:      body.Model,
			Stream:     body.Stream,
		})
		sp.mu <- struct{}{}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			chunk := func(content string, finish any) {
				payload := map[string]any{
					"id": "chatcmpl-stub", "object": "chat.completion.chunk",
					"created": 1700000000, "model": body.Model,
					"choices": []map[string]any{
						{"index": 0, "delta": deltaFor(content), "finish_reason": finish},
					},
				}
				b, _ := json.Marshal(payload)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if flusher != nil {
					flusher.Flush()
				}
			}
			chunk("", nil)
			chunk("stub-reply-model="+body.Model, nil)
			chunk("", "stop")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-stub", "object": "chat.completion",
			"created": 1700000000, "model": body.Model,
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant", "content": "stub-reply-model=" + body.Model,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entry := func(id string) map[string]any {
			return map[string]any{
				"id": id, "object": "model",
				"created": 1700000000, "owned_by": "stub",
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{entry("m1"), entry("m2"), entry("m3")},
		})
	})
	// Some client code paths request the catalog relative to the base origin
	// rather than the /v1 base; serve both so the stub never 404s a probe.
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entry := func(id string) map[string]any {
			return map[string]any{
				"id": id, "object": "model",
				"created": 1700000000, "owned_by": "stub",
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{entry("m1"), entry("m2"), entry("m3")},
		})
	})
	sp.srv = httptest.NewServer(mux) // binds 127.0.0.1 only
	sp.URL = sp.srv.URL + "/v1"
	t.Cleanup(sp.srv.Close)
	return sp
}

func deltaFor(content string) map[string]any {
	d := map[string]any{}
	if content != "" {
		d["content"] = content
	}
	return d
}

// requestCount returns how many provider requests have been recorded.
func (sp *stubProvider) requestCount() int {
	<-sp.mu
	defer func() { sp.mu <- struct{}{} }()
	return len(sp.reqs)
}

// lastRequest returns the most recent recorded provider request.
func (sp *stubProvider) lastRequest(t *testing.T) stubProviderReq {
	t.Helper()
	<-sp.mu
	defer func() { sp.mu <- struct{}{} }()
	if len(sp.reqs) == 0 {
		t.Fatal("stub provider recorded no requests")
	}
	return sp.reqs[len(sp.reqs)-1]
}

// lastRequestQuiet returns nil when nothing was recorded (non-fatal variant
// used inside timeout diagnostics).
func (sp *stubProvider) lastRequestQuiet() *stubProviderReq {
	<-sp.mu
	defer func() { sp.mu <- struct{}{} }()
	if len(sp.reqs) == 0 {
		return nil
	}
	r := sp.reqs[len(sp.reqs)-1]
	return &r
}
