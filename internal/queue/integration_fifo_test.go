package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_FIFOSkipFailedHead is §5's "FIFO + skip-failed-head" row:
// candidate A arrives first and holds the lane even while it's rejected;
// candidate B, arriving second, proceeds once A parks; A re-enters the
// queue only on a re-push (new SHA clears the park).
func TestIntegration_FIFOSkipFailedHead(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	refA := remote.PushCandidate("main", "alice", "first", distinctFiles(checkSpecFile("test")))
	h.reconcile() // A's trial starts (only candidate)
	runIDA := h.currentRunID()

	refB := remote.PushCandidate("main", "bob", "second", distinctFiles(checkSpecFile("test")))
	h.reconcile() // one lane: A still owns it; B just queues
	h.awaitStarted(gated, runIDA, "test")

	h.releaseGated(gated, runIDA, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // A rejected + parked
	h.reconcile()                                                                                   // next tick: B's trial starts
	runIDB := h.currentRunID()
	if runIDB == runIDA {
		t.Fatal("no new run started for B after A vacated the lane")
	}
	h.releaseGated(gated, runIDB, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	if remote.Ref(refA) == "" {
		t.Fatal("A's rejected slot vanished; it should only be parked")
	}
	if remote.Ref(refB) != "" {
		t.Fatal("B's landed slot survived")
	}

	// A re-enters the queue on re-push (park clears on SHA change).
	remote.MoveCandidate(refA, distinctFiles(checkSpecFile("test")))
	h.reconcile()
	runIDA2 := h.currentRunID()
	if runIDA2 == runIDA {
		t.Fatal("A not re-tested after re-push; the park outlived the SHA change")
	}
	h.releaseGated(gated, runIDA2, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if remote.Ref(refA) != "" {
		t.Fatal("A's slot survived after landing on its re-push")
	}
}
