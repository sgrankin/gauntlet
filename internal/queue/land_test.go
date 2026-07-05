package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestReconcile_Land_StaleTargetCAS simulates a human push landing in the
// exact window between this tick's ListRefs snapshot and the queue's own
// CAS attempt at the target ref — the race Invariant 2's CAS exists to
// catch. The target CAS must fail, the slot must survive untouched, and the
// run's outcome must be Skipped (retried next tick), never Landed.
func TestReconcile_Land_StaleTargetCAS(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts
	runID := h.currentRunID()

	triggered := false
	h.git.beforeCAS = func(remoteRef string) {
		if remoteRef == "refs/heads/main" && !triggered {
			triggered = true
			h.git.directPush("main", map[string]string{"human.txt": "raced in at land time"})
		}
	}

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land is attempted; target CAS is stale

	if !triggered {
		t.Fatal("beforeCAS hook never fired; test didn't exercise the race it's meant to")
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot deleted despite a stale target CAS")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
}

// TestReconcile_Land_StaleSlotDeleteCAS simulates an author re-pushing the
// same ref in the window between the target CAS succeeding and the queue's
// slot-delete CAS (Invariant 3). The land itself must still count as
// Landed — the target holds exactly the tested merge commit — but the slot
// must survive at its new SHA rather than being deleted.
func TestReconcile_Land_StaleSlotDeleteCAS(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts
	runID := h.currentRunID()

	var newSHA string
	triggered := false
	h.git.beforeCAS = func(remoteRef string) {
		if remoteRef == ref && !triggered {
			triggered = true
			newSHA = h.git.pushCandidate(ref, sha, checkSpecFile("test"))
		}
	}

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // target CAS succeeds; slot-delete CAS is stale

	if !triggered {
		t.Fatal("beforeCAS hook never fired for the candidate ref; test didn't exercise the race it's meant to")
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate ref deleted despite a re-push racing the slot delete")
	}
	if got := h.git.ref(ref); got != newSHA {
		t.Fatalf("candidate ref = %q, want the re-pushed SHA %q (slot survives)", got, newSHA)
	}

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed (target still landed despite the slot-delete race)", last.Outcome)
	}
	if got := h.git.ref("refs/heads/main"); got != last.MergeSHA {
		t.Fatalf("target ref = %q, want the tested merge SHA %q", got, last.MergeSHA)
	}
}

// TestReconcile_IsAncestorRecovery covers Invariant 4: a candidate whose SHA
// is already an ancestor of the target tip (as if a previous daemon
// instance landed it and crashed before deleting the slot) is recovered by
// deleting the slot directly — no trial merge, no new commit.
func TestReconcile_IsAncestorRecovery(t *testing.T) {
	h := newHarness(t)
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	files := checkSpecFile("test")
	candOID := h.git.pushCandidate(ref, "", files)

	// Craft a target tip whose history already contains candOID, as if the
	// merge had already happened.
	mergeOID := h.git.commit(files, base, candOID)
	h.git.setRef("refs/heads/main", mergeOID)

	h.reconcile()

	if h.git.hasRef(ref) {
		t.Fatal("candidate slot not deleted on recovery")
	}
	if h.git.mergeTreeCalls != 0 {
		t.Fatalf("MergeTree called %d times, want 0 (recovered without re-testing)", h.git.mergeTreeCalls)
	}
	if h.git.commitTreeCalls != 0 {
		t.Fatalf("CommitTree called %d times, want 0", h.git.commitTreeCalls)
	}

	var sawLanded bool
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventLanded {
			sawLanded = true
		}
	}
	if !sawLanded {
		t.Fatal("no EventLanded emitted for the recovered candidate")
	}
}
