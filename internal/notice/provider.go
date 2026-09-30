package notice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ProviderState is the reconciled provider-level status in a provider-only notice.
type ProviderState struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	// Reason is an optional sanitized attribution (currently only pace gating
	// carries one); omitted when empty.
	Reason string `json:"reason,omitempty"`
}

type providerDocument struct {
	Schema       int             `json:"schema"`
	Revision     uint64          `json:"revision"`
	PublishedAt  string          `json:"published_at"`
	ProviderOnly bool            `json:"provider_only"`
	Providers    []ProviderState `json:"providers"`
}

// RenderProvider produces a provider-only notice. It deliberately includes no
// model, group, route, or fallback claims.
func RenderProvider(revision uint64, publishedAt time.Time, providers []ProviderState) ([]byte, error) {
	if publishedAt.IsZero() {
		return nil, fmt.Errorf("notice: PublishedAt is required")
	}
	ordered := append([]ProviderState(nil), providers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	doc := providerDocument{Schema: SchemaVersion, Revision: revision, PublishedAt: publishedAt.UTC().Format(time.RFC3339), ProviderOnly: true, Providers: ordered}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("notice: encode provider status: %w", err)
	}
	return buf.Bytes(), nil
}
