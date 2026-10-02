package queue

import (
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_CheckSpecFromTrialTree is §5's "Check spec from trial
// tree" row: the target's own .gauntlet.kdl declares one check, but the
// candidate's declares two — proving the daemon reads the check spec out of
// the trial tree (the candidate's own definition), never the target's.
func TestIntegration_CheckSpecFromTrialTree(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", checkSpecFile("test")) // target's own spec: one check, never read for this run
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test", "extra"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(gated, runID, "test")
	h.releaseGated(gated, runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	h.awaitStarted(gated, runID, "extra")
	h.releaseGated(gated, runID, "extra", core.CheckResult{Name: "extra", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if len(last.Checks) != 2 {
		t.Fatalf("Checks = %+v, want 2 (the candidate's own spec, not the target's)", last.Checks)
	}
	if remote.Ref(ref) != "" {
		t.Fatal("candidate slot still exists on the remote after land")
	}
}

// TestIntegration_RunRecordShape is §5's "Run record shape" row: a
// terminal RunRecord's shape — stable RunID, per-check name/status/duration,
// outcome, and a StartedAt/EndedAt ordering that makes sense — independent
// of the landing assertions in green_multi_check_land.txtar.
func TestIntegration_RunRecordShape(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))
	candSHA := remote.Ref(ref)

	before := time.Now()
	h.reconcile()
	runID := h.currentRunID()
	h.releaseGated(gated, runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: 42 * time.Millisecond})

	recs := h.ch.Records()
	rec := recs[len(recs)-1]

	if !runIDPattern.MatchString(rec.RunID) {
		t.Fatalf("RunID %q does not match the §9.4 format", rec.RunID)
	}
	if rec.Target != "main" {
		t.Errorf("Target = %q, want %q", rec.Target, "main")
	}
	if rec.Candidate.SHA != candSHA || rec.Candidate.Ref != ref {
		t.Errorf("Candidate = %+v, want SHA=%q Ref=%q", rec.Candidate, candSHA, ref)
	}
	if len(rec.Checks) != 1 || rec.Checks[0].Name != "test" || rec.Checks[0].Status != core.CheckPassed || rec.Checks[0].Duration != 42*time.Millisecond {
		t.Fatalf("Checks = %+v, want one entry {test, Passed, 42ms}", rec.Checks)
	}
	if rec.Outcome != core.OutcomeLanded {
		t.Errorf("Outcome = %v, want Landed", rec.Outcome)
	}
	if rec.StartedAt.Before(before) {
		t.Errorf("StartedAt %v predates the run even beginning (%v)", rec.StartedAt, before)
	}
	if rec.EndedAt.Before(rec.StartedAt) {
		t.Errorf("EndedAt %v before StartedAt %v", rec.EndedAt, rec.StartedAt)
	}
}
