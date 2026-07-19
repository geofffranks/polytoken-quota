package cli

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/geofffranks/codexbar-hooks/internal/doctor"
	"github.com/geofffranks/codexbar-hooks/internal/hook"
	"github.com/geofffranks/codexbar-hooks/internal/service"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// NOTE: the production binding assertion
//   var _ Mutator = (*service.Coordinator)(nil)
// is deferred to Task 12, which introduces service.Coordinator. The Task 2
// command-tree and adapter tests use the local spy below.

// depsSpy is a test double that records Mutator/Diagnoser invocations. It
// satisfies both Mutator and Diagnoser.
type depsSpy struct {
	Mutations     int
	SetProvider   string
	SetPatch      state.ProviderPatch
	ClearSelector state.Selector
}

func newDepsSpy() *depsSpy { return &depsSpy{} }

func (s *depsSpy) Dependencies() Dependencies {
	return Dependencies{
		Mutator:     s,
		Diagnoser:   s,
		Environment: func() map[string]string { return map[string]string{} },
	}
}

func (s *depsSpy) Init(context.Context) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) HandleEvent(context.Context, hook.Event) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Reconcile(context.Context, bool) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Sync(context.Context, bool) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Set(_ context.Context, provider string, patch state.ProviderPatch) service.Outcome {
	s.Mutations++
	s.SetProvider = provider
	s.SetPatch = patch
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Clear(_ context.Context, selector state.Selector) service.Outcome {
	s.Mutations++
	s.ClearSelector = selector
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Status(context.Context, bool) StatusReport { return StatusReport{} }

func (s *depsSpy) Doctor(context.Context, bool) doctor.Report { return doctor.Report{} }

func TestCommandTree(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"init"}, 0},
		{[]string{"hook"}, 0},
		{[]string{"status", "--json"}, 0},
		{[]string{"reconcile", "--dry-run"}, 0},
		{[]string{"sync", "--from-polytoken", "--force"}, 0},
		{[]string{"state", "set", "codex", "--quota", "low"}, 0},
		{[]string{"state", "clear", "codex"}, 0},
		{[]string{"state", "clear", "--all"}, 0},
		{[]string{"doctor", "--json"}, 0},
		{[]string{"unknown"}, 1},
		{[]string{"sync"}, 1},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			spy := newDepsSpy()
			stdin := strings.NewReader("")
			if len(tc.args) > 0 && tc.args[0] == "hook" {
				stdin = strings.NewReader(`{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`)
			}
			if got := Run(context.Background(), tc.args, stdin, io.Discard, io.Discard, spy.Dependencies()); got != tc.want {
				t.Fatalf("exit=%d want=%d", got, tc.want)
			}
			if tc.want == 1 && spy.Mutations != 0 {
				t.Fatalf("invalid syntax mutated %d times", spy.Mutations)
			}
		})
	}
}

func TestStateAdaptersPassTypedValues(t *testing.T) {
	spy := newDepsSpy()
	if got := Run(context.Background(), []string{"state", "set", "codex", "--quota", "low", "--availability", "unavailable"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies()); got != 0 {
		t.Fatalf("exit=%d", got)
	}
	low, unavailable := state.QuotaLow, state.Unavailable
	if spy.SetProvider != "codex" || !reflect.DeepEqual(spy.SetPatch, state.ProviderPatch{Quota: &low, Availability: &unavailable}) {
		t.Fatalf("provider=%q patch=%+v", spy.SetProvider, spy.SetPatch)
	}

	spy = newDepsSpy()
	if got := Run(context.Background(), []string{"state", "clear", "--all"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies()); got != 0 {
		t.Fatalf("exit=%d", got)
	}
	if !reflect.DeepEqual(spy.ClearSelector, state.Selector{All: true}) {
		t.Fatalf("selector=%+v", spy.ClearSelector)
	}
}

func TestExitCodeRouting(t *testing.T) {
	pending := service.Outcome{Accepted: true, Targets: []service.TargetOutcome{{Pending: &state.ApplyFailure{}}}}
	if got := MutationExitCode(pending); got != ExitPending {
		t.Fatalf("mutation=%d", got)
	}
	if got := DiagnosticExitCode(StatusCommand, true); got != ExitOK {
		t.Fatalf("status=%d", got)
	}
	if got := DiagnosticExitCode(DoctorCommand, true); got != ExitRejected {
		t.Fatalf("doctor=%d", got)
	}
	if got := DiagnosticExitCode(DoctorCommand, false); got != ExitOK {
		t.Fatalf("healthy doctor=%d", got)
	}
}
