// Scenario harness for the deploy-ref tracker, following
// internal/queue/script_test.go's pattern exactly: one Cmds set, two
// Setups. TestScriptFake drives an in-memory fakeRemote; TestScriptReal
// drives a real bare "remote" (internal/testutil) with a real *gitx.Repo
// built with WithFetchRefspecs(deploy.FetchRefspec), so the scenarios
// prove the same statements against genuine git plumbing — real
// force-with-lease, real refspec mirroring, real prune.
//
// The critical thing both Setups model identically is the remote/mirror
// split: `tick` refreshes this daemon's mirrors and THEN reconciles, the
// way production's queue fetch and the tracker's pass interleave. A write
// the tracker makes in tick N is therefore visible to it in tick N+1, not
// within tick N — which is exactly why an environment chain propagates one
// link per tick.
//
// Command vocabulary:
//
//	deploy-env <name> branch|env <source> track|manual
//	    Declare an environment and rebuild the tracker over the
//	    accumulated set. Every scenario's first commands.
//
//	push-branch <branch> <dir>
//	    Land new content on <branch> (a fresh commit, a fresh OID).
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
//	    for D2's all-green graph completion, the only thing that will ever
//	    write that ref for real.
//
//	arm-race <env> <branch>
//	    Have another writer win refs/heads/deploy/<env> at <branch>'s OID
//	    immediately before the tracker's next CAS on it.
//
//	tick
//	    Refresh mirrors, then run one ReconcileOnce. Fails the script if
//	    ReconcileOnce returns an error — per-lane push failures are not
//	    errors, so any error here is a genuine ref-snapshot failure.
//
//	assert-desired <env> <branch|none>
//	assert-observed <env> <branch|none>
//	assert-ref <refname> <branch|none>
//	    Assert a ref on the REMOTE equals <branch>'s current OID, or is
//	    absent ("none"). Assertions read ground truth, never a mirror.
//
//	assert-cas-attempts <n>
//	    Assert how many CAS pushes the tracker has attempted in total —
//	    the only way to state "that tick was a no-op" rather than merely
//	    "the ref still looks right".
package deploy_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/sgrankin/gauntlet/internal/deploy"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

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
}

// --- fake harness ---

type fakeScriptHarness struct {
	t    *testing.T
	rem  *fakeRemote
	git  *racingGit
	envs []deploy.Environment
	tr   *deploy.Tracker
}

var _ scriptHarness = (*fakeScriptHarness)(nil)

func newFakeScriptHarness(t *testing.T) *fakeScriptHarness {
	rem := newFakeRemote()
	h := &fakeScriptHarness{t: t, rem: rem}
	h.git = newRacingGit(fakeGit{r: rem}, rem.setRef)
	h.rebuild()
	return h
}

func (h *fakeScriptHarness) rebuild() {
	h.tr = deploy.New(deploy.Params{Environments: h.envs, Git: h.git})
}

func (h *fakeScriptHarness) addEnv(env deploy.Environment) {
	h.envs = append(h.envs, env)
	h.rebuild()
}

func (h *fakeScriptHarness) pushBranch(branch string, _ map[string]string) { h.rem.commit(branch) }

func (h *fakeScriptHarness) forceBranch(branch, from string) {
	h.rem.setRef("refs/heads/"+branch, h.rem.ref("refs/heads/"+from))
}

func (h *fakeScriptHarness) setRef(ref, oid string)  { h.rem.setRef(ref, oid) }
func (h *fakeScriptHarness) ref(name string) string  { return h.rem.ref(name) }
func (h *fakeScriptHarness) armRace(ref, oid string) { h.git.arm(ref, oid) }
func (h *fakeScriptHarness) casAttempts() int        { return h.git.casAttempts() }

func (h *fakeScriptHarness) tick() {
	h.rem.refresh()
	if err := h.tr.ReconcileOnce(context.Background()); err != nil {
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
	h := &realScriptHarness{t: t, remote: remote, repo: repo}
	h.git = newRacingGit(repo, h.setRef)
	h.rebuild()
	return h
}

func (h *realScriptHarness) rebuild() {
	h.tr = deploy.New(deploy.Params{Environments: h.envs, Git: h.git})
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

func (h *realScriptHarness) tick() {
	ctx := context.Background()
	if err := h.repo.Fetch(ctx); err != nil {
		h.t.Fatalf("Fetch: %v", err)
	}
	if err := h.tr.ReconcileOnce(ctx); err != nil {
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
		"deploy-env":          cmdDeployEnv,
		"push-branch":         cmdPushBranch,
		"force-branch":        cmdForceBranch,
		"push-deploy-ref":     cmdPushDeployRef,
		"set-observed":        cmdSetObserved,
		"arm-race":            cmdArmRace,
		"tick":                cmdTick,
		"assert-desired":      cmdAssertDesired,
		"assert-observed":     cmdAssertObserved,
		"assert-ref":          cmdAssertRef,
		"assert-cas-attempts": cmdAssertCASAttempts,
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
	if len(args) != 4 {
		ts.Fatalf("usage: deploy-env <name> branch|env <source> track|manual")
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
