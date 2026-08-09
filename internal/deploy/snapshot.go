package deploy

import (
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Snapshot is an immutable, point-in-time view of every lane, published at
// the end of each successful ReconcileOnce pass — the same published-state
// pattern as queue.Snapshot, for the same reason: the dashboard and MCP
// surfaces read this instead of reaching into the tracker's own state.
//
// A pass that fails to read refs publishes nothing, so a stale Snapshot is
// always a COMPLETE past picture rather than a half-updated present one;
// At is how a reader tells how stale.
type Snapshot struct {
	At    time.Time
	Lanes []LaneState
}

// LaneState is one environment's published state. Every OID field is ""
// when the corresponding ref is absent or unresolvable — a state the
// tracker treats as ordinary, not exceptional.
type LaneState struct {
	Env  string
	Mode Mode

	// Source is the config spelling: "main" or "env=dev".
	Source string

	// SourceTip is what the source resolves to right now; Desired is the
	// environment's desired ref; Observed is the last revision it finished
	// deploying green.
	SourceTip string
	Desired   string
	Observed  string

	// InSync: the environment is running exactly what it was asked to run.
	InSync bool

	// Drift: the source has moved past what the environment was asked to
	// run. For a tracked lane this is transient (the next tick closes it,
	// or LastError says why it can't); for a manual lane it is the steady
	// state — the whole point of manual — and it is what an operator
	// promotes from.
	Drift bool

	// LastAdvance is when this tracker last moved this lane's desired ref;
	// the zero time if it never has (including a manual lane, which it
	// never will). Not persisted: a restart resets it.
	LastAdvance time.Time

	// LastError is the most recent tick's push failure for this lane, ""
	// when that tick was clean. A lost CAS lands here too — it is worth
	// showing, but it is not a failure to act on.
	LastError string

	// Running, Parked and LastResult are the lane RUNNER's published state
	// (run.go), each nil when it doesn't apply: no graph in flight, no
	// park, no run finished since this process started. They are freshly
	// allocated by the pass that publishes them and never mutated
	// afterwards, so a holder of a Snapshot may read them for as long as it
	// likes — the shallow copy Snapshot returns is safe precisely because
	// nothing here is ever written twice.
	Running    *LaneRun
	Parked     *LanePark
	LastResult *LaneResult
}

// LaneRun is the graph run currently in flight for a lane.
type LaneRun struct {
	// RunID is the deploy run's own ID (the one its events carry);
	// DeploySHA is the revision being deployed and DeployedSHA the observed
	// ref's value when it started ("" on a first-ever deploy).
	RunID       string
	DeploySHA   string
	DeployedSHA string

	StartedAt time.Time

	// Nodes are the node names executing RIGHT NOW — a live gauge, not a
	// record: it grows as nodes start and shrinks as they finish, and is
	// empty both before the first node starts and while the run is winding
	// down. The finished picture lives in the run's terminal record.
	Nodes []string
}

// LanePark is a lane parked at one desired revision: the level-triggered
// state a new desired SHA or an explicit Retry clears, and nothing else
// does. A parked lane runs no graph, however many ticks pass.
type LanePark struct {
	// SHA is the desired revision the lane parked AT — the park is about
	// that revision, which is why any other desired clears it.
	SHA string

	// RunID is the run that parked it; Outcome is OutcomeRejected (a red
	// node, or a spec rejection) or OutcomeError (daemon-side); Detail says
	// which, in words.
	RunID   string
	Outcome core.Outcome
	Detail  string

	At time.Time
}

// LaneResult summarizes the most recent finished graph run for a lane —
// enough for an overview card without holding the whole record. The record
// itself rides the terminal event to history and the detail page.
type LaneResult struct {
	RunID       string
	DeploySHA   string
	DeployedSHA string

	Outcome core.Outcome
	Culprit string
	Detail  string

	StartedAt time.Time
	EndedAt   time.Time
}

// Snapshot returns the most recently published view, or nil before the
// first successful pass. The returned value is the caller's own copy: the
// tracker never mutates a published Snapshot, and the Lanes slice is
// copied so a caller can hold or sort it freely.
func (t *Tracker) Snapshot() *Snapshot {
	s := t.snap.Load()
	if s == nil {
		return nil
	}
	out := *s
	out.Lanes = append([]LaneState(nil), s.Lanes...)
	return &out
}
