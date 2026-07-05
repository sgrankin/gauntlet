package queue

import (
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestNewRunID_DistinctForIdenticalTimeAndOID is the direct unit test for
// docs/plans/phase23.md §2.4: the phase-1 review (C7) found that two trials
// sharing an identical trial tree, minted within the same UTC second,
// produced identical run IDs under the timestamp+OID-prefix scheme alone.
// The monotonic per-process counter folded in here must keep them distinct
// even when both time and oid are held fixed.
func TestNewRunID_DistinctForIdenticalTimeAndOID(t *testing.T) {
	fixed := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const oid = "abc123abc123abc123" // >12 chars; newRunID truncates

	id1 := newRunID(fixed, oid)
	id2 := newRunID(fixed, oid)
	if id1 == id2 {
		t.Fatalf("newRunID minted identical IDs for identical (time, oid): %q", id1)
	}
	if !runIDPattern.MatchString(id1) || !runIDPattern.MatchString(id2) {
		t.Fatalf("run IDs don't match the §9.4+§2.4 shape: %q, %q", id1, id2)
	}
}

// TestReconcile_SameSecondIdenticalTreeRetestGetsDistinctRunID is the
// harness-level regression for the same defect: with the daemon's clock
// frozen, a re-push that restores byte-identical content mints the same
// trial tree OID as the original trial, at the same second. Before the
// run-ID counter, this collided (RunID, Name) in GatedExecutor's gate map —
// which panics on a second Started-channel close for the same key — so
// reaching the end of this test without a panic is itself part of what it
// proves.
func TestReconcile_SameSecondIdenticalTreeRetestGetsDistinctRunID(t *testing.T) {
	git := newFakeGitRepo()
	exec := executor.NewGatedExecutor()
	ch := channel.NewRecordingChannel()
	frozen := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	d, err := New(git, exec, []core.Channel{ch}, Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
	}, func() time.Time { return frozen }) // clock never advances
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { assertAllTerminalEventsHaveRecords(t, ch.Events()) })

	reconcile := func() {
		t.Helper()
		if err := d.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}
	}
	currentRunID := func() string {
		t.Helper()
		evs := ch.Events()
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].RunID != "" {
				return evs[i].RunID
			}
		}
		t.Fatal("no event with a RunID found")
		return ""
	}

	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	files := checkSpecFile("test")
	git.pushCandidate(ref, "", files)

	reconcile() // first trial starts
	runID1 := currentRunID()
	exec.Release(runID1, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // rejects + parks
	for i := 0; i < 100000 && len(ch.Records()) == 0; i++ {
		reconcile()
	}
	if len(ch.Records()) == 0 {
		t.Fatal("first trial never produced a terminal record")
	}

	// Re-push identical content: same trial tree OID as before, same frozen
	// second — the exact collision scenario the counter guards against. The
	// SHA changes (a new commit object), which also clears the park via the
	// ordinary §9.1 re-push rule, so no CommandRetry is needed here.
	git.pushCandidate(ref, "", files)
	reconcile() // re-queued; new trial starts
	runID2 := currentRunID()

	if runID2 == runID1 {
		t.Fatalf("re-push with identical content at a frozen clock minted the same run ID: %q", runID1)
	}
	exec.Release(runID2, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	for i := 0; i < 100000 && len(ch.Records()) < 2; i++ {
		reconcile()
	}
	if len(ch.Records()) < 2 {
		t.Fatal("second trial never produced a terminal record")
	}
}
