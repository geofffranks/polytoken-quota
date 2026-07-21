// Command polytoken-quota reconciles durable quota/availability state with the
// explicitly managed Polytoken model fields. This is the CLI shell: it parses
// arguments, wires a context cancelled on interrupt/terminate, constructs a
// fully-wired *service.Coordinator (Mutator + Diagnoser) with real production
// dependencies, and forwards everything to cli.Run.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/cli"
	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/publish"
	"github.com/geofffranks/codexbar-hooks/internal/service"
	"github.com/geofffranks/codexbar-hooks/internal/staging"
	"github.com/geofffranks/codexbar-hooks/internal/state"
	"github.com/geofffranks/codexbar-hooks/internal/validate"
)

// config resolves every path and setting the Coordinator needs from the
// environment. The utility root defaults to $HOME/.polytoken-quota and is
// overridable via POLYTOKEN_QUOTA_HOME. The Polytoken binary defaults to the
// supported pinned path and is overridable via POLYTOKEN_BINARY. The global
// Polytoken configuration directory defaults to $HOME/.config/polytoken and is
// overridable via POLYTOKEN_CONFIG_DIR.
type config struct {
	Home         string // utility root for state/lock/journal/backups/staging
	DesiredPath  string // desired.yaml
	StatePath    string // state.json
	LockPath     string // advisory apply lock
	JournalPath  string // write-ahead journal
	BackupsRoot  string // bounded backup store
	StagingRoot  string // parent of transient staging roots
	GlobalDir    string // canonical global Polytoken configuration dir
	PolytokenBin string // Polytoken executable for validation
	BackupCount  int
	Retention    time.Duration
	LockWait     time.Duration
	ValidateWait time.Duration
}

func resolveConfig() (config, error) {
	home := os.Getenv("POLYTOKEN_QUOTA_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return config{}, fmt.Errorf("resolve utility home: %w", err)
		}
		home = filepath.Join(h, ".polytoken-quota")
	}
	globalDir := os.Getenv("POLYTOKEN_CONFIG_DIR")
	if globalDir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return config{}, fmt.Errorf("resolve polytoken config dir: %w", err)
		}
		globalDir = filepath.Join(h, ".config", "polytoken")
	}
	bin := os.Getenv("POLYTOKEN_BINARY")
	if bin == "" {
		bin = defaultPolytokenBinary
	}
	return config{
		Home:         home,
		DesiredPath:  filepath.Join(home, "desired.yaml"),
		StatePath:    filepath.Join(home, "state.json"),
		LockPath:     filepath.Join(home, "lock", "apply.lock"),
		JournalPath:  filepath.Join(home, "journal", "apply.json"),
		BackupsRoot:  filepath.Join(home, "backups"),
		StagingRoot:  filepath.Join(home, "stage"),
		GlobalDir:    globalDir,
		PolytokenBin: bin,
		BackupCount:  5,
		Retention:    7 * 24 * time.Hour,
		LockWait:     10 * time.Second,
		ValidateWait: 30 * time.Second,
	}, nil
}

// defaultPolytokenBinary is the supported pinned Polytoken contract binary.
const defaultPolytokenBinary = "/home/linuxbrew/.linuxbrew/bin/polytoken"

// newCoordinator constructs a fully-wired *service.Coordinator with all real
// production dependencies. Every field is wired so no command nil-panics.
// Sources (Init/Sync's polytoken source reader) reads the managed subset of the
// global config and explicitly registered project roots without retaining
// credentials or unrelated configuration.
func newCoordinator(cfg config) *service.Coordinator {
	store := state.Store{
		Path:               cfg.StatePath,
		RecoveredRetention: cfg.Retention,
	}

	pub := publish.Publisher{
		Locker:      publish.NewFileLock(cfg.LockPath),
		State:       store,
		JournalPath: cfg.JournalPath,
		Backups:     publish.BackupStore{Root: cfg.BackupsRoot, Limit: cfg.BackupCount},
		ManagedRoot: cfg.GlobalDir,
	}

	runner := validate.Runner{
		Binary:   cfg.PolytokenBin,
		Commands: validate.ExecRunner{},
	}

	builder := staging.Builder{
		TempRoot: cfg.StagingRoot,
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: cfg.GlobalDir},
	}

	return &service.Coordinator{
		Lock:            publish.NewFileLock(cfg.LockPath),
		Policy:          service.FilePolicyLoader{DesiredPath: cfg.DesiredPath},
		PolicyWriter:    policy.NewWriter(cfg.DesiredPath),
		State:           service.StoreState{Store: store},
		Targets:         service.NewTargetRegistry(),
		Builder:         service.NewReconciler(),
		Stage:           service.StagingStager{Builder: builder},
		Validate:        service.ValidateRunner{Runner: runner},
		Publish:         service.PublisherAdapter{Publisher: pub},
		DiagnosticState: store,
		Sources:         policy.FilesystemSourceReader{GlobalDir: cfg.GlobalDir, DesiredPath: cfg.DesiredPath},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := resolveConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(cli.ExitRejected)
	}

	coord := newCoordinator(cfg)

	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.Dependencies{
		Mutator:     coord,
		Diagnoser:   coord,
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
