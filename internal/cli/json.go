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
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// --- status JSON ---

// statusProviderJSON is one provider row in the status JSON envelope.
type statusProviderJSON struct {
	Provider       string             `json:"provider"`
	Quota          state.Quota        `json:"quota"`
	Availability   state.Availability `json:"availability"`
	Mode           state.Mode         `json:"mode"`
	ManualDisabled bool               `json:"manual_disabled"`
	Reason         string             `json:"reason"`
}

// statusJSON is the normative top-level status shape:
//
//	{"as_of":"...Z","revision":1,"problem":false,"providers":[],"error":"optional"}
type statusJSON struct {
	AsOf      time.Time            `json:"as_of"`
	Revision  uint64               `json:"revision"`
	Problem   bool                 `json:"problem"`
	Providers []statusProviderJSON `json:"providers"`
	Error     string               `json:"error,omitempty"`
}

func statusEnvelope(r service.StatusReport) statusJSON {
	out := statusJSON{AsOf: r.AsOf, Revision: r.Revision, Problem: r.Problem, Error: r.Error}
	for _, p := range r.Providers {
		out.Providers = append(out.Providers, statusProviderJSON{
			Provider: p.Provider, Quota: p.Quota, Availability: p.Availability,
			Mode: p.Mode, ManualDisabled: p.ManualDisabled, Reason: p.Reason,
		})
	}
	if out.Providers == nil {
		out.Providers = []statusProviderJSON{}
	}
	return out
}

// --- routing JSON ---

// routeJSON is one effective route in the routing JSON envelopes.
type routeJSON struct {
	TargetID  string   `json:"target_id"`
	Name      string   `json:"name"`
	Source    string   `json:"source,omitempty"`
	Desired   []string `json:"desired,omitempty"`
	Effective []string `json:"effective"`
}

// diagErrorJSON is one safe diagnostic error projection.
type diagErrorJSON struct {
	Scope      string `json:"scope"`
	MappingID  string `json:"mapping_id,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	Summary    string `json:"summary"`
}

// routingJSON is the normative top-level bare routing shape:
//
//	{"as_of":"...Z","routing_enabled":true,"routes":[],"errors":[]}
type routingJSON struct {
	AsOf           time.Time       `json:"as_of"`
	RoutingEnabled bool            `json:"routing_enabled"`
	Routes         []routeJSON     `json:"routes"`
	Errors         []diagErrorJSON `json:"errors"`
	Error          string          `json:"error,omitempty"`
}

func routingEnvelope(r service.RoutingReport) routingJSON {
	out := routingJSON{AsOf: r.AsOf, RoutingEnabled: r.RoutingEnabled, Error: r.Error}
	for _, route := range r.Routes {
		rj := routeJSON{TargetID: route.TargetID, Name: route.Name, Source: route.SourcePath, Effective: route.Effective}
		if rj.Effective == nil {
			rj.Effective = []string{}
		}
		rj.Desired = append([]string(nil), route.Desired...)
		out.Routes = append(out.Routes, rj)
	}
	if out.Routes == nil {
		out.Routes = []routeJSON{}
	}
	for _, e := range r.Errors {
		out.Errors = append(out.Errors, diagErrorJSON{Scope: string(e.Scope), MappingID: e.MappingID, TargetID: e.TargetID, SourcePath: e.SourcePath, Summary: e.Summary})
	}
	if out.Errors == nil {
		out.Errors = []diagErrorJSON{}
	}
	return out
}

// rankJSON is one provider rank explanation in the explain envelope.
type rankJSON struct {
	MappingID   string `json:"mapping_id"`
	Rank        int    `json:"rank"`
	OffPeak     bool   `json:"off_peak"`
	Eligible    bool   `json:"eligible"`
	Explanation string `json:"explanation"`
}

// routingExplainJSON is the normative top-level routing explain shape:
//
//	{"as_of":"...Z","routing_enabled":true,"ranks":[],"routes":[],"errors":[]}
type routingExplainJSON struct {
	AsOf           time.Time       `json:"as_of"`
	RoutingEnabled bool            `json:"routing_enabled"`
	Ranks          []rankJSON      `json:"ranks"`
	Routes         []routeJSON     `json:"routes"`
	Errors         []diagErrorJSON `json:"errors"`
	Error          string          `json:"error,omitempty"`
}

func routingExplainEnvelope(r service.RoutingExplainReport) routingExplainJSON {
	out := routingExplainJSON{AsOf: r.AsOf, RoutingEnabled: r.RoutingEnabled, Error: r.Error}
	for _, rank := range r.Ranks {
		out.Ranks = append(out.Ranks, rankJSON{
			MappingID: rank.MappingID, Rank: rank.Rank, OffPeak: rank.OffPeak,
			Eligible: rank.Eligible, Explanation: rank.Explanation,
		})
	}
	if out.Ranks == nil {
		out.Ranks = []rankJSON{}
	}
	for _, route := range r.Routes {
		rj := routeJSON{TargetID: route.TargetID, Name: route.Name, Source: route.SourcePath, Effective: route.Effective}
		if rj.Effective == nil {
			rj.Effective = []string{}
		}
		rj.Desired = append([]string(nil), route.Desired...)
		out.Routes = append(out.Routes, rj)
	}
	if out.Routes == nil {
		out.Routes = []routeJSON{}
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
