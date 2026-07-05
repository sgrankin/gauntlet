package queue

// This file holds the published, read-only view of the reconcile loop's
// live state (docs/plans/phase23.md §2.1): the dashboard (D2) and the
// history store's depth sampler (D1) both read Daemon.Snapshot() instead of
// poking the reconcile loop's internals, keeping the queue ignorant of
// either (Invariant 8).

import (
	"sort"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

// Snapshot is an immutable, point-in-time view published at the end of each
// ReconcileOnce pass. Safe to read from any goroutine: built by deep-copying
// out of reconcile state (only the reconcile goroutine ever mutates that
// state) and never mutated after publication — Daemon.Snapshot callers get
// their own copy of every slice, never one still owned by the reconcile
// loop.
type Snapshot struct {
	At      time.Time
	Targets []TargetSnapshot
}

// TargetSnapshot is one target's live queue state.
type TargetSnapshot struct {
	Name      string
	Branch    string
	TargetTip string         // "" if the target branch doesn't exist yet
	InFlight  *RunSnapshot   // nil when the lane is idle
	Waiting   []WaitingEntry // FIFO order; excludes in-flight and parked refs
	Parked    []ParkedEntry  // refs parked at their current SHA, with reason
}

// RunSnapshot is the in-flight run for one target, if any.
type RunSnapshot struct {
	Candidate core.Candidate
	RunID     string
	BaseOID   string
	MergeSHA  string
	Done      []core.CheckResult // checks finished so far, in run order
	Current   *CurrentCheck      // the check running now; nil between checks
	StartedAt time.Time
}

// CurrentCheck is the check running right now within an in-flight run.
type CurrentCheck struct {
	Name      string
	StartedAt time.Time // elapsed = snapshot.At.Sub(StartedAt)
}

// WaitingEntry is a queued-but-not-yet-picked candidate.
type WaitingEntry struct {
	Candidate core.Candidate
	Seq       int64 // FIFO sequence (Daemon.order); lower = earlier
}

// ParkedEntry is a candidate parked at its current SHA (docs/plans/phase1.md
// §9.1): it will not be re-tested until the ref's SHA changes, the ref
// vanishes, or a CommandRetry clears it.
type ParkedEntry struct {
	Candidate core.Candidate
	Outcome   core.Outcome // why it parked (rejected/conflict/error)
	Reason    string       // RunRecord.Detail at park time
	At        time.Time
}

// buildSnapshot assembles a fresh Snapshot from the reconcile goroutine's
// in-memory state and refs (this tick's ListRefs result). Called once, at
// the end of a successful ReconcileOnce pass, on the reconcile goroutine —
// the same goroutine that owns d.order/d.done/d.runs, so no locking is
// needed here; every value copied out is independent of what it was copied
// from by the time this returns.
func (d *Daemon) buildSnapshot(refs map[string]string) *Snapshot {
	snap := &Snapshot{At: d.now()}
	for _, t := range d.cfg.Targets {
		snap.Targets = append(snap.Targets, d.buildTargetSnapshot(t, refs))
	}
	return snap
}

// buildTargetSnapshot builds one target's TargetSnapshot.
func (d *Daemon) buildTargetSnapshot(t config.Target, refs map[string]string) TargetSnapshot {
	cands := discoverCandidates(t.Name, refs)
	ts := TargetSnapshot{
		Name:      t.Name,
		Branch:    t.Branch,
		TargetTip: refs[targetRefName(t)],
	}

	r := d.runs[t.Name]
	if r != nil {
		ts.InFlight = buildRunSnapshot(r)
	}

	order := d.order[t.Name]
	done := d.done[t.Name]

	var waitingRefs []string
	for ref := range cands {
		if r != nil && ref == r.cand.Ref {
			continue // in flight, not waiting
		}
		if _, parked := done[ref]; parked {
			continue
		}
		waitingRefs = append(waitingRefs, ref)
	}
	sort.Slice(waitingRefs, func(i, j int) bool {
		if order[waitingRefs[i]] != order[waitingRefs[j]] {
			return order[waitingRefs[i]] < order[waitingRefs[j]]
		}
		return waitingRefs[i] < waitingRefs[j] // pickHead's lexical tie-break
	})
	for _, ref := range waitingRefs {
		ts.Waiting = append(ts.Waiting, WaitingEntry{Candidate: cands[ref], Seq: order[ref]})
	}

	var parkedRefs []string
	for ref := range done {
		parkedRefs = append(parkedRefs, ref)
	}
	sort.Strings(parkedRefs) // deterministic snapshot order
	for _, ref := range parkedRefs {
		entry := done[ref]
		cand, ok := cands[ref]
		if !ok {
			// The ref vanished this same tick, after syncBookkeeping ran but
			// before this snapshot was built — vanishingly rare, but don't
			// synthesize a Candidate with a stale Target/User/Topic split.
			cand = core.Candidate{Ref: ref, Target: t.Name, SHA: entry.SHA}
		}
		ts.Parked = append(ts.Parked, ParkedEntry{Candidate: cand, Outcome: entry.Outcome, Reason: entry.Reason, At: entry.At})
	}

	return ts
}

// buildRunSnapshot deep-copies r's observable state into a RunSnapshot: in
// particular Done, which must be an independent slice since r.rec.Checks is
// still live and grows via append on every future check completion this
// same run goroutine processes.
func buildRunSnapshot(r *run) *RunSnapshot {
	done := make([]core.CheckResult, len(r.rec.Checks))
	copy(done, r.rec.Checks)

	var cur *CurrentCheck
	if r.cur != nil {
		cur = &CurrentCheck{Name: r.cur.name, StartedAt: r.cur.start}
	}

	return &RunSnapshot{
		Candidate: r.cand,
		RunID:     r.runID,
		BaseOID:   r.baseOID,
		MergeSHA:  r.mergeOID,
		Done:      done,
		Current:   cur,
		StartedAt: r.rec.StartedAt,
	}
}
