// Batch-mode suite (docs/plans/phase5.md §2.5's batch refill, §2.6/§10
// amendment 2's red serial-fallback, §10 amendment 3's spec-change batch
// boundary, §3.3's per-member RunRecord shape). Built on the fake harness
// (daemon_test.go's testHarness/fakeGitRepo), the same tier
// TestReconcile_GreenMultiCheckLand and land_test.go's IsAncestor-recovery
// test use — batch's state-machine behavior (which member chains, which
// parks, which records share a BatchID) is exactly what the fake proves
// most cheaply and deterministically; chain_test.go already proves the
// underlying buildChainLink/specChanged mechanics against real git.
package queue

import (
	"fmt"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

// batchTarget is the one-target config every test in this file uses: batch
// mode, a generous max-batch so a test's own candidate count is always the
// limiting factor, not the config.
func batchTarget(maxBatch int) config.Target {
	return config.Target{Name: "main", Branch: "main", Mode: "batch", MaxBatch: maxBatch}
}

// TestBatchLand_OnePushNDeletes proves §2.4's one-push-N-deletes land shape
// for a genuine 3-member batch: exactly one CAS push to the target ref,
// followed by exactly one CAS delete per member's slot, in FIFO order, and
// the target tip lands byte-identical to the chain tip that was actually
// tested (Invariant 1).
func TestBatchLand_OnePushNDeletes(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	// The base already carries the check spec (unlike a single-candidate
	// serial test, which can introduce it via the one candidate's own
	// merge): a batch's specChanged boundary (§10 amendment 3) would
	// otherwise see every member "introduce" the spec from absent to
	// present and terminate the batch after member 0 every time.
	h.git.seed("main", checkSpecFile("test"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	shaA := h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	shaB := h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	shaC := h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})

	h.reconcile() // one refill: all three chain into one batch run; "test" starts once
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 3 {
		t.Fatalf("headRun members = %+v, want 3 chained members", r)
	}

	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // green: lands all three

	if len(h.git.casLog) != 4 {
		t.Fatalf("CAS calls = %d, want exactly 4 (1 target push + 3 slot deletes), log=%+v", len(h.git.casLog), h.git.casLog)
	}
	if h.git.casLog[0].ref != "refs/heads/main" {
		t.Errorf("CAS[0] ref = %q, want the target ref (the one push)", h.git.casLog[0].ref)
	}
	wantDeleteOrder := []string{refA, refB, refC}
	for i, want := range wantDeleteOrder {
		got := h.git.casLog[i+1].ref
		if got != want {
			t.Errorf("CAS[%d] ref = %q, want %q (member %d's slot delete, FIFO)", i+1, got, want, i)
		}
	}

	tip := h.git.ref("refs/heads/main")
	if tip == "" {
		t.Fatal("target has no tip after landing")
	}
	if h.git.hasRef(refA) || h.git.hasRef(refB) || h.git.hasRef(refC) {
		t.Fatal("a candidate slot still exists after the batch landed")
	}

	// The pushed tip must be byte-identical to the chain's own tip — never
	// a rebuilt/re-tested commit (Invariant 1).
	if tip != r.chainTip {
		t.Fatalf("target tip = %s, want the tested chain tip %s", tip, r.chainTip)
	}

	// --first-parent from the tip: one merge per member, parent[1] verbatim,
	// tip-first order is carol, bob, alice (member 2 chained last).
	wantParent1 := []string{shaC, shaB, shaA}
	oid := tip
	for i, want := range wantParent1 {
		parents := h.git.commits[oid].parents
		if len(parents) != 2 {
			t.Fatalf("commit %d (%s) parents = %v, want 2 (a --no-ff chain link)", i, oid, parents)
		}
		if parents[1] != want {
			t.Fatalf("commit %d parent[1] = %s, want candidate SHA %s verbatim (Invariant 1/6)", i, parents[1], want)
		}
		oid = parents[0]
	}
}

// TestBatchMemberRecords_ShareBatchID proves §3.3's per-member RunRecord
// shape: a batch of 3 lands with 3 separate RunRecords, all sharing one
// non-empty BatchID, Position 0..2, BatchSize 3, each with its own distinct
// MergeSHA (its own chain link) — and the shared check's result duplicated
// onto every one of them (not just the head member's).
func TestBatchMemberRecords_ShareBatchID(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	h.git.seed("main", checkSpecFile("test")) // see TestBatchLand_OnePushNDeletes's comment on why the base must carry the spec
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	shaA := h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	shaB := h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	shaC := h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: 42})

	recs := h.ch.Records()
	if len(recs) < 3 {
		t.Fatalf("got %d records, want at least 3", len(recs))
	}
	last3 := recs[len(recs)-3:]

	batchID := last3[0].BatchID
	if batchID == "" {
		t.Fatal("BatchID is empty, want a shared non-empty batch id")
	}
	// Data-loss fix under test: history's runs table PRIMARY KEYs on run_id
	// (INSERT OR REPLACE), so N members sharing one RunID would silently
	// collapse to the last member's row. Member 0 keeps the bare batch run
	// ID (BatchID) verbatim; members 1..N-1 get distinct "<batchID>-mN"
	// suffixes — every member gets its own history row.
	if last3[0].RunID != batchID {
		t.Errorf("member 0 RunID = %q, want it to equal the bare batch id %q", last3[0].RunID, batchID)
	}
	seenRunID := map[string]bool{}
	wantSHA := []string{shaA, shaB, shaC}
	seenMerge := map[string]bool{}
	for i, r := range last3 {
		if r.BatchID != batchID {
			t.Errorf("record %d BatchID = %q, want shared %q", i, r.BatchID, batchID)
		}
		if i > 0 {
			wantRunID := fmt.Sprintf("%s-m%d", batchID, i)
			if r.RunID != wantRunID {
				t.Errorf("member %d RunID = %q, want %q", i, r.RunID, wantRunID)
			}
		}
		if seenRunID[r.RunID] {
			t.Errorf("member %d RunID %q collides with an earlier member's — this is the data-loss bug", i, r.RunID)
		}
		seenRunID[r.RunID] = true
		if r.Position != i {
			t.Errorf("record %d Position = %d, want %d", i, r.Position, i)
		}
		if r.BatchSize != 3 {
			t.Errorf("record %d BatchSize = %d, want 3", i, r.BatchSize)
		}
		if r.Candidate.SHA != wantSHA[i] {
			t.Errorf("record %d Candidate.SHA = %q, want %q", i, r.Candidate.SHA, wantSHA[i])
		}
		if r.Outcome != core.OutcomeLanded {
			t.Errorf("record %d Outcome = %v, want Landed", i, r.Outcome)
		}
		if len(r.Checks) != 1 || r.Checks[0].Name != "test" || r.Checks[0].Status != core.CheckPassed {
			t.Errorf("record %d Checks = %+v, want the shared 'test' result duplicated onto it (§3.3)", i, r.Checks)
		}
		if r.MergeSHA == "" || seenMerge[r.MergeSHA] {
			t.Errorf("record %d MergeSHA = %q, want its own distinct chain link", i, r.MergeSHA)
		}
		seenMerge[r.MergeSHA] = true
	}
	if last3[2].MergeSHA != h.git.ref("refs/heads/main") {
		t.Fatalf("last member's MergeSHA = %q, want == the landed target tip %q", last3[2].MergeSHA, h.git.ref("refs/heads/main"))
	}
	// Per-member BaseOID chains: member i+1's BaseOID is member i's MergeSHA.
	if last3[1].BaseOID != last3[0].MergeSHA {
		t.Errorf("record 1 BaseOID = %q, want record 0's MergeSHA %q", last3[1].BaseOID, last3[0].MergeSHA)
	}
	if last3[2].BaseOID != last3[1].MergeSHA {
		t.Errorf("record 2 BaseOID = %q, want record 1's MergeSHA %q", last3[2].BaseOID, last3[1].MergeSHA)
	}
}

// TestBatchCrashRecovery covers §8's batch crash-recovery walkthrough: the
// chain already landed (target tip contains all three members' commits)
// but the candidate slots were never deleted — the crash-before-slot-delete
// window. Each member recovers independently, head-pick-only per refill
// (§2.5), with no re-merge and no re-test (Invariant 4).
func TestBatchCrashRecovery(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	base := h.git.seed("main", nil)
	files := checkSpecFile("test")
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	shaA := h.git.pushCandidate(refA, "", files)
	shaB := h.git.pushCandidate(refB, "", files)
	shaC := h.git.pushCandidate(refC, "", files)

	// Craft a chain exactly like a landed batch (one merge per member) as
	// if a previous daemon instance had landed it and crashed before
	// deleting the slots.
	linkA := h.git.commit(files, base, shaA)
	linkB := h.git.commit(files, linkA, shaB)
	linkC := h.git.commit(files, linkB, shaC)
	h.git.setRef("refs/heads/main", linkC)

	h.reconcile() // recovers the head member only (alice)
	if h.git.hasRef(refA) {
		t.Fatal("alice's slot not recovered on the first tick")
	}
	if !h.git.hasRef(refB) || !h.git.hasRef(refC) {
		t.Fatal("bob/carol recovered too early; recovery must be head-pick-only per refill (§2.5)")
	}

	h.reconcile() // recovers bob
	if h.git.hasRef(refB) {
		t.Fatal("bob's slot not recovered on the second tick")
	}
	if !h.git.hasRef(refC) {
		t.Fatal("carol recovered too early")
	}

	h.reconcile() // recovers carol
	if h.git.hasRef(refC) {
		t.Fatal("carol's slot not recovered on the third tick")
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
	if landedCount != 3 {
		t.Fatalf("EventLanded count = %d, want 3 (one recovered landing per member)", landedCount)
	}
}

// TestBatchSpecChangeBoundary covers §10 amendment 3: 3 queued candidates,
// the MIDDLE one (bob) modifies the check spec. The batch must form from
// alice+bob only (bob included, batch ends there); carol waits for the
// next batch, formed on a later refill.
func TestBatchSpecChangeBoundary(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	h.git.seed("main", checkSpecFile("test"))

	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{testCheckSpecPath: "check \"test2\" {\n    command \"true\"\n}\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})

	h.reconcile() // batch forms: alice+bob only; bob's link changes the spec and ends the batch there

	r := h.d.headRun("main")
	if r == nil {
		t.Fatal("no in-flight run after refill")
	}
	if len(r.members) != 2 {
		t.Fatalf("batch has %d members, want 2 (alice, bob) — carol must wait for the next batch", len(r.members))
	}
	if r.members[0].cand.Topic != "a" || r.members[1].cand.Topic != "b" {
		t.Fatalf("batch members = [%s %s], want [a b]", r.members[0].cand.Topic, r.members[1].cand.Topic)
	}
	if !h.git.hasRef(refC) {
		t.Fatal("carol's ref should be untouched, waiting for the next batch")
	}

	// The batch's check suite reads the TIP's spec — bob's, "test2" — not
	// alice's original "test" (the documented "tested by the tip's own
	// definition" shape, narrowed by the boundary rule above so the tip is
	// always the changing member itself).
	runID := h.currentRunID()
	h.release(runID, "test2", core.CheckResult{Name: "test2", Status: core.CheckPassed})

	if h.git.hasRef(refA) || h.git.hasRef(refB) {
		t.Fatal("alice/bob should have landed")
	}
	if !h.git.hasRef(refC) {
		t.Fatal("carol should not have been touched by the first batch")
	}

	h.reconcile() // next refill: carol forms her own (size-1) batch
	r2 := h.d.headRun("main")
	if r2 == nil || len(r2.members) != 1 || r2.members[0].cand.Topic != "c" {
		t.Fatalf("second batch members = %+v, want just carol", r2)
	}
}

// TestBatchRejectedRecordsHaveDistinctRunIDs covers rejectBatch — the
// batch-wide pre-check failure path (a chain forms, then the tip tree's
// check spec can't be parsed): every chained member parks with its own
// terminal EventRejected record, and — the data-loss fix again — those
// records carry distinct RunIDs (member 0 bare, member 1 "<batchID>-m1")
// sharing one BatchID, so history keeps a row per member here too.
//
// Trigger: bob's push corrupts the check spec. The §10 amendment 3
// spec-change boundary ends the batch at bob (alice+bob chained, carol
// untouched), then finishBatchStart's ParseChecks on the tip tree fails,
// routing both chained members through rejectBatch.
func TestBatchRejectedRecordsHaveDistinctRunIDs(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	h.git.seed("main", checkSpecFile("test"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{testCheckSpecPath: "not a valid check spec {{{\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})

	h.reconcile() // chain forms alice+bob (spec-change boundary), then the tip's spec fails to parse: rejectBatch

	var rejected []*core.RunRecord
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventRejected {
			rejected = append(rejected, e.Record)
			if e.RunID != e.Record.RunID {
				t.Errorf("event RunID = %q, want the member's own record RunID %q", e.RunID, e.Record.RunID)
			}
		}
	}
	if len(rejected) != 2 {
		t.Fatalf("EventRejected count = %d, want 2 (one per chained member; carol never chained)", len(rejected))
	}

	batchID := rejected[0].BatchID
	if batchID == "" {
		t.Fatal("BatchID is empty on the batch-rejected records")
	}
	if rejected[0].RunID != batchID {
		t.Errorf("member 0 RunID = %q, want it to equal the bare batch id %q", rejected[0].RunID, batchID)
	}
	if want := fmt.Sprintf("%s-m1", batchID); rejected[1].RunID != want {
		t.Errorf("member 1 RunID = %q, want %q", rejected[1].RunID, want)
	}
	for i, rec := range rejected {
		if rec.BatchID != batchID {
			t.Errorf("record %d BatchID = %q, want shared %q", i, rec.BatchID, batchID)
		}
		if rec.Position != i {
			t.Errorf("record %d Position = %d, want %d", i, rec.Position, i)
		}
		if rec.BatchSize != 2 {
			t.Errorf("record %d BatchSize = %d, want 2", i, rec.BatchSize)
		}
		if rec.Outcome != core.OutcomeRejected {
			t.Errorf("record %d Outcome = %v, want Rejected", i, rec.Outcome)
		}
	}

	// rejectBatch parks every chained member: neither slot is deleted, and
	// the next refill skips both parked refs, forming carol's own batch.
	if !h.git.hasRef(refA) || !h.git.hasRef(refB) {
		t.Fatal("parked members' slots must survive (parked, not landed)")
	}
	h.reconcile()
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 1 || r.members[0].cand.Topic != "c" {
		t.Fatalf("next refill = %+v, want just carol (alice/bob parked)", r)
	}
}

// TestBatchRedEmitsPerMemberSkipped covers §2.6, overridden by §10
// amendment 2: a batch of 3 whose combined check suite fails must NOT park
// anyone and must NOT emit EventRejected — every member gets its own
// EventSkipped (Outcome Skipped, the shared failed check duplicated onto
// it, a Detail naming the batch and the failing check), and batch-red
// serial fallback engages: the next refill forms a single-member run for
// the FIFO head instead of another batch.
func TestBatchRedEmitsPerMemberSkipped(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	h.git.seed("main", checkSpecFile("test")) // see TestBatchLand_OnePushNDeletes's comment on why the base must carry the spec
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})

	h.reconcile() // batch of 3 forms
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // batch red

	for _, ref := range []string{refA, refB, refC} {
		if !h.git.hasRef(ref) {
			t.Fatalf("%s deleted on batch red; nothing should park or land", ref)
		}
	}

	var skipped []*core.RunRecord
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventSkipped {
			skipped = append(skipped, e.Record)
		}
		if e.Kind == core.EventRejected {
			t.Fatal("no EventRejected should fire from the batch's own red verdict (§10 amendment 2)")
		}
	}
	if len(skipped) != 3 {
		t.Fatalf("EventSkipped count = %d, want 3 (one per member)", len(skipped))
	}
	batchID := skipped[0].BatchID
	if batchID == "" {
		t.Fatal("BatchID is empty on the batch-red records")
	}
	// Same data-loss fix as the green-batch case: red-skipped member records
	// must land distinct RunIDs too (member 0 bare, members 1..N-1
	// suffixed), or history's INSERT OR REPLACE would collapse them exactly
	// the same way it did for a green batch.
	if skipped[0].RunID != batchID {
		t.Errorf("member 0 RunID = %q, want it to equal the bare batch id %q", skipped[0].RunID, batchID)
	}
	seenRunID := map[string]bool{}
	for i, rec := range skipped {
		if i > 0 {
			wantRunID := fmt.Sprintf("%s-m%d", batchID, i)
			if rec.RunID != wantRunID {
				t.Errorf("member %d RunID = %q, want %q", i, rec.RunID, wantRunID)
			}
		}
		if seenRunID[rec.RunID] {
			t.Errorf("member %d RunID %q collides with an earlier member's — this is the data-loss bug", i, rec.RunID)
		}
		seenRunID[rec.RunID] = true
	}
	for i, rec := range skipped {
		if rec.Outcome != core.OutcomeSkipped {
			t.Errorf("record %d Outcome = %v, want Skipped", i, rec.Outcome)
		}
		wantDetail := fmt.Sprintf("batch %s red on check %q; serializing", batchID, "test")
		if rec.Detail != wantDetail {
			t.Errorf("record %d Detail = %q, want %q", i, rec.Detail, wantDetail)
		}
		if len(rec.Checks) != 1 || rec.Checks[0].Status != core.CheckFailed {
			t.Errorf("record %d Checks = %+v, want the shared failed check attached", i, rec.Checks)
		}
	}

	// Serial fallback: the next refill forms alice ALONE, not another batch.
	h.reconcile()
	rAlice := h.d.headRun("main")
	if rAlice == nil || len(rAlice.members) != 1 || rAlice.members[0].cand.Topic != "a" {
		t.Fatalf("serial fallback did not form a single-member run for alice: %+v", rAlice)
	}

	// Finish the serial walk: alice lands, clearing the fallback flag.
	runID2 := h.currentRunID()
	h.release(runID2, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if h.git.hasRef(refA) {
		t.Fatal("alice should have landed via the serial fallback round")
	}

	// Fallback cleared on landing: the next refill re-forms a batch (bob +
	// carol), not another singleton.
	h.reconcile()
	rNext := h.d.headRun("main")
	if rNext == nil {
		t.Fatal("no run formed after alice's landing cleared the fallback")
	}
	if len(rNext.members) != 2 {
		t.Fatalf("post-fallback run has %d members, want 2 (bob+carol re-batched)", len(rNext.members))
	}
}
