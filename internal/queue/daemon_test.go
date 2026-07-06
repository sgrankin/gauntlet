package queue

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

const testCheckSpecPath = ".gauntlet.kdl"

var testCommitter = core.Identity{Name: "Gauntlet", Email: "gauntlet@example.com"}

// runIDPattern matches the §9.4-shaped run-ID scheme, sharpened by
// docs/plans/phase23.md §2.4's monotonic counter: a UTC timestamp, a
// hyphen, the per-process sequence number, a hyphen, and 12 hex characters
// taken from an OID (the trial tree's for a run that got one; the
// candidate's own SHA for pre-trial outcomes and for the IsAncestor
// recovery stand-in — see tryStartTrial's, rejectPreMerge's, and
// recoverLanded's run-ID comments in reconcile.go).
var runIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-\d+-[0-9a-f]{12}$`)

// gatedExecutor is the subset of *executor.GatedExecutor that testHarness's
// own helpers (release, awaitStarted) actually need, narrowed to an
// interface (docs/plans/services-impl.md §4.6) so a wrapping test double —
// services_test.go's recordingGatedExecutor, which additionally captures
// every core.CheckJob RunCheck received, so tests can assert what the queue
// wrapper actually handed the executor — can stand in for a plain
// *executor.GatedExecutor without testHarness itself knowing anything about
// services.
type gatedExecutor interface {
	Started(runID, name string) <-chan struct{}
	Release(runID, name string, result core.CheckResult)
}

// testHarness wires a Daemon to a fakeGitRepo, a gatedExecutor, and a
// RecordingChannel, with an injectable clock — everything queue's tests
// need to drive ReconcileOnce deterministically and inspect the result.
type testHarness struct {
	t     *testing.T
	git   *fakeGitRepo
	exec  gatedExecutor
	ch    *channel.RecordingChannel
	d     *Daemon
	clock time.Time
}

// newHarness builds a testHarness for a single target named "main" tracking
// branch "main", unless targets is given explicitly.
func newHarness(t *testing.T, targets ...config.Target) *testHarness {
	return newHarnessWithExecutor(t, executor.NewGatedExecutor(), nil, targets...)
}

// newHarnessWithServices is newHarness, but wires svc into Config.Services
// (docs/plans/services-impl.md §4.4) — for tests whose check spec declares
// `needs` and need a Daemon whose service ensure/release/dead-check calls
// are scriptable. nil behaves exactly like newHarness (services disabled).
func newHarnessWithServices(t *testing.T, svc ServicePool, targets ...config.Target) *testHarness {
	return newHarnessWithExecutor(t, executor.NewGatedExecutor(), svc, targets...)
}

// newHarnessWithExecutor is the common constructor behind newHarness and
// newHarnessWithServices: exec is anything satisfying both core.Executor
// (what the Daemon actually runs checks against) and gatedExecutor (what
// testHarness's own release/awaitStarted helpers drive) — a plain
// *executor.GatedExecutor, or services_test.go's recordingGatedExecutor
// wrapping one.
func newHarnessWithExecutor(t *testing.T, exec interface {
	core.Executor
	gatedExecutor
}, svc ServicePool, targets ...config.Target) *testHarness {
	t.Helper()
	if len(targets) == 0 {
		targets = []config.Target{{Name: "main", Branch: "main"}}
	}
	git := newFakeGitRepo()
	ch := channel.NewRecordingChannel()
	h := &testHarness{
		t: t, git: git, exec: exec, ch: ch,
		clock: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
	}

	d, err := New(git, exec, []core.Channel{ch}, Config{
		Targets:   targets,
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
		Services:  svc,
	}, h.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
	// F1 (docs/plans/phase23.md §10): every terminal event must carry a
	// non-nil RunRecord. Enforced across every test built on this harness,
	// for free, rather than repeating the assertion per test.
	t.Cleanup(func() { assertAllTerminalEventsHaveRecords(t, ch.Events()) })
	return h
}

// assertAllTerminalEventsHaveRecords fails t if any terminal-kind event in
// events carries a nil Record (F1, docs/plans/phase23.md §10): a property
// every emit site — present and future — must uphold.
func assertAllTerminalEventsHaveRecords(t *testing.T, events []core.Event) {
	t.Helper()
	for _, e := range events {
		if !isTerminalEventKind(e.Kind) {
			continue
		}
		if e.Record == nil {
			t.Errorf("terminal event kind=%v target=%s ref=%s carries a nil Record", e.Kind, e.Target, e.Candidate.Ref)
		}
	}
}

// isTerminalEventKind reports whether k is one of the terminal event kinds
// documented on core.Event: the ones that must carry a *RunRecord.
func isTerminalEventKind(k core.EventKind) bool {
	switch k {
	case core.EventLanded, core.EventRejected, core.EventTrialConflict, core.EventSkipped, core.EventError:
		return true
	default:
		return false
	}
}

// now is the Daemon's injected clock: deterministic (no wall time) but
// advancing one second per call. Advancing matters because run IDs embed a
// second-resolution timestamp alongside the trial tree's OID — under a
// frozen clock, two runs of identical content (e.g. a re-push of the same
// files after a Skip) would mint the same run ID and collide in the
// GatedExecutor, which keys its gates by (RunID, Name).
func (h *testHarness) now() time.Time {
	h.clock = h.clock.Add(time.Second)
	return h.clock
}

func (h *testHarness) reconcile() {
	h.t.Helper()
	if err := h.d.ReconcileOnce(context.Background()); err != nil {
		h.t.Fatalf("ReconcileOnce: %v", err)
	}
}

// release delivers result to the executor gated on (runID, name) and then
// spins ReconcileOnce (yielding the scheduler between attempts, never
// sleeping) until (runID, name)'s own EventCheckFinished is observed.
//
// This exists because GatedExecutor.Release only enqueues result into a
// buffered channel — it does not wait for the executor goroutine it
// unblocks to actually resume, return, and deliver that result into the
// run's one-shot result channel (nothing synchronizes that ordering).
// Since ReconcileOnce's read of that channel is deliberately non-blocking
// (production must never stall the reconcile loop on a slow check), calling
// it exactly once immediately after Release can race ahead of the goroutine
// and simply observe "still running" — not a logic bug, just two
// independently-scheduled goroutines with no rendezvous between them.
//
// P5-F finding: waiting for "at least one new event" (this helper's
// pre-speculate condition) is unsound once a target's lane can hold more
// than one run. A speculate window refills on every quiet tick (§2.5), so
// the very tick that finally delivers this release's result can race
// against — and lose to — an unrelated refill for a DIFFERENT run in the
// same lane, which appears as a new event first (EventTrialClean/
// EventCheckStarted for the newly-chained candidate) and would satisfy a
// bare "len(events) > before" check without this release's own check having
// resolved at all. Waiting specifically for (runID, name)'s own
// EventCheckFinished — checkFinishedObserved, below — is precise regardless
// of what else the lane does concurrently, and is a strict tightening of
// the old condition for serial/batch (there, the very next event was always
// this one anyway, since only one run/check was ever in flight).
func (h *testHarness) release(runID, name string, result core.CheckResult) {
	h.t.Helper()
	before := len(h.ch.Events())
	h.exec.Release(runID, name, result)
	for i := 0; i < 100000; i++ {
		h.reconcile()
		if checkFinishedObserved(h.ch.Events()[before:], runID, name) {
			return
		}
		runtime.Gosched()
	}
	h.t.Fatalf("no EventCheckFinished for (%s,%s) after releasing; the check's executor goroutine never seemed to run", runID, name)
}

// checkFinishedObserved reports whether events contains (runID, name)'s own
// EventCheckFinished, OR any terminal event for runID — the precise "this
// specific release was fully processed" signal release/releaseGated wait on
// (see release's doc comment for why a bare "any new event" isn't sound
// once a lane can hold more than one run).
//
// The terminal-event fallback matters for a race integration tests
// deliberately provoke: a move/target-shift detected by advanceLane's
// validity sweep (§2.1a) runs BEFORE that tick's advanceChecks even looks at
// the delivered result, so cancelRun discards it and the run ends via
// invalidateSuffix's Skip — EventSkipped, never EventCheckFinished, for that
// specific delivery. Without this fallback, a release raced against exactly
// that condition (e.g. TestIntegration_ConcurrentDirectPush's direct push
// arriving before the release's first reconcile) would spin until the
// helper's own timeout, since the CheckFinished it's waiting for is never
// coming — the run concluded for an unrelated reason first, and nothing
// further will ever happen with this particular check delivery.
func checkFinishedObserved(events []core.Event, runID, name string) bool {
	for _, e := range events {
		if e.RunID != runID {
			continue
		}
		if e.Kind == core.EventCheckFinished && e.CheckName == name {
			return true
		}
		if isTerminalEventKind(e.Kind) {
			return true
		}
	}
	return false
}

// awaitStarted blocks until the executor gated on (runID, name) has
// registered (GatedExecutor.Started), or fails the test after a generous
// timeout. This is a synchronization wait, not a pacing sleep — it returns
// as soon as the check's executor goroutine actually runs, almost always
// within microseconds; the timeout is only a safety net against a genuine
// hang, the same idiom internal/executor's own gate_test.go uses.
//
// Only for *positive* "this check has started" assertions. A *negative*
// assertion ("this check must never start") doesn't need this: when the
// state machine never calls startCheck for it, no goroutine is ever
// spawned and Started's channel never closes — that's a standing logical
// guarantee, not a timing race, so a plain non-blocking check is correct.
func (h *testHarness) awaitStarted(runID, name string) {
	h.t.Helper()
	select {
	case <-h.exec.Started(runID, name):
	case <-time.After(5 * time.Second):
		h.t.Fatalf("check (%s,%s) never started", runID, name)
	}
}

// currentRunID returns the RunID of the most recently emitted event that
// carries one. Tests use it to address GatedExecutor.Release calls at the
// run currently in flight, since run IDs are content-derived and not
// predictable ahead of time.
func (h *testHarness) currentRunID() string {
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

// candidateRef builds a well-formed candidate ref (§9.3). An empty user
// produces the solo (no-user) form.
func candidateRef(target, user, topic string) string {
	if user == "" {
		return candidatePrefix + target + "/" + topic
	}
	return candidatePrefix + target + "/" + user + "/" + topic
}

// checkSpecFile renders a minimal .gauntlet.kdl declaring one check per
// name in names. The command's argv never actually runs — every test here
// uses the GatedExecutor test double — it only needs to satisfy
// config.ParseChecks's validation (non-empty name and command).
func checkSpecFile(names ...string) map[string]string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "check %q {\n    command \"true\"\n}\n", n)
	}
	return map[string]string{testCheckSpecPath: b.String()}
}

func TestReconcile_GreenMultiCheckLand(t *testing.T) {
	h := newHarness(t)
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	candSHA := h.git.pushCandidate(ref, "", checkSpecFile("lint", "test"))

	h.reconcile() // trial clean; lint started
	runID := h.currentRunID()
	if !runIDPattern.MatchString(runID) {
		t.Fatalf("run ID %q does not match the §9.4 format", runID)
	}

	h.release(runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckPassed, Duration: time.Second})     // lint recorded; test started
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: 2 * time.Second}) // both green: land

	// Land ordering (Invariants 2, 3): target CAS logged strictly before
	// the slot-delete CAS.
	if len(h.git.casLog) != 2 {
		t.Fatalf("CAS calls = %d, want exactly 2 (target push, slot delete)", len(h.git.casLog))
	}
	if h.git.casLog[0].ref != "refs/heads/main" {
		t.Errorf("first CAS call ref = %q, want target ref", h.git.casLog[0].ref)
	}
	if h.git.casLog[1].ref != ref {
		t.Errorf("second CAS call ref = %q, want candidate ref", h.git.casLog[1].ref)
	}

	mergeOID := h.git.ref("refs/heads/main")
	if mergeOID == "" || mergeOID == base {
		t.Fatalf("target ref = %q, want a new merge commit", mergeOID)
	}
	if h.git.hasRef(ref) {
		t.Fatal("candidate slot still exists after land")
	}
	if c := h.git.commits[mergeOID]; len(c.parents) != 2 || c.parents[0] != base || c.parents[1] != candSHA {
		t.Fatalf("merge commit parents = %v, want [%s %s] (Invariant 1: candidate SHA verbatim as parent[1])", c.parents, base, candSHA)
	}

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if last.RunID != runID {
		t.Fatalf("RunRecord.RunID = %q, want %q", last.RunID, runID)
	}
	if last.BaseOID != base {
		t.Errorf("BaseOID = %q, want %q", last.BaseOID, base)
	}
	if last.MergeSHA != mergeOID {
		t.Errorf("MergeSHA = %q, want %q", last.MergeSHA, mergeOID)
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

func TestNew_Validation(t *testing.T) {
	git := newFakeGitRepo()
	exec := executor.NewGatedExecutor()
	valid := Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
	}

	cases := []struct {
		name string
		git  core.GitRepo
		exec core.Executor
		cfg  func(Config) Config
	}{
		{name: "nil git", git: nil, exec: exec, cfg: func(c Config) Config { return c }},
		{name: "nil executor", git: git, exec: nil, cfg: func(c Config) Config { return c }},
		{name: "no targets", git: git, exec: exec, cfg: func(c Config) Config { c.Targets = nil; return c }},
		{name: "empty check spec", git: git, exec: exec, cfg: func(c Config) Config { c.CheckSpec = ""; return c }},
		{name: "empty committer", git: git, exec: exec, cfg: func(c Config) Config { c.Committer = core.Identity{}; return c }},
		{name: "committer missing email", git: git, exec: exec, cfg: func(c Config) Config { c.Committer.Email = ""; return c }},
		{name: "unparseable merge message", git: git, exec: exec, cfg: func(c Config) Config { c.MergeMessage = "{{.Nope"; return c }},
		{name: "target missing branch", git: git, exec: exec, cfg: func(c Config) Config {
			c.Targets = []config.Target{{Name: "main"}}
			return c
		}},
		{name: "duplicate target", git: git, exec: exec, cfg: func(c Config) Config {
			c.Targets = []config.Target{{Name: "main", Branch: "main"}, {Name: "main", Branch: "other"}}
			return c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.git, tc.exec, nil, tc.cfg(valid), nil); err == nil {
				t.Fatal("New: want an error")
			}
		})
	}

	if _, err := New(git, exec, nil, valid, nil); err != nil {
		t.Fatalf("New with a valid config: %v", err)
	}
}

func TestRun_TicksAndStops(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	h.git.pushCandidate(candidateRef("main", "alice", "widget"), "", checkSpecFile("test"))

	ctx, cancel := context.WithCancel(t.Context())
	tick := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx, tick) }()

	tick <- time.Time{} // one tick = one ReconcileOnce
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer waitCancel()
	if _, ok := h.ch.WaitForKind(waitCtx, core.EventQueued); !ok {
		t.Fatal("no EventQueued after a tick; Run is not reconciling")
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after ctx cancel, want ctx.Err()")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRun_TickChannelClosed(t *testing.T) {
	h := newHarness(t)
	tick := make(chan time.Time)
	close(tick)
	if err := h.d.Run(t.Context(), tick); err != nil {
		t.Fatalf("Run after tick close = %v, want nil", err)
	}
}

func TestReconcile_EventsPerTransition(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	var kinds []core.EventKind
	for _, e := range h.ch.Events() {
		kinds = append(kinds, e.Kind)
	}
	want := []core.EventKind{
		core.EventQueued,
		core.EventTrialClean,
		core.EventCheckStarted,
		core.EventCheckFinished,
		core.EventLanded,
	}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("events[%d] = %v, want %v", i, kinds[i], want[i])
		}
	}

	// The contract-level assertion that would have caught the ship-blocker
	// (docs/plans/phase23.md §10 review): every event from EventTrialClean
	// onward — including EventTrialClean itself — must carry a non-empty
	// RunID, and it must be the SAME RunID throughout the run. Channels
	// (Slack threading, ghstatus target_url) join a run's events by RunID;
	// an EventTrialClean minted without one breaks that join for the run's
	// entire lifetime even though every later event still carries one.
	events := h.ch.Events()
	for i, e := range events {
		if kinds[i] == core.EventQueued {
			continue // pre-trial: no run (and so no RunID) exists yet
		}
		if e.RunID == "" {
			t.Errorf("events[%d] (kind=%v) has empty RunID", i, e.Kind)
		}
		if e.RunID != runID {
			t.Errorf("events[%d] (kind=%v) RunID = %q, want %q (same run throughout)", i, e.Kind, e.RunID, runID)
		}
	}
}
