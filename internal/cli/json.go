package cli

// JSON contract DTOs (AC.9): normative top-level JSON shapes for every command.
//
// Every --json invocation writes exactly one JSON object to stdout, including
// exit 1 and exit 2 outcomes. Timestamps are RFC3339 strings in UTC. Optional
// unavailable scalars/objects are omitted (not null/zero). Required arrays are
// present as []. JSON is ANSI-free.

import (
	"encoding/json"
	"io"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// diagErrorJSON is one safe diagnostic error projection.
type diagErrorJSON struct {
	Scope      string `json:"scope"`
	MappingID  string `json:"mapping_id,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	Summary    string `json:"summary"`
}

// --- status JSON ---

// statusWindowJSON is one raw quota window in the status JSON envelope.
type statusWindowJSON struct {
	Name         string   `json:"name"`
	Used         *float64 `json:"used,omitempty"`
	Limit        *float64 `json:"limit,omitempty"`
	UsagePercent *float64 `json:"usage_percent,omitempty"`
	ResetAt      string   `json:"reset_at,omitempty"`
}

// statusProviderJSON is one provider row in the status JSON envelope.
type statusProviderJSON struct {
	Provider    string             `json:"provider"`
	Status      string             `json:"status"`
	Windows     []statusWindowJSON `json:"windows"`
	NextResetAt string             `json:"next_reset_at,omitempty"`
}

// statusSkippedJSON is one desired model absent from the effective chain.
type statusSkippedJSON struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// statusRouteJSON is one route with desired/effective chains and skip reasons.
type statusRouteJSON struct {
	Name            string              `json:"name"`
	TargetID        string              `json:"target_id,omitempty"`
	Desired         []string            `json:"desired"`
	Effective       []string            `json:"effective"`
	Skipped         []statusSkippedJSON `json:"skipped,omitempty"`
	ProjectionError bool                `json:"projection_error,omitempty"`
}

// statusJSON is the normative top-level merged status shape:
//
//	{"routing_enabled":true,"last_checked":"...Z","providers":[],"routes":[],
//	 "pending_targets":[],"problem":false,"errors":[],"error":"optional"}
type statusJSON struct {
	RoutingEnabled bool                 `json:"routing_enabled"`
	LastChecked    string               `json:"last_checked,omitempty"`
	Providers      []statusProviderJSON `json:"providers"`
	Routes         []statusRouteJSON    `json:"routes"`
	PendingTargets []string             `json:"pending_targets"`
	Problem        bool                 `json:"problem"`
	Errors         []diagErrorJSON      `json:"errors"`
	Error          string               `json:"error,omitempty"`
}

func statusEnvelope(r service.MergedStatusReport) statusJSON {
	out := statusJSON{
		RoutingEnabled: r.RoutingEnabled, Problem: r.Problem,
		PendingTargets: append([]string{}, r.PendingTargets...), Error: r.Error,
	}
	if !r.LastChecked.IsZero() {
		out.LastChecked = r.LastChecked.UTC().Format(time.RFC3339)
	}
	for _, p := range r.Providers {
		pj := statusProviderJSON{Provider: p.Provider, Status: p.Status}
		for _, win := range p.Windows {
			wj := statusWindowJSON{Name: win.Name, Used: win.Used, Limit: win.Limit, UsagePercent: win.UsagePercent}
			if win.ResetAt != nil {
				wj.ResetAt = win.ResetAt.UTC().Format(time.RFC3339)
			}
			pj.Windows = append(pj.Windows, wj)
		}
		if pj.Windows == nil {
			pj.Windows = []statusWindowJSON{}
		}
		if p.NextResetAt != nil {
			pj.NextResetAt = p.NextResetAt.UTC().Format(time.RFC3339)
		}
		out.Providers = append(out.Providers, pj)
	}
	if out.Providers == nil {
		out.Providers = []statusProviderJSON{}
	}
	for _, route := range r.Routes {
		rj := statusRouteJSON{
			Name: route.Name, TargetID: route.TargetID, ProjectionError: route.ProjectionError,
			Desired: append([]string{}, route.Desired...), Effective: append([]string{}, route.Effective...),
		}
		for _, s := range route.Skipped {
			rj.Skipped = append(rj.Skipped, statusSkippedJSON{Model: s.Model, Reason: s.Reason})
		}
		out.Routes = append(out.Routes, rj)
	}
	if out.Routes == nil {
		out.Routes = []statusRouteJSON{}
	}
	if out.PendingTargets == nil {
		out.PendingTargets = []string{}
	}
	for _, e := range r.Errors {
		out.Errors = append(out.Errors, diagErrorJSON{Scope: string(e.Scope), MappingID: e.MappingID, TargetID: e.TargetID, SourcePath: e.SourcePath, Summary: e.Summary})
	}
	if out.Errors == nil {
		out.Errors = []diagErrorJSON{}
	}
	return out
}

// --- doctor JSON ---

// findingJSON is one doctor finding in the doctor JSON envelope.
type findingJSON struct {
	Code        string          `json:"code"`
	Message     string          `json:"message"`
	TargetID    string          `json:"target_id,omitempty"`
	File        string          `json:"file,omitempty"`
	Chain       string          `json:"chain,omitempty"`
	Remediation string          `json:"remediation,omitempty"`
	Severity    doctor.Severity `json:"severity"`
}

// doctorJSON is the normative top-level doctor shape:
//
//	{"as_of":"...Z","actionable":false,"findings":[],"recovered":[],"error":"optional"}
type doctorJSON struct {
	AsOf       time.Time       `json:"as_of"`
	Actionable bool            `json:"actionable"`
	Findings   []findingJSON   `json:"findings"`
	Recovered  []recoveredJSON `json:"recovered"`
	Error      string          `json:"error,omitempty"`
}

type recoveredJSON struct {
	TargetID string `json:"target_id"`
	Stage    string `json:"stage"`
	Summary  string `json:"summary"`
}

func doctorEnvelope(r doctor.Report) doctorJSON {
	out := doctorJSON{AsOf: r.AsOf, Actionable: r.Actionable()}
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, findingJSON{
			Code: f.Code, Message: f.Message, TargetID: f.TargetID,
			File: f.File, Chain: f.Chain, Remediation: f.Remediation, Severity: f.Severity,
		})
	}
	if out.Findings == nil {
		out.Findings = []findingJSON{}
	}
	for _, rec := range r.Recovered {
		out.Recovered = append(out.Recovered, recoveredJSON{
			TargetID: rec.TargetID, Stage: rec.Stage,
			Summary: validate.DefaultSanitize([]byte(rec.Summary)),
		})
	}
	if out.Recovered == nil {
		out.Recovered = []recoveredJSON{}
	}
	return out
}

// --- check/reconcile/mutation JSON ---

// attemptJSON is one sanitized quota attempt diagnostic.
type attemptJSON struct {
	MappingID string `json:"mapping_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// targetJSON is one mutation target diagnostic.
type targetJSON struct {
	TargetID string `json:"target_id"`
	Pending  bool   `json:"pending"`
	Stage    string `json:"stage,omitempty"`
}

// mutationJSON is the normative top-level check/mutation shape:
//
//	{"accepted":true,"revision":2,"problem":false,"attempts":[],"targets":[],"error":"optional"}
type mutationJSON struct {
	Accepted bool          `json:"accepted"`
	Revision uint64        `json:"revision"`
	Problem  bool          `json:"problem"`
	Attempts []attemptJSON `json:"attempts"`
	Targets  []targetJSON  `json:"targets"`
	Error    string        `json:"error,omitempty"`
}

func mutationEnvelope(o service.Outcome) mutationJSON {
	out := mutationJSON{Accepted: o.Accepted, Revision: o.Revision, Problem: o.Problem}
	if o.Error != nil {
		out.Error = validate.DefaultSanitize([]byte(o.Error.Error()))
	}
	for _, a := range o.ProviderAttempts {
		out.Attempts = append(out.Attempts, attemptJSON{MappingID: a.MappingID, Status: a.Status, Error: a.Error})
	}
	if out.Attempts == nil {
		out.Attempts = []attemptJSON{}
	}
	for _, t := range o.Targets {
		tj := targetJSON{TargetID: t.TargetID, Pending: t.Pending != nil}
		if t.Pending != nil {
			tj.Stage = t.Pending.Stage
		}
		out.Targets = append(out.Targets, tj)
	}
	if out.Targets == nil {
		out.Targets = []targetJSON{}
	}
	return out
}

// encodeJSON writes exactly one JSON object to w. stderr stays empty except on
// encoder failure.
func encodeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
