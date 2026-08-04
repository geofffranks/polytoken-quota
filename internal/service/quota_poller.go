package service

// QuotaPoller polls configured provider adapters behind the evidence gate and
// returns sanitized snapshots. Provider failures are ISOLATED: a failed provider
// never prevents another valid snapshot from being accepted. Production wires the
// real adapter-backed implementation; tests inject a fake that returns preset
// snapshots.

import (
	"context"
	"errors"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/quota"
)

// QuotaPoller polls all configured providers (or one mapping when provider is
// non-empty). It returns a map of mapping ID → QuotaSnapshot, the attempt —
// including failures, which carry Status=failed. Each provider is polled
// independently; a failure for one never blocks another.
type QuotaPoller interface {
	Poll(ctx context.Context, desired policy.Desired, provider string, now time.Time) (map[string]quota.QuotaSnapshot, error)
}

// quotaEvidenceProvider exposes the poller's shared evidence gate to read-only
// diagnostics without widening the polling interface used by test doubles.
type quotaEvidenceProvider interface {
	EvidenceRegistry() *quota.EvidenceRegistry
}

func (p *QuotaPollerImpl) EvidenceRegistry() *quota.EvidenceRegistry { return p.Evidence }

// QuotaPollerImpl is the production QuotaPoller. It builds provider adapters on
// demand from the desired policy's provider configs and polls each behind the
// shared evidence gate. The HTTP transport, credential resolver, and evidence
// registry are shared (injected); each adapter resolves credentials transiently
// and discards them. Failures are isolated: one provider's failure never blocks
// another, and every provider appears in the result map.
type QuotaPollerImpl struct {
	Client      *quota.BoundedClient
	Credentials quota.CredentialResolver
	Evidence    *quota.EvidenceRegistry
	// Now fixes the evidence timestamps. When nil the Poll's now argument is
	// used.
	Now func() time.Time
}

// Poll iterates the desired provider mappings (filtered to provider when
// non-empty), builds each adapter, and polls it independently. The returned map
// is keyed by provider MAPPING ID and includes every polled provider —
// successes with their observation, failures with a Status=failed snapshot.
func (p *QuotaPollerImpl) Poll(ctx context.Context, desired policy.Desired, provider string, now time.Time) (map[string]quota.QuotaSnapshot, error) {
	reg := p.Evidence
	if reg == nil {
		reg = quota.NewEvidenceRegistry()
	}
	nowFn := p.Now
	if nowFn == nil {
		t := now
		nowFn = func() time.Time { return t }
	}
	out := make(map[string]quota.QuotaSnapshot, len(desired.Providers))
	for _, id := range sortedMappingIDs(desired) {
		m := desired.Providers[policy.MappingID(id)]
		if m.Quota == nil {
			continue // no quota config: not a routing/polling participant
		}
		if provider != "" && id != provider {
			continue
		}
		src := p.sourceFor(id, m.Quota.Adapter, reg, nowFn)
		out[id] = pollOne(ctx, src)
	}
	return out, nil
}

// sourceFor constructs the QuotaSource adapter for one mapping. Known adapters
// consult the shared release-owned evidence registry; polling never registers or
// refreshes evidence. An unknown adapter yields an explicitly unsupported source.
func (p *QuotaPollerImpl) sourceFor(mappingID, adapter string, reg *quota.EvidenceRegistry, now func() time.Time) quota.QuotaSource {
	switch adapter {
	case "codex":
		return quota.NewCodexSource(mappingID, p.Client, p.Credentials, "", reg, now())
	case "zai":
		return quota.NewZaiSource(mappingID, p.Client, p.Credentials, "", reg, now())
	default:
		reason := "unknown quota adapter " + adapter + "; record evidence before enabling"
		return unsupportedSource{mappingID: mappingID, reason: reason}
	}
}

// pollOne polls one source independently, always returning a snapshot. On
// success the observation is returned; on failure the adapter's sanitized
// failed snapshot is returned (Fetch returns one alongside its error). A
// defensive failed snapshot is synthesized if an adapter returns a non-failed
// snapshot with an error.
func pollOne(ctx context.Context, src quota.QuotaSource) quota.QuotaSnapshot {
	snap, err := src.Fetch(ctx)
	if err == nil {
		return snap
	}
	if snap.Status == quota.SourceFailed {
		return snap
	}
	return quota.QuotaSnapshot{
		MappingID:    src.MappingID(),
		Availability: quota.QuotaUnknown,
		Status:       quota.SourceFailed,
		Error:        quota.SanitizeError(err),
	}
}

// unsupportedSource is a QuotaSource whose evidence is absent. It fails closed:
// Status reports unsupported and Fetch returns a sanitized failed snapshot
// without making any request. It backs unknown adapter types in production.
type unsupportedSource struct {
	mappingID string
	reason    string
}

func (u unsupportedSource) MappingID() string { return u.mappingID }

func (u unsupportedSource) Status() quota.SupportStatus {
	return quota.SupportStatus{Supported: false, Reason: u.reason}
}

func (u unsupportedSource) Fetch(context.Context) (quota.QuotaSnapshot, error) {
	snap := quota.QuotaSnapshot{
		MappingID:    u.mappingID,
		Availability: quota.QuotaUnknown,
		Status:       quota.SourceFailed,
		Error:        u.reason,
	}
	return snap, errors.New(u.reason)
}

// NewQuotaPoller returns the production QuotaPoller backed by real provider
// adapters sharing a real bounded client, the default credential resolver, and
// the release-owned reviewed evidence registry.
func NewQuotaPoller() QuotaPoller {
	evidence := quota.NewEvidenceRegistry()
	now := time.Now()
	evidence.Register(quota.CodexEvidence(now))
	evidence.Register(quota.ZaiEvidence(now))
	return &QuotaPollerImpl{
		Client:      &quota.BoundedClient{},
		Credentials: quota.DefaultCredentialResolver(),
		Evidence:    evidence,
	}
}
