// Deploy-tracker wiring: mapping cfg.Deploy's environments into
// deploy.Params lives here, following the same config->package-local-Params
// pattern hooks.go and channels.go use — internal/deploy never imports
// internal/config.
package main

import (
	"io"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/deploy"
)

// buildDeployTracker constructs the deploy-ref tracker (internal/deploy,
// docs/design/deployment.md) from the daemon's configured environments. It
// returns nil (no error) when no environment is configured — the common
// case — so callers know not to start a tick goroutine at all, exactly
// like buildHooksRunner.
//
// config.validate() has already rejected anything outside the value sets
// below (exactly one source form, on-desired-move in {finish,cancel}), so
// the mapping never has to decide what a malformed environment means.
func buildDeployTracker(cfg *config.Daemon, git deploy.Git, log io.Writer) *deploy.Tracker {
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
	return deploy.New(deploy.Params{Environments: envs, Git: git, Log: log})
}
