package main

import (
	"fmt"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// buildExecutor selects the check-execution backend per cfg.Executor
// (docs/plans/phase23.md §4.5): "local" (config's default) is the phase-1
// in-process executor; "container" wraps a docker-compatible CLI.
// config.Daemon.validate already rejects any other Kind, so the switch's
// default case is unreachable in practice — it still returns an error
// rather than panicking, since this constructs from a caller-supplied
// struct, not only from LoadDaemon.
//
// scratchDir roots every check's ephemeral scratch dir (S16, phase-6 audit
// synthesis): main.go sweeps it at startup exactly like trialsDir, now that
// AcquireLock (S2) makes that sweep safe. Empty preserves each executor's
// prior os.MkdirTemp("", ...) fallback verbatim — every existing caller
// that built one of these executors directly (tests) is unaffected.
//
// token namespaces the container executor's container names (B1, phase-6
// B-track review) — see executor.Params.Token's doc. Unused by the local
// executor, which has no host-global naming namespace to collide on.
func buildExecutor(cfg *config.Daemon, scratchDir, token string) (core.Executor, error) {
	switch cfg.Executor.Kind {
	case "", "local":
		return executor.LocalExecutor{BaseDir: scratchDir}, nil
	case "container":
		caches := make([]executor.Cache, len(cfg.Executor.Caches))
		for i, c := range cfg.Executor.Caches {
			caches[i] = executor.Cache{Name: c.Name, Path: c.Path}
		}
		mounts := make([]executor.Mount, len(cfg.Executor.Mounts))
		for i, m := range cfg.Executor.Mounts {
			mounts[i] = executor.Mount{Host: m.Host, Path: m.Path, ReadOnly: m.ReadOnly}
		}
		ex, err := executor.New(executor.Params{
			Runtime:    cfg.Executor.Runtime,
			Image:      cfg.Executor.Image,
			Workdir:    cfg.Executor.Workdir,
			Caches:     caches,
			Mounts:     mounts,
			ScratchDir: scratchDir,
			Token:      token,
		})
		if err != nil {
			return nil, fmt.Errorf("container executor: %w", err)
		}
		return ex, nil
	default:
		return nil, fmt.Errorf("executor: unknown kind %q", cfg.Executor.Kind)
	}
}
