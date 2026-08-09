// Package deploy implements the deploy-ref tracker: the half of
// docs/design/deployment.md that keeps each environment's DESIRED ref
// pointing at what that environment is supposed to be running.
//
// Two refs describe an environment, and nothing else does:
//
//   - refs/heads/deploy/<env> — DESIRED, an ordinary branch. For a
//     TRACKED environment the daemon owns it and keeps it at the source's
//     tip; for a MANUAL one only a human (or porcelain) ever pushes it.
//   - refs/gauntlet/deployed/<env> — OBSERVED, daemon-owned, advanced only
//     once that environment's whole deploy graph has finished green. The
//     last revision known to be fully deployed.
//
// The Tracker here reads both refs, CAS-advances desired refs, and — with
// a lane runner wired in (run.go) — runs the deploy graph that closes the
// gap between them. It never fetches and never deletes a ref. Everything it
// does is level-triggered: every tick re-derives the whole picture from
// refs, so a missed tick, a crash, or a restart costs nothing but latency
// and, at worst, one re-run of a graph that was already in flight.
//
// graph.go adds D2's Scheduler — the node-graph runner an environment's
// deploy executes under, a deliberate second implementation of the queue's
// scheduler rather than an extraction of it. run.go is the LANE RUNNER that
// joins the two: when a lane's desired and observed refs disagree it pins
// the revision, reads that revision's OWN deploy graph out of its tree,
// exports it, runs the graph, and CAS-advances the observed ref only on an
// all-green run. A Tracker built without Params.Exec keeps D1's behavior
// exactly — it advances desired refs and runs nothing — and a daemon with
// no deploy config remains byte-identical to one built before any of this
// existed.
//
// The package imports internal/core and, in run.go only, internal/config —
// for the REPO spec (config.ParseChecks, CheckSpec.SelectDeployNodes), the
// same way internal/queue reads a candidate's own `.gauntlet.kdl`. It still
// deliberately does NOT read the daemon's OPERATOR config: cmd/gauntlet
// maps that onto Params (environments, the known-profile predicate, dirs),
// exactly as it does for internal/hooks. Re-implementing the deploy-spec
// gates outside this package would have put the fail-closed rules and their
// scenario tests in different packages, which is the drift they exist to
// prevent.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

const (
	// DesiredRefPrefix is the desired-ref namespace. It is under
	// refs/heads/ on purpose — a deploy ref is an ordinary branch an
	// operator can push, protect, and see in a host's branch UI — and it
	// is invisible to the queue: parseCandidateRef requires
	// "refs/heads/for/", so a deploy ref is never a candidate and never
	// even raises an ignored-ref event.
	DesiredRefPrefix = "refs/heads/deploy/"

	// ObservedRefPrefix is the observed-ref namespace: daemon-owned, well
	// outside refs/heads/ so it triggers no branch machinery anywhere.
	ObservedRefPrefix = "refs/gauntlet/deployed/"

	// FetchRefspec is what cmd/gauntlet hands gitx.WithFetchRefspecs when
	// any environment is configured, so observed refs ride the queue's
	// existing fetch at zero extra round trips. Local and remote names are
	// identical: these are not a remote-tracking view of someone else's
	// branches, they are this daemon's own bookkeeping mirrored back.
	FetchRefspec = "+refs/gauntlet/deployed/*:refs/gauntlet/deployed/*"
)

// DesiredRef returns env's desired ref name.
func DesiredRef(env string) string { return DesiredRefPrefix + env }

// ObservedRef returns env's observed ref name.
func ObservedRef(env string) string { return ObservedRefPrefix + env }

// Mode is whether the daemon owns an environment's desired ref.
type Mode string

const (
	// ModeTrack: the daemon keeps desired at the source's tip.
	ModeTrack Mode = "track"
	// ModeManual: the daemon NEVER writes this environment's desired ref —
	// not to create it, not to advance it. No desired ref means "never
	// deployed"; a human push is what creates one.
	ModeManual Mode = "manual"
)

// OnDesiredMove is what happens to an in-flight graph run when the desired
// ref moves under it: "finish" lets it complete against the revision it
// started on and reconciles again afterwards; "cancel" kills it now.
type OnDesiredMove string

const (
	OnMoveFinish OnDesiredMove = "finish"
	OnMoveCancel OnDesiredMove = "cancel"
)

// Environment is one deployment lane, already resolved from operator
// config: exactly one of SourceBranch/SourceEnv is set.
type Environment struct {
	Name string

	// SourceBranch names a branch whose tip this environment follows;
	// SourceEnv names another environment whose OBSERVED ref it follows.
	// The second form is what makes promotion work: prod's source is not
	// "whatever dev was asked to run" but "what dev has actually finished
	// deploying green".
	SourceBranch string
	SourceEnv    string

	Mode Mode

	// Nodes, MaxParallel and OnDesiredMove describe how this
	// environment's deploy graph runs: which subgraph it deploys (empty =
	// the whole graph, closed over `after` edges by the spec), how many of
	// its nodes may run at once, and what happens to an in-flight run when
	// the desired ref moves under it.
	Nodes         []string
	MaxParallel   int
	OnDesiredMove OnDesiredMove
}

// Git is the tracker's whole VCS surface, satisfied by *gitx.Repo. It is
// narrower than core.GitRepo and wider in exactly one place
// (ListLocalRefs), which is why it lives here rather than in core: adding
// that method to core.GitRepo would force internal/queue's fake to grow a
// method for a surface the queue never touches.
type Git interface {
	// ListRefs returns the remote's refs under their REMOTE names
	// (refs/heads/...), as of the most recent fetch.
	ListRefs(ctx context.Context) (map[string]string, error)

	// ListLocalRefs returns local refs under prefix, named verbatim — the
	// observed-ref mirror FetchRefspec brings down.
	ListLocalRefs(ctx context.Context, prefix string) (map[string]string, error)

	// CASUpdate compare-and-swaps a ref on the remote; oldOID "" asserts
	// the ref does not yet exist. Returns core.ErrCASStale when it does.
	CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error

	// The rest is the LANE RUNNER's surface (run.go), a strict subset of
	// core.GitRepo with identical semantics — *gitx.Repo satisfies both
	// with no adapter, and a Tracker built without Params.Exec never calls
	// any of it.

	// Pin anchors oid against garbage collection for the duration of one
	// graph run; Unpin releases it and is a no-op for an OID that was never
	// pinned, so terminal paths may unpin unconditionally.
	Pin(ctx context.Context, oid string) error
	Unpin(ctx context.Context, oid string) error

	// ReadFileFromTree reads path out of tree-ish (here: the desired
	// commit) without a checkout — how a revision's own deploy graph is
	// read from the revision being deployed.
	ReadFileFromTree(ctx context.Context, tree, path string) ([]byte, error)

	// ExportTree materializes tree's contents into dir for the graph's
	// nodes to run against; RestoreMtimes applies the daemon's
	// `export { mtimes "history" }` policy to that export.
	ExportTree(ctx context.Context, tree, dir string) error
	RestoreMtimes(ctx context.Context, commit, dir string) (core.MtimeStats, error)
}

// Params configures New. Now and Log are optional; so is the whole
// lane-runner block below — a Tracker built without Exec advances desired
// refs and runs nothing at all, which is D1's Tracker exactly.
type Params struct {
	// Environments is processed in this order, every tick.
	Environments []Environment
	Git          Git
	Now          func() time.Time
	Log          io.Writer

	// --- lane runner (run.go) ---

	// Exec runs each deploy node's command, exactly as it runs a check's:
	// the CheckJob carries the GAUNTLET_DEPLOY_* contract and the executor
	// owns rendering it. Nil disables graph execution entirely.
	Exec core.Executor

	// Slots is the daemon-wide execution cap (`max-executions`) every
	// bounded invocation shares — checks, hooks, image builds, and deploy
	// nodes — so a deploy burst and a check burst negotiate over the same
	// honest host capacity. nil means unlimited.
	Slots *core.Slots

	// Emit fans a deploy event out to the daemon's channels, built by cmd
	// from the same channel slice queue.Daemon uses. nil emits nothing.
	Emit func(context.Context, core.Event)

	// CheckSpec is the repo-spec path read out of the DEPLOYED revision's
	// tree (the daemon's `check-spec`, e.g. ".gauntlet.kdl"): the deploy
	// graph is declared in the same file the checks are.
	CheckSpec string

	// KnownExecutorProfile reports whether a named executor profile exists
	// on this daemon, mirroring queue.Config's field of the same name and
	// its nil semantics: nil means no named profiles are defined, so any
	// non-empty selection is unknown and rejects the spec.
	KnownExecutorProfile func(string) bool

	// WorkDir is the scratch root each run's tree export is created under
	// (one directory per graph run, removed when the run ends); LogDir is
	// the root per-node log files are written under, matching
	// queue.Config's fields of the same names. Empty LogDir writes no log
	// files, exactly as for a check.
	WorkDir string
	LogDir  string

	// HistoryMtimes mirrors queue.Config.HistoryMtimes: `export { mtimes
	// "history" }` is a daemon-wide statement about every materialization,
	// so it covers a deploy export too.
	HistoryMtimes bool

	// AutoRetryErrors enables the standing auto-retry-once budget for
	// OutcomeError parks (never for a red node), per (env, desired SHA) —
	// the same knob, and the same semantics, as queue.Config's field of the
	// same name.
	AutoRetryErrors bool
}

// Tracker keeps tracked environments' desired refs at their sources' tips
// and, when built with a lane runner (Params.Exec), runs each environment's
// deploy graph to close the gap between desired and observed.
//
// ReconcileOnce is not safe for concurrent use — cmd/gauntlet drives it
// from one goroutine, and the lane goroutines it starts NEVER call it — but
// Snapshot, Retry, CancelCurrent, Drain and Wait may be called from any
// goroutine.
type Tracker struct {
	envs []Environment
	git  Git
	now  func() time.Time
	log  io.Writer

	// Lane-runner configuration, all read-only after New.
	exec         core.Executor
	slots        *core.Slots
	notify       func(context.Context, core.Event)
	specPath     string
	knownProfile func(string) bool
	workDir      string
	logDir       string
	mtimes       bool
	autoRetry    bool

	// lastAdvance is the tracker's only cross-tick memory about desired
	// refs: when each lane last moved one. Everything else about a ref is
	// re-derived from refs on every pass. Touched only by the ReconcileOnce
	// goroutine.
	lastAdvance map[string]time.Time

	// mu guards lanes, the one piece of state a reconcile pass and the lane
	// goroutines both touch (run.go). Its scope is deliberately tiny: no
	// I/O, no command execution, and no callback into anything that could
	// take it again.
	mu    sync.Mutex
	lanes map[string]*lane

	// draining is the admission gate a graceful shutdown flips: in-flight
	// graphs finish, no new one starts. It lives under mu — rather than in
	// an atomic — so that Drain returning is a real barrier: every
	// admission decision after it sees the gate closed, which is what lets
	// Wait below rely on the lane counter only ever falling. wg counts lane
	// goroutines so Wait can block until they're done.
	draining bool
	wg       sync.WaitGroup

	snap atomic.Pointer[Snapshot]
}

// New builds a Tracker over p's environments.
func New(p Params) *Tracker {
	t := &Tracker{
		envs:         append([]Environment(nil), p.Environments...),
		git:          p.Git,
		now:          p.Now,
		log:          p.Log,
		exec:         p.Exec,
		slots:        p.Slots,
		notify:       p.Emit,
		specPath:     p.CheckSpec,
		knownProfile: p.KnownExecutorProfile,
		workDir:      p.WorkDir,
		logDir:       p.LogDir,
		mtimes:       p.HistoryMtimes,
		autoRetry:    p.AutoRetryErrors,
		lastAdvance:  make(map[string]time.Time, len(p.Environments)),
		lanes:        make(map[string]*lane, len(p.Environments)),
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.log == nil {
		t.log = io.Discard
	}
	return t
}

// ReconcileOnce runs one full pass: read every ref once, then process each
// environment in declaration order against that single snapshot.
//
// It returns an error ONLY when the ref snapshot itself fails — that is
// the case where the tracker knows nothing and must not act on it. A push
// that fails is a per-lane fact (recorded on the lane, logged unless it is
// an ordinary lost CAS) that must not stop the other lanes: environments
// are independent, and one unwritable ref is no reason to freeze the rest.
//
// Because refs are read once per pass and never re-read, a chain of
// environments (prod sourced from dev) propagates one link per tick. That
// is the level-triggered contract, not a limitation to engineer around: a
// lane goroutine that advances an observed ref mid-pass writes it on the
// REMOTE, and this pass is reading the local mirror, so the picture a pass
// acts on is internally consistent either way — the runner's own
// already-synced marker (run.go) is what keeps the window between that CAS
// and the next fetch from looking like fresh drift.
func (t *Tracker) ReconcileOnce(ctx context.Context) error {
	refs, err := t.git.ListRefs(ctx)
	if err != nil {
		return fmt.Errorf("deploy: list refs: %w", err)
	}
	observed, err := t.git.ListLocalRefs(ctx, ObservedRefPrefix)
	if err != nil {
		return fmt.Errorf("deploy: list observed refs: %w", err)
	}
	now := t.now()
	lanes := make([]LaneState, 0, len(t.envs))
	for _, env := range t.envs {
		lanes = append(lanes, t.reconcileLane(ctx, env, refs, observed, now))
	}
	t.snap.Store(&Snapshot{At: now, Lanes: lanes})
	return nil
}

// reconcileLane processes one environment against the pass's ref snapshot
// and returns its published state.
func (t *Tracker) reconcileLane(ctx context.Context, env Environment, refs, observed map[string]string, now time.Time) LaneState {
	lane := LaneState{
		Env:      env.Name,
		Mode:     env.Mode,
		Source:   sourceDisplay(env),
		Desired:  refs[DesiredRef(env.Name)],
		Observed: observed[ObservedRef(env.Name)],
	}
	// A branch source resolves against the fetched remote branches; an
	// env source resolves against that environment's OBSERVED ref. Either
	// way an unresolvable source is "" and makes the lane a no-op, never
	// an error: a source branch that doesn't exist yet, and a source
	// environment that has never deployed anything, are both ordinary
	// states of a config that is simply ahead of reality.
	if env.SourceBranch != "" {
		lane.SourceTip = refs["refs/heads/"+env.SourceBranch]
	} else if env.SourceEnv != "" {
		lane.SourceTip = observed[ObservedRef(env.SourceEnv)]
	}

	if env.Mode == ModeTrack && lane.SourceTip != "" && lane.Desired != lane.SourceTip {
		// Deliberately NO ancestry gate: desired is set to the source tip
		// verbatim, including BACKWARDS when the source was force-reset.
		// "Tracked" means "runs the source"; pinning an environment
		// during an incident is the config edit track -> manual, per the
		// design. An IsAncestor check here would silently refuse to
		// follow a rollback — the exact moment following matters most.
		err := t.git.CASUpdate(ctx, DesiredRef(env.Name), lane.Desired, lane.SourceTip)
		switch {
		case err == nil:
			lane.Desired = lane.SourceTip
			t.lastAdvance[env.Name] = now
		case errors.Is(err, core.ErrCASStale):
			// Someone else moved the ref between the snapshot and the
			// push. Not an error and not worth a log line: the next tick
			// re-derives from the winner's value and does the right thing
			// from there. Retrying inside this tick would only race the
			// same writer against a snapshot that is already stale.
			lane.LastError = err.Error()
		default:
			lane.LastError = err.Error()
			fmt.Fprintf(t.log, "deploy: %s: advance desired ref: %v\n", env.Name, err)
		}
	}

	lane.LastAdvance = t.lastAdvance[env.Name]
	lane.InSync = lane.Desired != "" && lane.Desired == lane.Observed
	lane.Drift = lane.SourceTip != "" && lane.Desired != lane.SourceTip

	// The lane runner (run.go): start a graph run for the drift this pass
	// found, cancel one whose desired moved under it, and publish whatever
	// the lane's goroutines have reported back since the last pass. A
	// D1-shaped Tracker (no Params.Exec) only ever does the publishing.
	t.stepLane(ctx, env, &lane)
	return lane
}

// sourceDisplay renders an environment's source the way it is written in
// config — "main" or "env=dev" — for the published lane state.
func sourceDisplay(env Environment) string {
	if env.SourceEnv != "" {
		return "env=" + env.SourceEnv
	}
	return env.SourceBranch
}
