package executor

import (
	"context"
	"fmt"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Mux routes each CheckJob to the executor profile it names
// (CheckJob.Executor; "" = Default) — the core.Executor implementation a
// daemon with named profiles hands the queue and the hooks runner, so
// one candidate can mix local and container checks while the queue core
// stays executor-agnostic (Invariant 8: it sees one core.Executor either
// way).
//
// An unknown profile name returns an Err result (park-as-error), but only
// as defense in depth: the queue rejects a spec naming an unknown profile
// before any of its commands start (queue.Config.KnownExecutorProfile),
// so reaching that branch means the wiring changed underneath a run.
type Mux struct {
	// Default runs every job that names no profile — the daemon's default
	// executor, exactly what a profile-less daemon uses directly.
	Default core.Executor

	// Named maps profile name -> executor. Never mutated after
	// construction (RunCheck is called from many goroutines).
	Named map[string]core.Executor
}

var _ core.Executor = Mux{}

func (m Mux) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	if job.Executor == "" {
		return m.Default.RunCheck(ctx, job)
	}
	ex, ok := m.Named[job.Executor]
	if !ok {
		return core.CheckResult{Name: job.Name, Command: job.Command, Err: fmt.Errorf("unknown executor profile %q", job.Executor)}
	}
	return ex.RunCheck(ctx, job)
}
