// Deploy-tracker wiring: mapping cfg.Deploy's environments into
// deploy.Params lives here, following the same config->package-local-Params
// pattern hooks.go and channels.go use — internal/deploy never reads the
// daemon's operator config itself.
package main

import (
	"context"
	"io"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/deploy"
)

// deployRuntime is the lane runner's half of the wiring (internal/deploy's
// run.go): everything a Tracker needs to actually EXECUTE an environment's
// deploy graph, as opposed to merely tracking its desired ref. Grouped into
// one struct so buildDeployTracker keeps a readable signature and so a
// caller that has no runner to offer (a wiring test) can pass the zero
// value and get D1's tracker exactly.
type deployRuntime struct {
	// Exec runs each node's command; Slots is the daemon-wide
	// max-executions cap deploy nodes share with checks and hooks.
	Exec  core.Executor
	Slots *core.Slots

	// Emit fans deploy events out to the daemon's channels — the same
	// closure shape hooks gets, over the same channel snapshot.
	Emit func(context.Context, core.Event)

	// KnownExecutorProfile is executorPredicates' known-profile predicate,
	// shared verbatim with the queue's spec-load gate so a deploy node and
	// a check are held to the same profile vocabulary.
	KnownExecutorProfile func(string) bool

	// WorkDir is the scratch root per-run tree exports live under (swept at
	// startup like the trials and hooks dirs); LogDir is the shared log
	// root, so deploy node logs land inside the existing retention sweep.
	WorkDir string
	LogDir  string
}

// buildDeployTracker constructs the deploy tracker (internal/deploy,
// docs/design/deployment.md) from the daemon's configured environments. It
// returns nil (no error) when no environment is configured — the common
// case — so callers know not to start a tick goroutine at all, exactly
// like buildHooksRunner.
//
// config.validate() has already rejected anything outside the value sets
// below (exactly one source form, on-desired-move in {finish,cancel}), so
// the mapping never has to decide what a malformed environment means.
func buildDeployTracker(cfg *config.Daemon, git deploy.Git, rt deployRuntime, log io.Writer) *deploy.Tracker {
	if len(cfg.Deploy.Environments) == 0 {
		return nil
	}
	envs := make([]deploy.Environment, len(cfg.Deploy.Environments))
	for i, e := range cfg.Deploy.Environments {
		mode := deploy.ModeManual
		if e.Track != nil {
			mode = deploy.ModeTrack
		}
		envs[i] = deploy.Environment{
			Name:          e.Name,
			SourceBranch:  e.Source.Branch,
			SourceEnv:     e.Source.Env,
			Mode:          mode,
			Nodes:         e.Nodes,
			MaxParallel:   e.MaxParallel,
			OnDesiredMove: deploy.OnDesiredMove(e.OnDesiredMove),
		}
	}
	return deploy.New(deploy.Params{
		Environments: envs,
		Git:          git,
		Log:          log,

		Exec:                 rt.Exec,
		Slots:                rt.Slots,
		Emit:                 rt.Emit,
		CheckSpec:            cfg.CheckSpec,
		KnownExecutorProfile: rt.KnownExecutorProfile,
		WorkDir:              rt.WorkDir,
		LogDir:               rt.LogDir,
		HistoryMtimes:        cfg.Export.Mtimes == "history",
		// AutoRetryErrors is a *bool defaulted true in config.applyDefaults
		// (absent-vs-explicit-false needs the pointer); the deploy lane
		// takes the resolved value, the same one the queue takes.
		AutoRetryErrors: *cfg.AutoRetryErrors,
	})
}
