package deploy_test

// Lane-runner unit tests: the statements the txtar scenarios can't make as
// cheaply — the operator entry points' nil-safety and return values, the
// drain gate, the auto-retry-once budget, and the park bookkeeping rules —
// plus a standing assertion that every event this package emits satisfies
// core.ValidateEvent. (That last one is enforced at the emit site by
// eventLog, so it holds for the scenario suites too; the test here is what
// proves the emitter actually produced all three deploy kinds rather than
// silently emitting none.)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/deploy"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// --- the lane-runner half of stubGit (the struct lives in tracker_test.go) ---

func (g *stubGit) Pin(_ context.Context, oid string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pinErr != nil {
		return g.pinErr
	}
	if g.pins == nil {
		g.pins = map[string]bool{}
	}
	g.pins[oid] = true
	return nil
}

func (g *stubGit) Unpin(_ context.Context, oid string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pins, oid)
	return nil
}

func (g *stubGit) ReadFileFromTree(_ context.Context, tree, path string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.readErr != nil {
		return nil, g.readErr
	}
	content, ok := g.tree[path]
	if !ok {
		return nil, fmt.Errorf("stub: %s does not exist in %s", path, tree)
	}
	return []byte(content), nil
}

func (g *stubGit) ExportTree(_ context.Context, _, dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.exportErr != nil {
		return g.exportErr
	}
	return os.MkdirAll(dir, 0o755)
}

func (g *stubGit) RestoreMtimes(context.Context, string, string) (core.MtimeStats, error) {
	return core.MtimeStats{}, nil
}

func (g *stubGit) pinCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pins)
}

func (g *stubGit) setRef(name, oid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refs == nil {
		g.refs = map[string]string{}
	}
	g.refs[name] = oid
}

// mirror is the stub's fetch: it copies observed refs from the "remote"
// into the local mirror the tracker actually reads, so the one-tick lag
// between a green run's CAS and the tracker seeing it is modelled here too.
func (g *stubGit) mirror() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.local == nil {
		g.local = map[string]string{}
	}
	for name, oid := range g.refs {
		if strings.HasPrefix(name, deploy.ObservedRefPrefix) {
			g.local[name] = oid
		}
	}
}

// --- harness ---

// runSpec is the graph every test here deploys unless it says otherwise:
// two nodes, one edge, so both "a node failed" and "a node was blocked by
// the failure" are reachable. A spec must declare at least one check
// (config.ParseChecks), hence the unused one.
const runSpec = `
check "test" {
    command "true"
}
deploy "migrate" {
    command "./deploy" "migrate"
}
deploy "app" {
    command "./deploy" "app"
    after "migrate"
}
`

type runHarness struct {
	t      *testing.T
	git    *stubGit
	ex     *executor.GatedExecutor
	events *eventLog
	tr     *deploy.Tracker
	ctx    context.Context
}

func newRunHarness(t *testing.T, envs ...deploy.Environment) *runHarness {
	t.Helper()
	if len(envs) == 0 {
		envs = []deploy.Environment{{
			Name:          "dev",
			SourceBranch:  "main",
			Mode:          deploy.ModeTrack,
			MaxParallel:   2,
			OnDesiredMove: deploy.OnMoveFinish,
		}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := &runHarness{
		t:      t,
		git:    &stubGit{refs: map[string]string{}, local: map[string]string{}, tree: map[string]string{".gauntlet.kdl": runSpec}},
		ex:     executor.NewGatedExecutor(),
		events: newEventLog(t),
		ctx:    ctx,
	}
	h.tr = deploy.New(deploy.Params{
		Environments:    envs,
		Git:             h.git,
		Exec:            h.ex,
		Emit:            h.events.emit,
		CheckSpec:       ".gauntlet.kdl",
		WorkDir:         t.TempDir(),
		AutoRetryErrors: true,
	})
	return h
}

// tick mirrors observed refs, then runs one reconcile pass.
func (h *runHarness) tick() {
	h.t.Helper()
	h.git.mirror()
	if err := h.tr.ReconcileOnce(h.ctx); err != nil {
		h.t.Fatalf("ReconcileOnce: %v", err)
	}
}

// release delivers one node's result to whichever run is live, blocking
// until that node has actually started.
func (h *runHarness) release(node string, res core.CheckResult) {
	h.t.Helper()
	res.Name = node
	runID := ""
	if !h.events.await(func(evs []core.Event) bool {
		runID = liveRunID(evs, "dev")
		return runID != ""
	}) {
		h.t.Fatalf("release %q: no graph run in flight", node)
	}
	select {
	case <-h.ex.Started(runID, node):
	case <-time.After(scriptTimeout):
		h.t.Fatalf("release %q: node never started in run %s", node, runID)
	}
	h.ex.Release(runID, node, res)
}

// releaseGreen releases the whole two-node graph.
func (h *runHarness) releaseGreen() {
	h.t.Helper()
	h.release("migrate", core.CheckResult{Status: core.CheckPassed})
	h.release("app", core.CheckResult{Status: core.CheckPassed})
}

// awaitFinished blocks until n graph runs have concluded.
func (h *runHarness) awaitFinished(n int) {
	h.t.Helper()
	if !h.events.await(func(evs []core.Event) bool {
		return countKind(evs, core.EventDeployFinished, "dev") >= n
	}) {
		h.t.Fatalf("timed out waiting for %d finished runs (have %d)", n,
			countKind(h.events.all(), core.EventDeployFinished, "dev"))
	}
}

func (h *runHarness) starts() int {
	return countKind(h.events.all(), core.EventDeployStarted, "dev")
}

// running reports whether the LAST tick found (or started) a graph run for
// the lane. It reads the published Snapshot, which stepLane fills
// synchronously inside the reconcile pass — so unlike the emitted started
// event, which a lane goroutine sends a moment later, this is an immediate,
// race-free answer to "did that tick start a run?" in both directions.
func (h *runHarness) running() bool {
	h.t.Helper()
	return h.lane().Running != nil
}

func (h *runHarness) lastRecord() *core.DeployRecord {
	h.t.Helper()
	rec := lastRecord(h.events.all(), "dev")
	if rec == nil {
		h.t.Fatal("no finished graph run")
	}
	return rec
}

func (h *runHarness) lane() deploy.LaneState {
	h.t.Helper()
	snap := h.tr.Snapshot()
	if snap == nil || len(snap.Lanes) == 0 {
		h.t.Fatal("no published Snapshot")
	}
	return snap.Lanes[0]
}

// --- tests ---

// TestOperatorSurfaces_NilSafe: cmd threads Retry/CancelCurrent/Drain/Wait
// whether or not deployment is configured (buildDeployTracker returns nil
// then), exactly as it threads hooks.CancelCurrent. A nil Tracker must
// therefore answer "nothing to do" rather than panic.
func TestOperatorSurfaces_NilSafe(t *testing.T) {
	var tr *deploy.Tracker
	if tr.Retry("prod") {
		t.Error("nil Tracker.Retry = true, want false")
	}
	if tr.CancelCurrent("prod") {
		t.Error("nil Tracker.CancelCurrent = true, want false")
	}
	tr.Drain()
	tr.Wait(context.Background())
}

// TestOperatorSurfaces_ReturnValues pins what the bools mean on a real
// Tracker: false for an environment that isn't configured at all, false for
// a lane with nothing to act on, true only when something was actually
// cleared or signalled.
func TestOperatorSurfaces_ReturnValues(t *testing.T) {
	h := newRunHarness(t)
	if h.tr.Retry("nope") || h.tr.CancelCurrent("nope") {
		t.Error("an unknown environment answered true")
	}
	if h.tr.Retry("dev") {
		t.Error("Retry on an unparked lane = true, want false")
	}
	if h.tr.CancelCurrent("dev") {
		t.Error("CancelCurrent on an idle lane = true, want false")
	}

	h.git.setRef("refs/heads/main", "sha1")
	h.tick() // desired := sha1, and the graph run starts
	h.events.await(func(evs []core.Event) bool { return liveRunID(evs, "dev") != "" })

	if !h.tr.CancelCurrent("dev") {
		t.Fatal("CancelCurrent on a running lane = false, want true")
	}
	h.awaitFinished(1)

	// A cancelled run is concluded from OUTSIDE: recorded, never parked —
	// so the lane is free to reconcile the same desired revision again.
	rec := h.lastRecord()
	if rec.Outcome != core.OutcomeSkipped {
		t.Errorf("cancelled run outcome = %v, want OutcomeSkipped", rec.Outcome)
	}
	if !strings.Contains(rec.Detail, "operator") {
		t.Errorf("cancelled run detail = %q, want it to name the operator", rec.Detail)
	}
	if rec.Culprit != "" {
		t.Errorf("cancelled run culprit = %q, want none attributed", rec.Culprit)
	}
	h.tick()
	if h.lane().Parked != nil {
		t.Error("a cancelled run parked the lane; cancellation must not park")
	}
	if !h.running() {
		t.Error("the lane did not re-run after a cancel; a cancel interrupts one attempt, it does not stop deploying")
	}
}

// TestPark_NewDesiredClears_SameDesiredStays is the level-triggered park
// contract: a red node parks the lane at ONE revision, ticks alone never
// re-run it, and any new desired SHA — including one pushed while the lane
// is parked — clears it.
func TestPark_NewDesiredClears_SameDesiredStays(t *testing.T) {
	h := newRunHarness(t)
	h.git.setRef("refs/heads/main", "sha1")
	h.tick()
	h.release("migrate", core.CheckResult{Status: core.CheckFailed})
	h.awaitFinished(1)

	rec := h.lastRecord()
	if rec.Outcome != core.OutcomeRejected || rec.Culprit != "migrate" {
		t.Fatalf("record = %v/%q, want OutcomeRejected culprit migrate", rec.Outcome, rec.Culprit)
	}
	// The blocked row is the half-deployed picture a parked lane owes an
	// operator: app never ran, and says so, naming what stopped it.
	if len(rec.Nodes) != 2 || rec.Nodes[1].Status != core.CheckBlocked {
		t.Fatalf("rows = %+v, want app blocked", rec.Nodes)
	}
	if h.git.refs[deploy.ObservedRef("dev")] != "" {
		t.Error("observed ref moved for a failed graph")
	}

	h.tick()
	lane := h.lane()
	if lane.Parked == nil || lane.Parked.SHA != "sha1" {
		t.Fatalf("lane park = %+v, want a park at sha1", lane.Parked)
	}
	if lane.LastResult == nil || lane.LastResult.Culprit != "migrate" {
		t.Fatalf("lane last result = %+v, want the culprit recorded", lane.LastResult)
	}
	if h.running() {
		t.Fatal("a parked lane started a run")
	}
	h.tick()
	if h.running() || h.starts() != 1 {
		t.Fatalf("a parked lane re-ran on a later tick (running=%v starts=%d)", h.running(), h.starts())
	}

	// A new desired revision is a new question: the park clears and the
	// graph runs again, same tick.
	h.git.setRef("refs/heads/main", "sha2")
	h.tick()
	if !h.running() {
		t.Fatal("a new desired SHA did not clear the park and re-run")
	}
	if h.lane().Parked != nil {
		t.Error("a new desired SHA left the old park in place")
	}
}

// TestRetry_ClearsParkAndRerunsWholeGraph: the D3 operator surface. Retry
// re-runs the WHOLE graph (both nodes), never the red suffix — resuming
// from recorded per-node rows would make history correctness state.
func TestRetry_ClearsParkAndRerunsWholeGraph(t *testing.T) {
	h := newRunHarness(t)
	h.git.setRef("refs/heads/main", "sha1")
	h.tick()
	h.release("migrate", core.CheckResult{Status: core.CheckFailed})
	h.awaitFinished(1)
	h.tick()

	if !h.tr.Retry("dev") {
		t.Fatal("Retry on a parked lane = false, want true")
	}
	if h.tr.Retry("dev") {
		t.Error("a second Retry found a park to clear; the first should have taken it")
	}
	h.tick()
	h.releaseGreen()
	h.awaitFinished(2)

	rec := h.lastRecord()
	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("retried run outcome = %v, want OutcomeLanded", rec.Outcome)
	}
	if len(rec.Nodes) != 2 {
		t.Fatalf("retried run rows = %d, want the whole graph (2)", len(rec.Nodes))
	}
	if got := h.git.refs[deploy.ObservedRef("dev")]; got != "sha1" {
		t.Errorf("observed ref = %q, want sha1", got)
	}
}

// TestAutoRetryOnce_OnErrorPark: an OutcomeError park (no verdict — the
// command never reported one) spends a once-per-(env, SHA) budget by
// clearing its own park, so the next tick re-runs; a second error on the
// same revision stays parked for a human. A red verdict gets no such
// budget, which the park test above covers by staying parked on the first
// failure.
func TestAutoRetryOnce_OnErrorPark(t *testing.T) {
	h := newRunHarness(t)
	h.git.setRef("refs/heads/main", "sha1")
	h.tick()
	h.release("migrate", core.CheckResult{Err: errors.New("executor unreachable")})
	h.awaitFinished(1)

	if got := h.lastRecord().Outcome; got != core.OutcomeError {
		t.Fatalf("outcome = %v, want OutcomeError", got)
	}
	h.tick() // the budget cleared the park, so this re-runs
	if !h.running() {
		t.Fatal("the lane did not auto-retry its infra-error park")
	}
	if h.lane().Parked != nil {
		t.Error("lane still parked after its auto-retry")
	}

	h.release("migrate", core.CheckResult{Err: errors.New("executor unreachable")})
	h.awaitFinished(2)
	h.tick()
	if h.running() || h.starts() != 2 {
		t.Fatalf("the budget was spent, yet the lane re-ran (running=%v starts=%d)", h.running(), h.starts())
	}
	lane := h.lane()
	if lane.Parked == nil || lane.Parked.Outcome != core.OutcomeError {
		t.Fatalf("lane park = %+v, want an error park", lane.Parked)
	}

	// A NEW revision gets a fresh budget: the same infra flake on a
	// different SHA is a different fact.
	h.git.setRef("refs/heads/main", "sha2")
	h.tick()
	if !h.running() {
		t.Fatal("a new desired SHA did not clear the error park")
	}
	h.release("migrate", core.CheckResult{Err: errors.New("executor unreachable")})
	h.awaitFinished(3)
	h.tick()
	if !h.running() {
		t.Fatal("the new SHA did not get its own fresh auto-retry budget")
	}
}

// TestDrain_RefusesNewGraphsAndLetsInFlightFinish is the graceful-drain
// bargain (issue #8): a draining daemon finishes the graph it started —
// including its observed-ref advance — and admits nothing new.
func TestDrain_RefusesNewGraphsAndLetsInFlightFinish(t *testing.T) {
	h := newRunHarness(t)
	h.git.setRef("refs/heads/main", "sha1")
	h.tick()
	h.events.await(func(evs []core.Event) bool { return liveRunID(evs, "dev") != "" })

	h.tr.Drain()
	h.releaseGreen()
	h.awaitFinished(1)
	if got := h.lastRecord().Outcome; got != core.OutcomeLanded {
		t.Fatalf("in-flight run outcome = %v, want it to finish green under drain", got)
	}
	if got := h.git.refs[deploy.ObservedRef("dev")]; got != "sha1" {
		t.Errorf("observed ref = %q, want the draining run to have advanced it", got)
	}

	// Wait returns once the lane goroutine is gone, which it now is.
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()
	h.tr.Wait(ctx)
	if ctx.Err() != nil {
		t.Fatal("Wait did not return before its deadline with no run in flight")
	}

	// New drift admits nothing: the desired ref still advances (that is
	// bookkeeping, not work), but no graph starts.
	h.git.setRef("refs/heads/main", "sha2")
	h.tick()
	if h.running() || h.starts() != 1 {
		t.Fatalf("a draining daemon admitted a new graph run (running=%v starts=%d)", h.running(), h.starts())
	}
	if got := h.git.refs[deploy.DesiredRef("dev")]; got != "sha2" {
		t.Errorf("desired ref = %q, want the tracker to keep tracking while draining", got)
	}
}

// TestSpecReject_ParksWithDistinctDetail walks every gate that rejects a
// revision before its graph can start. All four park the lane, none of them
// is a red verdict (no node ran), each says something different, and none
// of them emits a started event — a run that never started never claims to
// have. The pin is released on every one of these paths.
func TestSpecReject_ParksWithDistinctDetail(t *testing.T) {
	tests := []struct {
		name    string
		env     deploy.Environment
		tree    map[string]string
		profile func(string) bool
		want    string
	}{
		{
			name: "unreadable",
			tree: map[string]string{},
			want: "deploy spec",
		},
		{
			name: "invalid",
			tree: map[string]string{".gauntlet.kdl": "check \"a\" {\n command \"x\"\n}\ndeploy \"a1\" {\n command \"x\"\n after \"nope\"\n}\n"},
			want: "no such deploy node declared",
		},
		{
			name: "unknown node selected",
			env:  deploy.Environment{Nodes: []string{"nope"}},
			want: "deploy node \"nope\": no such deploy node declared",
		},
		{
			name: "unknown executor profile",
			tree: map[string]string{".gauntlet.kdl": "check \"a\" {\n command \"x\"\n}\ndeploy \"app\" {\n command \"x\"\n executor \"builder\"\n}\n"},
			want: "unknown executor profile \"builder\"",
		},
		{
			name: "no deploy nodes",
			tree: map[string]string{".gauntlet.kdl": "check \"a\" {\n command \"x\"\n}\n"},
			want: "no deploy nodes declared",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.env
			env.Name, env.SourceBranch, env.Mode = "dev", "main", deploy.ModeTrack
			env.MaxParallel, env.OnDesiredMove = 2, deploy.OnMoveFinish
			h := newRunHarness(t, env)
			if tt.tree != nil {
				h.git.tree = tt.tree
			}
			h.git.setRef("refs/heads/main", "sha1")
			h.tick()
			h.awaitFinished(1)

			rec := h.lastRecord()
			if rec.Outcome != core.OutcomeRejected {
				t.Fatalf("outcome = %v, want OutcomeRejected", rec.Outcome)
			}
			if !strings.HasPrefix(rec.Detail, "spec reject: ") {
				t.Errorf("detail = %q, want the spec-reject prefix", rec.Detail)
			}
			if !strings.Contains(rec.Detail, tt.want) {
				t.Errorf("detail = %q, want it to contain %q", rec.Detail, tt.want)
			}
			if len(rec.Nodes) != 0 || rec.Culprit != "" {
				t.Errorf("rows = %+v culprit = %q, want a spec rejection to blame no node", rec.Nodes, rec.Culprit)
			}
			if h.starts() != 0 {
				t.Errorf("started events = %d, want none: the graph never started", h.starts())
			}
			if got := h.git.pinCount(); got != 0 {
				t.Errorf("live pins = %d, want the pin released on this terminal path", got)
			}
			h.tick()
			if lane := h.lane(); lane.Parked == nil || lane.Parked.SHA != "sha1" {
				t.Fatalf("lane park = %+v, want a park at sha1", lane.Parked)
			}
		})
	}
}

// TestPreGraphFailures_AreErrorParks: a pin or export failure is
// daemon-side infrastructure, not a bad revision, so it parks as
// OutcomeError (and is therefore auto-retry eligible) rather than as a
// rejection.
func TestPreGraphFailures_AreErrorParks(t *testing.T) {
	for _, tt := range []struct {
		name  string
		set   func(g *stubGit)
		want  string
		pinOK bool
	}{
		{name: "pin", set: func(g *stubGit) { g.pinErr = errors.New("boom") }, want: "pin deploy revision"},
		{name: "export", set: func(g *stubGit) { g.exportErr = errors.New("boom") }, want: "export tree", pinOK: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newRunHarness(t)
			tt.set(h.git)
			h.git.setRef("refs/heads/main", "sha1")
			h.tick()
			h.awaitFinished(1)

			rec := h.lastRecord()
			if rec.Outcome != core.OutcomeError {
				t.Fatalf("outcome = %v, want OutcomeError", rec.Outcome)
			}
			if !strings.Contains(rec.Detail, tt.want) {
				t.Errorf("detail = %q, want it to contain %q", rec.Detail, tt.want)
			}
			if got := h.git.pinCount(); got != 0 {
				t.Errorf("live pins = %d, want 0", got)
			}
		})
	}
}

// TestObservedCASStale_ParksWithAnHonestDetail: only this daemon writes an
// observed ref, so a lost CAS means something outside the model moved it.
// The run does not force-push over it — it parks and says what happened.
func TestObservedCASStale_ParksWithAnHonestDetail(t *testing.T) {
	h := newRunHarness(t)
	h.git.casErr = func(ref string) error {
		if strings.HasPrefix(ref, deploy.ObservedRefPrefix) {
			return fmt.Errorf("stub: %w", core.ErrCASStale)
		}
		return nil
	}
	h.git.setRef("refs/heads/main", "sha1")
	h.tick()
	h.releaseGreen()
	h.awaitFinished(1)

	rec := h.lastRecord()
	if rec.Outcome != core.OutcomeError {
		t.Fatalf("outcome = %v, want OutcomeError", rec.Outcome)
	}
	if !strings.Contains(rec.Detail, "another writer moved it") {
		t.Errorf("detail = %q, want it to name the lost race", rec.Detail)
	}
}

// TestEmittedEvents_SatisfyTheContract: every event this package emits is
// held to core.ValidateEvent at the emit site (eventLog), so this test's
// job is to prove the emitter actually produces all three deploy kinds —
// a validator that only ever sees an empty stream proves nothing — and
// that each carries the lane coordinates its consumers join on.
func TestEmittedEvents_SatisfyTheContract(t *testing.T) {
	h := newRunHarness(t)
	h.git.setRef("refs/heads/main", "sha1")
	h.tick()
	h.releaseGreen()
	h.awaitFinished(1)

	seen := map[core.EventKind]int{}
	for _, ev := range h.events.all() {
		seen[ev.Kind]++
		if err := core.ValidateEvent(ev); err != nil {
			t.Errorf("event %+v: %v", ev, err)
		}
		if ev.DeployEnv != "dev" || ev.RunID == "" || ev.DeploySHA != "sha1" {
			t.Errorf("event %+v: missing the lane coordinates every deploy event carries", ev)
		}
		if ev.DeployedSHA != "" {
			t.Errorf("event %+v: DeployedSHA should be empty on a first-ever deploy", ev)
		}
	}
	if seen[core.EventDeployStarted] != 1 {
		t.Errorf("started events = %d, want 1", seen[core.EventDeployStarted])
	}
	if seen[core.EventDeployNodeFinished] != 2 {
		t.Errorf("node-finished events = %d, want one per node", seen[core.EventDeployNodeFinished])
	}
	if seen[core.EventDeployFinished] != 1 {
		t.Errorf("terminal events = %d, want 1", seen[core.EventDeployFinished])
	}

	// The second deploy of the same environment carries the FIRST one's
	// revision as DeployedSHA — the baseline the diff-based skip protocol
	// reads as GAUNTLET_DEPLOYED_SHA.
	h.git.setRef("refs/heads/main", "sha2")
	h.tick()
	h.releaseGreen()
	h.awaitFinished(2)
	rec := h.lastRecord()
	if rec.DeployedSHA != "sha1" || rec.DeploySHA != "sha2" {
		t.Fatalf("second run = %s -> %s, want sha1 -> sha2", rec.DeployedSHA, rec.DeploySHA)
	}
}

// TestSyncedMarker_NoDoubleRunBeforeTheMirrorCatchesUp: the observed ref is
// WRITTEN on the remote and READ from the local mirror, so between a green
// run's CAS and the next fetch the lane still looks like it has drift. The
// runner's own already-synced marker is what stops that window from
// starting the same graph a second time.
func TestSyncedMarker_NoDoubleRunBeforeTheMirrorCatchesUp(t *testing.T) {
	h := newRunHarness(t)
	h.git.setRef("refs/heads/main", "sha1")
	h.tick()
	h.releaseGreen()
	h.awaitFinished(1)

	// Reconcile WITHOUT mirroring: the tracker's observed view is still
	// empty, exactly as it is before the queue's next fetch.
	if err := h.tr.ReconcileOnce(h.ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if lane := h.lane(); lane.Running != nil || h.starts() != 1 {
		t.Fatalf("the CAS'd revision re-ran before the mirror caught up (running=%+v starts=%d)", lane.Running, h.starts())
	}
	h.tick()
	lane := h.lane()
	if !lane.InSync {
		t.Errorf("lane = %+v, want it in sync once the mirror caught up", lane)
	}
	if lane.Running != nil {
		t.Errorf("lane still reports a run in flight: %+v", lane.Running)
	}
}
