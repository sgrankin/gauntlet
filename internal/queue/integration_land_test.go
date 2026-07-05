package queue

import (
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_GreenMultiCheckLand is §5's "Green multi-check land" row:
// both checks pass, the target advances to the tested merge commit, the
// slot is deleted — all asserted against the REMOTE's own ref state
// (testutil.Ref), not just events. It pins Invariant 6 (the candidate SHA
// appears verbatim as the merge commit's second parent) and Invariant 1
// (the landed OID is exactly the RunRecord's MergeSHA — the tested commit,
// never a re-merge) directly against real git objects.
func TestIntegration_GreenMultiCheckLand(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("lint", "test"))
	candSHA := remote.Ref(ref)

	h.reconcile() // trial clean; lint started
	runID := h.currentRunID()
	if !runIDPattern.MatchString(runID) {
		t.Fatalf("run ID %q does not match the §9.4 format", runID)
	}

	h.releaseGated(gated, runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckPassed, Duration: time.Second})
	h.releaseGated(gated, runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: 2 * time.Second})

	landedOID := remote.Ref("refs/heads/main")
	if landedOID == "" || landedOID == base {
		t.Fatalf("target ref = %q, want a new merge commit (base was %q)", landedOID, base)
	}
	if remote.Ref(ref) != "" {
		t.Fatal("candidate slot still exists on the remote after land")
	}

	parents := remote.Parents(landedOID)
	if len(parents) != 2 || parents[0] != base || parents[1] != candSHA {
		t.Fatalf("landed commit parents = %v, want [%s %s] (Invariant 6: candidate SHA verbatim as parent[1])", parents, base, candSHA)
	}

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if last.RunID != runID {
		t.Fatalf("RunRecord.RunID = %q, want %q", last.RunID, runID)
	}
	if last.MergeSHA != landedOID {
		t.Fatalf("RunRecord.MergeSHA = %q, want the landed OID %q (Invariant 1: land exactly the tested SHA)", last.MergeSHA, landedOID)
	}
	if last.BaseOID != base {
		t.Errorf("BaseOID = %q, want %q", last.BaseOID, base)
	}
	if last.Candidate.SHA != candSHA {
		t.Errorf("Candidate.SHA = %q, want %q", last.Candidate.SHA, candSHA)
	}
	if len(last.Checks) != 2 {
		t.Fatalf("Checks = %+v, want 2 entries in run order", last.Checks)
	}
	if last.Checks[0].Name != "lint" || last.Checks[0].Status != core.CheckPassed || last.Checks[0].Duration != time.Second {
		t.Errorf("Checks[0] = %+v", last.Checks[0])
	}
	if last.Checks[1].Name != "test" || last.Checks[1].Status != core.CheckPassed || last.Checks[1].Duration != 2*time.Second {
		t.Errorf("Checks[1] = %+v", last.Checks[1])
	}
}

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
// of the land-specific assertions in TestIntegration_GreenMultiCheckLand.
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
