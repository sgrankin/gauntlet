package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestSnapshot_MidRun covers docs/plans/phase23.md §2.1: a Snapshot taken
// while a check is running shows the in-flight run's shape — its identity,
// the checks finished so far (none yet), and the currently running check —
// and advances as checks complete. It also pins the deep-copy discipline
// the plan calls for: mutating the returned Done slice must not corrupt the
// daemon's own live RunRecord.
func TestSnapshot_MidRun(t *testing.T) {
	h := newHarness(t)
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	candSHA := h.git.pushCandidate(ref, "", checkSpecFile("lint", "test"))

	h.reconcile() // trial clean; lint started
	runID := h.currentRunID()
	h.awaitStarted(runID, "lint")

	snap := h.d.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() returned nil after a completed pass")
	}
	if snap.At.IsZero() {
		t.Error("Snapshot.At is zero")
	}
	if len(snap.Targets) != 1 {
		t.Fatalf("Targets = %+v, want exactly 1", snap.Targets)
	}
	ts := snap.Targets[0]
	if ts.Name != "main" || ts.Branch != "main" {
		t.Fatalf("TargetSnapshot = %+v, want Name=main Branch=main", ts)
	}
	if ts.TargetTip != base {
		t.Fatalf("TargetTip = %q, want %q", ts.TargetTip, base)
	}
	if ts.InFlight == nil {
		t.Fatal("InFlight is nil while a check is running")
	}
	if ts.InFlight.Candidate.SHA != candSHA || ts.InFlight.Candidate.Ref != ref {
		t.Fatalf("InFlight.Candidate = %+v, want SHA=%q Ref=%q", ts.InFlight.Candidate, candSHA, ref)
	}
	if ts.InFlight.RunID != runID {
		t.Fatalf("InFlight.RunID = %q, want %q", ts.InFlight.RunID, runID)
	}
	if ts.InFlight.BaseOID != base {
		t.Fatalf("InFlight.BaseOID = %q, want %q", ts.InFlight.BaseOID, base)
	}
	if ts.InFlight.MergeSHA == "" {
		t.Error("InFlight.MergeSHA is empty")
	}
	if len(ts.InFlight.Done) != 0 {
		t.Fatalf("InFlight.Done = %+v, want none finished yet", ts.InFlight.Done)
	}
	if ts.InFlight.Current == nil || ts.InFlight.Current.Name != "lint" {
		t.Fatalf("InFlight.Current = %+v, want the running lint check", ts.InFlight.Current)
	}
	if ts.InFlight.Current.StartedAt.IsZero() {
		t.Error("InFlight.Current.StartedAt is zero")
	}
	// docs/plans/phase5.md §3.4: today's serial-only code degenerately
	// populates Pipeline as a single-element mirror of InFlight, and
	// Members as the one candidate.
	if len(ts.Pipeline) != 1 {
		t.Fatalf("Pipeline = %+v, want exactly 1 (serial-only degenerate population)", ts.Pipeline)
	}
	if ts.Pipeline[0].RunID != ts.InFlight.RunID || ts.Pipeline[0].Candidate != ts.InFlight.Candidate {
		t.Fatalf("Pipeline[0] = %+v, want it to mirror InFlight %+v", ts.Pipeline[0], *ts.InFlight)
	}
	if len(ts.InFlight.Members) != 1 || ts.InFlight.Members[0] != ts.InFlight.Candidate {
		t.Fatalf("InFlight.Members = %+v, want exactly [Candidate]", ts.InFlight.Members)
	}
	if ts.InFlight.ChainTip != ts.InFlight.MergeSHA {
		t.Fatalf("InFlight.ChainTip = %q, want == MergeSHA %q", ts.InFlight.ChainTip, ts.InFlight.MergeSHA)
	}
	if ts.InFlight.Predicted {
		t.Error("InFlight.Predicted = true, want false (no speculation yet)")
	}
	if ts.InFlight.BatchID != "" {
		t.Errorf("InFlight.BatchID = %q, want empty (no batching yet)", ts.InFlight.BatchID)
	}
	if len(ts.Waiting) != 0 {
		t.Fatalf("Waiting = %+v, want none (the only candidate is in flight)", ts.Waiting)
	}
	if len(ts.Parked) != 0 {
		t.Fatalf("Parked = %+v, want none", ts.Parked)
	}

	// Release lint and confirm the snapshot advances: Done grows, Current
	// moves to test.
	h.release(runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckPassed})
	h.awaitStarted(runID, "test")
	snap2 := h.d.Snapshot()
	ts2 := snap2.Targets[0]
	if len(ts2.InFlight.Done) != 1 || ts2.InFlight.Done[0].Name != "lint" {
		t.Fatalf("Done after lint finishes = %+v, want [lint]", ts2.InFlight.Done)
	}
	if ts2.InFlight.Current == nil || ts2.InFlight.Current.Name != "test" {
		t.Fatalf("Current after lint finishes = %+v, want test running", ts2.InFlight.Current)
	}

	// Deep-copy discipline: mutating the snapshot's Done slice must not
	// alias the live run's RunRecord.Checks.
	ts2.InFlight.Done[0].Name = "corrupted"
	if h.d.headRun("main").members[0].rec.Checks[0].Name != "lint" {
		t.Fatal("Snapshot's Done slice aliases the live RunRecord.Checks slice")
	}

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land; drains the run
}

// TestSnapshot_Waiting covers the Waiting list: a second candidate that
// arrives while the lane is occupied shows up in FIFO order with the same
// sequence number pickHead itself would use, not as InFlight or Parked.
func TestSnapshot_Waiting(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)

	refA := candidateRef("main", "alice", "first")
	h.git.pushCandidate(refA, "", checkSpecFile("test"))
	h.reconcile() // A's trial starts; A claims the lane

	refB := candidateRef("main", "bob", "second")
	shaB := h.git.pushCandidate(refB, "", checkSpecFile("test"))
	h.reconcile() // one lane: B just queues

	snap := h.d.Snapshot()
	ts := snap.Targets[0]
	if ts.InFlight == nil || ts.InFlight.Candidate.Ref != refA {
		t.Fatalf("InFlight = %+v, want A in flight", ts.InFlight)
	}
	if len(ts.Waiting) != 1 {
		t.Fatalf("Waiting = %+v, want exactly B", ts.Waiting)
	}
	if ts.Waiting[0].Candidate.Ref != refB || ts.Waiting[0].Candidate.SHA != shaB {
		t.Fatalf("Waiting[0].Candidate = %+v, want ref=%q sha=%q", ts.Waiting[0].Candidate, refB, shaB)
	}
	if got, want := ts.Waiting[0].Seq, h.d.order["main"][refB]; got != want {
		t.Fatalf("Waiting[0].Seq = %d, want %d (pickHead's own FIFO key)", got, want)
	}
	if len(ts.Parked) != 0 {
		t.Fatalf("Parked = %+v, want none", ts.Parked)
	}

	h.release(h.currentRunID(), "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land A; drain
}

// TestSnapshot_ParkedWithReason covers ParkedEntry: a rejected candidate
// shows up parked, with the outcome and detail from its terminal event, not
// as InFlight or Waiting.
func TestSnapshot_ParkedWithReason(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // rejects + parks

	snap := h.d.Snapshot()
	ts := snap.Targets[0]
	if ts.InFlight != nil {
		t.Fatalf("InFlight = %+v, want nil after the run parked", ts.InFlight)
	}
	if len(ts.Waiting) != 0 {
		t.Fatalf("Waiting = %+v, want none", ts.Waiting)
	}
	if len(ts.Parked) != 1 {
		t.Fatalf("Parked = %+v, want exactly one entry", ts.Parked)
	}
	p := ts.Parked[0]
	if p.Candidate.Ref != ref || p.Candidate.SHA != sha {
		t.Fatalf("Parked[0].Candidate = %+v, want ref=%q sha=%q", p.Candidate, ref, sha)
	}
	if p.Outcome != core.OutcomeRejected {
		t.Fatalf("Parked[0].Outcome = %v, want Rejected", p.Outcome)
	}
	if p.Reason == "" {
		t.Error("Parked[0].Reason is empty")
	}
	if p.At.IsZero() {
		t.Error("Parked[0].At is zero")
	}
}

// TestSnapshot_WaitingExcludesEveryPipelineMember proves the F3 fix (the
// phase-5 review): buildTargetSnapshot's old Waiting filter excluded only
// ts.InFlight.Candidate.Ref — the HEAD run's head member — so a filled
// speculation window double-counted every OTHER in-flight member as
// Waiting too, inflating the queue-depth metric this phase's own tuning
// relies on. A window filled to depth 3 with exactly 3 queued candidates
// must show all 3 as Pipeline and NONE as Waiting.
func TestSnapshot_WaitingExcludesEveryPipelineMember(t *testing.T) {
	h := newHarness(t, speculateTarget(3))
	pushThreeSpeculateCandidates(h)

	h.reconcile() // window fills: alice(0)->bob(1)->carol(2), all three in flight

	snap := h.d.Snapshot()
	ts := snap.Targets[0]
	if len(ts.Pipeline) != 3 {
		t.Fatalf("Pipeline = %+v, want exactly 3 (window fully filled)", ts.Pipeline)
	}
	if len(ts.Waiting) != 0 {
		t.Fatalf("Waiting = %+v, want none (F3: every pipeline member, not just the head run's, must be excluded)", ts.Waiting)
	}
}

// TestSnapshot_WaitingExcludesBatchMembers is TestSnapshot_
// WaitingExcludesEveryPipelineMember's batch-mode counterpart: a batch's
// non-head members (bob, carol — chained into the SAME run as alice, not
// separate pipeline entries) must also not double-count as Waiting.
func TestSnapshot_WaitingExcludesBatchMembers(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	h.git.seed("main", checkSpecFile("test"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})

	h.reconcile() // one refill: all three chain into one batch run

	snap := h.d.Snapshot()
	ts := snap.Targets[0]
	if len(ts.Pipeline) != 1 || len(ts.Pipeline[0].Members) != 3 {
		t.Fatalf("Pipeline = %+v, want exactly 1 run with 3 chained members", ts.Pipeline)
	}
	if len(ts.Waiting) != 0 {
		t.Fatalf("Waiting = %+v, want none (F3: a batch's non-head members must not double-count as waiting)", ts.Waiting)
	}
}

// TestSnapshot_IdleSince covers the queue-idleness signal (docs/plans/
// scale.md §2): an idle repo stamps IdleSince on its very first pass, holds
// that instant steady across further idle passes, zeroes it the instant a
// candidate arrives, and stamps a FRESH instant (not the original) once the
// queue drains back to idle.
func TestSnapshot_IdleSince(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)

	h.reconcile() // nothing pushed yet: idle from the first pass
	snap := h.d.Snapshot()
	if snap.IdleSince.IsZero() {
		t.Fatal("IdleSince is zero on an idle daemon's first pass")
	}
	firstIdle := snap.IdleSince

	h.reconcile() // still idle: IdleSince must not move
	if got := h.d.Snapshot().IdleSince; got != firstIdle {
		t.Fatalf("IdleSince = %v after a second idle pass, want unchanged %v", got, firstIdle)
	}

	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile() // candidate claims the lane: busy
	if got := h.d.Snapshot().IdleSince; !got.IsZero() {
		t.Fatalf("IdleSince = %v while a candidate is in flight, want zero", got)
	}

	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // lands
	// buildSnapshot uses the SAME refs map ReconcileOnce fetched at the top of
	// this tick (captured before this tick's own CAS-delete of the candidate
	// slot ref), so the just-landed candidate still shows up as Waiting in
	// THIS tick's snapshot — one more tick re-fetches refs and sees it truly
	// gone.
	h.reconcile()
	snap2 := h.d.Snapshot()
	if snap2.IdleSince.IsZero() {
		t.Fatal("IdleSince is zero after the queue drained back to idle")
	}
	if !snap2.IdleSince.After(firstIdle) {
		t.Fatalf("IdleSince = %v after re-idling, want a fresh instant after %v, not the original", snap2.IdleSince, firstIdle)
	}
}
