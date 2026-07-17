package queue

import (
	"errors"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// The daemon-side infra-failure paths in startRun share one policy
// (reconcile.go): OutcomeError + park + EventError, uniformly. These tests
// pin that for each injectable failure point and prove the park prevents a
// retry-every-tick spin even after the failure clears.

func TestReconcile_MergeTreeError(t *testing.T) {
	h := newHarness(t)
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.git.mergeTreeErr = errors.New("merge-tree: transport wedged")

	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}
	if got := h.git.ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on an infra error: %q, want unchanged %q", got, base)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("slot removed on an infra error")
	}

	// The park must hold even once the failure clears: no spin, re-test
	// only on re-push (§9.1/§9.2).
	h.git.mergeTreeErr = nil
	calls := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != calls {
		t.Fatal("parked candidate re-tested after the infra error cleared")
	}

	h.git.pushCandidate(ref, "", checkSpecFile("test")) // re-push: new SHA clears the park
	h.reconcile()
	if h.git.mergeTreeCalls != calls+1 {
		t.Fatal("re-pushed candidate not re-tested; the park outlived the SHA change")
	}
}

func TestReconcile_IsAncestorError(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.git.isAncestorErr = errors.New("is-ancestor: object store unreadable")

	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}
	if h.git.mergeTreeCalls != 0 {
		t.Fatal("MergeTree reached despite the IsAncestor failure")
	}
	if !h.git.hasRef(ref) {
		t.Fatal("slot removed on an infra error")
	}
}

func TestReconcile_CommitTreeError(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.git.commitTreeErr = errors.New("commit-tree: disk full")

	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}

	h.git.commitTreeErr = nil
	calls := h.git.commitTreeCalls
	h.reconcile()
	if h.git.commitTreeCalls != calls {
		t.Fatal("parked candidate re-tested after the infra error cleared")
	}
}

func TestReconcile_ExportTreeError(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.git.exportErr = errors.New("export: no space left on device")

	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}
	// This failure happens after the merge commit exists, so the record
	// carries the full trial context.
	if last.MergeSHA == "" || last.BaseOID == "" {
		t.Errorf("RunRecord missing trial context: MergeSHA=%q BaseOID=%q", last.MergeSHA, last.BaseOID)
	}

	h.git.exportErr = nil
	calls := h.git.exportCalls
	h.reconcile()
	if h.git.exportCalls != calls {
		t.Fatal("parked candidate re-exported after the infra error cleared")
	}
}

func TestReconcile_RestoreMtimesError(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.HistoryMtimes = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.git.mtimeErr = errors.New("gitx: restore-mtimes: history exhausted")

	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error", last.Outcome)
	}
	// The tree must never silently run checks with wall-clock metadata
	// while the config promises history-derived mtimes, and the record
	// must say which pass failed.
	if !strings.Contains(last.Detail, "restore mtimes") {
		t.Fatalf("Detail = %q, want it to name the mtimes pass", last.Detail)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("slot removed on an infra error")
	}

	h.git.mtimeErr = nil
	calls := h.git.mtimeCalls
	h.reconcile()
	if h.git.mtimeCalls != calls {
		t.Fatal("parked candidate re-tested after the infra error cleared")
	}
}

// The mtimes pass is strictly opt-in: with Config.HistoryMtimes unset (the
// default, as in newHarness), a run's export must not touch RestoreMtimes
// at all.
func TestReconcile_MtimesOffNeverCalled(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	h.git.pushCandidate(candidateRef("main", "alice", "widget"), "", checkSpecFile("test"))

	h.reconcile()

	if h.git.exportCalls == 0 {
		t.Fatal("run never exported; the test exercised nothing")
	}
	if h.git.mtimeCalls != 0 {
		t.Fatalf("RestoreMtimes called %d time(s) with HistoryMtimes off", h.git.mtimeCalls)
	}
}
