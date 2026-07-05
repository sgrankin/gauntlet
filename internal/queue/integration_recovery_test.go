package queue

import (
	"context"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// TestIntegration_CrashBetweenLandAndDelete is §5's "Crash between land and
// delete" row (Invariant 4): the daemon crashes after the target CAS
// succeeds but before the slot-delete CAS runs. There is no hook to
// interrupt Daemon.land mid-call against real git, so this simulates the
// crash the way the invariant is actually specified: it performs exactly
// the first CAS land() would (the same core.GitRepo.CASUpdate call, on the
// same gitx.Repo the in-flight run belongs to), leaving the slot
// deliberately undeleted — precisely the on-disk/on-remote state a real
// crash between the two pushes would leave. A brand-new Daemon (fresh
// bare clone, fresh in-memory state) must then recover via IsAncestor
// without re-merging anything.
func TestIntegration_CrashBetweenLandAndDelete(t *testing.T) {
	gated := executor.NewGatedExecutor()
	remote := testutil.NewRemote(t)
	h1 := newIntegrationHarness(t, remote, gated)
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))
	candSHA := remote.Ref(ref)

	h1.reconcile() // trial starts
	r := h1.d.runs["main"]
	if r == nil {
		t.Fatal("no in-flight run after trial start")
	}
	mergeOID := r.mergeOID

	if err := h1.git.CASUpdate(context.Background(), "refs/heads/main", base, mergeOID); err != nil {
		t.Fatalf("simulated target CAS (the first half of land): %v", err)
	}
	if got := remote.Ref("refs/heads/main"); got != mergeOID {
		t.Fatalf("target = %q, want the simulated landed merge %q", got, mergeOID)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot already gone before the simulated crash; test setup is wrong")
	}
	if parents := remote.Parents(mergeOID); len(parents) != 2 || parents[1] != candSHA {
		t.Fatalf("simulated landed commit parents = %v, want [.. %s]", parents, candSHA)
	}
	// Release the abandoned check so its goroutine doesn't leak past the
	// test; h1 is never reconciled again, so this can't perturb anything.
	gated.Release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	// "Crash": a brand-new Daemon over a brand-new bare clone of the same
	// remote — no in-memory state survives (Invariant 4).
	h2 := newIntegrationHarness(t, remote, executor.NewGatedExecutor())
	h2.reconcile() // IsAncestor recovery: candSHA is already an ancestor of target

	if remote.Ref(ref) != "" {
		t.Fatal("candidate slot not CAS-deleted on recovery")
	}
	if got := remote.Ref("refs/heads/main"); got != mergeOID {
		t.Fatalf("target moved during recovery: %q, want unchanged %q (no re-merge)", got, mergeOID)
	}
	var sawLanded bool
	for _, e := range h2.ch.Events() {
		if e.Kind == core.EventLanded {
			sawLanded = true
		}
	}
	if !sawLanded {
		t.Fatal("no EventLanded emitted for the recovered candidate")
	}
}

// TestIntegration_DuplicateDaemon is §5's "Duplicate daemon" row
// (Invariants 2 and 4): two Daemons, each with its own gitx.Repo bare clone
// but the same underlying remote, both test the same candidate
// concurrently. Exactly one may land; the loser's target (or slot) CAS
// comes up stale, and the target ends up holding a single merge commit
// with no corruption.
func TestIntegration_DuplicateDaemon(t *testing.T) {
	remote := testutil.NewRemote(t)
	gated1 := executor.NewGatedExecutor()
	gated2 := executor.NewGatedExecutor()
	h1 := newIntegrationHarness(t, remote, gated1)
	h2 := newIntegrationHarness(t, remote, gated2)

	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))
	candSHA := remote.Ref(ref)

	h1.reconcile() // d1's trial starts
	h2.reconcile() // d2's trial starts on the same candidate, same base
	run1ID := h1.currentRunID()
	run2ID := h2.currentRunID()
	if h2.d.runs["main"] == nil {
		t.Fatal("d2 has no in-flight run")
	}

	// d1 lands first.
	h1.releaseGated(gated1, run1ID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	landedOID := remote.Ref("refs/heads/main")
	if landedOID == base || remote.Ref(ref) != "" {
		t.Fatalf("d1 did not land cleanly: target=%q slot=%q", landedOID, remote.Ref(ref))
	}

	// d2's check now goes green too — but its next tick sees the candidate
	// ref gone (d1 deleted it), cancels, and Skips; even if the timing had
	// instead reached d2's land attempt, its target CAS (old=base) would be
	// stale. Either way the target must be exactly d1's merge commit.
	gated2.Release(run2ID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	h2.reconcile()
	h2.reconcile()

	if got := remote.Ref("refs/heads/main"); got != landedOID {
		t.Fatalf("target = %q after d2's pass, want d1's merge commit %q (no corruption)", got, landedOID)
	}
	if parents := remote.Parents(landedOID); len(parents) != 2 || parents[1] != candSHA {
		t.Fatalf("landed commit parents = %v, want [.. %s]", parents, candSHA)
	}
	for _, r := range h2.ch.Records() {
		if r.Outcome == core.OutcomeLanded {
			t.Fatal("d2 also reported Landed; both daemons landed the same candidate")
		}
	}
	if h2.d.runs["main"] != nil {
		t.Fatal("d2 still has an in-flight run after the candidate vanished")
	}
}
