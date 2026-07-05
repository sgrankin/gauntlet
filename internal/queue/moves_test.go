package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestReconcile_CandidateMovedMidCheck covers Invariant 5: a re-push
// (same ref, new SHA) while a check is gated cancels the run and discards
// its verdict; the ref re-queues (it isn't parked) and a fresh trial starts
// on its new SHA the next tick — not the same one, since a run present at
// the start of a tick claims that whole tick (reconcile.go's
// reconcileTarget doc).
func TestReconcile_CandidateMovedMidCheck(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts
	oldRunID := h.currentRunID()

	newSHA := h.git.pushCandidate(ref, "", checkSpecFile("test")) // re-push: new SHA, same ref

	h.reconcile() // detects the move, cancels + Skips the old run

	recs := h.ch.Records()
	var sawSkipped bool
	for _, r := range recs {
		if r.Outcome == core.OutcomeSkipped {
			sawSkipped = true
		}
	}
	if !sawSkipped {
		t.Fatal("no Skipped RunRecord emitted for the moved candidate")
	}

	h.reconcile() // next tick: not parked, so it re-queues on its new SHA

	if h.git.mergeTreeCalls != 2 {
		t.Fatalf("MergeTree called %d times, want 2 (original trial + re-queued trial on the new SHA)", h.git.mergeTreeCalls)
	}
	newRunID := h.currentRunID()
	if newRunID == oldRunID {
		t.Fatal("no new run started for the re-pushed candidate")
	}
	// The candidate ref must still be at its new SHA (not parked, not
	// touched by the cancellation).
	if got := h.git.ref(ref); got != newSHA {
		t.Fatalf("candidate ref = %q, want %q", got, newSHA)
	}
}

// TestReconcile_CandidateDeletedMidCheck covers Invariant 5's other half: a
// deleted ref while a check is gated cancels the run and Skips, with no new
// trial (the queue is empty again).
func TestReconcile_CandidateDeletedMidCheck(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts
	h.git.deleteCandidate(ref)
	h.reconcile() // detects the deletion, cancels + Skips; nothing to re-queue

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
	if h.git.hasRef(ref) {
		t.Fatal("deleted candidate ref reappeared")
	}
}

// TestReconcile_TargetMovedMidCheck covers the early-cancel path (distinct
// from a stale CAS at land time, docs/plans/phase1.md §5 delta): a target
// branch that moves out from under a run mid-check is detected at the very
// start of the tick, before the check's verdict is even looked at.
func TestReconcile_TargetMovedMidCheck(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts; baseOID = the original target tip

	h.git.directPush("main", map[string]string{"human.txt": "a direct human push"})

	h.reconcile() // detects the target move before polling the check; cancels + Skips

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot removed on a target-moved Skip")
	}
}
