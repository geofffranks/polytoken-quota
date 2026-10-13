package notice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Bound the entire snapshot, not just the fields retained by decoding. /state
// includes environment and conversation data; none of it enters diagnostics.
const activityBodyLimit = 32 * 1024

// daemonIdle checks only this hook's own daemon. This is not atomic with POST
// /reload: the installed API has no documented no-queue admission flag. A turn
// can start after this check; postReload records unexpected queue acceptance.
func (d HookDeps) daemonIdle(port int, token string) bool {
	started := time.Now()
	result := diagnosticRecord{Event: "activity_check", Reason: "transport_failed"}
	defer func() {
		result.DurationMS = time.Since(started).Milliseconds()
		d.diagnose(result)
		if result.Reason != "idle" && result.Reason != "idle_authority" {
			d.diagnose(diagnosticRecord{Event: "reload_deferred", Reason: result.Reason})
		}
	}()
	client := d.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	// Do not follow a redirect to another daemon or host. Clone the injected
	// client so its policy remains unchanged for callers and reload requests.
	activityClient := *client
	activityClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// One deadline covers /state and the optional pair, even with an injected
	// client that has no timeout. There is no retry or polling in this check.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/state", port), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := activityClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		result.Reason = "http_non_200"
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, activityBodyLimit+1))
	if err != nil {
		result.Reason = "read_failed"
		return false
	}
	if len(body) > activityBodyLimit {
		result.Reason = "too_large"
		return false
	}
	result.Reason = activityReason(body)
	if result.Reason == "activity_unknown" && authorityFallbackAllowed(body) {
		// A valid but inconclusive legacy snapshot is not idle evidence. Only
		// the complete paired authority projection can resolve it.
		d.diagnose(diagnosticRecord{Event: "activity_check", Reason: result.Reason, HTTPStatus: result.HTTPStatus})
		result.Reason, result.HTTPStatus = authorityIdle(ctx, &activityClient, port, token)
	}
	return result.Reason == "idle" || result.Reason == "idle_authority"
}

// Only an object with omitted/null legacy activity and no provider wait can
// use the compatibility fallback. Malformed or contradictory legacy values
// remain unknown rather than being overridden by another observation.
func authorityFallbackAllowed(body []byte) bool {
	var snapshot struct {
		Turn json.RawMessage `json:"turn_in_flight"`
		Wait json.RawMessage `json:"main_provider_wait"`
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(body, &snapshot) != nil {
		return false
	}
	turn, wait := bytes.TrimSpace(snapshot.Turn), bytes.TrimSpace(snapshot.Wait)
	return (len(turn) == 0 || bytes.Equal(turn, []byte("null"))) &&
		(len(wait) == 0 || bytes.Equal(wait, []byte("null")))
}

func activityReason(body []byte) string {
	// RawMessage distinguishes missing/null from explicit false. Only the two
	// activity fields are decoded; unknown fields (including env) are discarded.
	var snapshot struct {
		Turn json.RawMessage `json:"turn_in_flight"`
		Wait json.RawMessage `json:"main_provider_wait"`
	}
	if json.Unmarshal(body, &snapshot) != nil {
		return "invalid_json"
	}
	switch string(bytes.TrimSpace(snapshot.Turn)) {
	case "true":
		return "active"
	case "false":
		// main_provider_wait is optional and nullable in the installed schema.
		if len(snapshot.Wait) != 0 && !bytes.Equal(bytes.TrimSpace(snapshot.Wait), []byte("null")) {
			return "provider_wait"
		}
		return "idle"
	default:
		// Missing, null or unexpected activity values do not authorize reload.
		// daemonIdle may resolve them with a separate authority-owned pair.
		return "activity_unknown"
	}
}
