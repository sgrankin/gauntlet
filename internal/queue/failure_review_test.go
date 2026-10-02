package queue

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/flaky"
)

type failureDecisionFunc func(context.Context, core.CheckJob, core.CheckResult) (flaky.Decision, error)

func (f failureDecisionFunc) Classify(ctx context.Context, j core.CheckJob, r core.CheckResult) (flaky.Decision, error) {
	return f(ctx, j, r)
}

type retryGate struct {
	*executor.GatedExecutor
	mu           sync.Mutex
	jobs         map[string][]core.CheckJob
	retryStarted chan core.CheckJob
	retryResult  chan core.CheckResult
}

func (e *retryGate) RunCheck(ctx context.Context, j core.CheckJob) core.CheckResult {
	e.mu.Lock()
	e.jobs[j.RunID] = append(e.jobs[j.RunID], j)
	attempt := len(e.jobs[j.RunID])
	e.mu.Unlock()
	if attempt == 1 {
		return e.GatedExecutor.RunCheck(ctx, j)
	}
	e.retryStarted <- j
	select {
	case result := <-e.retryResult:
		result.Name = j.Name
		return result
	case <-ctx.Done():
		return core.CheckResult{Name: j.Name, Err: ctx.Err()}
	}
}

func TestFailureReviewPreservesSpeculationWindow(t *testing.T) {
	exec := &retryGate{GatedExecutor: executor.NewGatedExecutor(), jobs: map[string][]core.CheckJob{}, retryStarted: make(chan core.CheckJob, 1), retryResult: make(chan core.CheckResult, 1)}
	h := newHarnessWithExecutor(t, exec, nil, speculateTarget(3))
	deciding := make(chan struct{})
	releaseDecision := make(chan struct{})
	h.d.cfg.FailureReview = &flaky.Retrier{Checks: []string{"test"}, MaxRetries: 1, MinConfidence: .8, Timeout: 5 * time.Second, Classifier: failureDecisionFunc(func(ctx context.Context, _ core.CheckJob, _ core.CheckResult) (flaky.Decision, error) {
		close(deciding)
		select {
		case <-releaseDecision:
			return flaky.Decision{Action: "retry", Confidence: .95, Reason: "transient reset"}, nil
		case <-ctx.Done():
			return flaky.Decision{}, ctx.Err()
		}
	})}
	pushThreeSpeculateCandidates(h)
	h.reconcile()
	runs := append([]*run(nil), h.d.lanes["main"].runs...)
	for _, r := range runs {
		h.awaitStarted(r.runID, "test")
	}
	exec.Release(runs[0].runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed, Output: "connection reset"})
	select {
	case <-deciding:
	case <-time.After(5 * time.Second):
		t.Fatal("classification did not start")
	}
	h.reconcile()
	assertWindow := func() {
		t.Helper()
		current := h.d.lanes["main"].runs
		if len(current) != len(runs) {
			t.Fatal("flake reset the window")
		}
		for i, r := range current {
			if r != runs[i] {
				t.Fatal("flake rebuilt a tested successor")
			}
		}
	}
	assertWindow()
	// A verified successor can finish while its predecessor is deciding.
	h.release(runs[1].runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	assertWindow()
	close(releaseDecision)
	var retryJob core.CheckJob
	select {
	case retryJob = <-exec.retryStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not start")
	}
	if retryJob.RunID != runs[0].runID || retryJob.MergeSHA != runs[0].chainTip || retryJob.Dir != runs[0].dir {
		t.Fatal("retry changed the tested identity")
	}
	h.reconcile()
	assertWindow()
	exec.retryResult <- core.CheckResult{Status: core.CheckPassed, Output: "passed rerun"}
	finished := false
	for range 100000 {
		h.reconcile()
		if checkFinishedObserved(h.ch.Events(), runs[0].runID, "test") {
			finished = true
			break
		}
		runtime.Gosched()
	}
	if !finished {
		t.Fatal("retry never concluded")
	}
	h.release(runs[2].runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	landed := 0
	for _, event := range h.ch.Events() {
		switch event.Kind {
		case core.EventLanded:
			landed++
		case core.EventSkipped, core.EventRejected, core.EventError:
			t.Fatalf("flake caused terminal failure %v", event.Kind)
		}
	}
	if landed != 3 {
		t.Fatalf("landed %d want 3", landed)
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.jobs[runs[0].runID]) != 2 || len(exec.jobs[runs[1].runID]) != 1 || len(exec.jobs[runs[2].runID]) != 1 {
		t.Fatal("successful successors were rerun")
	}
}
