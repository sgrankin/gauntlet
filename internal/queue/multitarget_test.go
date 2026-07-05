package queue

import (
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestReconcile_TargetsIndependent drives two targets through one Daemon:
// one target's park and the other's landing must not perturb each other
// (order/done/runs are all per-target state).
func TestReconcile_TargetsIndependent(t *testing.T) {
	h := newHarness(t,
		config.Target{Name: "main", Branch: "main"},
		config.Target{Name: "release", Branch: "release/v2"},
	)
	h.git.seed("main", nil)
	h.git.seed("release/v2", nil)

	refMain := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(refMain, "", checkSpecFile("test"))
	refRel := candidateRef("release", "bob", "hotfix")
	h.git.pushCandidate(refRel, "", checkSpecFile("test"))

	// One tick starts both trials — one lane *per target*, not globally.
	h.reconcile()

	runMain := h.d.headRun("main")
	runRel := h.d.headRun("release")
	if runMain == nil || runRel == nil {
		t.Fatal("expected an in-flight run on each target after one tick")
	}

	// main's candidate fails and parks; release's lands.
	h.release(runMain.runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed})
	h.release(runRel.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	if !h.git.hasRef(refMain) {
		t.Fatal("main's rejected candidate slot vanished")
	}
	if h.git.hasRef(refRel) {
		t.Fatal("release's landed candidate slot survived")
	}

	// release's landing must not clear main's park (§9.1 across targets,
	// a fortiori): main's candidate stays untested on further ticks.
	calls := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != calls {
		t.Fatal("main's parked candidate re-tested after release's landing")
	}

	var outcomes []core.Outcome
	for _, r := range h.ch.Records() {
		outcomes = append(outcomes, r.Outcome)
	}
	wantSeen := map[core.Outcome]bool{core.OutcomeRejected: false, core.OutcomeLanded: false}
	for _, o := range outcomes {
		if _, ok := wantSeen[o]; ok {
			wantSeen[o] = true
		}
	}
	for o, seen := range wantSeen {
		if !seen {
			t.Errorf("no RunRecord with outcome %v; got %v", o, outcomes)
		}
	}
}

// TestReconcile_DuplicateDaemon runs two Daemons over the same remote (the
// shared fake), both testing the same candidate concurrently — the
// second-daemon scenario of Invariants 2 and 4. Exactly one may land; the
// loser's target CAS must come up stale, leaving the target with a single
// merge commit and no corruption.
func TestReconcile_DuplicateDaemon(t *testing.T) {
	h1 := newHarness(t)
	git := h1.git
	base := git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	candSHA := git.pushCandidate(ref, "", checkSpecFile("test"))

	// Second daemon over the same repo, with its own executor, channel, and
	// clock (offset so the two daemons' run IDs can never coincide).
	exec2 := executor.NewGatedExecutor()
	ch2 := channel.NewRecordingChannel()
	clock2 := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	d2, err := New(git, exec2, []core.Channel{ch2}, Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
	}, func() time.Time { clock2 = clock2.Add(time.Second); return clock2 })
	if err != nil {
		t.Fatalf("New(d2): %v", err)
	}
	reconcile2 := func() {
		t.Helper()
		if err := d2.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("d2.ReconcileOnce: %v", err)
		}
	}

	h1.reconcile() // d1's trial starts
	reconcile2()   // d2's trial starts on the same candidate, same base
	run2 := d2.headRun("main")
	if run2 == nil {
		t.Fatal("d2 has no in-flight run")
	}

	// d1 lands first.
	h1.release(h1.currentRunID(), "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	landedOID := git.ref("refs/heads/main")
	if landedOID == base || git.hasRef(ref) {
		t.Fatalf("d1 did not land cleanly: target=%q slot exists=%v", landedOID, git.hasRef(ref))
	}

	// d2's check now goes green too — but its next tick sees the candidate
	// ref gone (d1 deleted it), cancels, and Skips; even if the timing had
	// instead reached d2's land attempt, its target CAS (old=base) would be
	// stale. Either way the target must be exactly d1's merge commit.
	exec2.Release(run2.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	reconcile2()
	reconcile2()

	if got := git.ref("refs/heads/main"); got != landedOID {
		t.Fatalf("target = %q after d2's pass, want d1's merge commit %q (no corruption)", got, landedOID)
	}
	if c := git.commits[landedOID]; len(c.parents) != 2 || c.parents[1] != candSHA {
		t.Fatalf("landed commit parents = %v, want [base %s]", c.parents, candSHA)
	}
	// d2 must have concluded without landing: its terminal record is
	// Skipped, never a second Landed.
	for _, r := range ch2.Records() {
		if r.Outcome == core.OutcomeLanded {
			t.Fatal("d2 also reported Landed; both daemons landed the same candidate")
		}
	}
	if d2.headRun("main") != nil {
		t.Fatal("d2 still has an in-flight run after the candidate vanished")
	}
}
