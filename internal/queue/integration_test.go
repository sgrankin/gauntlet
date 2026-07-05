// Integration suite: proves DESIGN.md's Invariants hold when the queue
// drives REAL git (internal/gitx) against real bare-repo remotes
// (internal/testutil), not the in-memory fake the rest of this package's
// tests use (daemon_test.go and friends, C6). This is the second of the
// plan's two test tiers (docs/plans/phase1.md §5): state-machine
// ordering/aggregation is proven once against fakes; CAS semantics,
// merge-tree, read-file-from-tree, and crash recovery are proven here
// against the genuine plumbing, exactly as §5's integration table
// prescribes. Every test drives the daemon through the public API only
// (queue.New + Daemon.ReconcileOnce); nothing here pokes gitx or executor
// internals directly except where a row is fundamentally about that
// boundary (e.g. simulating a mid-land crash by calling the same
// core.GitRepo.CASUpdate the daemon itself would call).
//
// Two executors are used, matching the row's intent: GatedExecutor for rows
// that need a "check running" window to exercise a mid-check race
// (Invariant 5, CAS races at land), and the real LocalExecutor with actual
// shell-command check specs for rows that test the executor contract
// end-to-end (skipped verdict via the result file, the env-var contract,
// exec-start failure, process-group cancellation) — GatedExecutor never
// execs job.Command at all, so it can't stand in for any of those.
package queue

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// integrationHarness wires a Daemon to a REAL gitx.Repo (a bare clone of a
// testutil.Remote), a caller-supplied core.Executor, and a
// RecordingChannel. Unlike testHarness (daemon_test.go), git here shells out
// to the real git CLI: every ref mutation, merge-tree, and commit-tree is
// the genuine plumbing operation the production daemon runs.
type integrationHarness struct {
	t      *testing.T
	remote *testutil.Remote
	git    *gitx.Repo
	ch     *channel.RecordingChannel
	d      *Daemon
}

// newIntegrationHarness builds an integrationHarness for a single target
// named "main" tracking branch "main" (unless targets is given explicitly),
// wired to exec. remote may be nil to create a fresh one (the common case);
// pass an existing *testutil.Remote to attach a second/subsequent Daemon to
// it — the shape duplicate-daemon and crash-recovery tests need: each
// Daemon gets its own fresh bare clone (so no in-memory gitx state is
// shared), but all of them observe and mutate the same underlying remote
// refs.
func newIntegrationHarness(t *testing.T, remote *testutil.Remote, exec core.Executor, targets ...config.Target) *integrationHarness {
	t.Helper()
	if remote == nil {
		remote = testutil.NewRemote(t)
	}
	if len(targets) == 0 {
		targets = []config.Target{{Name: "main", Branch: "main"}}
	}
	dir := remote.BareClone()
	repo, err := gitx.New(context.Background(), dir, remote.Dir)
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	ch := channel.NewRecordingChannel()
	d, err := New(repo, exec, []core.Channel{ch}, Config{
		Targets:   targets,
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// F1 (docs/plans/phase23.md §10): every terminal event must carry a
	// non-nil RunRecord, asserted across the whole RecordingChannel stream
	// for every integration test built on this harness.
	t.Cleanup(func() { assertAllTerminalEventsHaveRecords(t, ch.Events()) })
	return &integrationHarness{t: t, remote: remote, git: repo, ch: ch, d: d}
}

func (h *integrationHarness) reconcile() {
	h.t.Helper()
	if err := h.d.ReconcileOnce(context.Background()); err != nil {
		h.t.Fatalf("ReconcileOnce: %v", err)
	}
}

// currentRunID returns the RunID of the most recently emitted event that
// carries one (mirrors testHarness.currentRunID in daemon_test.go). Run IDs
// are content-derived (the trial tree's OID; §9.4) and not predictable
// ahead of time, so tests address GatedExecutor.Release calls through this.
func (h *integrationHarness) currentRunID() string {
	h.t.Helper()
	evs := h.ch.Events()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].RunID != "" {
			return evs[i].RunID
		}
	}
	h.t.Fatal("no event with a RunID found")
	return ""
}

// releaseGated delivers result to gated's call registered on (runID, name),
// then spins ReconcileOnce until (runID, name)'s own EventCheckFinished is
// observed — the same rendezvous problem daemon_test.go's release() solves
// (Release only enqueues into a buffered channel; nothing synchronizes the
// executor goroutine actually resuming and delivering into the run's
// one-shot result channel, which ReconcileOnce reads non-blockingly). Real
// git makes each spin of ReconcileOnce cost a real fetch, so the bound is
// wall-clock time rather than a fixed iteration count, but the loop still
// spins on an observed condition — never a sleep for a guessed duration.
//
// Waits for the specific (runID, name) EventCheckFinished, not "any new
// event" (checkFinishedObserved, daemon_test.go): a speculate window's
// quiet-tick refill can inject an unrelated event for a DIFFERENT run in
// the same lane before this release's own result is actually consumed — see
// release's doc comment (P5-F finding) for the full story.
func (h *integrationHarness) releaseGated(gated *executor.GatedExecutor, runID, name string, result core.CheckResult) {
	h.t.Helper()
	before := len(h.ch.Events())
	gated.Release(runID, name, result)
	deadline := time.Now().Add(30 * time.Second)
	for {
		h.reconcile()
		if checkFinishedObserved(h.ch.Events()[before:], runID, name) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no EventCheckFinished for (%s,%s) after releasing; the check's executor goroutine never seemed to run", runID, name)
		}
		runtime.Gosched()
	}
}

// awaitStarted blocks until the executor gated on (runID, name) has
// registered (GatedExecutor.Started), or fails the test after a generous
// timeout. Only for positive "this check has started" assertions.
func (h *integrationHarness) awaitStarted(gated *executor.GatedExecutor, runID, name string) {
	h.t.Helper()
	select {
	case <-gated.Started(runID, name):
	case <-time.After(10 * time.Second):
		h.t.Fatalf("check (%s,%s) never started", runID, name)
	}
}

// pumpUntilRecord spins ReconcileOnce until at least one new terminal
// RunRecord has been captured since before (len(h.ch.Records()) at call
// time), returning it. Used with LocalExecutor, where a real subprocess
// takes genuine wall-clock time to finish and there is nothing to gate —
// this is the LocalExecutor analogue of releaseGated's spin, bounded the
// same way (a generous deadline, never a sleep for a guessed duration).
func (h *integrationHarness) pumpUntilRecord(before int) *core.RunRecord {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		h.reconcile()
		if recs := h.ch.Records(); len(recs) > before {
			return recs[len(recs)-1]
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no new run record after repeated ReconcileOnce calls")
		}
		runtime.Gosched()
	}
}

// distinctMarkerCounter hands out a fresh integer per call so
// distinctFiles's marker file is unique even across calls within the same
// nanosecond.
var distinctMarkerCounter int64

// distinctFiles returns a copy of files with an extra per-call-unique marker
// file merged in, under a per-call-unique PATH (not just unique content).
// Real git content-addresses trees: if two candidates (or a re-push of the
// same candidate onto an unchanged base) end up with byte-for-byte identical
// net content, their trial tree is the exact same object, and since a run ID
// is <timestamp>-<trial tree OID prefix> (§9.4), two such trials started
// within the same second mint the identical run ID — colliding
// GatedExecutor's (RunID, Name) gate key (internal/executor/gate.go), which
// panics on a double Started-channel close. Production candidates
// practically never collide this way (real changes differ by definition);
// tests that deliberately push more than one candidate sharing one
// GatedExecutor, or re-push a candidate without otherwise changing its
// content, use this to keep trial trees — and so run IDs — distinguishable.
//
// The marker's path, not just its content, must vary per call: two
// candidates built from a common ancestor that both independently "add" a
// file at the *same* path is a genuine add/add conflict to real
// merge-tree — reusing one fixed marker filename across, say, a landed
// candidate and a since-diverged re-push of another candidate would trip
// exactly that conflict instead of the clean trial the test wants.
func distinctFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files)+1)
	for k, v := range files {
		out[k] = v
	}
	distinctMarkerCounter++
	marker := fmt.Sprintf(".gauntlet-marker-%d-%d", time.Now().UnixNano(), distinctMarkerCounter)
	out[marker] = "distinct\n"
	return out
}

// shellCheckSpec returns a files map (for testutil.PushCandidate /
// MoveCandidate) declaring a single real check named name that runs
// scriptBody as an actual /bin/sh script exported alongside the candidate
// tree. Unlike checkSpecFile (daemon_test.go), whose "true" command never
// actually runs under the GatedExecutor test double, this is for rows that
// exercise LocalExecutor's real subprocess contract end-to-end.
func shellCheckSpec(name, scriptBody string) map[string]string {
	script := name + ".sh"
	return map[string]string{
		testCheckSpecPath: fmt.Sprintf("check %q {\n    command \"/bin/sh\" %q\n}\n", name, script),
		script:            scriptBody,
	}
}
