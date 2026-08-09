// Scenario harness for the deploy tracker and its lane runner, following
// internal/queue/script_test.go's pattern exactly: one Cmds set, two
// Setups. TestScriptFake drives an in-memory fakeRemote; TestScriptReal
// drives a real bare "remote" (internal/testutil) with a real *gitx.Repo
// built with WithFetchRefspecs(deploy.FetchRefspec), so the scenarios
// prove the same statements against genuine git plumbing — real
// force-with-lease, real refspec mirroring, real prune, real tree exports.
//
// The critical thing both Setups model identically is the remote/mirror
// split: `tick` refreshes this daemon's mirrors and THEN reconciles, the
// way production's queue fetch and the tracker's pass interleave. A write
// the tracker makes in tick N is therefore visible to it in tick N+1, not
// within tick N — which is exactly why an environment chain propagates one
// link per tick, and why a green graph's observed-ref CAS needs the
// runner's own already-synced marker to not look like fresh drift.
//
// What is NOT faked, in either Setup, is the deploy graph itself: the
// scheduler, the spec read from the deployed revision's own tree, the
// export, the parks, and the observed-ref CAS are the real code under both.
// The one seam is the EXECUTOR — an executor.GatedExecutor, so a scenario
// steps a graph node by node instead of shelling out to real deploy
// commands. That mirrors the queue's real-git harness, which gates checks
// the same way for the same reason.
//
// Command vocabulary:
//
//	deploy-env <name> branch|env <source> track|manual [key=value ...]
//	    Declare an environment and rebuild the tracker over the
//	    accumulated set. Every scenario's first commands. Optional keys:
//	    max-parallel=<n>, on-desired-move=finish|cancel, nodes=a,b (the
//	    environment's node selection).
//
//	push-branch <branch> <dir>
//	    Land new content on <branch> (a fresh commit, a fresh OID). The
//	    directory's files ARE the revision's tree under both harnesses, so
//	    a "-- dir/.gauntlet.kdl --" section is how a scenario declares the
//	    deploy graph that revision will be deployed by.
//
//	force-branch <branch> <from-branch>
//	    Force-set <branch> to <from-branch>'s CURRENT OID, creating it if
//	    needed — how a scenario both snapshots an OID under a readable
//	    name and force-resets a branch backwards onto one.
//
//	push-deploy-ref <env> <branch>
//	    Force-push refs/heads/deploy/<env> to <branch>'s OID: a human (or
//	    porcelain) deploying an environment by hand.
//
//	set-observed <env> <branch>
//	    Force-set refs/gauntlet/deployed/<env> to <branch>'s OID: stands in
//	    for an all-green graph run when a scenario wants the state without
//	    the run.
//
//	arm-race <env> <branch>
//	    Have another writer win refs/heads/deploy/<env> at <branch>'s OID
//	    immediately before the tracker's next CAS on it.
//
//	tick
//	    Refresh mirrors, then run one ReconcileOnce. Fails the script if
//	    ReconcileOnce returns an error — per-lane push failures are not
//	    errors, so any error here is a genuine ref-snapshot failure. A tick
//	    is also what publishes a Snapshot, so the state assertions below
//	    read what the LAST tick saw: assert after a tick, not before one.
//
//	release-node <env> <node> [passed|failed|skipped|error]
//	    Deliver <node>'s result to the environment's in-flight graph run,
//	    blocking until that node has actually started. "passed" is the
//	    default; "skipped" is the result-file protocol's own verdict (and
//	    counts green); "error" is a daemon-side failure with no verdict at
//	    all, the shape auto-retry-once exists for.
//
//	await-deploy <env> <n>
//	    Block until <env> has recorded <n> finished graph runs — the
//	    rendezvous every post-run assertion needs, and the reason no
//	    scenario here sleeps.
//
//	restart-tracker
//	    Kill every in-flight graph run and rebuild the tracker from
//	    scratch: a crash, modelled honestly. Refs survive, all in-memory
//	    state (parks, budgets, synced markers) does not.
//
//	retry-deploy <env> / cancel-deploy <env>
//	    The D3 operator surfaces, called directly: clear <env>'s park, or
//	    cancel its in-flight run. Both assert they found something to act
//	    on; negate with ! to assert they didn't.
//
//	drain-deploys
//	    Flip the drain gate: in-flight runs finish, no new one starts.
//
//	assert-desired <env> <branch|none>
//	assert-observed <env> <branch|none>
//	assert-ref <refname> <branch|none>
//	    Assert a ref on the REMOTE equals <branch>'s current OID, or is
//	    absent ("none"). Assertions read ground truth, never a mirror.
//
//	assert-cas-attempts <n>
//	    Assert how many CAS pushes have been attempted in total — desired
//	    AND observed refs — the only way to state "that tick was a no-op"
//	    rather than merely "the ref still looks right".
//
//	[!] assert-deploy-running <env>
//	    Assert the last tick found a graph run in flight for <env> (or,
//	    negated, that it found none). Synchronous — stepLane publishes this
//	    inside the reconcile pass — which is what makes the negated form a
//	    real assertion rather than a race against an event that hasn't been
//	    emitted yet.
//
//	assert-deploy-starts <env> <n>
//	    Assert how many graph runs <env> has STARTED. The negative a park
//	    needs: "another tick passed and nothing re-ran".
//
//	assert-deploy-park <env> <branch|none> [detail-substring]
//	    Assert the lane is parked at <branch>'s OID (or not parked at all),
//	    optionally requiring a substring of the park's detail.
//
//	assert-deploy-outcome <env> <landed|rejected|error|skipped>
//	    Assert the outcome of <env>'s most recent finished run.
//
//	assert-node-status <env> <node> <passed|failed|skipped|blocked>
//	    Assert one node's row in <env>'s most recent terminal record.
//
//	assert-events <kind>...
//	    Assert every named kind appears, in the order given (as a
//	    subsequence of the whole captured stream). Kinds: deploy-started,
//	    deploy-node-finished, deploy-finished.
//
//	assert-pins <n>
//	    Assert how many GC pins (refs/gauntlet/pin/*) are live: one per
//	    in-flight graph run, back to zero once every terminal path has
//	    released them.
package deploy_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/deploy"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// scriptSpecPath is the repo-spec filename every scenario commits its
// deploy graph in — the daemon's `check-spec`, exactly as in production.
const scriptSpecPath = ".gauntlet.kdl"

// scriptTimeout bounds every rendezvous below. These are waits on real
// signals (a node registering with the gated executor, a terminal event
// arriving), not pacing sleeps: they return the instant the thing happens,
// and the timeout is only a safety net against a genuine hang.
const scriptTimeout = 30 * time.Second

// harnessKey is the Env.Values key Setup stores the scriptHarness under.
type harnessKey struct{}

// scriptHarness is the minimal surface the command DSL needs, satisfied by
// both the fake and the real harness.
type scriptHarness interface {
	// addEnv declares one more environment and rebuilds the tracker.
	addEnv(env deploy.Environment)

	// pushBranch lands files on branch as a new commit.
	pushBranch(branch string, files map[string]string)

	// forceBranch force-sets branch to from's current OID.
	forceBranch(branch, from string)

	// setRef force-writes ref on the remote (deploy/observed refs).
	setRef(ref, oid string)

	// ref returns a ref's OID on the REMOTE, "" if absent.
	ref(name string) string

	// armRace makes another writer win ref at oid before the next CAS.
	armRace(ref, oid string)

	// tick refreshes this daemon's mirrors, then reconciles once.
	tick()

	// casAttempts is the running count of CAS pushes attempted.
	casAttempts() int

	// restart kills every in-flight graph run and rebuilds the tracker
	// over the same refs — a crash and a fresh boot.
	restart()

	// pins is the number of live GC pins in this daemon's repo.
	pins() int

	// kit exposes the lane-runner test fixtures shared by both harnesses;
	// tracker is the CURRENT Tracker, which restart replaces.
	kit() *laneKit
	tracker() *deploy.Tracker
}

// --- lane-runner fixtures, shared by both harnesses ---

// laneKit is everything the LANE RUNNER half of a harness needs, identical
// under both Setups: a gated executor (the one seam), the captured event
// stream, the scratch dirs, and the context lane goroutines run under —
// whose cancellation is how restart-tracker models a crash.
type laneKit struct {
	t      *testing.T
	ex     *executor.GatedExecutor
	events *eventLog
	work   string
	logs   string

	ctx    context.Context
	cancel context.CancelFunc
}

func newLaneKit(t *testing.T) *laneKit {
	dir := t.TempDir()
	k := &laneKit{
		t:      t,
		ex:     executor.NewGatedExecutor(),
		events: newEventLog(t),
		work:   filepath.Join(dir, "deploys"),
		logs:   filepath.Join(dir, "logs"),
	}
	if err := os.MkdirAll(k.work, 0o755); err != nil {
		t.Fatalf("laneKit: %v", err)
	}
	k.ctx, k.cancel = context.WithCancel(context.Background())
	t.Cleanup(k.cancel)
	return k
}

// params fills the lane-runner half of deploy.Params. AutoRetryErrors is on
// (the daemon's own default) so scenarios exercise the shipped behavior.
func (k *laneKit) params(p deploy.Params) deploy.Params {
	p.Exec = k.ex
	p.Emit = k.events.emit
	p.CheckSpec = scriptSpecPath
	p.WorkDir = k.work
	p.LogDir = k.logs
	p.AutoRetryErrors = true
	return p
}

// crash cancels every in-flight lane goroutine and hands back a fresh
// context for the rebuilt tracker. The gated executor is deliberately kept:
// its keys are (RunID, Name), and a fresh run mints a fresh RunID, so the
// abandoned run's channels can never be mistaken for the new one's.
func (k *laneKit) crash() {
	k.cancel()
	k.ctx, k.cancel = context.WithCancel(context.Background())
	k.t.Cleanup(k.cancel)
}

// eventLog captures every event the tracker emits and, at the emit site,
// holds each one to core.ValidateEvent — the emit-site contract in
// executable form (core/events.go), applied to every shape this package
// produces in every scenario, rather than to whichever shapes a bespoke
// test remembered to build.
type eventLog struct {
	t *testing.T

	mu      sync.Mutex
	events  []core.Event
	changed chan struct{} // closed and replaced on every append
}

func newEventLog(t *testing.T) *eventLog {
	return &eventLog{t: t, changed: make(chan struct{})}
}

func (l *eventLog) emit(_ context.Context, ev core.Event) {
	if err := core.ValidateEvent(ev); err != nil {
		// Errorf, not Fatalf: this runs on a lane goroutine.
		l.t.Errorf("emitted event fails the emit-site contract: %v (%+v)", err, ev)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *eventLog) all() []core.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]core.Event(nil), l.events...)
}

// await blocks until pred is satisfied by the captured stream, or the
// timeout expires. It re-evaluates on every append rather than polling, so
// a scenario's pace is set by the daemon's own progress.
func (l *eventLog) await(pred func([]core.Event) bool) bool {
	deadline := time.After(scriptTimeout)
	for {
		l.mu.Lock()
		ok := pred(l.events)
		changed := l.changed
		l.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-changed:
		case <-deadline:
			return false
		}
	}
}

// countKind counts events of one kind for one environment.
func countKind(events []core.Event, kind core.EventKind, env string) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind && ev.DeployEnv == env {
			n++
		}
	}
	return n
}

// liveRunID returns the run ID of env's currently-live graph run: the
// newest started run with no terminal event yet. "" when the lane has
// nothing in flight — which is also what makes waiting for it correct
// rather than racy, since a run that already finished never matches.
func liveRunID(events []core.Event, env string) string {
	finished := map[string]bool{}
	for _, ev := range events {
		if ev.Kind == core.EventDeployFinished && ev.DeployEnv == env {
			finished[ev.RunID] = true
		}
	}
	live := ""
	for _, ev := range events {
		if ev.Kind == core.EventDeployStarted && ev.DeployEnv == env && !finished[ev.RunID] {
			live = ev.RunID
		}
	}
	return live
}

// lastRecord returns env's most recent terminal DeployRecord, or nil.
func lastRecord(events []core.Event, env string) *core.DeployRecord {
	var rec *core.DeployRecord
	for _, ev := range events {
		if ev.Kind == core.EventDeployFinished && ev.DeployEnv == env && ev.Deploy != nil {
			rec = ev.Deploy
		}
	}
	return rec
}

// --- fake harness ---

type fakeScriptHarness struct {
	t    *testing.T
	rem  *fakeRemote
	git  *racingGit
	envs []deploy.Environment
	tr   *deploy.Tracker
	k    *laneKit
}

var _ scriptHarness = (*fakeScriptHarness)(nil)

func newFakeScriptHarness(t *testing.T) *fakeScriptHarness {
	rem := newFakeRemote()
	h := &fakeScriptHarness{t: t, rem: rem, k: newLaneKit(t)}
	h.git = newRacingGit(fakeGit{r: rem}, rem.setRef)
	h.rebuild()
	return h
}

func (h *fakeScriptHarness) rebuild() {
	h.tr = deploy.New(h.k.params(deploy.Params{Environments: h.envs, Git: h.git}))
}

func (h *fakeScriptHarness) addEnv(env deploy.Environment) {
	h.envs = append(h.envs, env)
	h.rebuild()
}

func (h *fakeScriptHarness) pushBranch(branch string, files map[string]string) {
	h.rem.commit(branch, files)
}

func (h *fakeScriptHarness) forceBranch(branch, from string) {
	h.rem.setRef("refs/heads/"+branch, h.rem.ref("refs/heads/"+from))
}

func (h *fakeScriptHarness) setRef(ref, oid string)  { h.rem.setRef(ref, oid) }
func (h *fakeScriptHarness) ref(name string) string  { return h.rem.ref(name) }
func (h *fakeScriptHarness) armRace(ref, oid string) { h.git.arm(ref, oid) }
func (h *fakeScriptHarness) casAttempts() int        { return h.git.casAttempts() }
func (h *fakeScriptHarness) pins() int               { return h.rem.pinCount() }
func (h *fakeScriptHarness) kit() *laneKit           { return h.k }

func (h *fakeScriptHarness) tracker() *deploy.Tracker { return h.tr }

func (h *fakeScriptHarness) restart() {
	h.k.crash()
	h.rebuild()
}

func (h *fakeScriptHarness) tick() {
	h.rem.refresh()
	if err := h.tr.ReconcileOnce(h.k.ctx); err != nil {
		h.t.Fatalf("ReconcileOnce: %v", err)
	}
}

// --- real harness ---

type realScriptHarness struct {
	t      *testing.T
	remote *testutil.Remote
	repo   *gitx.Repo
	git    *racingGit
	envs   []deploy.Environment
	tr     *deploy.Tracker
	k      *laneKit
}

var _ scriptHarness = (*realScriptHarness)(nil)

// The cross-package seam this whole slice rests on: cmd/gauntlet hands
// the daemon's own *gitx.Repo to the tracker, so *gitx.Repo must satisfy
// deploy.Git with no adapter in between.
var _ deploy.Git = (*gitx.Repo)(nil)

func newRealScriptHarness(t *testing.T) *realScriptHarness {
	remote := testutil.NewRemote(t)
	repo, err := gitx.New(context.Background(), remote.BareClone(), remote.Dir,
		gitx.WithFetchRefspecs(deploy.FetchRefspec))
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	h := &realScriptHarness{t: t, remote: remote, repo: repo, k: newLaneKit(t)}
	h.git = newRacingGit(repo, h.setRef)
	h.rebuild()
	return h
}

func (h *realScriptHarness) rebuild() {
	h.tr = deploy.New(h.k.params(deploy.Params{Environments: h.envs, Git: h.git}))
}

func (h *realScriptHarness) addEnv(env deploy.Environment) {
	h.envs = append(h.envs, env)
	h.rebuild()
}

func (h *realScriptHarness) pushBranch(branch string, files map[string]string) {
	h.remote.Seed(branch, files)
}

func (h *realScriptHarness) forceBranch(branch, from string) {
	h.remote.SetRef("refs/heads/"+branch, h.remote.Ref("refs/heads/"+from))
}

func (h *realScriptHarness) setRef(ref, oid string)  { h.remote.SetRef(ref, oid) }
func (h *realScriptHarness) ref(name string) string  { return h.remote.Ref(name) }
func (h *realScriptHarness) armRace(ref, oid string) { h.git.arm(ref, oid) }
func (h *realScriptHarness) casAttempts() int        { return h.git.casAttempts() }
func (h *realScriptHarness) kit() *laneKit           { return h.k }

func (h *realScriptHarness) tracker() *deploy.Tracker { return h.tr }

func (h *realScriptHarness) pins() int {
	refs, err := h.repo.ListLocalRefs(context.Background(), "refs/gauntlet/pin/")
	if err != nil {
		h.t.Fatalf("ListLocalRefs(pins): %v", err)
	}
	return len(refs)
}

func (h *realScriptHarness) restart() {
	h.k.crash()
	h.rebuild()
}

func (h *realScriptHarness) tick() {
	if err := h.repo.Fetch(h.k.ctx); err != nil {
		h.t.Fatalf("Fetch: %v", err)
	}
	if err := h.tr.ReconcileOnce(h.k.ctx); err != nil {
		h.t.Fatalf("ReconcileOnce: %v", err)
	}
}

// --- suites ---

func TestScriptFake(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:  "testdata/script",
		Cmds: commands(),
		Setup: func(env *testscript.Env) error {
			env.Values[harnessKey{}] = scriptHarness(newFakeScriptHarness(t))
			return nil
		},
	})
}

// TestScriptReal runs every scenario against real git. Under -race
// (raceScenariosSerial, race_test.go) scenarios run serially through
// serialScriptT — the real harness forks `git` in every command, and
// testscript's unconditional per-scenario t.Parallel() otherwise puts
// dozens of concurrent forks under TSan. See internal/queue/race_test.go
// for the full diagnosis; this package copies the mitigation rather than
// re-deriving it.
func TestScriptReal(t *testing.T) {
	params := testscript.Params{
		Dir:  "testdata/script",
		Cmds: commands(),
		Setup: func(env *testscript.Env) error {
			env.Values[harnessKey{}] = scriptHarness(newRealScriptHarness(t))
			return nil
		},
	}
	if raceScenariosSerial {
		testscript.RunT(serialScriptT{t}, params)
	} else {
		testscript.Run(t, params)
	}
}

// serialScriptT adapts *testing.T to testscript.T exactly like
// testscript's own shim, except Parallel is a deliberate no-op.
type serialScriptT struct{ *testing.T }

func (t serialScriptT) Run(name string, f func(testscript.T)) {
	t.T.Run(name, func(t *testing.T) { f(serialScriptT{t}) })
}

func (t serialScriptT) Parallel() {}

func (t serialScriptT) Verbose() bool { return testing.Verbose() }

// --- commands ---

func commands() map[string]func(ts *testscript.TestScript, neg bool, args []string) {
	return map[string]func(ts *testscript.TestScript, neg bool, args []string){
		"deploy-env":            cmdDeployEnv,
		"push-branch":           cmdPushBranch,
		"force-branch":          cmdForceBranch,
		"push-deploy-ref":       cmdPushDeployRef,
		"set-observed":          cmdSetObserved,
		"arm-race":              cmdArmRace,
		"tick":                  cmdTick,
		"release-node":          cmdReleaseNode,
		"await-deploy":          cmdAwaitDeploy,
		"restart-tracker":       cmdRestartTracker,
		"retry-deploy":          cmdRetryDeploy,
		"cancel-deploy":         cmdCancelDeploy,
		"drain-deploys":         cmdDrainDeploys,
		"assert-desired":        cmdAssertDesired,
		"assert-observed":       cmdAssertObserved,
		"assert-ref":            cmdAssertRef,
		"assert-cas-attempts":   cmdAssertCASAttempts,
		"assert-deploy-running": cmdAssertDeployRunning,
		"assert-deploy-starts":  cmdAssertDeployStarts,
		"assert-deploy-park":    cmdAssertDeployPark,
		"assert-deploy-outcome": cmdAssertDeployOutcome,
		"assert-node-status":    cmdAssertNodeStatus,
		"assert-events":         cmdAssertEvents,
		"assert-pins":           cmdAssertPins,
	}
}

func getHarness(ts *testscript.TestScript) scriptHarness {
	h, _ := ts.Value(harnessKey{}).(scriptHarness)
	if h == nil {
		ts.Fatalf("no scriptHarness registered; Setup did not set harnessKey{} in Env.Values")
	}
	return h
}

// readFilesDir reads every regular file under dir (relative to the
// script's directory) into a path -> content map.
func readFilesDir(ts *testscript.TestScript, dir string) map[string]string {
	abs := ts.MkAbs(dir)
	info, err := os.Stat(abs)
	if err != nil {
		ts.Fatalf("read files dir %s: %v", dir, err)
	}
	if !info.IsDir() {
		ts.Fatalf("read files dir %s: not a directory", dir)
	}
	out := map[string]string{}
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		ts.Fatalf("read files dir %s: %v", dir, err)
	}
	return out
}

func cmdDeployEnv(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("deploy-env does not support !")
	}
	if len(args) < 4 {
		ts.Fatalf("usage: deploy-env <name> branch|env <source> track|manual [key=value ...]")
	}
	env := deploy.Environment{
		Name:          args[0],
		MaxParallel:   1,
		OnDesiredMove: deploy.OnMoveFinish,
	}
	switch args[1] {
	case "branch":
		env.SourceBranch = args[2]
	case "env":
		env.SourceEnv = args[2]
	default:
		ts.Fatalf("deploy-env: source kind must be \"branch\" or \"env\", got %q", args[1])
	}
	switch args[3] {
	case "track":
		env.Mode = deploy.ModeTrack
	case "manual":
		env.Mode = deploy.ModeManual
	default:
		ts.Fatalf("deploy-env: mode must be \"track\" or \"manual\", got %q", args[3])
	}
	for _, arg := range args[4:] {
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			ts.Fatalf("deploy-env: extra arguments are key=value, got %q", arg)
		}
		switch key {
		case "max-parallel":
			n, err := strconv.Atoi(value)
			if err != nil {
				ts.Fatalf("deploy-env: max-parallel: %v", err)
			}
			env.MaxParallel = n
		case "on-desired-move":
			switch value {
			case "finish":
				env.OnDesiredMove = deploy.OnMoveFinish
			case "cancel":
				env.OnDesiredMove = deploy.OnMoveCancel
			default:
				ts.Fatalf("deploy-env: on-desired-move must be finish|cancel, got %q", value)
			}
		case "nodes":
			env.Nodes = strings.Split(value, ",")
		default:
			ts.Fatalf("deploy-env: unknown key %q", key)
		}
	}
	getHarness(ts).addEnv(env)
}

func cmdPushBranch(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("push-branch does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: push-branch <branch> <dir>")
	}
	h := getHarness(ts)
	h.pushBranch(args[0], readFilesDir(ts, args[1]))
	ts.Logf("push-branch: %s -> %s", args[0], h.ref("refs/heads/"+args[0]))
}

func cmdForceBranch(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("force-branch does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: force-branch <branch> <from-branch>")
	}
	h := getHarness(ts)
	if h.ref("refs/heads/"+args[1]) == "" {
		ts.Fatalf("force-branch: source branch %q does not exist", args[1])
	}
	h.forceBranch(args[0], args[1])
}

func cmdPushDeployRef(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("push-deploy-ref does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: push-deploy-ref <env> <branch>")
	}
	h := getHarness(ts)
	h.setRef(deploy.DesiredRef(args[0]), mustBranch(ts, h, args[1]))
}

func cmdSetObserved(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("set-observed does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: set-observed <env> <branch>")
	}
	h := getHarness(ts)
	h.setRef(deploy.ObservedRef(args[0]), mustBranch(ts, h, args[1]))
}

func cmdArmRace(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("arm-race does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: arm-race <env> <branch>")
	}
	h := getHarness(ts)
	h.armRace(deploy.DesiredRef(args[0]), mustBranch(ts, h, args[1]))
}

func cmdTick(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("tick does not support !")
	}
	if len(args) != 0 {
		ts.Fatalf("usage: tick")
	}
	getHarness(ts).tick()
}

func cmdReleaseNode(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("release-node does not support !")
	}
	if len(args) < 2 || len(args) > 3 {
		ts.Fatalf("usage: release-node <env> <node> [passed|failed|skipped|error]")
	}
	env, node := args[0], args[1]
	verdict := "passed"
	if len(args) == 3 {
		verdict = args[2]
	}
	res := core.CheckResult{Name: node}
	switch verdict {
	case "passed":
		res.Status = core.CheckPassed
	case "failed":
		res.Status = core.CheckFailed
	case "skipped":
		// The result-file protocol's own verdict: "this revision needs
		// nothing from me". It counts green and satisfies `after` edges.
		res.Status = core.CheckSkipped
	case "error":
		// No verdict at all — a daemon-side failure (executor unreachable),
		// which is the only shape auto-retry-once applies to.
		res.Err = fmt.Errorf("executor unreachable")
	default:
		ts.Fatalf("release-node: unknown verdict %q, want passed|failed|skipped|error", verdict)
	}

	k := getHarness(ts).kit()
	runID := ""
	if !k.events.await(func(evs []core.Event) bool {
		runID = liveRunID(evs, env)
		return runID != ""
	}) {
		ts.Fatalf("release-node: %s has no graph run in flight", env)
	}
	select {
	case <-k.ex.Started(runID, node):
	case <-time.After(scriptTimeout):
		ts.Fatalf("release-node: node %q of run %s never started", node, runID)
	}
	k.ex.Release(runID, node, res)
}

func cmdAwaitDeploy(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("await-deploy does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: await-deploy <env> <n>")
	}
	want, err := strconv.Atoi(args[1])
	if err != nil {
		ts.Fatalf("await-deploy: %v", err)
	}
	k := getHarness(ts).kit()
	if !k.events.await(func(evs []core.Event) bool {
		return countKind(evs, core.EventDeployFinished, args[0]) >= want
	}) {
		ts.Fatalf("await-deploy: %s finished %d runs, want %d",
			args[0], countKind(k.events.all(), core.EventDeployFinished, args[0]), want)
	}
}

func cmdRestartTracker(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("restart-tracker does not support !")
	}
	if len(args) != 0 {
		ts.Fatalf("usage: restart-tracker")
	}
	getHarness(ts).restart()
}

func cmdRetryDeploy(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: [!] retry-deploy <env>")
	}
	got := getHarness(ts).tracker().Retry(args[0])
	switch {
	case got && neg:
		ts.Fatalf("retry-deploy %s cleared a park, want none to clear", args[0])
	case !got && !neg:
		ts.Fatalf("retry-deploy %s found no park to clear", args[0])
	}
}

func cmdCancelDeploy(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: [!] cancel-deploy <env>")
	}
	got := getHarness(ts).tracker().CancelCurrent(args[0])
	switch {
	case got && neg:
		ts.Fatalf("cancel-deploy %s cancelled a run, want none in flight", args[0])
	case !got && !neg:
		ts.Fatalf("cancel-deploy %s found no run in flight", args[0])
	}
}

func cmdDrainDeploys(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("drain-deploys does not support !")
	}
	if len(args) != 0 {
		ts.Fatalf("usage: drain-deploys")
	}
	getHarness(ts).tracker().Drain()
}

func cmdAssertDesired(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: assert-desired <env> <branch|none>")
	}
	assertRefIs(ts, neg, deploy.DesiredRef(args[0]), args[1])
}

func cmdAssertObserved(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: assert-observed <env> <branch|none>")
	}
	assertRefIs(ts, neg, deploy.ObservedRef(args[0]), args[1])
}

func cmdAssertRef(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: assert-ref <refname> <branch|none>")
	}
	assertRefIs(ts, neg, args[0], args[1])
}

// assertRefIs compares refName's OID on the remote against want, which is
// either "none" (the ref must not exist) or a branch name whose current
// OID it must equal.
func assertRefIs(ts *testscript.TestScript, neg bool, refName, want string) {
	h := getHarness(ts)
	got := h.ref(refName)
	var wantOID string
	if want != "none" {
		wantOID = mustBranch(ts, h, want)
	}
	equal := got == wantOID
	switch {
	case equal && neg && want == "none":
		ts.Fatalf("%s is absent, want it to exist", refName)
	case equal && neg:
		ts.Fatalf("%s = %s (branch %s), want it to differ", refName, got, want)
	case !equal && !neg && want == "none":
		ts.Fatalf("%s = %s, want it absent", refName, got)
	case !equal && !neg:
		ts.Fatalf("%s = %q, want branch %s's OID %q", refName, got, want, wantOID)
	}
}

func cmdAssertCASAttempts(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-cas-attempts does not support !")
	}
	if len(args) != 1 {
		ts.Fatalf("usage: assert-cas-attempts <n>")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("assert-cas-attempts: %v", err)
	}
	if got := getHarness(ts).casAttempts(); got != want {
		ts.Fatalf("CAS attempts = %d, want %d", got, want)
	}
}

func cmdAssertDeployRunning(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: [!] assert-deploy-running <env>")
	}
	lane := laneState(ts, getHarness(ts), args[0])
	switch {
	case lane.Running != nil && neg:
		ts.Fatalf("%s is running graph %s (%v), want the lane idle", args[0], lane.Running.RunID, lane.Running.Nodes)
	case lane.Running == nil && !neg:
		ts.Fatalf("%s has no graph run in flight", args[0])
	}
}

func cmdAssertDeployStarts(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-deploy-starts does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: assert-deploy-starts <env> <n>")
	}
	want, err := strconv.Atoi(args[1])
	if err != nil {
		ts.Fatalf("assert-deploy-starts: %v", err)
	}
	got := countKind(getHarness(ts).kit().events.all(), core.EventDeployStarted, args[0])
	if got != want {
		ts.Fatalf("%s started %d graph runs, want %d", args[0], got, want)
	}
}

func cmdAssertDeployPark(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-deploy-park does not support ! (use \"none\")")
	}
	if len(args) < 2 || len(args) > 3 {
		ts.Fatalf("usage: assert-deploy-park <env> <branch|none> [detail-substring]")
	}
	h := getHarness(ts)
	lane := laneState(ts, h, args[0])
	switch {
	case args[1] == "none":
		if lane.Parked != nil {
			ts.Fatalf("%s is parked at %s (%s), want no park", args[0], lane.Parked.SHA, lane.Parked.Detail)
		}
		return
	case lane.Parked == nil:
		ts.Fatalf("%s is not parked, want a park at branch %s", args[0], args[1])
	}
	if want := mustBranch(ts, h, args[1]); lane.Parked.SHA != want {
		ts.Fatalf("%s is parked at %s, want branch %s's OID %s", args[0], lane.Parked.SHA, args[1], want)
	}
	if len(args) == 3 && !strings.Contains(lane.Parked.Detail, args[2]) {
		ts.Fatalf("%s park detail = %q, want it to contain %q", args[0], lane.Parked.Detail, args[2])
	}
}

func cmdAssertDeployOutcome(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-deploy-outcome does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: assert-deploy-outcome <env> <landed|rejected|error|skipped>")
	}
	rec := lastRecord(getHarness(ts).kit().events.all(), args[0])
	if rec == nil {
		ts.Fatalf("%s has no finished graph run", args[0])
	}
	if got := outcomeName(rec.Outcome); got != args[1] {
		ts.Fatalf("%s last run outcome = %s (%s), want %s", args[0], got, rec.Detail, args[1])
	}
}

func cmdAssertNodeStatus(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-node-status does not support !")
	}
	if len(args) != 3 {
		ts.Fatalf("usage: assert-node-status <env> <node> <passed|failed|skipped|blocked>")
	}
	rec := lastRecord(getHarness(ts).kit().events.all(), args[0])
	if rec == nil {
		ts.Fatalf("%s has no finished graph run", args[0])
	}
	for _, row := range rec.Nodes {
		if row.Name != args[1] {
			continue
		}
		if got := statusName(row); got != args[2] {
			ts.Fatalf("%s node %q = %s, want %s", args[0], args[1], got, args[2])
		}
		return
	}
	ts.Fatalf("%s last run has no row for node %q", args[0], args[1])
}

func cmdAssertEvents(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-events does not support !")
	}
	if len(args) == 0 {
		ts.Fatalf("usage: assert-events <kind>...")
	}
	events := getHarness(ts).kit().events.all()
	i := 0
	for _, want := range args {
		kind := eventKind(ts, want)
		found := false
		for ; i < len(events); i++ {
			if events[i].Kind == kind {
				i++
				found = true
				break
			}
		}
		if !found {
			ts.Fatalf("no %s event after the ones already matched (stream: %s)", want, eventKinds(events))
		}
	}
}

func cmdAssertPins(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-pins does not support !")
	}
	if len(args) != 1 {
		ts.Fatalf("usage: assert-pins <n>")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("assert-pins: %v", err)
	}
	if got := getHarness(ts).pins(); got != want {
		ts.Fatalf("live GC pins = %d, want %d", got, want)
	}
}

// laneState returns env's lane from the LAST published Snapshot — which is
// to say, from the last tick. Assertions about parks and runs therefore
// describe what the tracker itself last saw, never a peek at state it
// hasn't published.
func laneState(ts *testscript.TestScript, h scriptHarness, env string) deploy.LaneState {
	snap := h.tracker().Snapshot()
	if snap == nil {
		ts.Fatalf("no Snapshot published yet; run a tick first")
	}
	for _, lane := range snap.Lanes {
		if lane.Env == env {
			return lane
		}
	}
	ts.Fatalf("no lane for environment %q", env)
	return deploy.LaneState{}
}

func eventKind(ts *testscript.TestScript, name string) core.EventKind {
	switch name {
	case "deploy-started":
		return core.EventDeployStarted
	case "deploy-node-finished":
		return core.EventDeployNodeFinished
	case "deploy-finished":
		return core.EventDeployFinished
	}
	ts.Fatalf("unknown event kind %q", name)
	return 0
}

func eventKinds(events []core.Event) string {
	var names []string
	for _, ev := range events {
		switch ev.Kind {
		case core.EventDeployStarted:
			names = append(names, "deploy-started")
		case core.EventDeployNodeFinished:
			names = append(names, "deploy-node-finished")
		case core.EventDeployFinished:
			names = append(names, "deploy-finished")
		default:
			names = append(names, fmt.Sprintf("kind(%d)", int(ev.Kind)))
		}
	}
	return strings.Join(names, " ")
}

func outcomeName(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "landed"
	case core.OutcomeRejected:
		return "rejected"
	case core.OutcomeError:
		return "error"
	case core.OutcomeSkipped:
		return "skipped"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

func statusName(res core.CheckResult) string {
	if res.Err != nil {
		return "error"
	}
	switch res.Status {
	case core.CheckPassed:
		return "passed"
	case core.CheckSkipped:
		return "skipped"
	case core.CheckBlocked:
		return "blocked"
	default:
		return "failed"
	}
}

// mustBranch resolves a branch name to its OID on the remote, failing the
// script if it doesn't exist — a scenario naming a branch it never created
// is a script bug, and silently comparing against "" would hide it.
func mustBranch(ts *testscript.TestScript, h scriptHarness, branch string) string {
	oid := h.ref("refs/heads/" + branch)
	if oid == "" {
		ts.Fatalf("branch %q does not exist on the remote", branch)
	}
	return oid
}
