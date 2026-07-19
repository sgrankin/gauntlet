// Receipt-notes SCHEDULING/CAPTURE/PUBLICATION suite (issue #13's core
// slice): the receipt node's scheduling and result-capture (mirroring
// image_test.go's build-node coverage), and landRun's publish-then-CAS
// gate — the correctness heart of this slice. receipt_notes_test.go (the
// earlier config-surface slice) already covers SpecRejectReason's own
// gates directly; this file exercises everything downstream of a spec that
// already passed those gates, through the public ReconcileOnce API on the
// fake-git harness (daemon_test.go's testHarness/fakeGitRepo) — the same
// tier image_test.go and land_test.go use.
package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/history"
)

// testReceiptRef is the receipt-notes ref every test in this file uses.
const testReceiptRef = "refs/notes/gauntlet/receipts"

// newReceiptHarness builds a testHarness with a receipt-notes policy
// enabled (Config.ReceiptNotes non-nil) at the given max-bytes ceiling,
// otherwise identical to newHarness — the same "set the field directly on
// h.d.cfg after construction" pattern image_test.go's imageCapableHarness
// uses for ImageCapableProfile.
func newReceiptHarness(t *testing.T, maxBytes int, targets ...config.Target) *testHarness {
	h := newHarness(t, targets...)
	h.d.cfg.ReceiptNotes = &config.ReceiptNotes{Ref: testReceiptRef, MaxBytes: maxBytes}
	return h
}

// receiptSpecFile renders a .gauntlet.kdl with one plain check "unit" and
// one receipt node named name — a receipt-only spec is invalid
// (config.CheckSpec.validate rejects zero checks), so every fixture in this
// file needs the one plain check alongside it. Default max-parallel (1)
// keeps scheduling deterministic: "unit" (declared first) runs before
// "receipt:<name>" (buildRunNodes always appends the receipt node last).
func receiptSpecFile(name string) map[string]string {
	return map[string]string{testCheckSpecPath: "check \"unit\" {\n    command \"true\"\n}\n" +
		"receipt \"" + name + "\" {\n    command \"true\"\n}\n"}
}

// receiptNode is the node name receiptSpecFile("deploy") schedules.
const receiptNode = "receipt:deploy"

// TestReceipt_DisabledPolicyNoReceipt_Unchanged covers the acceptance
// list's baseline: no receipt-notes policy, no receipt in the spec —
// PublishNote must never be called, byte-identical to gauntlet before
// issue #13.
func TestReceipt_DisabledPolicyNoReceipt_Unchanged(t *testing.T) {
	h := newHarness(t) // Config.ReceiptNotes nil (the default)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("unit"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if len(h.git.publishNoteCalls) != 0 {
		t.Fatalf("PublishNote calls = %d, want 0 (policy disabled, no receipt declared)", len(h.git.publishNoteCalls))
	}
	if last.ReceiptRef != "" || last.ReceiptBlob != "" || last.ReceiptPublished != "" {
		t.Errorf("record receipt provenance = %+v, want all empty", last)
	}
}

// TestReceipt_PolicyEnabledSpecMissingReceipt_Parks covers the OTHER half
// of the rollout gate through the full ReconcileOnce path (the disabled/no-
// receipt half is TestReceipt_DisabledPolicyNoReceipt_Unchanged above):
// this daemon has a receipt-notes policy configured, but the pushed
// candidate's check spec declares no receipt node at all. SpecRejectReason
// itself is already covered directly (receipt_notes_test.go's
// TestSpecRejectReason_ReceiptPolicy), but that's a pure-predicate test —
// nothing before this exercised the gate actually firing through a real
// run, the same way TestExecutorProfile_UnknownRejectsBeforeAnyCommand and
// TestServices_Gating_RejectedLoud do for the other two spec-load gates.
func TestReceipt_PolicyEnabledSpecMissingReceipt_Parks(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := h.git.pushCandidate(ref, "", checkSpecFile("unit")) // no receipt node declared
	base := h.git.ref("refs/heads/main")

	h.reconcile()

	recs := h.ch.Records()
	if len(recs) == 0 {
		t.Fatal("expected a terminal record")
	}
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	const wantDetail = "this daemon requires a receipt (receipt-notes is configured) but the check spec declares none"
	if last.Detail != wantDetail {
		t.Fatalf("Detail = %q, want SpecRejectReason's exact wording %q", last.Detail, wantDetail)
	}
	// Rejected before any command started — a config mismatch, never a red
	// verdict.
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventCheckStarted {
			t.Fatalf("check %q started despite the spec being rejected", e.CheckName)
		}
	}
	if got := h.git.ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a receipt-policy rejection: %q, want unchanged %q", got, base)
	}
	if entry, ok := h.d.done["main"][ref]; !ok || entry.SHA != sha {
		t.Fatalf("park entry = %+v ok=%v, want parked at %s", entry, ok, sha)
	}
	if len(h.git.publishNoteCalls) != 0 {
		t.Fatalf("PublishNote calls = %d, want 0", len(h.git.publishNoteCalls))
	}
}

// TestReceipt_ProducerFailure_NoPublish covers an ordinary red receipt
// command (non-zero exit, modeled by CheckFailed): the run rejects on the
// receipt node's own red row, the target never moves, and PublishNote is
// never called.
func TestReceipt_ProducerFailure_NoPublish(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	h.reconcile()
	runID := h.currentRunID()
	base := h.git.ref("refs/heads/main")

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckFailed, Output: "deploy script exited 1"})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if !strings.Contains(last.Detail, `check "receipt:deploy" failed`) {
		t.Fatalf("Detail = %q, want the receipt node named as the root cause", last.Detail)
	}
	if got := h.git.ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a receipt producer failure: %q, want unchanged %q", got, base)
	}
	if len(h.git.publishNoteCalls) != 0 {
		t.Fatalf("PublishNote calls = %d, want 0", len(h.git.publishNoteCalls))
	}
}

// TestReceipt_InvalidCapturedResult covers the three "green-shaped but
// unusable" captures — empty, unreadable, and oversized — each: the
// RECEIPT node's own red row with a distinct one-line root cause, no
// target movement, and no PublishNote call. maxBytes is small (8) so a
// legitimate-looking payload can trip "oversized" without an unwieldy
// fixture.
func TestReceipt_InvalidCapturedResult(t *testing.T) {
	cases := []struct {
		name       string
		receipt    []byte // nil = unreadable; non-nil empty = empty; else oversized
		wantDetail string
	}{
		{"unreadable result", nil, "could not be read"},
		{"empty result", []byte{}, "empty"},
		{"oversized result", []byte("this payload is far larger than the eight byte ceiling"), "exceeds the configured max-bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReceiptHarness(t, 8)
			h.git.seed("main", nil)
			ref := candidateRef("main", "alice", "w-"+strings.ReplaceAll(tc.name, " ", "-"))
			h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

			h.reconcile()
			runID := h.currentRunID()
			base := h.git.ref("refs/heads/main")

			h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
			h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: tc.receipt})

			recs := h.ch.Records()
			last := recs[len(recs)-1]
			if last.Outcome != core.OutcomeRejected {
				t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
			}
			var receiptRow *core.CheckResult
			for i := range last.Checks {
				if last.Checks[i].Name == receiptNode {
					receiptRow = &last.Checks[i]
				}
			}
			if receiptRow == nil {
				t.Fatal("no receipt node row in the terminal record")
			}
			if receiptRow.Status != core.CheckFailed || !strings.Contains(receiptRow.Output, tc.wantDetail) {
				t.Errorf("receipt row = %+v, want failed with %q in output", receiptRow, tc.wantDetail)
			}
			if got := h.git.ref("refs/heads/main"); got != base {
				t.Fatalf("target moved on an invalid receipt capture: %q, want unchanged %q", got, base)
			}
			if len(h.git.publishNoteCalls) != 0 {
				t.Fatalf("PublishNote calls = %d, want 0", len(h.git.publishNoteCalls))
			}
		})
	}
}

// TestReceipt_ProducerCancelled_NoPublishNoLanding covers an operator
// cancel landing mid-receipt: no publish, no landing — the same "cancel
// aborts the run and parks it" machinery cancel_test.go exercises for an
// ordinary check, applied to the receipt node.
func TestReceipt_ProducerCancelled_NoPublishNoLanding(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.awaitStarted(runID, receiptNode)

	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: ref})
	h.reconcile() // drains the cancel: aborts the run, parks (ref, sha)

	if h.d.headRun("main") != nil {
		t.Fatal("lane still holds a run after cancelling its sole member")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected || last.Detail != cancelDetail {
		t.Fatalf("terminal = %v %q, want Rejected %q", last.Outcome, last.Detail, cancelDetail)
	}
	if len(h.git.publishNoteCalls) != 0 {
		t.Fatalf("PublishNote calls = %d, want 0", len(h.git.publishNoteCalls))
	}
}

// TestReceipt_GreenSerialRun_PublishesBeforeTargetCAS is the correctness
// heart of this slice: a green run publishes the exact captured payload
// bytes, addressed at the run's chainTip (the tested merge SHA — never the
// candidate SHA), and does so strictly BEFORE the target CAS — proven by
// comparing the fake's own call-ordering sequence numbers, not merely
// slice position.
func TestReceipt_GreenSerialRun_PublishesBeforeTargetCAS(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	h.reconcile()
	runID := h.currentRunID()
	run := h.d.headRun("main")
	chainTip := run.chainTip
	candidateSHA := run.members[0].cand.SHA

	payload := []byte("deployment-receipt-v1")
	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: payload})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if len(h.git.publishNoteCalls) != 1 {
		t.Fatalf("PublishNote calls = %d, want 1", len(h.git.publishNoteCalls))
	}
	call := h.git.publishNoteCalls[0]
	if call.remoteRef != testReceiptRef {
		t.Errorf("PublishNote ref = %q, want %q", call.remoteRef, testReceiptRef)
	}
	if call.sha != chainTip {
		t.Errorf("PublishNote sha = %q, want the run's chainTip %q", call.sha, chainTip)
	}
	if call.sha == candidateSHA {
		t.Fatal("PublishNote addressed at the candidate SHA, want the tested chain-tip merge SHA")
	}
	if !bytes.Equal(call.payload, payload) {
		t.Errorf("PublishNote payload = %q, want exactly %q", call.payload, payload)
	}

	var targetCASSeq int
	for _, c := range h.git.casLog {
		if c.ref == "refs/heads/main" && c.new == chainTip {
			targetCASSeq = c.seq
			break
		}
	}
	if targetCASSeq == 0 {
		t.Fatal("no target CAS to chainTip found in casLog")
	}
	if call.seq >= targetCASSeq {
		t.Fatalf("PublishNote seq %d, want strictly less than the target CAS seq %d (publish must precede land)", call.seq, targetCASSeq)
	}

	if last.ReceiptRef != testReceiptRef {
		t.Errorf("record ReceiptRef = %q, want %q", last.ReceiptRef, testReceiptRef)
	}
	if last.ReceiptPublished != receiptPublishedFresh {
		t.Errorf("record ReceiptPublished = %q, want %q", last.ReceiptPublished, receiptPublishedFresh)
	}
	if last.ReceiptBlob == "" {
		t.Error("record ReceiptBlob is empty, want the published note's blob SHA")
	}
}

// TestReceipt_AlreadyPublished_StillLands covers PublishNote's idempotent
// AlreadyPublished outcome (a crash-retried or duplicate publish of the
// SAME receipt): the run still lands, and provenance records
// "already-present" rather than "published".
func TestReceipt_AlreadyPublished_StillLands(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	h.reconcile()
	runID := h.currentRunID()
	chainTip := h.d.headRun("main").chainTip
	payload := []byte("already-there-bytes")
	h.git.seedNote(testReceiptRef, chainTip, payload)

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: payload})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if last.ReceiptPublished != receiptPublishedAlready {
		t.Fatalf("record ReceiptPublished = %q, want %q", last.ReceiptPublished, receiptPublishedAlready)
	}
	if len(h.git.publishNoteCalls) != 1 {
		t.Fatalf("PublishNote calls = %d, want 1 (the queue still calls it; AlreadyPublished is its return, not a skip)", len(h.git.publishNoteCalls))
	}
}

// TestReceipt_NoteConflict_ParksNamingInvariant covers PublishNote's
// fail-closed ErrNoteConflict: a pre-existing DIFFERENT note for the same
// tested SHA parks the run as OutcomeError with a detail naming the
// invariant violation distinctly, and the target never moves.
func TestReceipt_NoteConflict_ParksNamingInvariant(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	h.reconcile()
	runID := h.currentRunID()
	chainTip := h.d.headRun("main").chainTip
	h.git.seedNote(testReceiptRef, chainTip, []byte("a disjoint receipt computed by someone else"))
	base := h.git.ref("refs/heads/main")

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("my own receipt")})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}
	if !strings.Contains(last.Detail, "invariant violation") {
		t.Fatalf("Detail = %q, want it to name the invariant violation distinctly", last.Detail)
	}
	if got := h.git.ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a note conflict: %q, want unchanged %q", got, base)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot removed on a note-conflict park")
	}
}

// TestReceipt_TransportError_ParksAndAutoRetries covers a PublishNote
// transport failure: OutcomeError park, target unmoved, and the existing
// auto-retry-once behavior fires exactly as it does for any other
// OutcomeError park (autoretry_test.go's TestAutoRetry_ErrorParkRequeuesOnce
// proves the general mechanism; this proves the publish-gate site wires
// into the SAME mechanism, not a bespoke one).
func TestReceipt_TransportError_ParksAndAutoRetries(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.d.cfg.AutoRetryErrors = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	h.reconcile()
	runID := h.currentRunID()
	base := h.git.ref("refs/heads/main")
	h.git.publishNoteErr = errors.New("notes: transport wedged")

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("payload")})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}
	if !strings.Contains(last.Detail, "publish receipt note") {
		t.Fatalf("Detail = %q, want it to name the publish step", last.Detail)
	}
	if got := h.git.ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a publish transport error: %q, want unchanged %q", got, base)
	}

	var sawAutoRetry bool
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventRetryRequested && e.Candidate.Ref == ref && e.Detail == autoRetryDetail {
			sawAutoRetry = true
		}
	}
	if !sawAutoRetry {
		t.Fatal("no automatic EventRetryRequested after the publish transport error park; auto-retry-once must cover this park site too")
	}
}

// TestReceipt_StaleTargetCASAfterPublish covers the documented harmless
// orphan: a target CAS that fails AFTER a successful publish (a human push
// racing the land, land_test.go's TestReconcile_Land_StaleTargetCAS
// scenario) takes its normal Skip/re-trial path unchanged — no note
// cleanup is attempted — and the re-trial, once it lands, publishes again
// under its OWN new merge SHA.
func TestReceipt_StaleTargetCASAfterPublish(t *testing.T) {
	h := newReceiptHarness(t, 65536)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	h.reconcile()
	runID := h.currentRunID()
	firstChainTip := h.d.headRun("main").chainTip

	triggered := false
	h.git.beforeCAS = func(remoteRef string) {
		if remoteRef == "refs/heads/main" && !triggered {
			triggered = true
			h.git.directPush("main", map[string]string{"human.txt": "raced in at land time"})
		}
	}

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("payload-v1")})

	if !triggered {
		t.Fatal("beforeCAS hook never fired; test didn't exercise the race it's meant to")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot deleted despite a stale target CAS")
	}
	if len(h.git.publishNoteCalls) != 1 {
		t.Fatalf("PublishNote calls after the stale land = %d, want 1 (the successful publish is a harmless orphan; no cleanup attempted)", len(h.git.publishNoteCalls))
	}
	if h.git.publishNoteCalls[0].sha != firstChainTip {
		t.Fatalf("first publish sha = %q, want the first trial's chainTip %q", h.git.publishNoteCalls[0].sha, firstChainTip)
	}
	// The orphan case this test is named for: the note published
	// successfully before the target CAS lost the race, so the record must
	// still carry its provenance — never empty just because the run ended
	// Skipped rather than Landed (the gap this test now closes).
	if last.ReceiptRef != testReceiptRef {
		t.Errorf("orphaned run's record ReceiptRef = %q, want %q", last.ReceiptRef, testReceiptRef)
	}
	if last.ReceiptBlob == "" {
		t.Error("orphaned run's record ReceiptBlob is empty, want the published note's blob SHA")
	}
	if last.ReceiptPublished != receiptPublishedFresh {
		t.Errorf("orphaned run's record ReceiptPublished = %q, want %q", last.ReceiptPublished, receiptPublishedFresh)
	}

	// Re-trial against the new (human-pushed) tip; the slot survived
	// untouched, so it re-forms on the next tick.
	h.git.beforeCAS = nil
	h.reconcile()
	newRunID := h.currentRunID()
	if newRunID == runID {
		t.Fatal("no new run started for the re-trial")
	}
	newChainTip := h.d.headRun("main").chainTip
	if newChainTip == firstChainTip {
		t.Fatal("re-trial chained onto the same merge SHA as the raced-away first attempt")
	}

	h.release(newRunID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(newRunID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("payload-v2")})

	recs = h.ch.Records()
	last = recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome after re-trial = %v, want Landed", last.Outcome)
	}
	if len(h.git.publishNoteCalls) != 2 {
		t.Fatalf("PublishNote calls after re-trial = %d, want 2", len(h.git.publishNoteCalls))
	}
	second := h.git.publishNoteCalls[1]
	if second.sha != newChainTip {
		t.Fatalf("re-trial publish sha = %q, want its own new chainTip %q", second.sha, newChainTip)
	}
	if second.sha == firstChainTip {
		t.Fatal("re-trial published under the SAME merge SHA as the first (raced-away) attempt")
	}
}

// TestReceipt_Batch_OnePublishOnChainTip covers batch mode: the batch's
// single receipt node runs once against the chain tip's combined tree, and
// landing publishes exactly once, addressed at the BATCH chain tip (never
// any one member's own candidate SHA).
func TestReceipt_Batch_OnePublishOnChainTip(t *testing.T) {
	h := newReceiptHarness(t, 65536, batchTarget(8))
	// Seed the base WITH the receipt spec already present — a batch's
	// specChanged boundary would otherwise see every member "introduce" the
	// spec from absent to present and terminate the batch after member 0
	// (batch_test.go's own TestBatchLand_OnePushNDeletes does the same).
	h.git.seed("main", receiptSpecFile("deploy"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	shaA := h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	shaB := h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})

	h.reconcile() // one refill: both chain into one batch run
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 2 {
		t.Fatalf("headRun members = %+v, want 2 chained members", r)
	}
	chainTip := r.chainTip
	runID := h.currentRunID()

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("batch-receipt")})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if len(h.git.publishNoteCalls) != 1 {
		t.Fatalf("PublishNote calls = %d, want exactly 1 (one suite, one publish, regardless of member count)", len(h.git.publishNoteCalls))
	}
	call := h.git.publishNoteCalls[0]
	if call.sha != chainTip {
		t.Errorf("PublishNote sha = %q, want the batch's own chain tip %q", call.sha, chainTip)
	}
	if call.sha == shaA || call.sha == shaB {
		t.Fatal("PublishNote addressed at a member's own candidate SHA, want the batch chain tip")
	}
}

// TestReceipt_Speculation_PublishOnlyOnLanding covers speculation: a
// non-head successor's checks (including its own receipt node) can finish
// green while its predecessor is still unresolved, but PublishNote must
// not be called until that run actually becomes the lane head and lands —
// a speculative run's captured payload sits harmlessly in memory until
// then.
func TestReceipt_Speculation_PublishOnlyOnLanding(t *testing.T) {
	h := newReceiptHarness(t, 65536, speculateTarget(2))
	h.git.seed("main", receiptSpecFile("deploy"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})

	h.reconcile() // window fills: run0 (head, real base), run1 (predicted)
	l := h.d.lanes["main"]
	if l == nil || len(l.runs) != 2 {
		t.Fatalf("lane = %+v, want 2 chained runs", l)
	}
	run0, run1 := l.runs[0], l.runs[1]
	run0ID, run1ID := run0.runID, run1.runID
	run0ChainTip, run1ChainTip := run0.chainTip, run1.chainTip

	// The successor (run1) finishes green first, predecessor unresolved:
	// zero PublishNote calls — only the lane head may ever land.
	h.release(run1ID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(run1ID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("run1-receipt")})
	if len(h.git.publishNoteCalls) != 0 {
		t.Fatalf("PublishNote calls while the successor is green but not head = %d, want 0", len(h.git.publishNoteCalls))
	}

	// The predecessor finishes green too: run0 lands, and — in the same
	// tick's prefix-drain — the now-head run1 lands right behind it.
	h.release(run0ID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(run0ID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("run0-receipt")})

	if len(h.git.publishNoteCalls) != 2 {
		t.Fatalf("PublishNote calls after both land = %d, want 2", len(h.git.publishNoteCalls))
	}
	if h.git.publishNoteCalls[0].sha != run0ChainTip {
		t.Errorf("first publish sha = %q, want run0's chainTip %q", h.git.publishNoteCalls[0].sha, run0ChainTip)
	}
	if h.git.publishNoteCalls[1].sha != run1ChainTip {
		t.Errorf("second publish sha = %q, want run1's chainTip %q", h.git.publishNoteCalls[1].sha, run1ChainTip)
	}
}

// TestIntegration_ReceiptPublishEndToEnd exercises the whole receipt-notes
// seam with a REAL LocalExecutor subprocess and REAL git (internal/gitx)
// against a local bare remote (internal/testutil): spec parse -> the
// receipt node -> the executor exporting GAUNTLET_RECEIPT_RESULT_FILE (and
// not the check result file) -> the script writing known bytes -> queue
// capture/validation -> landRun's publish-then-CAS gate -> a REAL git-notes
// commit CAS-pushed to the bare remote -> fetching the notes ref back and
// reading the note for the landed merge SHA, proving it byte-identical.
// This also proves the read-path incantation the docs will document: an
// explicit FetchNotesRef of the configured ref, then ReadNote keyed on the
// landed RunRecord's own MergeSHA.
func TestIntegration_ReceiptPublishEndToEnd(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	h.d.cfg.ReceiptNotes = &config.ReceiptNotes{Ref: testReceiptRef, MaxBytes: 65536}
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	// envDumpPath is OUTSIDE the trial tree (a fresh t.TempDir(), not a
	// path under the materialized workspace) so it survives past the
	// trial export's own cleanup — the receipt producer dumps its full
	// subprocess environment here, letting the assertions below inspect
	// exactly what LocalExecutor handed it (credential-invisibility, F4).
	envDumpPath := filepath.Join(t.TempDir(), "receipt-env.dump")

	const payload = "deployment-receipt-sha256-deadbeefcafebabe"
	files := map[string]string{
		testCheckSpecPath: "check \"unit\" {\n    command \"/bin/sh\" \"unit.sh\"\n}\n" +
			"receipt \"deploy\" {\n    command \"/bin/sh\" \"receipt.sh\"\n}\n",
		"unit.sh": "#!/bin/sh\nexit 0\n",
		"receipt.sh": fmt.Sprintf(`#!/bin/sh
set -eu
env > %q
[ -n "$%s" ] || exit 1
[ -z "${%s+x}" ] || { echo "check result file leaked into a receipt"; exit 1; }
printf '%%s' %q > "$%s"
`, envDumpPath, core.EnvReceiptResultFile, core.EnvResultFile, payload, core.EnvReceiptResultFile),
	}
	remote.PushCandidate("main", "alice", "widget", files)

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)

	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed; Detail=%q Checks=%+v", rec.Outcome, rec.Detail, rec.Checks)
	}
	if len(rec.Checks) != 2 || rec.Checks[0].Name != "unit" || rec.Checks[1].Name != receiptNode {
		t.Fatalf("Checks = %+v, want [unit receipt:deploy]", rec.Checks)
	}
	if rec.ReceiptRef != testReceiptRef {
		t.Errorf("record ReceiptRef = %q, want %q", rec.ReceiptRef, testReceiptRef)
	}
	if rec.ReceiptPublished != receiptPublishedFresh {
		t.Errorf("record ReceiptPublished = %q, want %q", rec.ReceiptPublished, receiptPublishedFresh)
	}
	if rec.ReceiptBlob == "" {
		t.Error("record ReceiptBlob is empty, want the published note's blob SHA")
	}

	// Credential-invisibility (F4): the receipt producer's own dumped
	// environment must carry EXACTLY the receipt result-file protocol —
	// GAUNTLET_RECEIPT_RESULT_FILE present, the plain-check and image
	// result-file variables absent (LocalExecutor.RunCheck's "distinct
	// protocol, never conflated" contract for a receipt job) — and gauntlet
	// itself must never have ADDED anything beyond the documented GAUNTLET_*
	// vocabulary (core.Env*/job.ServiceEnv) to what the subprocess got.
	//
	// This harness (newIntegrationHarness -> gitx.New with no
	// WithTokenSource) never configures a credential at all, so there is no
	// real secret value here to search for byte-exact — asserting that
	// would be dishonest (nothing could ever make it fail). What's asserted
	// below instead is diffed against THIS test process's own os.Environ():
	// LocalExecutor.RunCheck deliberately passes the daemon's whole ambient
	// environment through to every check/receipt job (cmd.Env =
	// append(os.Environ(), ...) — by design, not a gap this test exists to
	// close, and this sandbox's own ambient env happens to carry real
	// GH_TOKEN/GITHUB_TOKEN/etc. vars unrelated to gauntlet, which a naive
	// "no GITHUB_/TOKEN-suffixed name anywhere" check would misfire on. So
	// this only flags a name that is BOTH new (not already in the parent's
	// own environment — i.e. something gauntlet's own code added) AND
	// outside the GAUNTLET_ vocabulary: exactly the shape a leaked
	// credential injected by gauntlet's own env-assembly would take. The
	// deeper guarantee — a real token never reaching a check/receipt
	// subprocess at all — is enforced by that same shared env-assembly path
	// (RunCheck only ever appends core.Env*/job.ServiceEnv, never a
	// credential) plus gitx's own askpass tests (auth_test.go), which prove
	// a git credential rides ONLY the git subprocess's ephemeral
	// GIT_ASKPASS environment, never anywhere a check or receipt command
	// could read it from.
	envDump, err := os.ReadFile(envDumpPath)
	if err != nil {
		t.Fatalf("read receipt env dump: %v", err)
	}
	envNames := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimRight(string(envDump), "\n"), "\n") {
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		envNames[name] = true
	}
	if !envNames[core.EnvReceiptResultFile] {
		t.Errorf("receipt env missing %s: %v", core.EnvReceiptResultFile, envDump)
	}
	if envNames[core.EnvResultFile] {
		t.Errorf("receipt env carries the plain-check result file var %s, want it absent (receipt jobs use %s instead)", core.EnvResultFile, core.EnvReceiptResultFile)
	}
	if envNames[core.EnvImageResultFile] {
		t.Errorf("receipt env carries the image-build result file var %s, want it absent", core.EnvImageResultFile)
	}
	ambientNames := make(map[string]bool)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		ambientNames[name] = true
	}
	for name := range envNames {
		if ambientNames[name] {
			continue // pass-through of this test process's own environment, not gauntlet's addition
		}
		if strings.HasPrefix(name, "GAUNTLET_") {
			continue // documented vocabulary
		}
		t.Errorf("receipt env carries %q: not in this process's own ambient environment and outside the documented GAUNTLET_* vocabulary — looks like something gauntlet's own env-assembly added beyond protocol", name)
	}

	// The read-path incantation: an explicit fetch of the notes ref into its
	// local work ref, then a read keyed on the landed merge SHA.
	ctx := context.Background()
	if _, err := h.git.FetchNotesRef(ctx, testReceiptRef); err != nil {
		t.Fatalf("FetchNotesRef: %v", err)
	}
	got, exists, err := h.git.ReadNote(ctx, gitx.NotesWorkRef(testReceiptRef), rec.MergeSHA)
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if !exists {
		t.Fatalf("no note found for the landed merge SHA %s", rec.MergeSHA)
	}
	if string(got) != payload {
		t.Fatalf("note payload = %q, want byte-identical %q", got, payload)
	}
}

// buildReceiptDaemon builds a Daemon directly against git (not through
// testHarness, whose h.d/h.exec/h.ch are fixed to ONE daemon instance for
// its lifetime) — mirrors retryintent_test.go's buildDaemonWithStore, minus
// the history.Store wiring this suite doesn't need, with a receipt-notes
// policy configured. Used to construct a SECOND, wholly independent Daemon
// over the same git state, simulating an actual process restart (a fresh
// in-memory state machine, everything re-derived from refs) rather than the
// same daemon instance simply reconciling again.
func buildReceiptDaemon(t *testing.T, git *fakeGitRepo, clock *time.Time) (*Daemon, *channel.RecordingChannel) {
	t.Helper()
	exec := executor.NewGatedExecutor()
	ch := channel.NewRecordingChannel()
	now := func() time.Time {
		*clock = clock.Add(time.Second)
		return *clock
	}
	d, err := New(git, exec, []core.Channel{ch}, Config{
		Targets:      []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec:    testCheckSpecPath,
		Committer:    testCommitter,
		ReceiptNotes: &config.ReceiptNotes{Ref: testReceiptRef, MaxBytes: 65536},
	}, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { assertAllTerminalEventsHaveRecords(t, ch.Events()) })
	return d, ch
}

// TestReceipt_RestartAfterPublishBeforeLand_Converges is issue #13's
// crash-boundary acceptance test: a candidate runs to green and publishes
// its receipt note, but the LANDING push (the target CAS right after)
// fails as stale — the exact "published, not yet landed" window a crash
// could land in. TestReceipt_StaleTargetCASAfterPublish already proves the
// re-trial half of recovery, but does so by calling h.reconcile() again on
// the SAME Daemon instance, which still holds the first run's in-memory
// state; it does not prove recovery survives losing that state entirely.
// This test's distinct value is the SECOND, independently-constructed
// Daemon over the same git state (buildReceiptDaemon) — the actual
// restart shape: a fresh process re-deriving everything from refs
// (Invariant 4), with no memory of the run buildReceiptDaemon's first
// instance drove. It must re-trial under a NEW chainTip, publish a SECOND
// note, and land — with the first (orphaned) note surviving untouched,
// never deleted or overwritten.
func TestReceipt_RestartAfterPublishBeforeLand_Converges(t *testing.T) {
	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	clock := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	d1, ch1 := buildReceiptDaemon(t, git, &clock)

	mustReconcile(t, d1)
	runID1 := currentRunIDFrom(t, ch1)
	firstChainTip := d1.headRun("main").chainTip

	// Fail the target CAS exactly once, right after the publish succeeds —
	// the same stale-lease race TestReceipt_StaleTargetCASAfterPublish
	// provokes (a human push lands in the window between landRun's publish
	// and its own target CAS).
	triggered := false
	git.beforeCAS = func(remoteRef string) {
		if remoteRef == "refs/heads/main" && !triggered {
			triggered = true
			git.directPush("main", map[string]string{"human.txt": "raced in at land time"})
		}
	}

	releaseCheck(t, d1, ch1, runID1, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	releaseCheck(t, d1, ch1, runID1, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("payload-v1")})

	if !triggered {
		t.Fatal("beforeCAS hook never fired; test didn't exercise the race it's meant to")
	}
	recs1 := ch1.Records()
	last1 := recs1[len(recs1)-1]
	if last1.Outcome != core.OutcomeSkipped {
		t.Fatalf("first daemon's Outcome = %v, want Skipped (published, then raced on the target CAS)", last1.Outcome)
	}
	if len(git.publishNoteCalls) != 1 {
		t.Fatalf("publishNoteCalls after the first daemon = %d, want 1", len(git.publishNoteCalls))
	}
	if git.publishNoteCalls[0].sha != firstChainTip {
		t.Fatalf("first publish sha = %q, want the first trial's own chainTip %q", git.publishNoteCalls[0].sha, firstChainTip)
	}
	if !git.hasRef(ref) {
		t.Fatal("candidate slot deleted despite a stale target CAS; the re-trial has nothing left to pick up")
	}
	git.beforeCAS = nil // already fired once; clear before the "restart" so it doesn't interfere again

	// Simulated crash + restart: a brand new Daemon instance over the SAME
	// git state, with no memory of d1's run at all.
	restartClock := clock
	d2, ch2 := buildReceiptDaemon(t, git, &restartClock)

	mustReconcile(t, d2)
	runID2 := currentRunIDFrom(t, ch2)
	if runID2 == runID1 {
		t.Fatal("second daemon reused the first daemon's run ID; not an independent re-trial")
	}
	run2 := d2.headRun("main")
	if run2 == nil {
		t.Fatal("second daemon has no head run after reconciling; the candidate was not picked back up")
	}
	newChainTip := run2.chainTip
	if newChainTip == firstChainTip {
		t.Fatal("restart re-derived the SAME chain tip as the raced-away first attempt, want a fresh trial")
	}
	if git.mergeTreeCalls == 0 {
		t.Error("mergeTreeCalls = 0 after restart, want at least 1 (a genuine re-trial, not a no-op)")
	}

	releaseCheck(t, d2, ch2, runID2, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	releaseCheck(t, d2, ch2, runID2, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("payload-v2")})

	recs2 := ch2.Records()
	last2 := recs2[len(recs2)-1]
	if last2.Outcome != core.OutcomeLanded {
		t.Fatalf("second daemon's Outcome = %v, want Landed", last2.Outcome)
	}
	if len(git.publishNoteCalls) != 2 {
		t.Fatalf("publishNoteCalls after the restart = %d, want 2 (both notes present)", len(git.publishNoteCalls))
	}
	second := git.publishNoteCalls[1]
	if second.sha != newChainTip {
		t.Fatalf("restart's publish sha = %q, want its own new chainTip %q", second.sha, newChainTip)
	}
	if second.sha == firstChainTip {
		t.Fatal("restart published under the SAME merge SHA as the raced-away first attempt")
	}

	// The first note — d1's successful publish, orphaned by the raced-away
	// target CAS — must survive UNTOUCHED: never deleted, never
	// overwritten by the restart's own (differently-addressed) publish.
	firstNote, exists := git.notes[[2]string{testReceiptRef, firstChainTip}]
	if !exists {
		t.Fatal("first note (the first daemon's orphaned publish) is missing after the restart; it must never be cleaned up")
	}
	if string(firstNote) != "payload-v1" {
		t.Fatalf("first note payload = %q, want untouched %q", firstNote, "payload-v1")
	}
	secondNote, exists := git.notes[[2]string{testReceiptRef, newChainTip}]
	if !exists {
		t.Fatal("second note (the restart's own publish) is missing")
	}
	if string(secondNote) != "payload-v2" {
		t.Fatalf("second note payload = %q, want %q", secondNote, "payload-v2")
	}
}

// TestReceipt_StaleTargetCASAfterPublish_HistoryRow proves the orphan case's
// provenance actually reaches durable history — not just the in-memory
// RunRecord TestReceipt_StaleTargetCASAfterPublish checks — through the
// exact same real *history.Store path buildDaemonWithStore uses elsewhere
// (retryintent_test.go): the store is wired as an ordinary core.Channel, so
// EventLanded/EventSkipped's carried Record is what writeRecord persists.
// The scenario is the identical race — a successful PublishNote immediately
// followed by a target CAS that loses to a human push — but this time
// queried back out of runs.receipt_ref/receipt_blob/receipt_published via
// Store.Run, proving the schema.sql column comment's claim (these three
// columns are NOT gated on outcome=landed) end to end.
func TestReceipt_StaleTargetCASAfterPublish_HistoryRow(t *testing.T) {
	dbPath := t.TempDir() + "/history.db"
	store, err := history.Open(dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer store.Close()

	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", receiptSpecFile("deploy"))

	exec := executor.NewGatedExecutor()
	ch := channel.NewRecordingChannel()
	clock := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	d, err := New(git, exec, []core.Channel{ch, store}, Config{
		Targets:      []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec:    testCheckSpecPath,
		Committer:    testCommitter,
		ReceiptNotes: &config.ReceiptNotes{Ref: testReceiptRef, MaxBytes: 65536},
	}, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mustReconcile(t, d)
	runID := currentRunIDFrom(t, ch)

	triggered := false
	git.beforeCAS = func(remoteRef string) {
		if remoteRef == "refs/heads/main" && !triggered {
			triggered = true
			git.directPush("main", map[string]string{"human.txt": "raced in at land time"})
		}
	}

	releaseCheck(t, d, ch, runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	releaseCheck(t, d, ch, runID, receiptNode, core.CheckResult{Name: receiptNode, Status: core.CheckPassed, Receipt: []byte("payload-v1")})

	if !triggered {
		t.Fatal("beforeCAS hook never fired; test didn't exercise the race it's meant to")
	}
	recs := ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}

	row, _, err := store.Run(runID)
	if err != nil {
		t.Fatalf("store.Run(%q): %v", runID, err)
	}
	if row.Outcome != "skipped" {
		t.Fatalf("history row outcome = %q, want %q (un-landed, per the raced-away target CAS)", row.Outcome, "skipped")
	}
	if row.ReceiptRef != testReceiptRef {
		t.Errorf("history row ReceiptRef = %q, want %q — an un-landed orphan run must still carry it", row.ReceiptRef, testReceiptRef)
	}
	if row.ReceiptBlob == "" {
		t.Error("history row ReceiptBlob is empty, want the published note's blob SHA")
	}
	if row.ReceiptPublished != receiptPublishedFresh {
		t.Errorf("history row ReceiptPublished = %q, want %q", row.ReceiptPublished, receiptPublishedFresh)
	}
}
