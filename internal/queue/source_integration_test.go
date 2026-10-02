package queue

import (
	"context"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

type slowCancellationExecutor struct {
	started, cancelled, release chan struct{}
}

func (e *slowCancellationExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	close(e.started)
	<-ctx.Done()
	close(e.cancelled)
	<-e.release
	return core.CheckResult{Name: job.Name, Err: ctx.Err()}
}

func TestSourceSurvivesPruningUntilCancelledWorkerFinishes(t *testing.T) {
	ex := &slowCancellationExecutor{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	h := newIntegrationHarness(t, nil, ex, config.Target{Name: "main", Branch: "main", Landing: "squash"})
	h.remote.Seed("main", checkSpecFile("test"))
	ref := h.remote.PushCandidate("main", "author", "change", map[string]string{"a": "change"})
	source := h.remote.Ref(ref)
	h.reconcile()
	select {
	case <-ex.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	h.remote.DeleteCandidate(ref)
	h.reconcile()
	select {
	case <-ex.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not receive cancellation")
	}
	if h.d.headRun("main") != nil {
		t.Fatal("withdrawn run still active")
	}
	cutoff := time.Now().Add(time.Hour)
	result, err := h.git.PruneSources(t.Context(), cutoff, true)
	if err != nil || len(result.Expired) != 0 {
		t.Fatalf("cancelled worker source pruned: %+v %v", result, err)
	}
	testutil.GCPruneNow(t, h.dir)
	if out, err := h.gitDirQuery("cat-file", "-e", source); err != nil {
		t.Fatalf("source collected before worker cleanup: %v %s", err, out)
	}
	close(ex.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		result, err = h.git.PruneSources(t.Context(), cutoff, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Expired) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker leaked source lease")
		}
	}
	testutil.GCPruneNow(t, h.dir)
	if _, err := h.gitDirQuery("cat-file", "-e", source); err == nil {
		t.Fatal("released source remained reachable")
	}
}
