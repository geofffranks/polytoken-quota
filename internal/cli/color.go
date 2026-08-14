package cli

// Color policy (AC.9): normative ANSI precedence and semantic styles.
//
// Precedence (highest wins):
//  1. --json always disables ANSI.
//  2. A present, non-empty NO_COLOR env var disables ANSI.
//  3. Otherwise ANSI is enabled only when the destination stream is an
//     interactive terminal.
//  4. CLICOLOR_FORCE is deliberately NOT supported.
//
// Semantic styles:
//   - Green: healthy/available/normal/success
//   - Yellow: low/reserve/partial/stale/warning
//   - Red: exhausted/unavailable/manually disabled/failed/error
//   - Cyan/dim: headings/context

import (
	"io"
	"os"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// ANSI escape sequences for terminal styling. These are only emitted when the
// color policy determines ANSI is enabled (see colorEnabled).
const (
	ansiReset   = "\x1b[0m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiRed     = "\x1b[31m"
	ansiCyan    = "\x1b[36m"
	ansiDim     = "\x1b[2m"
	ansiBoldRed = "\x1b[1;31m"
	ansiBoldYel = "\x1b[1;33m"
)

// isTerminal reports whether w is an interactive terminal. It uses a seam so
// tests can override it.
var isTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// noColorEnv returns the NO_COLOR environment variable value. It is a seam so
// tests can override it.
var noColorEnv = func() string { return os.Getenv("NO_COLOR") }

// colorEnabled reports whether ANSI styling should be emitted for the given
// output stream, following the normative color precedence:
//
//  1. --json always disables ANSI.
//  2. A present, non-empty NO_COLOR env var disables ANSI.
//  3. Otherwise ANSI is enabled only when the destination stream is an
//     interactive terminal.
//  4. CLICOLOR_FORCE is deliberately NOT supported.
func colorEnabled(w io.Writer, jsonOut bool) bool {
	if jsonOut {
		return false
	}
	if v := noColorEnv(); v != "" {
		return false
	}
	return isTerminal(w)
}

// styler wraps a writer and conditionally applies ANSI styling. When enabled is
// false, all styling methods return the plain string unchanged.
type styler struct {
	enabled bool
}

func newStyler(w io.Writer, jsonOut bool) styler {
	return styler{enabled: colorEnabled(w, jsonOut)}
}

// wrap wraps s in the given ANSI sequence + reset when styling is enabled.
func (s styler) wrap(seq, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return seq + text + ansiReset
}

func (s styler) green(text string) string  { return s.wrap(ansiGreen, text) }
func (s styler) yellow(text string) string { return s.wrap(ansiYellow, text) }
func (s styler) red(text string) string    { return s.wrap(ansiRed, text) }
func (s styler) cyan(text string) string   { return s.wrap(ansiCyan, text) }
func (s styler) dim(text string) string    { return s.wrap(ansiDim, text) }

// boldRed colors a severity marker bold-red for error, bold-yellow for warning.
func (s styler) severity(sev doctor.Severity) string {
	switch sev {
	case doctor.Error:
		return s.wrap(ansiBoldRed, string(sev))
	case doctor.Warning:
		return s.wrap(ansiBoldYel, string(sev))
	default:
		return s.dim(string(sev))
	}
}

// styleQuota colors a quota level: normal=green, low=yellow, exhausted=red.
func (s styler) styleQuota(q state.Quota) string {
	return s.quotaStyler(q)(string(q))
}

// quotaStyler returns a string transform that colors arbitrary text with the
// quota semantic color, so padded table cells keep their alignment.
func (s styler) quotaStyler(q state.Quota) func(string) string {
	switch q {
	case state.QuotaNormal:
		return s.green
	case state.QuotaLow:
		return s.yellow
	case state.QuotaExhausted:
		return s.red
	default:
		return noopStyler
	}
}

// styleAvailability colors an availability: available=green, unavailable=red.
func (s styler) styleAvailability(a state.Availability) string {
	return s.availabilityStyler(a)(string(a))
}

func (s styler) availabilityStyler(a state.Availability) func(string) string {
	switch a {
	case state.Available:
		return s.green
	case state.Unavailable:
		return s.red
	default:
		return noopStyler
	}
}

// styleMode colors an effective mode: normal=green, reserve=yellow, disabled=red.
func (s styler) styleMode(m state.Mode) string {
	return s.modeStyler(m)(string(m))
}

func (s styler) modeStyler(m state.Mode) func(string) string {
	switch m {
	case state.ModeNormal:
		return s.green
	case state.ModeReserve:
		return s.yellow
	case state.ModeDisabled:
		return s.red
	default:
		return noopStyler
	}
}

// noopStyler leaves text unchanged (used for unrecognized enum values).
func noopStyler(t string) string { return t }

// styleFreshness colors a freshness: fresh=green, stale=yellow, missing=red.
func (s styler) styleFreshness(f string) string {
	switch strings.ToLower(f) {
	case "fresh":
		return s.green(f)
	case "stale":
		return s.yellow(f)
	case "missing":
		return s.red(f)
	default:
		return f
	}
}
