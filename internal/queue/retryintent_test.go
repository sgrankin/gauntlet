// S3's end-to-end acceptance test: an operator retry, persisted durably
// (internal/history's retry_intents table, written via applyRetry's new
// EventRetryRequested emit — command.go), must survive a simulated daemon
// restart without the stale pre-retry rejection silently re-parking the
// ref. This file owns no symbol any other chunk-1 test file defines (new
// file, no overlap with daemon_test.go's newHarness or seedparks_test.go's
// newSeededDaemon) and drives a REAL history.Store (not a fake) as one of
// the Daemon's core.Channel — the actual production write path — rather
// than asserting on retry_intents rows directly.
package queue

import (
	"context"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/history"
)

// parseOutcomeForTest mirrors cmd/gauntlet/channels.go's unexported
// parseOutcome (which maps history's stored outcome string back to a
// core.Outcome for the real SeedParks closure) — duplicated here rather than
// imported, since cmd/gauntlet imports internal/queue and a reverse import
// would cycle. Kept in lockstep with history.outcomeString's vocabulary.
func parseOutcomeForTest(s string) (core.Outcome, bool) {
	switch s {
	case "landed":
		return core.OutcomeLanded, true
	case "rejected":
		return core.OutcomeRejected, true
	case "conflict":
		return core.OutcomeConflict, true
	case "skipped":
		return core.OutcomeSkipped, true
	case "error":
		return core.OutcomeError, true
	}
	return 0, false
}

// seedParksFromStore builds a queue.Config.SeedParks closure backed by a
// real history.Store — the same shape cmd/gauntlet/channels.go's
// buildSeedParks wires in production, reproduced here (not imported, see
// parseOutcomeForTest) so this test exercises the actual read query
// (history.Store.LatestTerminalPerRef, including its v6 retry-intent
// suppression) rather than a fake standing in for it.
func seedParksFromStore(t *testing.T, store *history.Store) func(target string) []ParkSeed {
	t.Helper()
	return func(target string) []ParkSeed {
		verdicts, err := store.LatestTerminalPerRef(target)
		if err != nil {
			t.Fatalf("LatestTerminalPerRef(%s): %v", target, err)
		}
		out := make([]ParkSeed, 0, len(verdicts))
		for _, v := range verdicts {
			outcome, ok := parseOutcomeForTest(v.Outcome)
			if !ok {
				continue
			}
			out = append(out, ParkSeed{Ref: v.Ref, SHA: v.SHA, Outcome: outcome, Reason: v.Detail, At: v.EndedAt})
		}
		return out
	}
}

// buildDaemonWithStore wires a Daemon exactly like daemon_test.go's
// newHarness, except its channels are [a RecordingChannel for command
// injection/introspection, store itself] — store is a REAL core.Channel
// here, so every event the daemon emits (including command.go's new
// EventRetryRequested) is durably persisted, not just recorded in memory.
func buildDaemonWithStore(t *testing.T, git *fakeGitRepo, store *history.Store, seedParks func(string) []ParkSeed, clock *time.Time) (*Daemon, *channel.RecordingChannel) {
	t.Helper()
	exec := executor.NewGatedExecutor()
	ch := channel.NewRecordingChannel()
	now := func() time.Time {
		*clock = clock.Add(time.Second)
		return *clock
	}
	d, err := New(git, exec, []core.Channel{ch, store}, Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
		SeedParks: seedParks,
	}, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, ch
}

// TestRetryIntent_SurvivesRestart_NotReParked is S3's crash-timing
// acceptance criterion, end to end: reject a candidate (a real park), issue
// an operator retry (CommandRetry -> applyRetry -> EventQueued +
// EventRetryRequested, the latter durably upserted into history's
// retry_intents table), then — WITHOUT ever letting the retried run reach a
// new terminal outcome (simulating a daemon crash in exactly that window,
// synthesis.md's S3 crash-timing bullet) — build a brand new Daemon against
// the same git state and a SeedParks closure backed by the SAME real Store
// (simulating a restart that re-reads history from disk). The ref must NOT
// be re-parked: the stale pre-retry rejection's ended_at predates the
// retry_intents row, so LatestTerminalPerRef's v6 join omits it entirely,
// and the fresh daemon proceeds to trial-merge it like any other new
// candidate.
func TestRetryIntent_SurvivesRestart_NotReParked(t *testing.T) {
	dbPath := t.TempDir() + "/history.db"
	store, err := history.Open(dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer store.Close()

	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("test"))

	clock := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	d1, ch1 := buildDaemonWithStore(t, git, store, nil, &clock)

	// Reconcile until the check starts, then reject it — a genuine park,
	// durably written to `runs` via store's real Emit (writeRecord).
	mustReconcile(t, d1)
	runID := currentRunIDFrom(t, ch1)
	releaseCheck(t, d1, ch1, runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed, Output: "red"})

	if entry, ok := d1.done["main"][ref]; !ok || entry.Outcome != core.OutcomeRejected {
		t.Fatalf("d1.done[main][%s] = %+v (ok=%v), want a Rejected park before retrying", ref, entry, ok)
	}

	// Operator retries: applyRetry clears the in-memory park and durably
	// records the retry intent (command.go's new EventRetryRequested emit,
	// persisted by history.Store.writeRetryIntent).
	ch1.SendCommand(core.Command{Kind: core.CommandRetry, Target: "main", Ref: ref})
	mustReconcile(t, d1)

	if _, ok := d1.done["main"][ref]; ok {
		t.Fatalf("d1.done[main][%s] still parked after retry", ref)
	}

	// Simulated crash: stop here, before the retried run reaches any new
	// terminal outcome. mergeTreeCalls tells us whether d1 even picked the
	// ref back up this tick — either way is fine for this test; what
	// matters is that history's runs table for this ref still only holds
	// the ORIGINAL (now-stale) rejection, with an ended_at older than the
	// retry_intents row just written.
	verdictsBeforeRestart, err := store.LatestTerminalPerRef("main")
	if err != nil {
		t.Fatalf("LatestTerminalPerRef (pre-restart sanity check): %v", err)
	}
	for _, v := range verdictsBeforeRestart {
		if v.Ref == ref {
			t.Fatalf("LatestTerminalPerRef already omits %s before any restart happened: %+v (sanity check failed — retry_intents join must only suppress AFTER a restart re-consults it, not change runs/checks themselves)", ref, v)
		}
	}

	// Restart: a brand new Daemon, same git state (the crash never let the
	// candidate ref move or get picked to a new terminal), SeedParks wired
	// against the SAME on-disk Store.
	restartClock := clock
	d2, _ := buildDaemonWithStore(t, git, store, seedParksFromStore(t, store), &restartClock)

	mustReconcile(t, d2)

	if entry, ok := d2.done["main"][ref]; ok {
		t.Fatalf("d2.done[main][%s] = %+v, want NOT parked (the retry must suppress the stale pre-retry rejection across restart)", ref, entry)
	}
	if git.mergeTreeCalls == 0 {
		t.Error("mergeTreeCalls = 0 after restart, want at least 1 (the ref must be picked up for a fresh trial, not left parked)")
	}
}

// mustReconcile runs one ReconcileOnce pass against d, failing t on error.
func mustReconcile(t *testing.T, d *Daemon) {
	t.Helper()
	if err := d.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
}

// currentRunIDFrom returns the RunID of the most recently emitted event
// carrying one, read off ch — the same lookup daemon_test.go's testHarness
// exposes as currentRunID, reimplemented here since this file builds its
// Daemon directly rather than through testHarness.
func currentRunIDFrom(t *testing.T, ch *channel.RecordingChannel) string {
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

// releaseCheck delivers result to d's GatedExecutor for (runID, name) and
// spins ReconcileOnce (yielding the scheduler, never sleeping) until that
// check's own terminal signal is observed on ch — the same synchronization
// idiom as daemon_test.go's testHarness.release, reimplemented here since
// this file doesn't use testHarness.
func releaseCheck(t *testing.T, d *Daemon, ch *channel.RecordingChannel, runID, name string, result core.CheckResult) {
	t.Helper()
	exec, ok := d.exec.(*executor.GatedExecutor)
	if !ok {
		t.Fatalf("releaseCheck: Daemon's executor is %T, want *executor.GatedExecutor", d.exec)
	}
	before := len(ch.Events())
	exec.Release(runID, name, result)
	for i := 0; i < 100000; i++ {
		mustReconcile(t, d)
		if checkFinishedObserved(ch.Events()[before:], runID, name) {
			return
		}
	}
	t.Fatalf("no terminal signal for (%s,%s) after releasing", runID, name)
}
