package notice

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

type noticeHistoryConfig struct {
	Mode   string
	Seed   int64
	Budget int
}

func readNoticeHistoryConfig(t *testing.T, modeEnv, seedEnv, budgetEnv string, fastBudget, fullBudget, maxBudget int) noticeHistoryConfig {
	t.Helper()
	mode := os.Getenv(modeEnv)
	if mode == "" {
		mode = "fast"
	}
	if mode != "fast" && mode != "full" {
		t.Fatalf("invalid %s %q (want fast or full)", modeEnv, mode)
	}
	seed := int64(0)
	if raw := os.Getenv(seedEnv); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			t.Fatalf("invalid %s %q (want nonnegative base-10 integer)", seedEnv, raw)
		}
		seed = parsed
	}
	budget := fastBudget
	if mode == "full" {
		budget = fullBudget
	}
	if raw := os.Getenv(budgetEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxBudget {
			t.Fatalf("invalid %s %q (want integer from 1 to %d)", budgetEnv, raw, maxBudget)
		}
		budget = parsed
	}
	if int64(budget-1) > int64(^uint64(0)>>1)-seed {
		t.Fatalf("invalid %s and %s: case seeds overflow", seedEnv, budgetEnv)
	}
	return noticeHistoryConfig{Mode: mode, Seed: seed, Budget: budget}
}

func noticeHistorySeeds(config noticeHistoryConfig) []int64 {
	seeds := make([]int64, config.Budget)
	for i := range seeds {
		seeds[i] = config.Seed + int64(i)
	}
	return seeds
}

func logNoticeHistoryRun(t *testing.T, config noticeHistoryConfig) {
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
	h := fnv.New64a()
	for _, seed := range noticeHistorySeeds(config) {
		_, _ = fmt.Fprintf(h, "%d\n", seed)
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%016x", h.Sum64())))
	t.Logf("history replay: test=%s mode=%s seed=%d budget=%d cases_fingerprint=%x sha=%s go=%s GOOS=%s GOARCH=%s", t.Name(), config.Mode, config.Seed, config.Budget, fingerprint, sha, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
