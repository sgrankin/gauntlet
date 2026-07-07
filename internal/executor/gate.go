package executor

import (
	"context"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// GatedExecutor is a core.Executor test double. RunCheck registers by
// (RunID, Name) and blocks until the test calls
// Release with the same (RunID, Name), which supplies the CheckResult
// RunCheck returns. This lets tests step a run check-by-check.
//
// The zero value is not usable; construct with NewGatedExecutor.
type GatedExecutor struct {
	mu      sync.Mutex
	release map[gateKey]chan core.CheckResult
	started map[gateKey]chan struct{}
}

type gateKey struct {
	runID string
	name  string
}

// NewGatedExecutor returns a ready-to-use GatedExecutor.
func NewGatedExecutor() *GatedExecutor {
	return &GatedExecutor{
		release: make(map[gateKey]chan core.CheckResult),
		started: make(map[gateKey]chan struct{}),
	}
}

// Started returns a channel that closes once a check matching (runID, name)
// has registered with RunCheck. Safe to call before or after that
// registration happens — tests can wait on it to avoid racing RunCheck's
// goroutine.
func (g *GatedExecutor) Started(runID, name string) <-chan struct{} {
	return g.startedChan(gateKey{runID, name})
}

// Release delivers result to the RunCheck call gated on (runID, name),
// unblocking it. Release must be called at most once per (runID, name).
func (g *GatedExecutor) Release(runID, name string, result core.CheckResult) {
	g.releaseChan(gateKey{runID, name}) <- result
}

// RunCheck implements core.Executor.
func (g *GatedExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	start := time.Now()
	k := gateKey{job.RunID, job.Name}

	release := g.releaseChan(k)
	close(g.startedChan(k))

	select {
	case res := <-release:
		return res
	case <-ctx.Done():
		return core.CheckResult{
			Name:     job.Name,
			Err:      ctx.Err(),
			Duration: time.Since(start),
		}
	}
}

func (g *GatedExecutor) releaseChan(k gateKey) chan core.CheckResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.release[k]
	if !ok {
		ch = make(chan core.CheckResult, 1)
		g.release[k] = ch
	}
	return ch
}

func (g *GatedExecutor) startedChan(k gateKey) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.started[k]
	if !ok {
		ch = make(chan struct{})
		g.started[k] = ch
	}
	return ch
}
