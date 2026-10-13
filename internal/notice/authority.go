package notice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// /sync includes public history handles and runtime projections. Bound the
// complete body; retain only the fields needed for idle admission, never log it.
const authorityBodyLimit = 256 * 1024

// authorityIdle makes exactly one paired observation, without retries. Equal
// epoch/cursor values establish a common authority revision, not atomic reload
// admission: a turn can still start before POST /reload.
func authorityIdle(ctx context.Context, client *http.Client, port int, token string) (string, int) {
	snapshot, reason, status := authorityGET(ctx, client, port, token, "/sync")
	if reason != "available" {
		return reason, status
	}
	summary, reason, status := authorityGET(ctx, client, port, token, "/sync/summary")
	if reason != "available" {
		return reason, status
	}
	return authorityReason(snapshot, summary), status
}

func authorityGET(ctx context.Context, client *http.Client, port int, token, path string) ([]byte, string, int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return nil, "authority_unavailable", 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "authority_unavailable", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "authority_http_non_200", resp.StatusCode
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, authorityBodyLimit+1))
	if err != nil {
		return nil, "authority_read_failed", resp.StatusCode
	}
	if len(body) > authorityBodyLimit {
		return nil, "authority_too_large", resp.StatusCode
	}
	return body, "available", resp.StatusCode
}

func authorityReason(snapshotBody, summaryBody []byte) string {
	// RawMessage and pointers preserve required-field presence and nullability.
	// These are the idle-relevant fields of SessionSyncSnapshotV1 and
	// SyncSummaryV1; unrelated projections are deliberately discarded.
	var snapshot struct {
		Version        *int            `json:"version"`
		Epoch          string          `json:"daemon_epoch"`
		Cursor         string          `json:"cursor"`
		Turn           json.RawMessage `json:"turn"`
		Interrogatives json.RawMessage `json:"interrogatives"`
		Queue          *struct {
			Revision string          `json:"revision"`
			Items    json.RawMessage `json:"items"`
		} `json:"queue"`
	}
	var summary struct {
		Version           *int    `json:"version"`
		Epoch             string  `json:"daemon_epoch"`
		Cursor            string  `json:"cursor"`
		Busy              *bool   `json:"busy"`
		Attention         *bool   `json:"attention"`
		HistoryCheckpoint string  `json:"history_checkpoint"`
		HistoryTotalItems *uint64 `json:"history_total_items"`
	}
	if json.Unmarshal(snapshotBody, &snapshot) != nil || json.Unmarshal(summaryBody, &summary) != nil {
		return "authority_invalid_json"
	}
	if snapshot.Version == nil || summary.Version == nil || *snapshot.Version != 1 || *summary.Version != 1 {
		return "authority_unsupported_version"
	}
	if snapshot.Epoch == "" || summary.Epoch == "" || snapshot.Cursor == "" || summary.Cursor == "" ||
		summary.Busy == nil || summary.Attention == nil || summary.HistoryCheckpoint == "" || summary.HistoryTotalItems == nil ||
		len(snapshot.Turn) == 0 || snapshot.Queue == nil || snapshot.Queue.Revision == "" {
		return "authority_incomplete"
	}
	var questions, items []json.RawMessage
	if !jsonArray(snapshot.Interrogatives, &questions) || !jsonArray(snapshot.Queue.Items, &items) {
		return "authority_incomplete"
	}
	if snapshot.Epoch != summary.Epoch || snapshot.Cursor != summary.Cursor {
		return "authority_mismatch"
	}
	if *summary.Busy {
		return "authority_busy"
	}
	if !bytes.Equal(bytes.TrimSpace(snapshot.Turn), []byte("null")) {
		return "authority_turn_present"
	}
	if *summary.Attention || len(questions) != 0 {
		return "authority_attention"
	}
	if len(items) != 0 {
		return "authority_queue_pending"
	}
	return "idle_authority"
}

func jsonArray(raw json.RawMessage, dst *[]json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '[' && json.Unmarshal(trimmed, dst) == nil
}
