// Command polytoken-quota reconciles durable quota/availability state with the
// explicitly managed Polytoken model fields. This is the CLI shell: it parses
// arguments, wires a context cancelled on interrupt/terminate, and forwards
// everything to cli.Run. The Mutator/Diagnoser coordinator is wired in Task 12.
package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/geofffranks/codexbar-hooks/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.Dependencies{
		// Mutator and Diagnoser are bound to service.Coordinator in Task 12.
		Environment: envSnapshot,
	})
	os.Exit(code)
}

// envSnapshot returns the supported CODEXBAR_* environment variables as a map.
// Only these are forwarded to hook.Decode; the rest of the process environment
// is never inspected or leaked.
func envSnapshot() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		if strings.HasPrefix(key, "CODEXBAR_") {
			out[key] = kv[eq+1:]
		}
	}
	return out
}
