package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestCommand_RetryClearsParkAndRetests covers docs/plans/phase23.md §2.2:
// a CommandRetry for a parked (target, ref) clears the park and re-tests
// the same SHA on the very reconcile pass that drains it — drainCommands
// runs before syncBookkeeping/pickHead within one ReconcileOnce, so the
// park is gone before the target loop looks for a head to pick.
func TestCommand_RetryClearsParkAndRetests(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // rejects + parks

	callsBefore := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != callsBefore {
		t.Fatal("candidate re-tested before any retry; test setup is wrong")
	}

	h.ch.SendCommand(core.Command{Kind: core.CommandRetry, Target: "main", Ref: ref})
	h.reconcile() // drains the retry command and, same pass, re-tests

	var sawRetryQueued bool
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventQueued && e.Detail == "retry: park cleared" && e.Candidate.Ref == ref {
			sawRetryQueued = true
		}
	}
	if !sawRetryQueued {
		t.Fatal("no EventQueued with the retry detail after CommandRetry")
	}
	if h.git.mergeTreeCalls != callsBefore+1 {
		t.Fatalf("MergeTree calls = %d, want %d (re-tested exactly once after retry)", h.git.mergeTreeCalls, callsBefore+1)
	}
	newRunID := h.currentRunID()
	if newRunID == runID {
		t.Fatal("no new run started for the retried candidate")
	}

	h.release(newRunID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land; drain
}

// TestCommand_RetryIdempotentWhenNotParked covers the idempotence half of
// §2.2: a retry for a ref that isn't currently parked (never rejected, or
// already cleared by an earlier retry/re-push) is a silent no-op — no
// EventQueued, no perturbation of any in-flight run.
func TestCommand_RetryIdempotentWhenNotParked(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts; nothing parked yet
	runID := h.currentRunID()
	before := len(h.ch.Events())

	h.ch.SendCommand(core.Command{Kind: core.CommandRetry, Target: "main", Ref: ref})
	h.reconcile()

	for _, e := range h.ch.Events()[before:] {
		if e.Kind == core.EventQueued && e.Detail == "retry: park cleared" {
			t.Fatal("retry emitted a park-cleared event for a ref that was never parked")
		}
	}
	if h.d.headRun("main") == nil || h.d.headRun("main").runID != runID {
		t.Fatal("the in-flight run was perturbed by a no-op retry")
	}

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land; drain
}

// TestCommand_RetryUnknownTargetIsNoop covers a retry naming a target the
// daemon has no bookkeeping for at all (d.done[target] == nil): must not
// panic and must not touch anything.
func TestCommand_RetryUnknownTargetIsNoop(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)

	h.ch.SendCommand(core.Command{Kind: core.CommandRetry, Target: "does-not-exist", Ref: "refs/heads/for/does-not-exist/alice/x"})
	h.reconcile() // must not panic
}

// TestCommand_UnknownKindIsNoop covers applyCommand's default case: a
// Command whose Kind the queue doesn't recognize (a future channel's
// command it doesn't yet support) is ignored, not an error.
func TestCommand_UnknownKindIsNoop(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // rejects + parks

	h.ch.SendCommand(core.Command{Kind: "some-future-command", Target: "main", Ref: ref})
	callsBefore := h.git.mergeTreeCalls
	h.reconcile()
	if h.git.mergeTreeCalls != callsBefore {
		t.Fatal("an unrecognized Command Kind perturbed the parked candidate")
	}
}
