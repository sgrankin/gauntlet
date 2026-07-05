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
func buildExecutor(cfg *config.Daemon) (core.Executor, error) {
	switch cfg.Executor.Kind {
	case "", "local":
		return executor.LocalExecutor{}, nil
	case "container":
		caches := make([]executor.Cache, len(cfg.Executor.Caches))
		for i, c := range cfg.Executor.Caches {
			caches[i] = executor.Cache{Name: c.Name, Path: c.Path}
		}
		ex, err := executor.New(executor.Params{
			Runtime: cfg.Executor.Runtime,
			Image:   cfg.Executor.Image,
			Workdir: cfg.Executor.Workdir,
			Caches:  caches,
		})
		if err != nil {
			return nil, fmt.Errorf("container executor: %w", err)
		}
		return ex, nil
	default:
		return nil, fmt.Errorf("executor: unknown kind %q", cfg.Executor.Kind)
	}
}
