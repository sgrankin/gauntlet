package queue

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_SkippedCheck is §5's "Skipped check" row: a check that
// writes "skipped" to $GAUNTLET_RESULT_FILE and exits 0 must be recorded as
// CheckSkipped (not CheckPassed, so run history doesn't lie), and the run
// still lands overall. Runs a real LocalExecutor subprocess against the
// exported trial tree — the executor contract end-to-end, not a
// GatedExecutor stand-in.
func TestIntegration_SkippedCheck(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	files := map[string]string{
		testCheckSpecPath: "check \"skip\" {\n    command \"/bin/sh\" \"skip.sh\"\n}\n" +
			"check \"pass\" {\n    command \"/bin/sh\" \"pass.sh\"\n}\n",
		"skip.sh": fmt.Sprintf("#!/bin/sh\nprintf skipped > \"$%s\"\nexit 0\n", core.EnvResultFile),
		"pass.sh": "#!/bin/sh\nexit 0\n",
	}
	ref := remote.PushCandidate("main", "alice", "widget", files)

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)

	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed; Detail=%q", rec.Outcome, rec.Detail)
	}
	if len(rec.Checks) != 2 {
		t.Fatalf("Checks = %+v, want 2", rec.Checks)
	}
	if rec.Checks[0].Name != "skip" || rec.Checks[0].Status != core.CheckSkipped {
		t.Errorf("Checks[0] = %+v, want {skip, CheckSkipped}", rec.Checks[0])
	}
	if rec.Checks[1].Name != "pass" || rec.Checks[1].Status != core.CheckPassed {
		t.Errorf("Checks[1] = %+v, want {pass, CheckPassed}", rec.Checks[1])
	}
	if remote.Ref(ref) != "" {
		t.Fatal("candidate slot still exists on the remote after land (Skipped must still count as green)")
	}
	if got := remote.Ref("refs/heads/main"); got != rec.MergeSHA {
		t.Fatalf("target ref = %q, want the tested merge %q", got, rec.MergeSHA)
	}
}

// TestIntegration_CheckEnvExported is §5's "Check env exported" row: the
// four SHA/REF env vars LocalExecutor exports must match the daemon's own
// bookkeeping. BaseSHA and Ref are known before the candidate is even
// pushed (base is whatever "main" already is; Ref is a name we choose), so
// the check script asserts those directly and fails the check if either is
// wrong. CandidateSHA and MergeSHA can't be baked into the very script
// whose content determines those hashes (the same hash-circularity
// docs/plans/phase1.md §9.4 hit for run IDs), so the script only reports
// them and the test compares against the RunRecord's own authoritative
// fields — still a real assertion, just performed in Go instead of shell.
func TestIntegration_CheckEnvExported(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")

	// The candidate ref name follows a fixed grammar from (target, user,
	// topic) alone (§9.3) — independent of the pushed content — so it's
	// known before the push, unlike CandidateSHA/MergeSHA below.
	ref := candidateRef("main", "alice", "widget")
	script := fmt.Sprintf(`#!/bin/sh
test "$%s" = %q || { echo "bad base sha: $%s"; exit 1; }
test "$%s" = %q || { echo "bad ref: $%s"; exit 1; }
echo "MERGE=$%s"
echo "CAND=$%s"
exit 0
`,
		core.EnvBaseSHA, base, core.EnvBaseSHA,
		core.EnvRef, ref, core.EnvRef,
		core.EnvMergeSHA, core.EnvCandidateSHA,
	)
	files := shellCheckSpec("envcheck", script)
	gotRef := remote.PushCandidate("main", "alice", "widget", files)
	if gotRef != ref {
		t.Fatalf("PushCandidate ref = %q, want %q", gotRef, ref)
	}
	candSHA := remote.Ref(ref)

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)

	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed; Detail=%q Checks=%+v", rec.Outcome, rec.Detail, rec.Checks)
	}
	if len(rec.Checks) != 1 {
		t.Fatalf("Checks = %+v, want 1", rec.Checks)
	}
	out := rec.Checks[0].Output
	if !strings.Contains(out, "MERGE="+rec.MergeSHA) {
		t.Errorf("output %q missing MERGE=%s (GAUNTLET_MERGE_SHA must equal the RunRecord's MergeSHA)", out, rec.MergeSHA)
	}
	if !strings.Contains(out, "CAND="+candSHA) {
		t.Errorf("output %q missing CAND=%s (GAUNTLET_CANDIDATE_SHA must equal the candidate's own commit)", out, candSHA)
	}
	if got := remote.Ref("refs/heads/main"); got != rec.MergeSHA {
		t.Fatalf("target ref = %q, want the tested merge %q", got, rec.MergeSHA)
	}
}

// TestIntegration_ExecStartFailureIsVerdict is §5's "Exec-start failure is a
// verdict" row: a check command that doesn't exist must be a CheckFailed
// verdict (never Err), reject the run, park the (ref, SHA), and never
// spin-retry — §9.2's ruling, exercised against LocalExecutor's real
// exec-start failure (a GatedExecutor never execs anything, so it can't
// stand in for this).
func TestIntegration_ExecStartFailureIsVerdict(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")
	files := map[string]string{
		testCheckSpecPath: "check \"missing\" {\n    command \"/no/such/gauntlet-test-binary-xyz\"\n}\n",
	}
	ref := remote.PushCandidate("main", "alice", "widget", files)

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)

	if rec.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected; Detail=%q", rec.Outcome, rec.Detail)
	}
	if len(rec.Checks) != 1 {
		t.Fatalf("Checks = %+v, want 1", rec.Checks)
	}
	if rec.Checks[0].Err != nil {
		t.Fatalf("Checks[0].Err = %v, want nil (exec-start failure is a verdict, not Err)", rec.Checks[0].Err)
	}
	if rec.Checks[0].Status != core.CheckFailed {
		t.Fatalf("Checks[0].Status = %v, want CheckFailed", rec.Checks[0].Status)
	}
	if got := remote.Ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a rejected run: %q, want unchanged %q", got, base)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot removed for a rejected run; should be parked, not deleted")
	}

	// No retry loop (§9.2): further ticks must not start a new trial (which
	// would itself produce at least one new event before it could finish).
	evBefore := len(h.ch.Events())
	h.reconcile()
	h.reconcile()
	if len(h.ch.Events()) != evBefore {
		t.Fatal("parked candidate re-tested on subsequent ticks (spin)")
	}
}

// TestIntegration_ShortCircuitOnFail is §5's "Short-circuit on fail" row: a
// failing check stops the run immediately against real git — later checks
// never start, the target is untouched, and the RunRecord holds exactly the
// checks that ran.
func TestIntegration_ShortCircuitOnFail(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "widget", checkSpecFile("lint", "test", "build"))

	h.reconcile() // lint starts
	runID := h.currentRunID()
	h.releaseGated(gated, runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckFailed}) // rejects; test/build never start

	select {
	case <-gated.Started(runID, "test"):
		t.Fatal("test started despite lint failing")
	default:
	}
	select {
	case <-gated.Started(runID, "build"):
		t.Fatal("build started despite lint failing")
	default:
	}

	if got := remote.Ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a rejected run: %q, want unchanged %q", got, base)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot removed for a rejected run; should be parked, not deleted")
	}
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if len(last.Checks) != 1 {
		t.Fatalf("Checks = %+v, want exactly 1 (short-circuit before test/build)", last.Checks)
	}
	if last.Checks[0].Name != "lint" || last.Checks[0].Status != core.CheckFailed {
		t.Errorf("Checks[0] = %+v", last.Checks[0])
	}
}
