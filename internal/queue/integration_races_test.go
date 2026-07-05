package queue

import (
	"context"
	"errors"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_ConcurrentDirectPush is §5's "Concurrent direct push" row:
// a human push lands on the target in the window between this run's trial
// (built against the original tip) and the queue's own land attempt.
// Real git's CAS evaluates the remote's live value at push time rather than
// at some hookable instant, so racing the push here — before releasing the
// check's green verdict, which is what triggers the land attempt — drives
// exactly the CAS failure Invariant 2 exists to catch.
func TestIntegration_ConcurrentDirectPush(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))

	h.reconcile() // trial starts against the original tip
	runID := h.currentRunID()

	remote.DirectPush("main", map[string]string{"human.txt": "raced in at land time"})

	h.releaseGated(gated, runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land is attempted; target CAS is stale

	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot deleted despite a stale target CAS")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
}

// TestIntegration_TargetMovedMidCheck is §5's "Target moved mid-check (tick
// path)" row: the early-cancel path, distinct from losing the CAS at land
// time — a target that moves while a check is gated is detected at the very
// start of the next tick, before the check's verdict is even looked at.
func TestIntegration_TargetMovedMidCheck(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))

	h.reconcile() // trial starts; baseOID = the original target tip

	remote.DirectPush("main", map[string]string{"human.txt": "a direct human push"})

	h.reconcile() // detects the target move before polling the check; cancels + Skips

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot removed on a target-moved Skip")
	}
}

// TestIntegration_RepushMidCheck is §5's "Re-push mid-check" row (Invariant
// 5): a re-push (same ref, new SHA) while a check is gated cancels the run
// and discards its verdict; the ref re-queues on its new SHA the next tick.
func TestIntegration_RepushMidCheck(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))

	h.reconcile() // trial starts
	oldRunID := h.currentRunID()

	newSHA := remote.MoveCandidate(ref, distinctFiles(checkSpecFile("test"))) // re-push: new SHA, same ref

	h.reconcile() // detects the move, cancels + Skips the old run

	var sawSkipped bool
	for _, r := range h.ch.Records() {
		if r.Outcome == core.OutcomeSkipped {
			sawSkipped = true
		}
	}
	if !sawSkipped {
		t.Fatal("no Skipped RunRecord emitted for the moved candidate")
	}

	h.reconcile() // next tick: not parked, so it re-queues on its new SHA
	newRunID := h.currentRunID()
	if newRunID == oldRunID {
		t.Fatal("no new run started for the re-pushed candidate")
	}
	if got := remote.Ref(ref); got != newSHA {
		t.Fatalf("candidate ref = %q, want %q", got, newSHA)
	}
}

// TestIntegration_CandidateDeletedMidCheck is §5's "Candidate deleted
// mid-check" row: a deleted ref while a check is gated cancels the run and
// Skips, with no new trial (the queue is empty again).
func TestIntegration_CandidateDeletedMidCheck(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))

	h.reconcile() // trial starts
	remote.DeleteCandidate(ref)
	h.reconcile() // detects the deletion, cancels + Skips; nothing to re-queue

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
	if remote.Ref(ref) != "" {
		t.Fatal("deleted candidate ref reappeared")
	}
}

// TestIntegration_RepushAtLandBoundary is §5's "Re-push at land boundary"
// row (Invariant 3): the author re-pushes the same ref in the window
// between the target CAS succeeding and the queue's own slot-delete CAS.
// The land itself must still count as Landed — the target holds exactly
// the tested merge commit (Invariant 1) — but the slot must survive at its
// new SHA rather than being deleted.
//
// Both of land()'s CAS calls happen back-to-back inside one Go call
// (reconcile.go), separated only by real subprocess latency — there's no
// hook (unlike the fake git double the rest of the package's tests use) to
// interrupt real gitx mid-call, and racing a concurrent re-push against
// that sub-millisecond window would just be a flaky sleep in disguise. So,
// as with TestIntegration_CrashBetweenLandAndDelete, this drives a real
// trial through ReconcileOnce (real baseOID/mergeOID/candSHA) and then
// performs land()'s exact two CASUpdate calls itself, in the exact same
// order, with the re-push injected between them — genuine real-git CAS
// semantics, deterministically sequenced instead of raced.
func TestIntegration_RepushAtLandBoundary(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))
	candSHA := remote.Ref(ref)

	h.reconcile() // trial starts
	r := h.d.runs["main"]
	if r == nil {
		t.Fatal("no in-flight run after trial start")
	}
	if r.baseOID != base || r.cand.SHA != candSHA {
		t.Fatalf("run = %+v, want baseOID=%q cand.SHA=%q", r, base, candSHA)
	}

	ctx := context.Background()
	if err := h.git.CASUpdate(ctx, "refs/heads/main", r.baseOID, r.mergeOID); err != nil {
		t.Fatalf("simulated target CAS (land's first call): %v", err)
	}

	newSHA := remote.MoveCandidate(ref, distinctFiles(checkSpecFile("test"))) // races the slot-delete CAS below

	delErr := h.git.CASUpdate(ctx, ref, r.cand.SHA, "")
	if !errors.Is(delErr, core.ErrCASStale) {
		t.Fatalf("simulated slot-delete CAS error = %v, want ErrCASStale (a re-push raced it)", delErr)
	}

	// Release the abandoned check so its goroutine doesn't leak past the
	// test; h is never reconciled again after this, so it can't perturb the
	// state just asserted directly against the remote.
	gated.Release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	landedOID := remote.Ref("refs/heads/main")
	if landedOID != r.mergeOID {
		t.Fatalf("target ref = %q, want the tested merge SHA %q", landedOID, r.mergeOID)
	}
	if landedOID == base {
		t.Fatalf("target ref = %q, did not advance", landedOID)
	}
	if got := remote.Ref(ref); got != newSHA {
		t.Fatalf("candidate ref = %q, want the re-pushed SHA %q (slot survives)", got, newSHA)
	}
	if parents := remote.Parents(landedOID); len(parents) != 2 || parents[1] != candSHA {
		t.Fatalf("landed commit parents = %v, want [.. %s] (Invariant 6: the tested SHA, not the re-push)", parents, candSHA)
	}
}
