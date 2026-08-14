package doctor

// Quota/routing diagnostics for the doctor. QuotaFindings is a pure function
// over sanitized per-provider probes plus a reconcile-pending flag; the
// caller builds those probes from the preloaded diagnostic snapshot (observed
// state, desired policy, and the evidence gate). Every message is sanitized —
// no credentials, raw bodies, auth headers, or account IDs.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// QuotaProbe carries the sanitized per-provider quota state for diagnostic
// evaluation. It is the pure-data input to QuotaFindings. Snapshot is the last
// successful observation; Attempt is the latest attempt (including failures);
// Supported and SupportReason come from the evidence gate. HasQuotaConfig is
// true only when the desired policy has a quota section for this provider.
type QuotaProbe struct {
	Provider       string
	HasQuotaConfig bool
	FreshnessTTL   time.Duration
	Snapshot       *quota.QuotaSnapshot
	Attempt        *quota.QuotaSnapshot
	Supported      bool
	SupportReason  string
}

const (
	maxDiscoverabilityKeys = 32
	maxDesiredScanBytes    = 1 << 20
)

// DiscoverabilityFindings reports ignored legacy desired.yaml keys and observed
// provider rows that no longer correspond to configured mapping IDs. It is
// read-only and emits only bounded, identifier-sanitized key names.
func DiscoverabilityFindings(rawDesired []byte, desiredProviders map[string]struct{}, observedProviders map[string]state.ProviderState) []Finding {
	var findings []Finding
	legacy := legacyConfigKeys(rawDesired)
	if len(legacy) > 0 {
		findings = append(findings, Finding{
			Code:        "legacy-config-keys",
			Severity:    Info,
			Message:     fmt.Sprintf("desired.yaml contains ignored legacy config keys: %s", strings.Join(legacy, ", ")),
			Remediation: "remove legacy keys from desired.yaml after reviewing the upgrade",
		})
	}

	if desiredProviders != nil {
		orphaned := make([]string, 0)
		for key := range observedProviders {
			if _, ok := desiredProviders[key]; !ok {
				orphaned = append(orphaned, safeIdentifier(key))
			}
		}
		sort.Strings(orphaned)
		if len(orphaned) > maxDiscoverabilityKeys {
			orphaned = orphaned[:maxDiscoverabilityKeys]
		}
		if len(orphaned) > 0 {
			findings = append(findings, Finding{
				Code:        "orphaned-provider-state",
				Severity:    Info,
				Message:     fmt.Sprintf("state.json contains provider state with no configured mapping: %s", strings.Join(orphaned, ", ")),
				Remediation: "review and remove orphaned provider state during operator-driven cleanup",
			})
		}
	}
	return findings
}

func legacyConfigKeys(raw []byte) []string {
	if len(raw) > maxDesiredScanBytes {
		raw = raw[:maxDesiredScanBytes]
	}
	text := string(raw)
	keys := make([]string, 0, 2)
	for _, key := range []string{"codexbar_providers", "polytoken_providers"} {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, key+":") {
				keys = append(keys, key)
				break
			}
		}
	}
	return keys
}

// QuotaFindings evaluates per-provider quota probes plus a reconcile-pending
// flag and returns sanitized findings. It is a pure function of its inputs: it
// never loads state, policy, or files. Findings are emitted in the order the
// probes are passed (the caller sorts); the reconcile-pending finding, when
// present, is emitted last. A healthy provider with a fresh snapshot, a
// supported adapter, and no failed attempt produces no finding.
//
// Sanitization: provider identifiers pass through safeIdentifier (rejecting
// non-identifier characters), and error/reason strings pass through
// quota.SanitizeText a second time for defense in depth.
func QuotaFindings(probes []QuotaProbe, reconcilePending bool, now time.Time) []Finding {
	var findings []Finding
	for _, p := range probes {
		clean := safeIdentifier(p.Provider)
		if p.HasQuotaConfig && !p.Supported {
			findings = append(findings, Finding{
				Code:        "quota-adapter-unsupported",
				TargetID:    clean,
				Severity:    Warning,
				Message:     fmt.Sprintf("provider %s adapter unsupported; %s", clean, quota.SanitizeText(p.SupportReason)),
				Remediation: "record or refresh contract evidence and re-verify the adapter",
			})
		}
		if p.Snapshot != nil && p.FreshnessTTL > 0 && now.Sub(p.Snapshot.CheckedAt) > p.FreshnessTTL {
			findings = append(findings, Finding{
				Code:     "quota-stale-snapshot",
				TargetID: clean,
				Severity: Warning,
				Message: fmt.Sprintf(
					"provider %s quota snapshot is stale (last checked %s; freshness TTL %dm); run `check` to refresh.",
					clean, p.Snapshot.CheckedAt.Format(time.RFC3339), int(p.FreshnessTTL.Minutes())),
				Remediation: "run `check` to refresh the snapshot",
			})
		}
		if p.Snapshot != nil {
			if p.Snapshot.Status == quota.SourcePartial {
				severity := Info
				code := "quota-partial"
				message := fmt.Sprintf("provider %s quota snapshot is partial; some windows unavailable.", clean)
				if p.Snapshot.EffectiveRemaining() == nil || p.Snapshot.Availability == quota.QuotaUnknown {
					severity = Warning
					code = "quota-partial-unusable"
					message = fmt.Sprintf("provider %s quota snapshot is partial with no usable quota signal; routing remains disabled for it.", clean)
				}
				findings = append(findings, Finding{Code: code, TargetID: clean, Severity: severity, Message: message, Remediation: "run `check` to refresh the snapshot"})
			} else if p.Snapshot.EffectiveRemaining() == nil || p.Snapshot.Availability == quota.QuotaUnknown {
				findings = append(findings, Finding{Code: "quota-unusable", TargetID: clean, Severity: Warning, Message: fmt.Sprintf("provider %s quota snapshot has no usable quota signal; routing remains disabled for it.", clean), Remediation: "run `check` to refresh the snapshot"})
			}
		}
		if p.Attempt != nil && p.Attempt.Status == quota.SourceFailed {
			findings = append(findings, Finding{
				Code:        "quota-attempt-failed",
				TargetID:    clean,
				Severity:    Warning,
				Message:     fmt.Sprintf("provider %s quota attempt failed: %s", clean, quota.SanitizeText(p.Attempt.Error)),
				Remediation: "run `check` to retry; verify credentials and adapter contract",
			})
		}
	}
	if reconcilePending {
		findings = append(findings, Finding{
			Code:     "quota-reconcile-pending",
			Severity: Warning,
			Message:  "a quota-check reconcile was interrupted; recovery will complete on the next invocation.",
		})
	}
	return findings
}
