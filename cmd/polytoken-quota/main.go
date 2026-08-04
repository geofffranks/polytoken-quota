// Command polytoken-quota reconciles durable quota/availability state with the
// explicitly managed Polytoken model fields. This is the CLI shell: it parses
// arguments, wires a context cancelled on interrupt/terminate, constructs a
// fully-wired *service.Coordinator (Mutator + Diagnoser) with real production
// dependencies, and forwards everything to cli.Run.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
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
// overridable via POLYTOKEN_QUOTA_HOME. The Polytoken binary is resolved from
// PATH and is overridable via POLYTOKEN_BINARY. The global
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
	PolytokenEnv map[string]string
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
	if bin != "" {
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return config{}, fmt.Errorf("resolve explicit polytoken binary %q: not executable", bin)
		}
	} else {
		var err error
		bin, err = exec.LookPath("polytoken")
		if err != nil {
			return config{}, fmt.Errorf("resolve polytoken binary: %w", err)
		}
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return config{}, fmt.Errorf("resolve polytoken env file: %w", err)
	}
	polytokenEnv, err := loadPolytokenEnv(filepath.Join(h, ".config", "polytoken.env"), os.Environ())
	if err != nil {
		return config{}, err
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
		PolytokenEnv: polytokenEnv,
	}, nil
}

func inheritedEnvironment(inherited []string) map[string]string {
	env := make(map[string]string, len(inherited))
	for _, kv := range inherited {
		key, value, ok := strings.Cut(kv, "=")
		if ok && key != "" {
			env[key] = value
		}
	}
	return env
}

func loadPolytokenEnv(path string, inherited []string) (map[string]string, error) {
	env := inheritedEnvironment(inherited)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return env, nil
		}
		return nil, fmt.Errorf("read polytoken env file: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimSpace(strings.TrimPrefix(text, "export "))
		key, value, ok := strings.Cut(text, "=")
		if !ok || !validEnvKey(key) {
			return nil, fmt.Errorf("invalid polytoken env file line %d", line)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			decoded, err := strconv.Unquote(value)
			if value[0] == '\'' {
				decoded = value[1 : len(value)-1]
				err = nil
			}
			if err != nil {
				return nil, fmt.Errorf("invalid polytoken env file line %d", line)
			}
			value = decoded
		}
		if existing, exists := env[key]; !exists || existing == "" {
			env[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read polytoken env file: %w", err)
	}
	return env, nil
}

func validEnvKey(key string) bool {
	if key == "" || (key[0] < 'A' || key[0] > 'Z') && (key[0] < 'a' || key[0] > 'z') && key[0] != '_' {
		return false
	}
	for _, r := range key[1:] {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

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
		Env:      cfg.PolytokenEnv,
	}

	builder := staging.Builder{
		TempRoot: cfg.StagingRoot,
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: cfg.GlobalDir},
	}

	return &service.Coordinator{
		Lock:            publish.NewFileLock(cfg.LockPath),
		Policy:          service.FilePolicyLoader{Path: cfg.DesiredPath},
		PolicyWriter:    policy.NewWriter(cfg.DesiredPath),
		State:           service.StoreState{Store: store},
		Targets:         service.NewTargetRegistry(),
		Builder:         service.NewReconciler(),
		Stage:           service.StagingStager{Builder: builder},
		Validate:        service.ValidateRunner{Runner: runner},
		Publish:         service.PublisherAdapter{Publisher: pub},
		DiagnosticState: store,
		Sources:         policy.FilesystemSourceReader{GlobalDir: cfg.GlobalDir, DesiredPath: cfg.DesiredPath},
		QuotaPoller:     service.NewQuotaPoller(),
		JournalPath:     cfg.JournalPath,
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
		Mutator:        coord,
		Diagnoser:      coord,
		RankExplainer:  coord,
		QuotaStater:    coord,
		RoutingToggler: coord,
		Environment:    envSnapshot,
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
