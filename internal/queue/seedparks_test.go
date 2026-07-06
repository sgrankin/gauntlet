// Park-persistence-across-restarts suite (Feature 2, Config.SeedParks):
// a fresh Daemon built with SeedParks plays the role a just-restarted
// process would — this file never actually restarts anything (that's
// cmd/gauntlet's job, wiring history.Store.LatestTerminalPerRef into the
// closure), it only proves the queue-side contract: what SeedParks reports
// before the very first reconcile pass shapes that pass's own park list,
// filtered exactly as documented (red-family only, current-SHA only).
package queue

import (
	"context"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// newSeededDaemon builds a single-target ("main") Daemon exactly like
// daemon_test.go's newHarness, but wired with seedParks — the one Config
// field newHarness itself doesn't expose.
func newSeededDaemon(t *testing.T, git *fakeGitRepo, seedParks func(target string) []ParkSeed) (*Daemon, *channel.RecordingChannel) {
	t.Helper()
	exec := executor.NewGatedExecutor()
	ch := channel.NewRecordingChannel()
	d, err := New(git, exec, []core.Channel{ch}, Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
		SeedParks: seedParks,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { assertAllTerminalEventsHaveRecords(t, ch.Events()) })
	return d, ch
}

// TestSeedParks_ParkedRefNotRePicked is Feature 2's core boot-recovery
// proof: a Daemon constructed with a SeedParks reporting a ref's latest
// verdict as Rejected must park that (ref, SHA) before its very FIRST pick
// — the ref is never trial-merged at all on this "restart", exactly as if
// the same long-running process had rejected it moments ago. A retry still
// clears a seeded park exactly like a real one (seeding is efficiency
// state, not a separate, stickier kind of park).
func TestSeedParks_ParkedRefNotRePicked(t *testing.T) {
	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := git.pushCandidate(ref, "", checkSpecFile("test"))

	seedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	const seedRunID = "run-pre-restart-1"
	seedParks := func(target string) []ParkSeed {
		if target != "main" {
			return nil
		}
		return []ParkSeed{{Ref: ref, SHA: sha, Outcome: core.OutcomeRejected, Reason: "prior red (pre-restart)", At: seedAt, RunID: seedRunID}}
	}
	d, ch := newSeededDaemon(t, git, seedParks)

	if err := d.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	if git.mergeTreeCalls != 0 {
		t.Fatalf("mergeTreeCalls = %d, want 0 (the seeded park must prevent any pick before the first reconcile)", git.mergeTreeCalls)
	}
	if d.headRun("main") != nil {
		t.Fatal("a run started for a ref the boot seed parked")
	}
	entry, ok := d.done["main"][ref]
	if !ok || entry.SHA != sha || entry.Outcome != core.OutcomeRejected || entry.RunID != seedRunID {
		t.Fatalf("d.done[main][%s] = %+v (ok=%v), want a Rejected park at %s with RunID %q", ref, entry, ok, sha, seedRunID)
	}

	snap := d.Snapshot()
	if snap == nil {
		t.Fatal("no snapshot published")
	}
	var found bool
	for _, ts := range snap.Targets {
		for _, p := range ts.Parked {
			if p.Candidate.Ref == ref {
				found = true
				if p.RunID != seedRunID {
					t.Errorf("Snapshot's ParkedEntry.RunID = %q, want %q (the seeded RunID)", p.RunID, seedRunID)
				}
			}
		}
	}
	if !found {
		t.Fatal("the seeded park does not appear in the published Snapshot's Parked list")
	}

	// A retry clears a seeded park exactly like a real one, AND proves
	// seeding only ever happens once: if seedParksOnce re-consulted
	// SeedParks on a later tick, this same (static) closure would just
	// re-park it and mergeTreeCalls would never move off 0.
	ch.SendCommand(core.Command{Kind: core.CommandRetry, Target: "main", Ref: ref})
	if err := d.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if git.mergeTreeCalls != 1 {
		t.Fatalf("mergeTreeCalls after retry = %d, want 1 (the seeded park clears like any other, and is never re-seeded)", git.mergeTreeCalls)
	}
}

// TestSeedParks_ParkedRefAtBootEmitsNoEventQueued is S11's regression case:
// a ref seeded straight into a park at boot (same test as
// TestSeedParks_ParkedRefNotRePicked) must not also emit a cosmetic
// EventQueued on the very first reconcile pass that sees it — it was never
// actually queued, only recognized as already-parked. syncBookkeeping gates
// the emission on exactly the "parked at this SHA" test pickHead itself
// uses; the sequence number is still assigned (proven by the retry-then-
// mergeTreeCalls assertion in TestSeedParks_ParkedRefNotRePicked already).
func TestSeedParks_ParkedRefAtBootEmitsNoEventQueued(t *testing.T) {
	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := git.pushCandidate(ref, "", checkSpecFile("test"))

	seedParks := func(target string) []ParkSeed {
		if target != "main" {
			return nil
		}
		return []ParkSeed{{Ref: ref, SHA: sha, Outcome: core.OutcomeRejected, Reason: "prior red (pre-restart)", At: time.Now()}}
	}
	d, ch := newSeededDaemon(t, git, seedParks)

	if err := d.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	for _, ev := range ch.Events() {
		if ev.Kind == core.EventQueued {
			t.Fatalf("got EventQueued for a ref seeded parked at boot: %+v", ev)
		}
	}
}

// TestSeedParks_RePushedRefStillEmitsEventQueued is
// TestSeedParks_ParkedRefAtBootEmitsNoEventQueued's counterpart: once the
// ref is re-pushed to a new SHA (the seeded park no longer applies —
// TestSeedParks_MovedSHASeedIsDropped's scenario), it is a genuine new
// arrival and must still emit EventQueued exactly as an unseeded ref would.
func TestSeedParks_RePushedRefStillEmitsEventQueued(t *testing.T) {
	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("test")) // the CURRENT (re-pushed) SHA

	const staleSHA = "0000000000000000000000000000000000000f" // no longer the ref's SHA
	seedParks := func(target string) []ParkSeed {
		return []ParkSeed{{Ref: ref, SHA: staleSHA, Outcome: core.OutcomeRejected, Reason: "stale", At: time.Now()}}
	}
	d, ch := newSeededDaemon(t, git, seedParks)

	if err := d.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	var found bool
	for _, ev := range ch.Events() {
		if ev.Kind == core.EventQueued && ev.Candidate.Ref == ref {
			found = true
		}
	}
	if !found {
		t.Fatal("no EventQueued for a re-pushed ref whose seeded park is stale (moved SHA)")
	}
}

// TestSeedParks_MovedSHASeedIsDropped covers the resurrection-adjacent edge
// explicitly: a seed naming a SHA that no longer matches the candidate
// ref's CURRENT SHA (a re-push since the seed's own verdict) must never
// park the current SHA — syncBookkeeping's ordinary SHA-currency check,
// which seedParksOnce deliberately does nothing to bypass, drops it on the
// very same first pass it was seeded on.
func TestSeedParks_MovedSHASeedIsDropped(t *testing.T) {
	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("test")) // the CURRENT SHA

	const staleSHA = "0000000000000000000000000000000000000f" // a SHA the ref no longer has
	seedParks := func(target string) []ParkSeed {
		return []ParkSeed{{Ref: ref, SHA: staleSHA, Outcome: core.OutcomeRejected, Reason: "stale", At: time.Now()}}
	}
	d, _ := newSeededDaemon(t, git, seedParks)

	if err := d.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	if git.mergeTreeCalls != 1 {
		t.Fatalf("mergeTreeCalls = %d, want 1 (a moved-SHA seed must not park the current SHA)", git.mergeTreeCalls)
	}
	if entry, ok := d.done["main"][ref]; ok {
		t.Fatalf("d.done[main][%s] = %+v, want no entry (a stale-SHA seed must be dropped, not parked)", ref, entry)
	}
}

// TestSeedParks_NonRedOutcomeNeverSeeded covers the other half of
// seedParksOnce's filter: a seed whose Outcome is Landed (or Skipped) is
// never sticky to begin with (§9.1) — the ref proceeds to a normal pick,
// exactly as if SeedParks had returned nothing for it at all.
func TestSeedParks_NonRedOutcomeNeverSeeded(t *testing.T) {
	git := newFakeGitRepo()
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := git.pushCandidate(ref, "", checkSpecFile("test"))

	seedParks := func(target string) []ParkSeed {
		return []ParkSeed{{Ref: ref, SHA: sha, Outcome: core.OutcomeLanded, Reason: "already landed before restart"}}
	}
	d, _ := newSeededDaemon(t, git, seedParks)

	if err := d.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	if git.mergeTreeCalls != 1 {
		t.Fatalf("mergeTreeCalls = %d, want 1 (a Landed seed must never park a ref)", git.mergeTreeCalls)
	}
	if entry, ok := d.done["main"][ref]; ok {
		t.Fatalf("d.done[main][%s] = %+v, want no entry (Landed is not a red-family outcome)", ref, entry)
	}
}

// TestSeedParks_NilIsByteIdenticalStartup covers Config.SeedParks == nil
// (every pre-Feature-2 Daemon, and any target it doesn't cover): reconcile
// proceeds exactly as if seeding didn't exist at all.
func TestSeedParks_NilIsByteIdenticalStartup(t *testing.T) {
	h := newHarness(t) // SeedParks left nil, as every other test in this package does
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	if h.git.mergeTreeCalls != 1 {
		t.Fatalf("mergeTreeCalls = %d, want 1 (nil SeedParks must not block the first pick)", h.git.mergeTreeCalls)
	}
}
