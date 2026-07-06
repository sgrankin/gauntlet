// Phase-B auto-retry-once suite (DESIGN.md decision ledger, "Auto-retry
// once on infra-error parks"; docs/plans/scale.md §5): an OutcomeError park
// — the CheckResult.Err path (executor unreachable, service-ensure
// failure, a service dying mid-run; services.md §7) — is auto-requeued
// exactly once per (ref, SHA) when Config.AutoRetryErrors is set, using the
// exact same clear+emit machinery command_test.go's TestCommand_Retry*
// suite exercises for an operator's CommandRetry. These tests set
// h.d.cfg.AutoRetryErrors directly (package-internal — no exported harness
// knob exists, since newHarness's zero-value Config leaves it off, matching
// every other queue test's expectations, e.g. park_test.go's
// TestReconcile_ExecErrParksNoSpin).
package queue

import (
	"errors"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestAutoRetry_ErrorParkRequeuesOnce covers requirement 1: an OutcomeError
// park auto-requeues once — the ref re-enters waiting with no operator
// action (no CommandRetry sent) — and EventRetryRequested carries the
// automatic Detail, distinguishing it from an operator's own retry.
func TestAutoRetry_ErrorParkRequeuesOnce(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.AutoRetryErrors = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts
	runID := h.currentRunID()
	mergeTreeCallsBefore := h.git.mergeTreeCalls
	h.release(runID, "test", core.CheckResult{Name: "test", Err: errors.New("executor unreachable")})

	var sawAutoRetry bool
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventRetryRequested && e.Candidate.Ref == ref && e.Detail == autoRetryDetail {
			sawAutoRetry = true
		}
	}
	if !sawAutoRetry {
		t.Fatal("no automatic EventRetryRequested (with the auto-retry Detail) after an OutcomeError park")
	}

	// The park clears synchronously (maybeAutoRetry runs right after
	// finishRun's own emit, within the same ReconcileOnce call that
	// processed the error), but advanceLane's "return true" on a
	// park/finish defers refill to the NEXT tick's fresh Fetch/ListRefs —
	// same as any other real verdict, human retry or not. One more
	// reconcile is what actually starts the re-test.
	h.reconcile()
	if h.git.mergeTreeCalls != mergeTreeCallsBefore+1 {
		t.Fatalf("mergeTreeCalls = %d, want %d (auto-retried trial)", h.git.mergeTreeCalls, mergeTreeCallsBefore+1)
	}
	newRunID := h.currentRunID()
	if newRunID == runID {
		t.Fatal("no new run started for the auto-retried candidate")
	}

	h.release(newRunID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land; drain
}

// TestAutoRetry_SecondErrorStaysParked covers requirement 2: a second
// OutcomeError for the very same (ref, SHA) — i.e. the auto-retried run
// errors again too — is never auto-retried a second time; it stays parked
// until a human (CommandRetry) clears it.
func TestAutoRetry_SecondErrorStaysParked(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.AutoRetryErrors = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Err: errors.New("executor unreachable")})

	h.reconcile() // auto-retry's own refill
	retryRunID := h.currentRunID()
	if retryRunID == runID {
		t.Fatal("auto-retry did not start a new run")
	}
	h.release(retryRunID, "test", core.CheckResult{Name: "test", Err: errors.New("still unreachable")})

	mergeTreeCallsBefore := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != mergeTreeCallsBefore {
		t.Fatal("candidate was auto-retried a second time for the same SHA; must stay parked for a human")
	}

	var retryEvents int
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventRetryRequested && e.Candidate.Ref == ref {
			retryEvents++
		}
	}
	if retryEvents != 1 {
		t.Fatalf("EventRetryRequested count = %d, want exactly 1 (auto-retried once, never twice)", retryEvents)
	}

	// An operator's explicit retry still works normally once the budget is
	// spent — the auto-retry-once guard only bounds the AUTOMATIC path.
	h.ch.SendCommand(core.Command{Kind: core.CommandRetry, Target: "main", Ref: ref})
	h.reconcile()
	if h.git.mergeTreeCalls != mergeTreeCallsBefore+1 {
		t.Fatal("operator retry after the auto-retry budget was spent did not re-test")
	}
	newRunID := h.currentRunID()
	h.release(newRunID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land; drain
}

// TestAutoRetry_NewSHAGetsFreshBudget covers requirement 3: once a ref's
// auto-retry budget is spent (and the retried run also errored, parking
// for a human per the previous test), a brand-new SHA pushed to the same
// ref — a materially different candidate — gets its own, fresh auto-retry
// budget rather than inheriting the old SHA's spent one.
func TestAutoRetry_NewSHAGetsFreshBudget(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.AutoRetryErrors = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Err: errors.New("executor unreachable")})
	h.reconcile() // auto-retry's own refill
	retryRunID := h.currentRunID()
	h.release(retryRunID, "test", core.CheckResult{Name: "test", Err: errors.New("still unreachable")})
	// budget spent for this SHA; stays parked (TestAutoRetry_SecondErrorStaysParked)

	h.git.pushCandidate(ref, "", checkSpecFile("test")) // re-push: a brand-new SHA
	h.reconcile()
	newSHARunID := h.currentRunID()
	if newSHARunID == retryRunID {
		t.Fatal("re-pushed SHA did not get a fresh trial; the old SHA's park outlived the re-push")
	}
	h.release(newSHARunID, "test", core.CheckResult{Name: "test", Err: errors.New("executor unreachable again")})

	var retryCount int
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventRetryRequested && e.Candidate.Ref == ref {
			retryCount++
		}
	}
	if retryCount != 2 {
		t.Fatalf("EventRetryRequested count = %d, want 2 (one per SHA, the new SHA got its own budget)", retryCount)
	}

	h.reconcile() // the new SHA's own auto-retry refill
	finalRunID := h.currentRunID()
	if finalRunID == newSHARunID {
		t.Fatal("the new SHA's own auto-retry did not start a new run")
	}
	h.release(finalRunID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // land; drain
}

// TestAutoRetry_RejectedNeverAutoRetried covers requirement 4: a red
// verdict (OutcomeRejected, a CheckFailed status — an author problem, not
// infra's) is never auto-retried, regardless of Config.AutoRetryErrors.
func TestAutoRetry_RejectedNeverAutoRetried(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.AutoRetryErrors = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // rejects + parks

	mergeTreeCallsBefore := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != mergeTreeCallsBefore {
		t.Fatal("a red verdict (OutcomeRejected) was auto-retried; must stay parked for a human")
	}
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventRetryRequested {
			t.Fatal("EventRetryRequested emitted for an OutcomeRejected park")
		}
	}
}

// TestAutoRetry_DisabledByConfig covers requirement 5: the knob off means
// no auto-retry at all. newHarness's zero-value Config leaves
// AutoRetryErrors false — this test deliberately does NOT set it, both to
// cover the disable path and to pin queue's own policy-free default (see
// Config.AutoRetryErrors's doc): every other queue test (park_test.go's
// TestReconcile_ExecErrParksNoSpin included) already relies on this.
func TestAutoRetry_DisabledByConfig(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Err: errors.New("executor unreachable")})

	mergeTreeCallsBefore := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != mergeTreeCallsBefore {
		t.Fatal("OutcomeError auto-retried despite AutoRetryErrors being off (the default)")
	}
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventRetryRequested {
			t.Fatal("EventRetryRequested emitted despite AutoRetryErrors being off")
		}
	}
}
