package publish

import (
	"crypto/sha256"
	"debug/buildinfo"
	"fmt"
	"hash/fnv"
	"os"
	"runtime"
	"strconv"
	"testing"
)

type historyConfig struct {
	Mode   string
	Seed   int64
	Budget int
}

func readHistoryConfig(t *testing.T, modeEnv, seedEnv, budgetEnv string, fastBudget, fullBudget, maxBudget int) historyConfig {
	t.Helper()
	config, err := parseHistoryConfig(os.Getenv(modeEnv), os.Getenv(seedEnv), os.Getenv(budgetEnv), fastBudget, fullBudget, maxBudget)
	if err != nil {
		t.Fatalf("invalid history controls: %v", err)
	}
	return config
}

func parseHistoryConfig(mode, seedRaw, budgetRaw string, fastBudget, fullBudget, maxBudget int) (historyConfig, error) {
	if mode == "" {
		mode = "fast"
	}
	if mode != "fast" && mode != "full" {
		return historyConfig{}, fmt.Errorf("mode %q (want fast or full)", mode)
	}
	if fastBudget < 1 || fullBudget < fastBudget || maxBudget < fullBudget {
		return historyConfig{}, fmt.Errorf("invalid test budget bounds")
	}
	seed := int64(1)
	if seedRaw != "" {
		parsed, err := strconv.ParseInt(seedRaw, 10, 64)
		if err != nil || parsed < 0 {
			return historyConfig{}, fmt.Errorf("seed %q (want nonnegative base-10 integer)", seedRaw)
		}
		seed = parsed
	}
	budget := fastBudget
	if mode == "full" {
		budget = fullBudget
	}
	if budgetRaw != "" {
		parsed, err := strconv.Atoi(budgetRaw)
		if err != nil || parsed < 1 || parsed > maxBudget {
			return historyConfig{}, fmt.Errorf("budget %q (want integer from 1 to %d)", budgetRaw, maxBudget)
		}
		budget = parsed
	}
	if int64(budget-1) > int64(^uint64(0)>>1)-seed {
		return historyConfig{}, fmt.Errorf("seed %d and budget %d overflow case seeds", seed, budget)
	}
	return historyConfig{Mode: mode, Seed: seed, Budget: budget}, nil
}

func historySeeds(config historyConfig) []int64 {
	seeds := make([]int64, config.Budget)
	for i := range seeds {
		seeds[i] = config.Seed + int64(i)
	}
	return seeds
}

func logHistoryRun(t *testing.T, config historyConfig) {
	t.Helper()
	sha := "unknown"
	if info, err := buildinfo.ReadFile(os.Args[0]); err == nil {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				sha = setting.Value
				break
			}
		}
	}
	seeds := historySeeds(config)
	h := fnv.New64a()
	for _, seed := range seeds {
		_, _ = fmt.Fprintf(h, "%d\n", seed)
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%016x", h.Sum64())))
	t.Logf("history replay: test=%s mode=%s seed=%d budget=%d cases_fingerprint=%x sha=%s go=%s GOOS=%s GOARCH=%s", t.Name(), config.Mode, config.Seed, config.Budget, fingerprint, sha, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
