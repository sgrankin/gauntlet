package queue

import (
	"errors"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestReconcile_ParkSkipSurvivesOtherLandings is the §9.1 park test: a
// parked (ref, SHA) is never re-tested, even after some other candidate on
// the same target lands.
func TestReconcile_ParkSkipSurvivesOtherLandings(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)

	refA := candidateRef("main", "alice", "first")
	h.git.pushCandidate(refA, "", checkSpecFile("test"))
	h.reconcile() // A's trial starts
	runIDA := h.currentRunID()
	h.release(runIDA, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // A rejected + parked

	refB := candidateRef("main", "bob", "second")
	h.git.pushCandidate(refB, "", checkSpecFile("test"))
	h.reconcile() // A is parked and skipped; B's trial starts
	runIDB := h.currentRunID()
	if runIDB == runIDA {
		t.Fatal("B did not get a new run; A's park did not free the queue")
	}
	h.release(runIDB, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // B lands

	if !h.git.hasRef(refA) {
		t.Fatal("A's slot was removed; it should only be parked, not landed")
	}
	if h.git.hasRef(refB) {
		t.Fatal("B's slot should be deleted after landing")
	}

	mergeTreeCallsBefore := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != mergeTreeCallsBefore {
		t.Fatal("A was re-tested after an unrelated landing; parks must be sticky per (ref, SHA) until re-push (§9.1)")
	}
}

func TestReconcile_TrialConflict(t *testing.T) {
	h := newHarness(t)
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	candSHA := h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.git.scriptConflict(base, candSHA, []string{"conflict.txt"})

	h.reconcile()

	if got := h.git.ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on conflict: %q, want unchanged %q", got, base)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot removed on conflict")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeConflict {
		t.Fatalf("Outcome = %v, want Conflict", last.Outcome)
	}

	mergeTreeCallsBefore := h.git.mergeTreeCalls
	h.reconcile()
	if h.git.mergeTreeCalls != mergeTreeCallsBefore {
		t.Fatal("parked conflicted candidate re-tested on the next tick")
	}
}

func TestReconcile_MissingCheckSpec(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", nil) // no .gauntlet.kdl at all

	h.reconcile()

	if !h.git.hasRef(ref) {
		t.Fatal("slot removed for a missing check spec")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if last.Detail == "" {
		t.Error("Detail should name the missing check-spec path")
	}

	commitsBefore := h.git.commitTreeCalls
	h.reconcile()
	if h.git.commitTreeCalls != commitsBefore {
		t.Fatal("parked missing-spec candidate re-tested on the next tick")
	}
}

func TestReconcile_InvalidCheckSpec(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", map[string]string{testCheckSpecPath: ""}) // parses, but "no checks defined"

	h.reconcile()

	if !h.git.hasRef(ref) {
		t.Fatal("slot removed for an invalid check spec")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
}

func TestReconcile_ExecErrParksNoSpin(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Err: errors.New("boom: tempdir creation failed")})

	if !h.git.hasRef(ref) {
		t.Fatal("slot removed after a daemon-caused Err")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}

	commitsBefore := h.git.commitTreeCalls
	h.reconcile()
	h.reconcile()
	h.reconcile()
	if h.git.commitTreeCalls != commitsBefore {
		t.Fatal("parked errored candidate re-tested across subsequent ticks (spin)")
	}
}
