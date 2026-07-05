// Post-land hooks wiring: mapping cfg.Targets' Hooks into hooks.Runner's
// Params lives here, per the same config->package-local-Params pattern
// channels.go uses for history/ghstatus/slack (docs/plans/phase23.md §9.5):
// internal/hooks never imports internal/config.
package main

import (
	"context"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/hooks"
)

// buildHooksRunner constructs the post-land hooks runner (internal/hooks,
// DESIGN.md's decision ledger "Deployments as post-land hooks") from every
// target's configured Hooks. It returns nil (no error) if no target
// configures any hooks — the common case — so callers know not to start a
// Run goroutine or register it as a channel at all.
func buildHooksRunner(cfg *config.Daemon, git core.GitRepo, ex core.Executor, workDir string, emit func(context.Context, core.Event)) *hooks.Runner {
	hookMap := make(map[string][]hooks.Hook)
	for _, t := range cfg.Targets {
		if len(t.Hooks) == 0 {
			continue
		}
		hs := make([]hooks.Hook, len(t.Hooks))
		for i, h := range t.Hooks {
			hs[i] = hooks.Hook{Name: h.Name, Command: h.Command}
		}
		hookMap[t.Name] = hs
	}
	if len(hookMap) == 0 {
		return nil
	}
	return hooks.New(hooks.Params{
		Hooks:   hookMap,
		Git:     git,
		Exec:    ex,
		Emit:    emit,
		WorkDir: workDir,
	})
}
