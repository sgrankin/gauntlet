// Manual operator cancellation suite (Feature 1, core.CommandCancel):
// command.go's applyCancel, cancelInFlight, cancelBatchMember, cancelWaiting.
// Built on the fake harness (daemon_test.go's testHarness/fakeGitRepo), the
// same tier command_test.go's retry suite uses — this is the sibling
// command's coverage, one mode per test plus the waiting/idempotent cases
// every command shares.
package queue

import (
	"fmt"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestCommand_CancelInFlightSerialParks covers serial mode (Feature 1): a
// cancel for the ref currently in flight aborts its run and parks it at its
// current SHA (OutcomeRejected, "cancelled by operator") — the same park
// machinery a real rejection uses. Also covers idempotency: cancelling an
// already-parked ref a second time is a silent no-op.
func TestCommand_CancelInFlightSerialParks(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts; "test" in flight
	runID := h.currentRunID()
	h.awaitStarted(runID, "test")

	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: ref})
	h.reconcile() // drains the cancel: aborts the run, parks (ref, sha)

	if h.d.headRun("main") != nil {
		t.Fatal("lane still holds a run after cancelling its sole member")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if last.Detail != cancelDetail {
		t.Fatalf("Detail = %q, want %q", last.Detail, cancelDetail)
	}
	if last.Candidate.SHA != sha {
		t.Fatalf("Candidate.SHA = %q, want %q", last.Candidate.SHA, sha)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot removed by a cancel; it should only be parked")
	}

	// Idempotency: cancelling an already-parked ref is a silent no-op.
	before := len(h.ch.Events())
	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: ref})
	h.reconcile()
	if len(h.ch.Events()) != before {
		t.Fatalf("cancel on an already-parked ref emitted %d new events, want 0", len(h.ch.Events())-before)
	}

	// Stays parked: never re-tested without a retry or a re-push.
	callsBefore := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != callsBefore {
		t.Fatal("cancelled candidate re-tested without a retry/re-push")
	}
}

// TestCommand_CancelWaitingParksBeforePickup covers the "queued but not yet
// picked" half of Feature 1: a cancel for a ref that's WAITING (not
// in-flight) parks it directly at its current SHA — cancel-before-start, so
// it is never trial-merged at all — while the actually in-flight candidate
// is left completely undisturbed.
func TestCommand_CancelWaitingParksBeforePickup(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	refAlice := candidateRef("main", "alice", "widget")
	refBob := candidateRef("main", "bob", "gadget")
	h.git.pushCandidate(refAlice, "", checkSpecFile("test"))
	bobSHA := h.git.pushCandidate(refBob, "", checkSpecFile("test"))

	h.reconcile() // serial: alice picked (FIFO), bob stays WAITING
	aliceRunID := h.currentRunID()
	callsBefore := h.git.mergeTreeCalls
	if callsBefore != 1 {
		t.Fatalf("mergeTreeCalls = %d, want 1 (only alice tried so far)", callsBefore)
	}

	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: refBob})
	h.reconcile() // drains the cancel: bob parks directly, never trial-merged

	if h.git.mergeTreeCalls != callsBefore {
		t.Fatal("cancelling a WAITING candidate trial-merged it")
	}
	if r := h.d.headRun("main"); r == nil || r.runID != aliceRunID {
		t.Fatal("cancelling a waiting candidate perturbed the in-flight run")
	}

	var bobRec *core.RunRecord
	for _, r := range h.ch.Records() {
		if r.Candidate.Ref == refBob {
			bobRec = r
		}
	}
	if bobRec == nil {
		t.Fatal("no run record for the cancelled waiting candidate")
	}
	if bobRec.Outcome != core.OutcomeRejected || bobRec.Detail != cancelDetail {
		t.Fatalf("bob's record = %+v, want Outcome=Rejected Detail=%q", bobRec, cancelDetail)
	}
	if bobRec.Candidate.SHA != bobSHA {
		t.Fatalf("bob's record SHA = %q, want %q", bobRec.Candidate.SHA, bobSHA)
	}
	if !h.git.hasRef(refBob) {
		t.Fatal("bob's slot removed by a cancel; it should only be parked")
	}
	if entry, parked := h.d.done["main"][refBob]; !parked || entry.RunID != bobRec.RunID {
		t.Fatalf("bob's done entry = %+v (parked=%v), want RunID %q (the record cancelWaiting synthesized)", entry, parked, bobRec.RunID)
	}

	h.release(aliceRunID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // alice lands

	h.reconcile()
	if h.git.mergeTreeCalls != callsBefore {
		t.Fatal("bob (cancelled while waiting) was re-tested without a retry/re-push")
	}
}

// TestCommand_CancelUnknownRefIsNoop covers applyCancel's third case: a ref
// this tick's refs snapshot doesn't contain at all (never pushed, or
// already deleted) is a silent no-op — must not panic, must not emit
// anything.
func TestCommand_CancelUnknownRefIsNoop(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)

	before := len(h.ch.Events())
	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: candidateRef("main", "nobody", "ghost")})
	h.reconcile() // must not panic
	if len(h.ch.Events()) != before {
		t.Fatal("cancelling an unknown ref emitted events")
	}
}

// TestCommand_CancelBatchMemberReQueuesSiblings covers batch mode (Feature
// 1): cancelling one member of an in-flight batch run parks ONLY that
// member (OutcomeRejected, cancelDetail); every other member Skips unparked
// with a "batch member cancelled" detail and re-queues — re-batching
// together again, since (unlike a genuine batch-red verdict) there's no
// ambiguity here for a serial fallback to resolve.
//
// The re-batch happens in the very SAME reconcile pass that drains the
// cancel, not a tick later: unlike a real bubble or a land (which mutate
// git refs the tick's own cands/targetTip snapshot would then be stale
// against, docs/plans/phase5.md §2's reconcileTarget doc), a cancel only
// touches in-memory bookkeeping, so refillLane immediately re-picking the
// now-unparked siblings against the very same snapshot is entirely safe —
// there is nothing for a deferred tick to protect against here.
func TestCommand_CancelBatchMemberReQueuesSiblings(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	h.git.seed("main", checkSpecFile("test"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})

	h.reconcile() // one refill: batch of 3 (alice, bob, carol); "test" starts once
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 3 {
		t.Fatalf("headRun members = %+v, want 3 chained members", r)
	}
	runID := h.currentRunID()
	h.awaitStarted(runID, "test")

	callsBefore := h.git.mergeTreeCalls
	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: refB})
	h.reconcile() // drains the cancel: bob parks; alice/carol Skip unparked and re-batch, same tick

	var recA, recB, recC *core.RunRecord
	for _, rec := range h.ch.Records() {
		switch rec.Candidate.Ref {
		case refA:
			recA = rec
		case refB:
			recB = rec
		case refC:
			recC = rec
		}
	}
	if recB == nil || recB.Outcome != core.OutcomeRejected || recB.Detail != cancelDetail {
		t.Fatalf("bob's record = %+v, want Outcome=Rejected Detail=%q", recB, cancelDetail)
	}
	wantRequeueDetail := fmt.Sprintf("batch member cancelled (%s)", refB)
	if recA == nil || recA.Outcome != core.OutcomeSkipped || recA.Detail != wantRequeueDetail {
		t.Fatalf("alice's record = %+v, want Outcome=Skipped Detail=%q", recA, wantRequeueDetail)
	}
	if recC == nil || recC.Outcome != core.OutcomeSkipped || recC.Detail != wantRequeueDetail {
		t.Fatalf("carol's record = %+v, want Outcome=Skipped Detail=%q", recC, wantRequeueDetail)
	}

	if !h.git.hasRef(refB) {
		t.Fatal("bob's slot removed; a cancel should only park it")
	}
	if !h.git.hasRef(refA) || !h.git.hasRef(refC) {
		t.Fatal("alice/carol's slots removed; a cancel-triggered batch re-queue must not delete anything")
	}

	// alice+carol are re-batched together, in the same tick.
	r2 := h.d.headRun("main")
	if r2 == nil || len(r2.members) != 2 {
		t.Fatalf("headRun after re-batch = %+v, want 2 members (alice, carol)", r2)
	}
	if h.git.mergeTreeCalls == callsBefore {
		t.Fatal("alice/carol were not re-tried after re-queuing")
	}
	gotRefs := []string{r2.members[0].cand.Ref, r2.members[1].cand.Ref}
	if !containsRef(gotRefs, refA) || !containsRef(gotRefs, refC) {
		t.Fatalf("re-batch members = %v, want alice and carol", gotRefs)
	}
	if containsRef(gotRefs, refB) {
		t.Fatal("bob (parked) re-entered a batch")
	}
}

func containsRef(refs []string, ref string) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

// TestCommand_CancelSpeculateHeadBubblesSuffix covers speculate mode
// (Feature 1): cancelling the HEAD run (lane position 0) parks its own
// member and bubbles every run behind it in the window (Skipped, unparked —
// their predicted base, "the cancelled run's chain tip", is no longer
// valid), exactly as a real head-run verdict would via invalidateSuffix.
//
// Both bubbled candidates are unparked, so — same tick, see
// TestCommand_CancelBatchMemberReQueuesSiblings's doc for why there's no
// staleness hazard a cancel needs to defer a tick for — refillLane
// immediately rebuilds a fresh window out of them; only the cancelled
// candidate (alice) is missing from it.
func TestCommand_CancelSpeculateHeadBubblesSuffix(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	refA, refB, refC := pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills: alice(#0)->bob(#1)->carol(#2)
	if d := snapshotPipelineDepth(h.d, "main"); d != 3 {
		t.Fatalf("pipeline depth = %d, want 3", d)
	}

	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: refA})
	h.reconcile() // drains the cancel: alice parks; bob+carol bubble and are re-picked, same tick

	if d := snapshotPipelineDepth(h.d, "main"); d != 2 {
		t.Fatalf("pipeline depth after cancelling the head = %d, want 2 (bob, carol re-picked same tick)", d)
	}

	var recA, recB, recC *core.RunRecord
	for _, rec := range h.ch.Records() {
		switch rec.Candidate.Ref {
		case refA:
			recA = rec
		case refB:
			recB = rec
		case refC:
			recC = rec
		}
	}
	if recA == nil || recA.Outcome != core.OutcomeRejected || recA.Detail != cancelDetail {
		t.Fatalf("alice's record = %+v, want Outcome=Rejected Detail=%q", recA, cancelDetail)
	}
	if recB == nil || recB.Outcome != core.OutcomeSkipped {
		t.Fatalf("bob's (original run's) record = %+v, want Outcome=Skipped (bubbled)", recB)
	}
	if recC == nil || recC.Outcome != core.OutcomeSkipped {
		t.Fatalf("carol's (original run's) record = %+v, want Outcome=Skipped (bubbled)", recC)
	}
	if !h.git.hasRef(refA) || !h.git.hasRef(refB) || !h.git.hasRef(refC) {
		t.Fatal("a cancel-triggered bubble must not delete any slot")
	}

	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 2 {
		t.Fatalf("lane = %+v, want 2 fresh runs (bob, carol)", l)
	}
	gotRefs := []string{l.runs[0].members[0].cand.Ref, l.runs[1].members[0].cand.Ref}
	if !containsRef(gotRefs, refB) || !containsRef(gotRefs, refC) || containsRef(gotRefs, refA) {
		t.Fatalf("re-filled window members = %v, want bob and carol (not alice)", gotRefs)
	}
}

// TestCommand_CancelSpeculateMiddleBubblesOnlySuffix covers the
// non-head-run half of speculate cancellation: cancelling a MIDDLE run
// parks its own member and bubbles only the run(s) behind it — the run
// ahead of it (alice, already tested against the real, unpredicted target
// tip) is left running completely undisturbed, same run object, same runID.
func TestCommand_CancelSpeculateMiddleBubblesOnlySuffix(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	_, refB, refC := pushThreeSpeculateCandidates(h)

	h.reconcile()
	if d := snapshotPipelineDepth(h.d, "main"); d != 3 {
		t.Fatalf("pipeline depth = %d, want 3", d)
	}
	aliceRunID := h.d.headRun("main").runID

	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: refB})
	h.reconcile() // drains the cancel: bob parks; carol bubbles and is re-picked behind alice, same tick

	if d := snapshotPipelineDepth(h.d, "main"); d != 2 {
		t.Fatalf("pipeline depth after cancelling the middle run = %d, want 2 (alice survives, carol re-picked)", d)
	}

	var recB, recC *core.RunRecord
	for _, rec := range h.ch.Records() {
		switch rec.Candidate.Ref {
		case refB:
			recB = rec
		case refC:
			recC = rec
		}
	}
	if recB == nil || recB.Outcome != core.OutcomeRejected || recB.Detail != cancelDetail {
		t.Fatalf("bob's record = %+v, want Outcome=Rejected Detail=%q", recB, cancelDetail)
	}
	if recC == nil || recC.Outcome != core.OutcomeSkipped {
		t.Fatalf("carol's (original run's) record = %+v, want Outcome=Skipped (bubbled behind bob)", recC)
	}

	r := h.d.headRun("main")
	if r == nil || r.runID != aliceRunID {
		t.Fatal("alice's run (ahead of bob) was disturbed by cancelling bob")
	}
	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 2 || l.runs[1].members[0].cand.Ref != refC {
		t.Fatalf("lane after cancel = %+v, want alice (unchanged) then a freshly re-picked carol", l)
	}
}
