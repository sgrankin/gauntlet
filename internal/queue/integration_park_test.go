package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_MissingCheckSpec is §5's "Missing/invalid check spec" row
// (missing half): a candidate with no .gauntlet.kdl at all in its trial
// tree is Rejected (ReadFileFromTree genuinely fails against real git — no
// file exists to cat-file), names the file, and leaves target and slot
// untouched; parked until re-push.
func TestIntegration_MissingCheckSpec(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	base := remote.Ref("refs/heads/main")
	// A candidate needs at least one differing file to be a genuine new
	// commit (PushCandidate builds on top of the target's current tip); the
	// point of this row is simply that none of them is the check spec.
	ref := remote.PushCandidate("main", "alice", "widget", map[string]string{"feature.txt": "no check spec here\n"})

	before := len(h.ch.Records())
	h.reconcile()

	recs := h.ch.Records()
	if len(recs) != before+1 {
		t.Fatalf("Records = %d, want %d", len(recs), before+1)
	}
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if last.Detail == "" {
		t.Error("Detail should name the missing check-spec path")
	}
	if got := remote.Ref("refs/heads/main"); got != base {
		t.Fatalf("target moved on a missing check spec: %q, want unchanged %q", got, base)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot removed for a missing check spec; should be parked, not deleted")
	}

	evBefore := len(h.ch.Events())
	h.reconcile()
	if len(h.ch.Events()) != evBefore {
		t.Fatal("parked missing-spec candidate re-tested on the next tick")
	}
}

// TestIntegration_InvalidCheckSpec is §5's "Missing/invalid check spec" row
// (invalid half): a .gauntlet.kdl that parses but declares no checks fails
// config.ParseChecks's own validation and is Rejected the same way.
func TestIntegration_InvalidCheckSpec(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	ref := remote.PushCandidate("main", "alice", "widget", map[string]string{testCheckSpecPath: ""}) // parses, but "no checks defined"

	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot removed for an invalid check spec; should be parked, not deleted")
	}
}

// TestIntegration_FailureParksPerSHA is §5's "Failure parks per SHA" row: a
// rejected (ref, SHA) stays parked across further ticks and even across an
// unrelated candidate landing on the same target (§9.1's sticky-park rule,
// a fortiori across candidates) — but a re-push (new SHA) clears the park
// and re-enters the queue.
func TestIntegration_FailureParksPerSHA(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	refA := remote.PushCandidate("main", "alice", "first", distinctFiles(checkSpecFile("test")))
	h.reconcile() // A's trial starts
	runIDA := h.currentRunID()
	h.releaseGated(gated, runIDA, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // A rejected + parked

	refB := remote.PushCandidate("main", "bob", "second", distinctFiles(checkSpecFile("test")))
	h.reconcile() // A is parked and skipped; B's trial starts
	runIDB := h.currentRunID()
	if runIDB == runIDA {
		t.Fatal("B did not get a new run; A's park did not free the queue")
	}
	h.releaseGated(gated, runIDB, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // B lands

	if remote.Ref(refA) == "" {
		t.Fatal("A's slot was removed; it should only be parked, not landed")
	}
	if remote.Ref(refB) != "" {
		t.Fatal("B's slot should be deleted after landing")
	}

	recsBefore := len(h.ch.Records())
	h.reconcile()
	h.reconcile()
	if len(h.ch.Records()) != recsBefore {
		t.Fatal("A was re-tested after an unrelated landing; parks must be sticky per (ref, SHA) until re-push (§9.1)")
	}

	// Re-push clears the park.
	remote.MoveCandidate(refA, distinctFiles(checkSpecFile("test")))
	h.reconcile()
	runIDA2 := h.currentRunID()
	if runIDA2 == runIDA {
		t.Fatal("re-pushed A not re-tested; the park outlived the SHA change")
	}
	h.releaseGated(gated, runIDA2, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if remote.Ref(refA) != "" {
		t.Fatal("A's slot survived after landing on its re-push")
	}
	if got := remote.Ref("refs/heads/main"); got == "" {
		t.Fatal("target never advanced for A's re-push landing")
	}
}

// TestIntegration_MergeConflict is §5's "Merge conflict" row: a real
// trial-merge conflict (the candidate and a direct push to the target edit
// the same line differently, mirroring gitx_test.go's own
// TestMergeTreeConflict setup at the queue level) leaves target and slot
// untouched and parks the candidate until it's re-pushed.
func TestIntegration_MergeConflict(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	remote := h.remote
	remote.Seed("main", map[string]string{"f.txt": "line1\n"})

	files := checkSpecFile("test")
	files["f.txt"] = "line1\nalice\n"
	ref := remote.PushCandidate("main", "alice", "widget", files)
	remote.DirectPush("main", map[string]string{"f.txt": "line1\nbob\n"})
	tipAfterDirectPush := remote.Ref("refs/heads/main")

	before := len(h.ch.Records())
	h.reconcile()

	recs := h.ch.Records()
	if len(recs) != before+1 {
		t.Fatalf("Records = %d, want %d", len(recs), before+1)
	}
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeConflict {
		t.Fatalf("Outcome = %v, want Conflict", last.Outcome)
	}
	if got := remote.Ref("refs/heads/main"); got != tipAfterDirectPush {
		t.Fatalf("target moved on a conflict: %q, want unchanged %q", got, tipAfterDirectPush)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot removed on a conflict; should be parked, not deleted")
	}

	evBefore := len(h.ch.Events())
	h.reconcile()
	if len(h.ch.Events()) != evBefore {
		t.Fatal("conflicted candidate re-tested on the next tick (spin)")
	}
}
