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
	TargetTip string       // "" if the target branch doesn't exist yet
	InFlight  *RunSnapshot // the HEAD run (lane.runs[0]); nil when the lane is idle

	// Pipeline is every in-flight run for this target, head first: nil/empty
	// when the lane is idle; at most one element for serial and batch; up to
	// Target.Window elements for speculate (docs/plans/phase5.md §3.4).
	Pipeline []RunSnapshot

	Waiting []WaitingEntry // FIFO order; excludes in-flight and parked refs
	Parked  []ParkedEntry  // refs parked at their current SHA, with reason
}

// RunSnapshot is one in-flight run within a target's pipeline.
type RunSnapshot struct {
	// Candidate is this run's head member (members[0]) — kept for
	// back-compat with pre-batch consumers. See Members.
	Candidate core.Candidate

	// Members is every candidate chained into this run: len 1 for
	// serial/speculate, up to max-batch for batch (docs/plans/phase5.md
	// §3.4). Candidate == Members[0] always.
	Members []core.Candidate

	RunID   string
	BaseOID string

	// ChainTip is the tested merge commit — the last member's chain link.
	// Equal to MergeSHA (kept for back-compat: MergeSHA == ChainTip always).
	ChainTip string
	MergeSHA string

	// Predicted is true iff this run was built on a predicted (unpushed,
	// not-yet-landed) base rather than the live target tip — a non-head
	// speculation-window member.
	Predicted bool

	// BatchID groups this run with its sibling per-member RunRecords when
	// it's part of a batch; "" for serial and speculate.
	BatchID string

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
// the same goroutine that owns d.order/d.done/d.lanes, so no locking is
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

	// Pipeline is every in-flight run for this target, head first
	// (docs/plans/phase5.md §3.4); InFlight mirrors its head element for
	// back-compat. Serial and batch hold at most one run, so Pipeline has at
	// most one element for those modes; speculate grows it up to
	// Target.Window.
	if l := d.lanes[t.Name]; l != nil {
		for _, r := range l.runs {
			ts.Pipeline = append(ts.Pipeline, *buildRunSnapshot(r))
		}
		if len(ts.Pipeline) > 0 {
			head := ts.Pipeline[0]
			ts.InFlight = &head
		}
	}

	order := d.order[t.Name]
	done := d.done[t.Name]

	// inFlightRefs is every member of every run already captured in
	// ts.Pipeline above, not just the head run's own head member (F3 fix,
	// docs/plans/phase5.md review): a filled speculation window or a
	// multi-member batch has members beyond ts.InFlight.Candidate that were
	// still being double-counted as Waiting before this fix, inflating
	// Waiting's count (and the depth series it feeds, this phase's own
	// tuning instrument) by however many other in-flight members the
	// pipeline happened to hold.
	inFlightRefs := make(map[string]bool)
	for _, r := range ts.Pipeline {
		for _, m := range r.Members {
			inFlightRefs[m.Ref] = true
		}
	}

	var waitingRefs []string
	for ref := range cands {
		if inFlightRefs[ref] {
			continue // in flight (any pipeline member, not just the head run's head), not waiting
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
// particular Done, which must be an independent slice since the head
// member's rec.Checks is still live and grows via append on every future
// check completion this same run goroutine processes.
func buildRunSnapshot(r *run) *RunSnapshot {
	head := r.members[0]
	done := make([]core.CheckResult, len(head.rec.Checks))
	copy(done, head.rec.Checks)

	var cur *CurrentCheck
	if r.cur != nil {
		cur = &CurrentCheck{Name: r.cur.name, StartedAt: r.cur.start}
	}

	members := make([]core.Candidate, len(r.members))
	for i, m := range r.members {
		members[i] = m.cand
	}

	return &RunSnapshot{
		Candidate: head.cand,
		Members:   members,
		RunID:     r.runID,
		BaseOID:   r.baseOID,
		ChainTip:  r.chainTip, // ChainTip == MergeSHA (back-compat, §3.4)
		MergeSHA:  r.chainTip,
		Predicted: r.predicted, // true iff this run's base is a predicted, unpushed predecessor chainTip (speculate, non-head)
		BatchID:   r.batchID,   // "" unless part of a batch
		Done:      done,
		Current:   cur,
		StartedAt: head.rec.StartedAt,
	}
}
