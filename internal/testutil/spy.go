package testutil

import (
	"sync"
)

// CommandCall records one observed external command invocation.
type CommandCall struct {
	Name string
	Args []string
	Dir  string
}

// CommandResult is the canned outcome returned by CommandSpy for a call. A
// non-nil Err short-circuits before ExitCode is consulted.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// CommandSpy records CommandCalls and returns canned CommandResults in order.
// It is concurrency-safe. When the canned results are exhausted, the last
// result is reused; a FailWith error overrides everything. Tests use it to
// drive the validator without invoking a real binary.
type CommandSpy struct {
	mu      sync.Mutex
	calls   []CommandCall
	results []CommandResult
	failErr error
}

// NewCommandSpy returns a spy that returns results in order for successive
// calls.
func NewCommandSpy(results ...CommandResult) *CommandSpy {
	return &CommandSpy{results: results}
}

// FailWith makes every subsequent Run return err instead of a canned result.
func (s *CommandSpy) FailWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failErr = err
}

// Run records the call and returns the next canned result (or failErr). dir is
// the working directory the command would run in, recorded for assertions.
func (s *CommandSpy) Run(dir, name string, args ...string) CommandResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := CommandCall{Name: name, Args: append([]string(nil), args...), Dir: dir}
	s.calls = append(s.calls, call)
	if s.failErr != nil {
		return CommandResult{Err: s.failErr}
	}
	if len(s.results) == 0 {
		return CommandResult{}
	}
	r := s.results[0]
	if len(s.results) > 1 {
		s.results = s.results[1:]
	}
	return r
}

// Calls returns a copy of the recorded calls in invocation order.
func (s *CommandSpy) Calls() []CommandCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CommandCall, len(s.calls))
	copy(out, s.calls)
	return out
}
