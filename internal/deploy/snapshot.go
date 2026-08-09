package deploy

import "time"

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
