package main

import (
	"fmt"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// buildExecutor constructs the daemon's check-execution backend: the
// default profile (cfg.Executor — what checks naming no `executor` and
// every post-land hook run on), plus one executor per named profile
// (cfg.Profiles), routed by executor.Mux on CheckJob.Executor. With no
// named profiles the default is returned bare — byte-identical to the
// pre-profiles daemon, no Mux hop.
//
// The returned set is the profile-name vocabulary the queue's
// KnownExecutorProfile predicate answers from, so a spec naming an
// undefined profile is rejected before any of its commands start.
//
// scratchDir roots every check's ephemeral scratch dir: main.go sweeps it
// at startup exactly like trialsDir, safe because AcquireLock guarantees
// only one daemon uses -state at a time. token namespaces container names
// (executor.Params.Token); every container profile shares it — a check
// runs on exactly one profile, so names can't collide across profiles.
// gitDir is the daemon's bare repo path, exported to every check as
// GAUNTLET_GIT_DIR by every profile alike (core.EnvGitDir).
func buildExecutor(cfg *config.Daemon, scratchDir, token, gitDir string) (core.Executor, map[string]bool, error) {
	def, err := buildOneExecutor(cfg.Executor, scratchDir, token, gitDir)
	if err != nil {
		return nil, nil, err
	}
	if len(cfg.Profiles) == 0 {
		return def, nil, nil
	}
	named := make(map[string]core.Executor, len(cfg.Profiles))
	names := make(map[string]bool, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		ex, err := buildOneExecutor(p, scratchDir, token, gitDir)
		if err != nil {
			return nil, nil, err
		}
		named[p.Name] = ex
		names[p.Name] = true
	}
	return executor.Mux{Default: def, Named: named}, names, nil
}

// buildOneExecutor constructs one profile's executor. config validation
// already rejected any Kind outside the switch, so the default case is
// unreachable in practice — it still errors rather than panicking, since
// this constructs from a caller-supplied struct, not only from LoadDaemon.
func buildOneExecutor(e config.Executor, scratchDir, token, gitDir string) (core.Executor, error) {
	label := "executor"
	if e.Name != "" {
		label = fmt.Sprintf("executor %q", e.Name)
	}
	switch e.Kind {
	case "", "local":
		return executor.LocalExecutor{BaseDir: scratchDir, GitDir: gitDir, Env: envPairs(e.Env)}, nil
	case "container":
		caches := make([]executor.Cache, len(e.Caches))
		for i, c := range e.Caches {
			caches[i] = executor.Cache{Name: c.Name, Path: c.Path}
		}
		mounts := make([]executor.Mount, len(e.Mounts))
		for i, m := range e.Mounts {
			mounts[i] = executor.Mount{Host: m.Host, Path: m.Path, ReadOnly: m.ReadOnly}
		}
		addHosts := make([]string, len(e.AddHosts))
		for i, ah := range e.AddHosts {
			addHosts[i] = ah.Host + ":" + ah.Gateway
		}
		ex, err := executor.New(executor.Params{
			Runtime:    e.Runtime,
			Image:      e.Image,
			Workdir:    e.Workdir,
			Caches:     caches,
			Mounts:     mounts,
			Env:        envPairs(e.Env),
			AddHosts:   addHosts,
			Memory:     e.Memory,
			CPUs:       e.CPUs,
			ScratchDir: scratchDir,
			Token:      token,
			GitDir:     gitDir,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		return ex, nil
	default:
		return nil, fmt.Errorf("%s: unknown kind %q", label, e.Kind)
	}
}

// envPairs renders a profile's fixed env as the "NAME=VALUE" strings both
// executors consume.
func envPairs(env []config.EnvVar) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, len(env))
	for i, ev := range env {
		out[i] = ev.Name + "=" + ev.Value
	}
	return out
}
