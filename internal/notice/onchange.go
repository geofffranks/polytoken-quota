package notice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

// OnChangeSpec is one operator-configured host-side action executed after a
// reconciled revision changed managed fields. Run must be an absolute path to
// an executable (policy validates this); Args and Env are literal values.
type OnChangeSpec struct {
	Run     string
	Args    []string
	Env     map[string]string
	Timeout time.Duration
}

// OnChangeResult reports one action's execution. Exactly one of success
// (Err == nil), failure (Err != nil), or budget skip (Skipped) holds.
type OnChangeResult struct {
	Run      string
	Err      error
	TimedOut bool
	Skipped  bool
}

// errBudgetExhausted marks actions never started because the aggregate budget
// elapsed; it is reported via Skipped rather than Err.
var errBudgetExhausted = errors.New("notice: on_change aggregate budget exhausted")

// ExecuteOnChange runs each spec in order with the notice document on stdin,
// a minimal sanitized environment plus the spec's configured additions, and a
// per-action timeout bounded by the remaining aggregate budget. Actions whose
// start time falls past the aggregate budget are skipped. Results are returned
// in spec order; one action's failure never prevents later actions from
// running.
func ExecuteOnChange(ctx context.Context, specs []OnChangeSpec, noticeDoc []byte, aggregateBudget time.Duration) []OnChangeResult {
	out := make([]OnChangeResult, 0, len(specs))
	deadline := time.Now().Add(aggregateBudget)
	for _, spec := range specs {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			out = append(out, OnChangeResult{Run: spec.Run, Skipped: true})
			continue
		}
		// The aggregate budget is enforced at action start boundaries: a running
		// action is bounded only by its own per-action timeout, and later
		// unstarted actions are skipped once the budget elapses.
		timeout := spec.Timeout
		if timeout <= 0 {
			timeout = remaining
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(actx, spec.Run, spec.Args...)
		cmd.Env = actionEnv(spec.Env)
		cmd.Stdin = bytes.NewReader(noticeDoc)
		err := cmd.Run()
		cancel()
		res := OnChangeResult{Run: spec.Run, Err: err}
		if actx.Err() == context.DeadlineExceeded && err != nil {
			res.TimedOut = true
			res.Err = fmt.Errorf("notice: on_change %s timed out after %s", spec.Run, timeout)
		}
		out = append(out, res)
	}
	return out
}

// actionEnv builds the action's environment: only PATH and HOME from the
// current process plus the spec's literal additions. Inherited variables —
// including any provider credentials in the environment — never reach the
// action. Keys are sorted for deterministic execution.
func actionEnv(configured map[string]string) []string {
	env := map[string]string{}
	for _, key := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	for k, v := range configured {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
