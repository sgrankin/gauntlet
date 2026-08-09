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
// The Tracker here is the D1 slice: it reads both refs and CAS-advances
// desired refs. It never fetches, never deletes a ref, never writes an
// observed ref, and never runs a command. That makes it purely
// level-triggered: every tick re-derives the whole picture from refs, so
// a missed tick, a crash, or a restart costs nothing but latency.
//
// graph.go adds D2's Scheduler — the node-graph runner an environment's
// deploy executes under, a deliberate second implementation of the queue's
// scheduler rather than an extraction of it. Nothing wires the two
// together yet: the lane runner that exports the desired revision, builds
// the CheckJobs, emits the deploy events, and CAS-advances the observed
// ref on an all-green graph is still to come, so a Tracker's behavior is
// unchanged and a daemon with no deploy config remains byte-identical to
// one built before any of this existed.
//
// It imports internal/core (for ErrCASStale) and the standard library,
// and deliberately NOT internal/config: cmd/gauntlet maps operator config
// onto Params, exactly as it does for internal/hooks.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// ref moves under it. Parsed and carried in D1; consumed in D2.
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
	// environment's deploy graph runs. Carried through D1 so the config
	// surface is complete and validated; nothing reads them until D2.
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
}

// Params configures New. Now and Log are optional.
type Params struct {
	// Environments is processed in this order, every tick.
	Environments []Environment
	Git          Git
	Now          func() time.Time
	Log          io.Writer
}

// Tracker keeps tracked environments' desired refs at their sources' tips.
// ReconcileOnce is not safe for concurrent use — cmd/gauntlet drives it
// from one goroutine — but Snapshot may be read from any goroutine.
type Tracker struct {
	envs []Environment
	git  Git
	now  func() time.Time
	log  io.Writer

	// lastAdvance is the tracker's only cross-tick memory: when each lane
	// last moved its desired ref. Everything else is re-derived from refs
	// on every pass. Touched only by the ReconcileOnce goroutine.
	lastAdvance map[string]time.Time

	snap atomic.Pointer[Snapshot]
}

// New builds a Tracker over p's environments.
func New(p Params) *Tracker {
	t := &Tracker{
		envs:        append([]Environment(nil), p.Environments...),
		git:         p.Git,
		now:         p.Now,
		log:         p.Log,
		lastAdvance: make(map[string]time.Time, len(p.Environments)),
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
// is the level-triggered contract, not a limitation to engineer around:
// in D1 nothing can advance an observed ref mid-pass anyway.
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
