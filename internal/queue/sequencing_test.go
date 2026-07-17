package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestReconcile_ShortCircuitOnFail: a failing check stops the run
// immediately — later checks never start, the target is untouched, and the
// RunRecord holds exactly the checks that ran.
func TestReconcile_ShortCircuitOnFail(t *testing.T) {
	h := newHarness(t)
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("lint", "test", "build"))

	h.reconcile() // lint starts
	runID := h.currentRunID()
	h.release(runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckFailed}) // rejects; test/build never start

	select {
	case <-h.exec.Started(runID, "test"):
		t.Fatal("test started despite lint failing")
	default:
	}
	select {
	case <-h.exec.Started(runID, "build"):
		t.Fatal("build started despite lint failing")
	default:
	}

	if got := h.git.ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a rejected run: %q, want unchanged %q", got, base)
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	// One row per DECLARED check, spec order: lint's real red verdict,
	// then test/build as blocked rows naming the root failure — never
	// absent (history must show they didn't run) and never "skipped"
	// (that's a check's own successful nothing-to-do verdict).
	if len(last.Checks) != 3 {
		t.Fatalf("Checks = %+v, want one row per declared check", last.Checks)
	}
	if last.Checks[0].Name != "lint" || last.Checks[0].Status != core.CheckFailed {
		t.Errorf("Checks[0] = %+v", last.Checks[0])
	}
	for _, blocked := range last.Checks[1:] {
		if blocked.Status != core.CheckBlocked {
			t.Errorf("check %q status = %v, want CheckBlocked", blocked.Name, blocked.Status)
		}
		if len(blocked.BlockedBy) != 1 || blocked.BlockedBy[0] != "lint" {
			t.Errorf("check %q BlockedBy = %v, want [lint]", blocked.Name, blocked.BlockedBy)
		}
		if blocked.Duration != 0 || blocked.Output != "" {
			t.Errorf("blocked check %q carries execution artifacts: %+v", blocked.Name, blocked)
		}
	}
}

// TestReconcile_SkippedCountsAsGreen: a check reporting Skipped (via the
// result file, per §5A) does not block the run — the aggregate verdict is
// still green and the run lands, with the RunRecord recording Skipped (not
// Passed) so history doesn't lie.
func TestReconcile_SkippedCountsAsGreen(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("lint", "test"))

	h.reconcile() // lint starts
	runID := h.currentRunID()
	h.release(runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckSkipped}) // recorded as Skipped; test starts
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})  // both green: lands

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed (a Skipped check should count as green)", last.Outcome)
	}
	if len(last.Checks) != 2 {
		t.Fatalf("Checks = %+v, want 2", last.Checks)
	}
	if last.Checks[0].Status != core.CheckSkipped {
		t.Errorf("Checks[0].Status = %v, want CheckSkipped (not Passed: history shouldn't lie)", last.Checks[0].Status)
	}
	if last.Checks[1].Status != core.CheckPassed {
		t.Errorf("Checks[1].Status = %v, want CheckPassed", last.Checks[1].Status)
	}
	if h.git.hasRef(ref) {
		t.Fatal("candidate slot still exists after landing")
	}
}
