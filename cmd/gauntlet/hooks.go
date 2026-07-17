// Post-land hooks wiring: mapping cfg.Targets' Hooks into hooks.Runner's
// Params lives here, following the same config->package-local-Params
// pattern channels.go uses for history/ghstatus/slack: internal/hooks
// never imports internal/config.
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
//
// logDir is wired straight into hooks.Params.LogDir — the same <state>/logs
// directory queue.Config.LogDir also gets (main.go), so a hook's full log
// file lands in the exact per-run directory its landing's own check logs
// already live under (log/history parity with checks, chunk P5-B): the
// existing retention sweep (logs.go's pruneLogFiles, keyed on that
// directory's mtime) covers hook logs for free, with no separate sweep.
func buildHooksRunner(cfg *config.Daemon, git core.GitRepo, ex core.Executor, slots *core.Slots, workDir, logDir string, emit func(context.Context, core.Event)) *hooks.Runner {
	hookMap := make(map[string][]hooks.Hook)
	policyMap := make(map[string]hooks.Policy)
	for _, t := range cfg.Targets {
		if len(t.Hooks) == 0 {
			continue
		}
		hs := make([]hooks.Hook, len(t.Hooks))
		for i, h := range t.Hooks {
			hs[i] = hooks.Hook{Name: h.Name, Command: h.Command}
		}
		hookMap[t.Name] = hs
		// config.validate() already rejected any value outside
		// hooks.Policy's set (and requires it be set at all whenever
		// Hooks is non-empty), so this cast is always one of
		// PolicyQueue/PolicyCoalesce/PolicyCancel here.
		policyMap[t.Name] = hooks.Policy(t.HooksPolicy)
	}
	if len(hookMap) == 0 {
		return nil
	}
	return hooks.New(hooks.Params{
		Hooks:    hookMap,
		Policies: policyMap,
		Git:      git,
		Exec:     ex,
		Slots:    slots,
		Emit:     emit,
		WorkDir:  workDir,
		LogDir:   logDir,
	})
}
