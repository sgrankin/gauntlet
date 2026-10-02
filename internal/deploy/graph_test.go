// Deploy node-graph scheduling suite: the divergence guard for the D2
// spike's "a second minimal scheduler, not an extraction" verdict
// (docs/design/deployment.md, Phase D2). Every behavior of
// internal/queue/parallel_test.go is ported here under the SAME test name,
// adapted to deploy.Scheduler's API — diamond overlap and join, fail-fast
// with blocked rows, drain-then-cull, slot starvation with Waited
// accounting, cancellation with several nodes running, and skipped counting
// green — plus the behaviors only this scheduler has (a blocking slot
// acquire, an externally cancelled run). When one scheduler's behavior
// changes, its twin's port is what makes the divergence visible instead of
// silent.
package deploy_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/deploy"
)

// testTimeout bounds every synchronization wait below. These are
// rendezvous, not pacing sleeps: they return as soon as the goroutine being
// waited on actually runs, and the timeout is only a safety net against a
// genuine hang.
const testTimeout = 10 * time.Second

// stepClock is the injected clock: deterministic (no wall time) and
// advancing one second per call, so a Waited stamp is an exact, reproducible
// duration rather than a timing measurement. Node goroutines share it, hence
// the mutex.
type stepClock struct {
	mu    sync.Mutex
	t     time.Time
	calls int
}

func newStepClock() *stepClock {
	return &stepClock{t: time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)}
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.t = c.t.Add(time.Second)
	return c.t
}

// awaitCalls spins until the clock has been read want times. The only
// clock reads in a scheduler run bracket a node's SLOT WAIT (a node that
// starts immediately never reads it at all), so this is precisely the
// "node a is now blocked in Acquire" rendezvous the starvation tests need
// before they free the cap — nothing else can observe a goroutine parked
// inside a semaphore.
func (c *stepClock) awaitCalls(t *testing.T, want int) {
	t.Helper()
	for range 1_000_000 {
		c.mu.Lock()
		got := c.calls
		c.mu.Unlock()
		if got >= want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("clock read %d times, want %d", c.calls, want)
}

// gatedExec is a deploy.Exec whose every node blocks until the test hands it
// a result — the same affordance internal/executor's GatedExecutor gives
// queue tests, and the reason concurrent in-flight nodes are exactly as
// steppable here as serial ones. A node whose ctx is cancelled while gated
// returns the Err result a killed command produces.
type gatedExec struct {
	mu      sync.Mutex
	started map[string]chan struct{}
	gates   map[string]chan core.CheckResult
}

func newGatedExec() *gatedExec {
	return &gatedExec{
		started: make(map[string]chan struct{}),
		gates:   make(map[string]chan core.CheckResult),
	}
}

func (g *gatedExec) chans(name string) (chan struct{}, chan core.CheckResult) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.started[name]; !ok {
		g.started[name] = make(chan struct{})
		g.gates[name] = make(chan core.CheckResult, 1)
	}
	return g.started[name], g.gates[name]
}

func (g *gatedExec) exec(ctx context.Context, _ int, n deploy.Node) core.CheckResult {
	started, gate := g.chans(n.Name)
	close(started)
	select {
	case res := <-gate:
		return res
	case <-ctx.Done():
		return core.CheckResult{Err: ctx.Err()}
	}
}

// awaitStarted blocks until name's command actually began. Positive
// assertions only — see hasStarted for the negative form.
func (g *gatedExec) awaitStarted(t *testing.T, name string) {
	t.Helper()
	started, _ := g.chans(name)
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatalf("node %q never started", name)
	}
}

// hasStarted is the negative-assertion form: a node the scheduler never
// launched has no goroutine at all, so this is a standing logical guarantee
// rather than a race.
func (g *gatedExec) hasStarted(name string) bool {
	started, _ := g.chans(name)
	select {
	case <-started:
		return true
	default:
		return false
	}
}

func (g *gatedExec) release(name string, res core.CheckResult) {
	_, gate := g.chans(name)
	gate <- res
}

// finishRecorder captures OnFinish callbacks and lets a test wait for one —
// the rendezvous that makes "this node's result has been RECORDED" (as
// opposed to merely produced) observable, without which every
// negative-assertion-after-a-release below would be a race.
type finishRecorder struct {
	mu    sync.Mutex
	names []string
	ch    chan string
}

func newFinishRecorder() *finishRecorder {
	return &finishRecorder{ch: make(chan string, 64)}
}

func (f *finishRecorder) onFinish(_ int, n deploy.Node, _ core.CheckResult) {
	f.mu.Lock()
	f.names = append(f.names, n.Name)
	f.mu.Unlock()
	f.ch <- n.Name
}

func (f *finishRecorder) await(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-f.ch:
		if got != want {
			t.Fatalf("OnFinish for %q, want %q", got, want)
		}
	case <-time.After(testTimeout):
		t.Fatalf("no OnFinish for %q", want)
	}
}

// schedResult is one Scheduler.Run return, ferried off the goroutine the
// test drives it on.
type schedResult struct {
	rows    []core.CheckResult
	culprit string
	err     error
}

func runAsync(ctx context.Context, s *deploy.Scheduler) <-chan schedResult {
	out := make(chan schedResult, 1)
	go func() {
		rows, culprit, err := s.Run(ctx)
		out <- schedResult{rows, culprit, err}
	}()
	return out
}

func awaitRun(t *testing.T, ch <-chan schedResult) schedResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(testTimeout):
		t.Fatal("Scheduler.Run never returned")
		return schedResult{}
	}
}

// waitSlotsInUse spins until the daemon-wide cap shows want holders. It is
// a genuine happens-after signal, not a timing guess: a node goroutine
// releases its slot only after it has delivered its result to the
// scheduler, so an observed release proves the delivery landed. That is
// what lets the two "same window" tests below buffer results in a KNOWN
// order without a wall-clock sleep.
func waitSlotsInUse(t *testing.T, s *core.Slots, want int) {
	t.Helper()
	for range 1_000_000 {
		if s.InUse() == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("slots in use = %d, want %d", s.InUse(), want)
}

func nodes(spec ...deploy.Node) []deploy.Node { return spec }

func node(name string, after ...string) deploy.Node {
	return deploy.Node{Name: name, After: after}
}

// row returns the named row, or fails: rows are one-per-declared-node in
// spec order, so a missing one is always a real failure rather than a
// lookup problem.
func row(t *testing.T, rows []core.CheckResult, name string) core.CheckResult {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row for %q in %+v", name, rows)
	return core.CheckResult{}
}

func TestParallel_DiamondOverlapAndJoin(t *testing.T) {
	exec := newGatedExec()
	fin := newFinishRecorder()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("unit"), node("lint"), node("package", "unit", "lint")),
		MaxParallel: 4,
		Exec:        exec.exec,
		Now:         newStepClock().now,
		OnFinish:    fin.onFinish,
	}
	run := runAsync(context.Background(), s)

	exec.awaitStarted(t, "unit") // both roots start together
	exec.awaitStarted(t, "lint")
	if exec.hasStarted("package") {
		t.Fatal("package started before its prerequisites finished")
	}

	exec.release("lint", core.CheckResult{Status: core.CheckPassed})
	fin.await(t, "lint")
	if exec.hasStarted("package") {
		t.Fatal(`package started with unit still running (after "unit" "lint" demands both)`)
	}

	exec.release("unit", core.CheckResult{Status: core.CheckPassed})
	exec.awaitStarted(t, "package") // join satisfied
	exec.release("package", core.CheckResult{Status: core.CheckPassed})

	got := awaitRun(t, run)
	if got.culprit != "" || got.err != nil {
		t.Fatalf("culprit=%q err=%v, want a clean green graph", got.culprit, got.err)
	}
	// Spec-declaration order is the durable row identity, regardless of the
	// order results arrived (lint finished before unit here), and Seq is the
	// 1-based spec position history keys on.
	wantOrder := []string{"unit", "lint", "package"}
	if len(got.rows) != 3 {
		t.Fatalf("rows = %+v, want 3", got.rows)
	}
	for i, want := range wantOrder {
		if got.rows[i].Name != want {
			t.Errorf("rows[%d] = %q, want %q (spec order, not completion order)", i, got.rows[i].Name, want)
		}
		if got.rows[i].Seq != i+1 {
			t.Errorf("rows[%d].Seq = %d, want %d", i, got.rows[i].Seq, i+1)
		}
	}
}

func TestParallel_FailFastCancelsSiblingsAndBlocksDependents(t *testing.T) {
	exec := newGatedExec()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b"), node("c", "b")),
		MaxParallel: 2,
		Exec:        exec.exec,
		Now:         newStepClock().now,
	}
	run := runAsync(context.Background(), s)

	exec.awaitStarted(t, "a")
	exec.awaitStarted(t, "b")

	// a goes red while b is mid-flight: fail fast — b is cancelled, c never
	// becomes ready, and a (not whichever row finished last) is the culprit.
	exec.release("a", core.CheckResult{Status: core.CheckFailed})

	got := awaitRun(t, run)
	if got.culprit != "a" {
		t.Fatalf("culprit = %q, want a", got.culprit)
	}
	if got.err != nil {
		t.Fatalf("err = %v, want nil (a red node is a verdict, not a run error)", got.err)
	}
	if exec.hasStarted("c") {
		t.Fatal("c started despite its prerequisite chain failing")
	}
	if len(got.rows) != 3 {
		t.Fatalf("rows = %+v, want one per declared node", got.rows)
	}
	if a := got.rows[0]; a.Name != "a" || a.Status != core.CheckFailed {
		t.Errorf("rows[0] = %+v, want a's red verdict", a)
	}
	// b was in flight with no failed edge of its own: blocked by the run's
	// root failure. c's proximate cause is its own unfinished edge, b.
	if b := got.rows[1]; b.Status != core.CheckBlocked || len(b.BlockedBy) != 1 || b.BlockedBy[0] != "a" {
		t.Errorf("rows[1] (b) = %+v, want blocked by the root failure a", b)
	}
	if c := got.rows[2]; c.Status != core.CheckBlocked || len(c.BlockedBy) != 1 || c.BlockedBy[0] != "b" {
		t.Errorf("rows[2] (c) = %+v, want blocked by its own edge b", c)
	}
}

// TestParallel_SameTickRedKeepsFinishedSiblingResult pins the
// drain-then-cull ordering: when a red node and a green sibling deliver in
// the same window, the sibling's REAL result (it ran to completion) must be
// recorded — never rewritten as "blocked, never ran" just because the red
// node came earlier in spec order.
//
// The window is constructed, not hoped for: OnStart runs on Run's own
// goroutine, so blocking inside it parks the scheduler where it reads
// nothing, and slot releases (which happen only after a result has been
// delivered) let the test buffer a's red and b's green in that exact order
// before letting the scheduler drain.
func TestParallel_SameTickRedKeepsFinishedSiblingResult(t *testing.T) {
	exec := newGatedExec()
	slots := core.NewSlots(4) // generous: the instrument here is InUse, not the cap
	parked := make(chan struct{})
	release := make(chan struct{})
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b")),
		MaxParallel: 2,
		Slots:       slots,
		Exec:        exec.exec,
		Now:         newStepClock().now,
		OnStart: func(_ int, n deploy.Node) {
			if n.Name != "b" { // park after the LAST start: both nodes are live
				return
			}
			close(parked)
			<-release
		},
	}
	run := runAsync(context.Background(), s)

	<-parked
	exec.awaitStarted(t, "a")
	exec.awaitStarted(t, "b")

	// a (spec-first) fails and b passes; a's result is delivered first, so
	// the scheduler's blocking receive will see the red one with b's green
	// already buffered behind it — the exact state an unlucky tick boundary
	// produces in the queue's twin of this test.
	exec.release("a", core.CheckResult{Status: core.CheckFailed})
	waitSlotsInUse(t, slots, 1)
	exec.release("b", core.CheckResult{Status: core.CheckPassed, Duration: 2 * time.Second, Output: "ok"})
	waitSlotsInUse(t, slots, 0)
	close(release)

	got := awaitRun(t, run)
	if got.culprit != "a" {
		t.Fatalf("culprit = %q, want a", got.culprit)
	}
	if len(got.rows) != 2 {
		t.Fatalf("rows = %+v, want both", got.rows)
	}
	b := row(t, got.rows, "b")
	if b.Status != core.CheckPassed || b.Duration != 2*time.Second || b.Output != "ok" {
		t.Fatalf("b = %+v, want its real completed result kept, not a blocked rewrite", b)
	}
}

// TestParallel_DeterministicCulpritAcrossOneDrainWindow: two nodes fail in
// one window and the culprit is the SPEC-first of them, never the one that
// happened to deliver first. Same construction as the test above, with the
// delivery order deliberately reversed.
func TestParallel_DeterministicCulpritAcrossOneDrainWindow(t *testing.T) {
	exec := newGatedExec()
	slots := core.NewSlots(4)
	parked := make(chan struct{})
	release := make(chan struct{})
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b")),
		MaxParallel: 2,
		Slots:       slots,
		Exec:        exec.exec,
		Now:         newStepClock().now,
		OnStart: func(_ int, n deploy.Node) {
			if n.Name != "b" {
				return
			}
			close(parked)
			<-release
		},
	}
	run := runAsync(context.Background(), s)

	<-parked
	exec.awaitStarted(t, "a")
	exec.awaitStarted(t, "b")

	exec.release("b", core.CheckResult{Status: core.CheckFailed}) // delivers FIRST
	waitSlotsInUse(t, slots, 1)
	exec.release("a", core.CheckResult{Status: core.CheckFailed})
	waitSlotsInUse(t, slots, 0)
	close(release)

	got := awaitRun(t, run)
	if got.culprit != "a" {
		t.Fatalf("culprit = %q, want a — the spec-first failure, not the first to deliver", got.culprit)
	}
	for _, r := range got.rows {
		if r.Status != core.CheckFailed {
			t.Errorf("row %+v: both nodes ran to a red verdict and must keep it", r)
		}
	}
}

// TestParallel_ExecutionCapStarvesAndRecordsWaited: the daemon-wide cap is
// the ONLY thing that stamps Waited. The cap is held here by a stand-in for
// another tenant (a check, a hook — they all draw on the same budget), so
// the starved node is deterministic; the node that merely queued behind
// this graph's own max-parallel reports zero, because it was waiting its
// turn, not starving for capacity.
func TestParallel_ExecutionCapStarvesAndRecordsWaited(t *testing.T) {
	exec := newGatedExec()
	slots := core.NewSlots(1)
	if !slots.TryAcquire() { // another tenant holds the daemon's only slot
		t.Fatal("could not take the daemon's only slot")
	}
	clock := newStepClock()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b")),
		MaxParallel: 1,
		Slots:       slots,
		Exec:        exec.exec,
		Now:         clock.now,
	}
	run := runAsync(context.Background(), s)

	clock.awaitCalls(t, 1) // a is parked in Acquire with its wait stamped
	if exec.hasStarted("a") {
		t.Fatal("a started with the daemon's only execution slot already held")
	}
	slots.Release() // the other tenant finishes
	exec.awaitStarted(t, "a")
	if exec.hasStarted("b") {
		t.Fatal("b started while a held this graph's only max-parallel place")
	}
	exec.release("a", core.CheckResult{Status: core.CheckPassed})
	exec.awaitStarted(t, "b")
	exec.release("b", core.CheckResult{Status: core.CheckPassed})

	got := awaitRun(t, run)
	if got.err != nil || got.culprit != "" {
		t.Fatalf("culprit=%q err=%v, want a clean green graph", got.culprit, got.err)
	}
	if a := row(t, got.rows, "a"); a.Waited <= 0 {
		t.Errorf("a.Waited = %v, want > 0 (it sat ready while the cap was saturated)", a.Waited)
	}
	if b := row(t, got.rows, "b"); b.Waited != 0 {
		t.Errorf("b.Waited = %v, want 0 (queued behind max-parallel is not starvation)", b.Waited)
	}
}

// TestParallel_WaitedIsClockDerived sanity-checks that Waited comes from the
// injected clock (one second per call) rather than wall time — two calls
// bracket the slot wait, so the stamp is exactly one step.
func TestParallel_WaitedIsClockDerived(t *testing.T) {
	exec := newGatedExec()
	slots := core.NewSlots(1)
	if !slots.TryAcquire() {
		t.Fatal("could not take the daemon's only slot")
	}
	clock := newStepClock()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a")),
		MaxParallel: 1,
		Slots:       slots,
		Exec:        exec.exec,
		Now:         clock.now,
	}
	run := runAsync(context.Background(), s)

	clock.awaitCalls(t, 1) // parked in Acquire: the wait's start is stamped
	slots.Release()
	exec.awaitStarted(t, "a")
	exec.release("a", core.CheckResult{Status: core.CheckPassed})

	got := awaitRun(t, run)
	if w := row(t, got.rows, "a").Waited; w != time.Second {
		t.Fatalf("a.Waited = %v, want exactly one injected-clock step", w)
	}
}

func TestParallel_CancelWhileSeveralRunning(t *testing.T) {
	exec := newGatedExec()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b"), node("c")),
		MaxParallel: 3,
		Exec:        exec.exec,
		Now:         newStepClock().now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := runAsync(ctx, s)

	exec.awaitStarted(t, "a")
	exec.awaitStarted(t, "b")
	exec.awaitStarted(t, "c")

	// Cancelled from outside (a `cancel`-policy desired move, a shutdown)
	// with three nodes running: every node is killed and the run reports
	// no fabricated verdicts — nothing finished, so no rows at all.
	cancel()

	got := awaitRun(t, run)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", got.err)
	}
	if got.culprit != "" {
		t.Fatalf("culprit = %q, want none (a cancellation attributes no failure)", got.culprit)
	}
	if len(got.rows) != 0 {
		t.Fatalf("rows = %+v, want none (nothing finished)", got.rows)
	}
}

// TestParallel_ExternalCancelKeepsFinishedRowsOnly: the other half of the
// externally-concluded contract — what DID finish is recorded, what didn't
// gets no row at all (never a blocked one: there is no failure to blame it
// on, and claiming otherwise would read as a verdict the graph never
// reached).
func TestParallel_ExternalCancelKeepsFinishedRowsOnly(t *testing.T) {
	exec := newGatedExec()
	fin := newFinishRecorder()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b")),
		MaxParallel: 2,
		Exec:        exec.exec,
		Now:         newStepClock().now,
		OnFinish:    fin.onFinish,
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := runAsync(ctx, s)

	exec.awaitStarted(t, "a")
	exec.awaitStarted(t, "b")
	exec.release("a", core.CheckResult{Status: core.CheckPassed, Output: "deployed"})
	fin.await(t, "a")
	cancel()

	got := awaitRun(t, run)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", got.err)
	}
	if got.culprit != "" {
		t.Fatalf("culprit = %q, want none", got.culprit)
	}
	if len(got.rows) != 1 {
		t.Fatalf("rows = %+v, want only a's finished row", got.rows)
	}
	if a := got.rows[0]; a.Name != "a" || a.Status != core.CheckPassed || a.Output != "deployed" {
		t.Fatalf("rows[0] = %+v, want a's real result", a)
	}
}

// TestParallel_CancelWhileWaitingForSlotRunsNoCommand: a node still blocked
// on the daemon-wide cap when the run is cancelled never executes anything
// — it reports back so the scheduler can finish waiting on it, and its
// (command-less) result is dropped like any other straggler.
func TestParallel_CancelWhileWaitingForSlotRunsNoCommand(t *testing.T) {
	exec := newGatedExec()
	slots := core.NewSlots(1)
	if !slots.TryAcquire() {
		t.Fatal("could not take the daemon's only slot")
	}
	defer slots.Release()

	s := &deploy.Scheduler{
		Nodes:       nodes(node("a")),
		MaxParallel: 1,
		Slots:       slots,
		Exec:        exec.exec,
		Now:         newStepClock().now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := runAsync(ctx, s)
	cancel()

	got := awaitRun(t, run)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", got.err)
	}
	if exec.hasStarted("a") {
		t.Fatal("a ran its command despite never getting a slot")
	}
	if len(got.rows) != 0 {
		t.Fatalf("rows = %+v, want none", got.rows)
	}
}

func TestParallel_SkippedPrerequisiteCountsGreen(t *testing.T) {
	exec := newGatedExec()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("affected"), node("deploy", "affected")),
		MaxParallel: 2,
		Exec:        exec.exec,
		Now:         newStepClock().now,
	}
	run := runAsync(context.Background(), s)

	// A prerequisite reporting skipped — its own successful nothing-to-do
	// verdict, which is how affected-only deploys stay cheap — satisfies an
	// after edge exactly like passed (core.NodeGreen, the one predicate
	// this scheduler shares with the queue's).
	exec.awaitStarted(t, "affected")
	exec.release("affected", core.CheckResult{Status: core.CheckSkipped})
	exec.awaitStarted(t, "deploy")
	exec.release("deploy", core.CheckResult{Status: core.CheckPassed})

	got := awaitRun(t, run)
	if got.culprit != "" || got.err != nil {
		t.Fatalf("culprit=%q err=%v, want a green graph (skipped counts green)", got.culprit, got.err)
	}
	if a := got.rows[0]; a.Status != core.CheckSkipped {
		t.Errorf("rows[0] = %+v, want the honest Skipped row", a)
	}
}

// TestParallel_MaxParallelOneIsStrictSpecOrder: at max-parallel 1 the graph
// is a queue in declaration order, one node in flight at a time — the
// serial default an environment gets unless its config says otherwise.
func TestParallel_MaxParallelOneIsStrictSpecOrder(t *testing.T) {
	exec := newGatedExec()
	var mu sync.Mutex
	var starts []string
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b"), node("c")),
		MaxParallel: 1,
		Exec:        exec.exec,
		Now:         newStepClock().now,
		OnStart: func(_ int, n deploy.Node) {
			mu.Lock()
			starts = append(starts, n.Name)
			mu.Unlock()
		},
	}
	run := runAsync(context.Background(), s)

	order := []string{"a", "b", "c"}
	for i, name := range order {
		exec.awaitStarted(t, name)
		for _, later := range order[i+1:] {
			if exec.hasStarted(later) {
				t.Fatalf("%s started while %s was still running", later, name)
			}
		}
		exec.release(name, core.CheckResult{Status: core.CheckPassed})
	}

	got := awaitRun(t, run)
	if got.err != nil || got.culprit != "" {
		t.Fatalf("culprit=%q err=%v, want a clean green graph", got.culprit, got.err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"a", "b", "c"}
	for i := range want {
		if i >= len(starts) || starts[i] != want[i] {
			t.Fatalf("start order = %v, want %v", starts, want)
		}
	}
}

// TestScheduler_UnsatisfiableEdgeIsAnError: config validates `after` edges
// at spec load and closes an environment's node selection over them, so a
// node whose edge names nothing in this run's graph is unreachable through
// the supported path. It must still fail loudly rather than report a green
// graph nobody ran.
func TestScheduler_UnsatisfiableEdgeIsAnError(t *testing.T) {
	exec := newGatedExec()
	s := &deploy.Scheduler{
		Nodes:       nodes(node("a"), node("b", "missing")),
		MaxParallel: 2,
		Exec:        exec.exec,
		Now:         newStepClock().now,
	}
	run := runAsync(context.Background(), s)

	exec.awaitStarted(t, "a")
	exec.release("a", core.CheckResult{Status: core.CheckPassed})

	got := awaitRun(t, run)
	if got.err == nil {
		t.Fatal("err = nil, want an unsatisfiable-edge failure")
	}
	if got.culprit != "" {
		t.Fatalf("culprit = %q, want none (no node reported a verdict)", got.culprit)
	}
	if len(got.rows) != 1 || got.rows[0].Name != "a" {
		t.Fatalf("rows = %+v, want only the node that actually ran", got.rows)
	}
}
