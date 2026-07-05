// Speculate-mode suite (docs/plans/phase5.md §2.5's speculate refill, §2.1's
// lane-general validity sweep/bubble/prefix-drain now actually exercised at
// depth > 1, §3.3's Speculated flag). Built on the fake harness
// (daemon_test.go's testHarness/fakeGitRepo), the same tier batch_test.go's
// state-machine suite uses — speculate's window-filling, bubble, and
// invalidation behavior is exactly what the fake proves most cheaply and
// deterministically. chain_test.go already proves buildChainLink/
// specChanged's underlying mechanics against real git; the speculate
// scenario files (testdata/script/speculate_*.txtar) exercise the same
// state machine through the DSL, against both the fake and real-git
// harnesses.
package queue

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

// speculateTarget is the one-target config every test in this file uses:
// speculate mode, window candidates deep.
func speculateTarget(window int) config.Target {
	return config.Target{Name: "main", Branch: "main", Mode: "speculate", Window: window}
}

// pushThreeSpeculateCandidates seeds "main" with the check spec and pushes
// alice/bob/carol (topics a/b/c), each touching their own file — the fixture
// every test in this file starts from.
func pushThreeSpeculateCandidates(h *testHarness) (refA, refB, refC string) {
	h.git.seed("main", checkSpecFile("test"))
	refA = candidateRef("main", "alice", "a")
	refB = candidateRef("main", "bob", "b")
	refC = candidateRef("main", "carol", "c")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})
	return refA, refB, refC
}

// TestSpeculateRefill_FillsWindowChained proves the basic refill shape
// (docs/plans/phase5.md §2.5): one tick, window 3, 3 queued candidates ->
// one refill fills the whole window in a single chain, each run's own
// (mergeOID, baseOID) chaining onto the previous run's chainTip, and the
// Predicted flag set correctly (false for the head run, true for every
// run behind it).
func TestSpeculateRefill_FillsWindowChained(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	pushThreeSpeculateCandidates(h)

	h.reconcile()

	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 3 {
		t.Fatalf("lane = %+v, want 3 chained runs", l)
	}
	run0, run1, run2 := l.runs[0], l.runs[1], l.runs[2]

	if run0.predicted {
		t.Error("run0 (head) baseOID is the live target tip; must not be marked predicted")
	}
	if !run1.predicted || !run2.predicted {
		t.Error("run1/run2 chain onto an unpushed predecessor chainTip; must be marked predicted")
	}
	if run1.baseOID != run0.chainTip {
		t.Errorf("run1.baseOID = %s, want run0.chainTip %s (chained, predicted base)", run1.baseOID, run0.chainTip)
	}
	if run2.baseOID != run1.chainTip {
		t.Errorf("run2.baseOID = %s, want run1.chainTip %s", run2.baseOID, run1.chainTip)
	}
	if run0.baseOID == "" || run0.baseOID != h.git.ref("refs/heads/main") {
		t.Errorf("run0.baseOID = %s, want the live target tip %s", run0.baseOID, h.git.ref("refs/heads/main"))
	}

	// Each run is its own one-member run — a run-per-candidate, own check
	// suite, serial-identical per run (constraint: "Run-per-candidate:
	// members len 1, own check suite from its own trial tree").
	for i, r := range l.runs {
		if len(r.members) != 1 {
			t.Errorf("run %d has %d members, want 1 (speculate is one candidate per run)", i, len(r.members))
		}
	}

	// RunRecord.Speculated mirrors run.predicted (§3.3).
	if run0.members[0].rec.Speculated {
		t.Error("run0's RunRecord.Speculated must be false")
	}
	if !run1.members[0].rec.Speculated || !run2.members[0].rec.Speculated {
		t.Error("run1/run2's RunRecord.Speculated must be true")
	}
}

// TestSpeculateConflictAgainstPredictedBase_Parks proves §2.5's per-candidate
// conflict handling: a candidate that conflicts against a non-head
// (predicted, unpushed) base parks with a Detail that explicitly documents
// the conflict as being against a PREDICTION — "conflicts with in-flight
// <topic>@<sha> (predicted base)" — not the generic "trial merge conflict"
// wording serial/batch use, since the base here was never pushed anywhere;
// window-extension stops for this tick once that candidate parks (carol
// never gets picked at all).
func TestSpeculateConflictAgainstPredictedBase_Parks(t *testing.T) {
	// Window starts at 1 so the first refill picks ONLY alice — bob/carol
	// must still be waiting when the conflict is scripted below (a fully
	// open window would chain all three in one tick, before there's any
	// chance to script anything, since the fake never conflicts on its
	// own). The window is widened to 3 right after, so the SECOND refill is
	// the one that attempts bob next, against alice's now-existing
	// (predicted) chainTip.
	h := newHarness(t, speculateTarget(1))
	refA, refB, refC := pushThreeSpeculateCandidates(h)

	h.reconcile() // window(1): only alice's own run forms

	run0 := h.d.headRun("main")
	if run0 == nil {
		t.Fatal("no head run after the first refill")
	}
	shaB := h.git.ref(refB)

	// Script a conflict for bob's candidate specifically against alice's
	// PREDICTED chainTip (bob's would-be base) — the fake never conflicts on
	// its own (git_test.go's MergeTree overlays trees, never fails, unless
	// scripted), so this is how a chained-base conflict is provoked
	// deterministically.
	h.git.scriptConflict(run0.chainTip, shaB, []string{"shared.txt"})
	h.d.cfg.Targets[0].Window = 3 // widen the window so this refill actually tries bob next

	h.reconcile() // refill: bob conflicts against alice's predicted chainTip and parks; carol is never even tried

	if !h.git.hasRef(refB) {
		t.Fatal("bob's ref should still exist (parked, not deleted)")
	}
	if l := h.d.lanes["main"]; l == nil || len(l.runs) != 1 {
		t.Fatalf("lane = %+v, want exactly 1 run (alice only; bob parked, carol never picked)", h.d.lanes["main"])
	}

	var rec *core.RunRecord
	for _, r := range h.ch.Records() {
		if r.Candidate.Ref == refB {
			rec = r
		}
	}
	if rec == nil {
		t.Fatal("no RunRecord captured for bob")
	}
	if rec.Outcome != core.OutcomeConflict {
		t.Fatalf("bob's Outcome = %v, want Conflict", rec.Outcome)
	}
	wantPrefix := "conflicts with in-flight a@" + run0.members[0].cand.SHA + " (predicted base)"
	if len(rec.Detail) < len(wantPrefix) || rec.Detail[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("bob's Detail = %q, want it to start with %q (documenting a conflict against a PREDICTION)", rec.Detail, wantPrefix)
	}

	// carol was never picked this tick (window-extension stopped at bob) —
	// still present, unparked, waiting for a future tick.
	if !h.git.hasRef(refC) {
		t.Fatal("carol's ref should be untouched")
	}
	for _, r := range h.ch.Records() {
		if r.Candidate.Ref == refC {
			t.Fatalf("carol should never have been picked this tick, got record %+v", r)
		}
	}
	_ = refA
}

// TestSpeculateBubble_CulpritParksSuffixSkipped proves §2.1c's bubble step
// at depth 3 (this is where P5-C built it lane-general but only ever
// exercised at depth 1): the middle run goes red -> only it parks
// (Rejected); everything behind it is Skipped with a Detail of exactly
// "pipeline bubble" and NOT parked (free to re-queue); everything ahead of
// it survives untouched, still in flight.
func TestSpeculateBubble_CulpritParksSuffixSkipped(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	refA, refB, refC := pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills: alice(0)->bob(1)->carol(2)
	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 3 {
		t.Fatalf("lane = %+v, want 3 runs", l)
	}
	run0, run1 := l.runs[0], l.runs[1]

	h.release(run1.runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // bob (middle) goes red

	if l := h.d.lanes["main"]; l == nil || len(l.runs) != 1 || l.runs[0] != run0 {
		t.Fatalf("lane after the bubble = %+v, want exactly [alice's run] surviving", h.d.lanes["main"])
	}

	// bob: parked, Rejected.
	if !h.git.hasRef(refB) {
		t.Fatal("bob's ref should survive (parked, not deleted)")
	}
	var bobRec, carolRec *core.RunRecord
	for _, r := range h.ch.Records() {
		switch r.Candidate.Ref {
		case refB:
			bobRec = r
		case refC:
			carolRec = r
		}
	}
	if bobRec == nil || bobRec.Outcome != core.OutcomeRejected {
		t.Fatalf("bob's record = %+v, want Outcome Rejected", bobRec)
	}

	// carol: Skipped, "pipeline bubble" detail, NOT parked.
	if carolRec == nil || carolRec.Outcome != core.OutcomeSkipped {
		t.Fatalf("carol's record = %+v, want Outcome Skipped", carolRec)
	}
	if carolRec.Detail != "pipeline bubble" {
		t.Fatalf("carol's Detail = %q, want exactly %q", carolRec.Detail, "pipeline bubble")
	}
	if !h.git.hasRef(refC) {
		t.Fatal("carol's ref should survive (Skipped, not parked)")
	}
	if _, parked := h.d.done["main"][refC]; parked {
		t.Fatal("carol must NOT be parked (a bubble re-queue, not a rejection)")
	}

	// alice: untouched, still in flight (never depended on bob).
	if run0.verdict == verdictRejected || run0.verdict == verdictErrored {
		t.Fatal("alice's run must be unaffected by bob's bubble")
	}
	if !h.git.hasRef(refA) {
		t.Fatal("alice hasn't landed yet")
	}

	// The freed window slots refill on the very next tick: carol re-queues,
	// rebuilt on the (still-predicted, alice-not-yet-landed) corrected base.
	h.reconcile()
	if l := h.d.lanes["main"]; l == nil || len(l.runs) != 2 {
		t.Fatalf("lane after refill = %+v, want 2 (alice survives + carol re-queued)", h.d.lanes["main"])
	}
}

// TestSpeculateValiditySweep_MemberMoveTruncatesSuffix proves §2.2's
// generalized Invariant-5 test at depth 3: a mid-window member re-push
// (lane position 1) truncates the suffix from THAT position on — Skipped,
// unparked, both it and everything behind it — while the predecessor
// (lane position 0) is untouched and survives.
func TestSpeculateValiditySweep_MemberMoveTruncatesSuffix(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	refA, refB, refC := pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills: alice(0)->bob(1)->carol(2)
	if l := h.d.lanes["main"]; l == nil || len(l.runs) != 3 {
		t.Fatalf("lane = %+v, want 3 runs", h.d.lanes["main"])
	}
	run0 := h.d.headRun("main")

	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b2\n"}) // bob re-pushed: new SHA, same ref

	h.reconcile() // validity sweep: bob's member moved -> invalidate suffix from lane index 1

	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 1 || l.runs[0] != run0 {
		t.Fatalf("lane after the move = %+v, want exactly [alice's run] surviving", h.d.lanes["main"])
	}

	var bobRec, carolRec *core.RunRecord
	for _, r := range h.ch.Records() {
		switch r.Candidate.Ref {
		case refB:
			bobRec = r
		case refC:
			carolRec = r
		}
	}
	for _, rec := range []*core.RunRecord{bobRec, carolRec} {
		if rec == nil || rec.Outcome != core.OutcomeSkipped {
			t.Fatalf("record = %+v, want Outcome Skipped (a move, not a rejection)", rec)
		}
	}
	if _, parked := h.d.done["main"][refB]; parked {
		t.Fatal("bob must NOT be parked; his new SHA re-queues")
	}
	if _, parked := h.d.done["main"][refC]; parked {
		t.Fatal("carol must NOT be parked; a suffix invalidation, not her fault")
	}
	if !h.git.hasRef(refA) {
		t.Fatal("alice's run must be unaffected")
	}
}

// TestSpeculateHeadTargetMoved_WholeWindowInvalidated proves §2.2's head-run
// rule at depth 3: only lane index 0's baseOID is the live target tip, so a
// direct push to the target moves it out from under the HEAD run
// specifically — invalidating the ENTIRE window (every run depends,
// directly or transitively, on that same live tip), Skipped and unparked
// throughout.
func TestSpeculateHeadTargetMoved_WholeWindowInvalidated(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	refA, refB, refC := pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills: alice(0)->bob(1)->carol(2)
	if l := h.d.lanes["main"]; l == nil || len(l.runs) != 3 {
		t.Fatalf("lane = %+v, want 3 runs", h.d.lanes["main"])
	}

	h.git.directPush("main", map[string]string{"human.txt": "a human push"}) // moves the real target tip

	h.reconcile() // validity sweep: index 0's baseOID != live target tip -> the WHOLE window invalidates

	if l := h.d.lanes["main"]; l == nil || len(l.runs) != 0 {
		t.Fatalf("lane after the target move = %+v, want empty (whole window invalidated)", h.d.lanes["main"])
	}
	for _, ref := range []string{refA, refB, refC} {
		if !h.git.hasRef(ref) {
			t.Fatalf("%s should survive (Skipped, not parked)", ref)
		}
		if _, parked := h.d.done["main"][ref]; parked {
			t.Fatalf("%s must NOT be parked (a target move, nobody's fault)", ref)
		}
	}
	var skippedCount int
	for _, r := range h.ch.Records() {
		if r.Outcome == core.OutcomeSkipped {
			skippedCount++
		}
	}
	if skippedCount != 3 {
		t.Fatalf("Skipped record count = %d, want 3 (the whole window)", skippedCount)
	}

	h.reconcile() // refill: the whole window rebuilds on the new (post-human-push) tip
	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 3 {
		t.Fatalf("rebuilt lane = %+v, want 3 runs", l)
	}
	newTip := h.git.ref("refs/heads/main")
	if l.runs[0].baseOID != newTip {
		t.Fatalf("rebuilt head run's baseOID = %s, want the new live tip %s", l.runs[0].baseOID, newTip)
	}
	if l.runs[0].predicted {
		t.Fatal("the rebuilt head run must not be marked predicted")
	}
}

// TestSpeculateGreenPrefixDrain_MultipleRunsOneTick proves §2.1d's
// prefix-drain loop lands a whole green prefix in ONE tick (not one land
// per tick): forcing all three runs green simultaneously (bypassing real
// check execution — a separate concern the depth-3 race soak below covers)
// and calling ReconcileOnce exactly once must land all three, each land's
// CAS old value equal to the PRIOR run's own chainTip (structural FIFO,
// constraint 5) — never the tick's stale target-tip snapshot.
func TestSpeculateGreenPrefixDrain_MultipleRunsOneTick(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills to 3
	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 3 {
		t.Fatalf("lane = %+v, want 3 runs", l)
	}
	run0, run1, run2 := l.runs[0], l.runs[1], l.runs[2]

	// Force every run green directly (white-box: this test is about
	// advanceLane's prefix-drain loop, not check execution) and release
	// each gated check so its abandoned goroutine doesn't leak past the
	// test.
	for _, r := range l.runs {
		r.verdict = verdictGreen
		h.exec.Release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	}

	casBefore := len(h.git.casLog)
	h.reconcile() // ONE tick: the whole green prefix (all 3) must land

	if len(l.runs) != 0 {
		t.Fatalf("lane after the tick = %+v, want empty (all three landed in one tick)", l)
	}

	var targetPushes []casCall
	for _, c := range h.git.casLog[casBefore:] {
		if c.ref == "refs/heads/main" {
			targetPushes = append(targetPushes, c)
		}
	}
	if len(targetPushes) != 3 {
		t.Fatalf("target CAS pushes in this one tick = %d, want 3, log=%+v", len(targetPushes), targetPushes)
	}
	wantPushes := []casCall{
		{ref: "refs/heads/main", old: run0.baseOID, new: run0.chainTip},
		{ref: "refs/heads/main", old: run0.chainTip, new: run1.chainTip},
		{ref: "refs/heads/main", old: run1.chainTip, new: run2.chainTip},
	}
	for i, want := range wantPushes {
		if targetPushes[i] != want {
			t.Errorf("push %d = %+v, want %+v", i, targetPushes[i], want)
		}
	}
	if got := h.git.ref("refs/heads/main"); got != run2.chainTip {
		t.Fatalf("final target tip = %s, want run2.chainTip %s", got, run2.chainTip)
	}
}

// TestSpeculateLand_FIFOCAS is docs/plans/phase5.md §5.3's targeted test:
// window 3, all green, released in run0/run1/run2 order (a separate tick's
// worth of check-completion per run, unlike the prefix-drain test above,
// which forces simultaneity) — land order must be run0,run1,run2, and a
// FORCED out-of-order land attempt (run1's own CAS, before run0 has landed)
// must CAS-fail structurally: run1.baseOID is a PREDICTION (run0.chainTip),
// not yet the live target ref, so nothing can land run1 first no matter
// what races the daemon (constraint 5).
func TestSpeculateLand_FIFOCAS(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills to 3, chained
	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 3 {
		t.Fatalf("lane = %+v, want 3 chained runs", l)
	}
	run0, run1, run2 := l.runs[0], l.runs[1], l.runs[2]

	// Forced out-of-order land: run1's own CAS, before run0 has landed, must
	// fail — the live target ref is still the original tip, not
	// run1.baseOID (run0's PREDICTED chainTip).
	ctx := context.Background()
	if err := h.git.CASUpdate(ctx, "refs/heads/main", run1.baseOID, run1.chainTip); !errors.Is(err, core.ErrCASStale) {
		t.Fatalf("out-of-order land attempt (run1 before run0) = %v, want ErrCASStale", err)
	}
	if got := h.git.ref("refs/heads/main"); got == run1.chainTip {
		t.Fatal("the out-of-order land attempt must not have mutated the target ref")
	}

	casOffset := len(h.git.casLog) // the failed attempt above is already logged; only count what follows

	h.release(run0.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	h.release(run1.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	h.release(run2.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	var landed []string
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventLanded {
			landed = append(landed, e.Candidate.Topic)
		}
	}
	if len(landed) != 3 || landed[0] != "a" || landed[1] != "b" || landed[2] != "c" {
		t.Fatalf("landed order = %v, want [a b c] (structural FIFO)", landed)
	}

	var targetPushes []casCall
	for _, c := range h.git.casLog[casOffset:] {
		if c.ref == "refs/heads/main" {
			targetPushes = append(targetPushes, c)
		}
	}
	if len(targetPushes) != 3 {
		t.Fatalf("target CAS pushes = %d, want 3, log=%+v", len(targetPushes), targetPushes)
	}
	if targetPushes[0].old != run0.baseOID || targetPushes[0].new != run0.chainTip {
		t.Errorf("run0's land CAS = %+v, want old=%s new=%s", targetPushes[0], run0.baseOID, run0.chainTip)
	}
	if targetPushes[1].old != run0.chainTip || targetPushes[1].new != run1.chainTip {
		t.Errorf("run1's land CAS = %+v, want old=%s new=%s (its own PREDICTED base, now real)", targetPushes[1], run0.chainTip, run1.chainTip)
	}
	if targetPushes[2].old != run1.chainTip || targetPushes[2].new != run2.chainTip {
		t.Errorf("run2's land CAS = %+v, want old=%s new=%s", targetPushes[2], run1.chainTip, run2.chainTip)
	}

	if got := h.git.ref("refs/heads/main"); got != run2.chainTip {
		t.Fatalf("final target tip = %s, want run2.chainTip %s", got, run2.chainTip)
	}
}

// TestSpeculateCrashRecovery covers §8's speculate crash-recovery
// walkthrough: a PREFIX of the window landed (target advanced past two
// members) before a crash interrupted slot deletion, and the un-landed
// suffix's predicted link (built against what was, at the time, an
// in-flight prediction) is now unreferenced garbage. A fresh daemon (no
// in-memory lane state — modeled here, as land_test.go/batch_test.go's own
// crash-recovery tests do, by reconciling a never-before-used Daemon
// against hand-crafted ground truth) must: recover the landed members' slots
// one at a time (head-pick-only per refill, §2.5), with no re-merge; then
// rebuild the un-landed suffix as a fresh window on the new live tip.
func TestSpeculateCrashRecovery(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	base := h.git.seed("main", nil)
	files := checkSpecFile("test")
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	shaA := h.git.pushCandidate(refA, "", files)
	shaB := h.git.pushCandidate(refB, "", files)
	shaC := h.git.pushCandidate(refC, "", files)

	// Craft a chain exactly like a landed prefix of a speculate window: run0
	// (alice) and run1 (bob) both landed — the target advanced to bob's
	// link — before a crash interrupted their slot deletes. carol's link
	// (chained onto bob's, a PREDICTED base at the time) was built but never
	// pushed anywhere: an unreferenced, garbage commit, exactly as §8
	// describes for the un-landed suffix.
	linkA := h.git.commit(files, base, shaA)
	linkB := h.git.commit(files, linkA, shaB)
	_ = h.git.commit(files, linkB, shaC) // carol's stale predicted link: inert garbage post-crash
	h.git.setRef("refs/heads/main", linkB)

	h.reconcile() // recovers alice's slot only (head-pick-only per refill, §2.5)
	if h.git.hasRef(refA) {
		t.Fatal("alice's slot not recovered on the first tick")
	}
	if !h.git.hasRef(refB) || !h.git.hasRef(refC) {
		t.Fatal("bob/carol recovered too early; recovery must be head-pick-only per refill")
	}

	h.reconcile() // recovers bob
	if h.git.hasRef(refB) {
		t.Fatal("bob's slot not recovered on the second tick")
	}
	if !h.git.hasRef(refC) {
		t.Fatal("carol recovered too early")
	}

	if h.git.mergeTreeCalls != 0 || h.git.commitTreeCalls != 0 {
		t.Fatalf("recovery re-tested instead of recovering: mergeTreeCalls=%d commitTreeCalls=%d", h.git.mergeTreeCalls, h.git.commitTreeCalls)
	}

	var landedCount int
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventLanded {
			landedCount++
		}
	}
	if landedCount != 2 {
		t.Fatalf("EventLanded count = %d, want 2 (alice + bob recovered)", landedCount)
	}

	// carol's ref is NOT an ancestor of the new tip (linkB has no trace of
	// her change) — she rebuilds as a fresh, non-predicted window on the
	// live tip, not a recovery.
	h.reconcile()
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 1 || r.members[0].cand.Topic != "c" {
		t.Fatalf("carol did not rebuild as a fresh head run: %+v", r)
	}
	if r.baseOID != linkB {
		t.Fatalf("carol's rebuilt run baseOID = %s, want the live tip %s", r.baseOID, linkB)
	}
	if r.predicted {
		t.Fatal("carol's rebuilt run is the new head; must not be marked predicted")
	}
	if got, want := h.git.ref(refC), shaC; got != want {
		t.Fatalf("carol's candidate SHA = %s, want her original push %s (never touched by recovery)", got, want)
	}
}

// TestSpeculateDepth3RaceSoak proves the executor gate's (RunID, name) key
// never collides across distinct concurrently-running runs (docs/plans/
// phase5.md's "verify the one-goroutine-per-run result plumbing holds at
// depth N"): window 3, all three runs' checks started (and so their
// executor goroutines all genuinely in flight, concurrently, blocked on
// their own gate) before any is released, then all three released
// CONCURRENTLY from separate goroutines — exercising GatedExecutor's
// (RunID, name)-keyed maps under real concurrent access. Run with -race.
func TestSpeculateDepth3RaceSoak(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	refA, refB, refC := pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills: 3 runs, each starting its own "test" check concurrently

	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 3 {
		t.Fatalf("lane = %+v, want 3 runs", l)
	}
	runIDs := []string{l.runs[0].runID, l.runs[1].runID, l.runs[2].runID}

	// Every run's check is already running (started synchronously within
	// the single reconcile() call above, one goroutine per run) — awaiting
	// each in turn merely confirms it, it doesn't force sequencing.
	for _, id := range runIDs {
		h.awaitStarted(id, "test")
	}

	// Release all three concurrently, from separate goroutines: the
	// interesting exercise for -race is GatedExecutor's internal
	// (RunID, name)-keyed maps under genuine concurrent access, not the
	// daemon (ReconcileOnce is never called concurrently with itself; only
	// the drain loop below touches it, from this one goroutine).
	var wg sync.WaitGroup
	for _, id := range runIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			h.exec.Release(id, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
		}(id)
	}
	wg.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for h.git.hasRef(refA) || h.git.hasRef(refB) || h.git.hasRef(refC) {
		if time.Now().After(deadline) {
			t.Fatal("not all three candidates landed; the concurrent release soak deadlocked or lost a result")
		}
		h.reconcile()
		runtime.Gosched()
	}

	var landed []string
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventLanded {
			landed = append(landed, e.Candidate.Topic)
		}
	}
	if len(landed) != 3 || landed[0] != "a" || landed[1] != "b" || landed[2] != "c" {
		t.Fatalf("landed order = %v, want [a b c] (FIFO holds even after a fully concurrent release)", landed)
	}
}
