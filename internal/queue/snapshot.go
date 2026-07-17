package queue

// This file holds the published, read-only view of the reconcile loop's
// live state: the dashboard and the
// history store's depth sampler both read Daemon.Snapshot() instead of
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

	// IdleSince is the instant the QUEUE (every target, this package's own
	// view) most recently became idle — no waiting candidates and no
	// in-flight pipeline runs, anywhere — or the zero time if the queue is
	// busy right now (the park-the-builder idle signal; see
	// docs/design/scaling.md, "Axis 2 — park the builder"). Parked
	// candidates don't count: they're dormant, not being worked on.
	//
	// This is QUEUE idleness only. The daemon's post-land hooks
	// (internal/hooks) live outside this package by design (Invariant 8: the
	// queue stays ignorant of channels), so whether a hook is still running
	// or backlogged can't be folded in here — the dashboard/MCP layer, which
	// already holds both this Snapshot and a hook-state snapshot func,
	// composes the two into the actual "daemon is fully idle" signal it
	// surfaces (internal/dashboard/api.go's idleSince, internal/mcp/
	// server.go's idleSince).
	IdleSince time.Time
}

// TargetSnapshot is one target's live queue state.
type TargetSnapshot struct {
	Name      string
	Branch    string
	TargetTip string       // "" if the target branch doesn't exist yet
	InFlight  *RunSnapshot // the HEAD run (lane.runs[0]); nil when the lane is idle

	// Pipeline is every in-flight run for this target, head first: nil/empty
	// when the lane is idle; at most one element for serial and batch; up to
	// Target.Window elements for speculate.
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
	// serial/speculate, up to max-batch for batch. Candidate == Members[0]
	// always.
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

	Done []core.CheckResult // checks finished so far, in spec-declaration order

	// Current is the longest-running check right now; nil when none is in
	// flight. Kept for back-compat with pre-parallelism consumers (the
	// Candidate-vs-Members precedent above): Current == &Running[0] in
	// spirit whenever Running is non-empty.
	Current *CurrentCheck

	// Running is every check in flight right now, ordered by start time
	// (earliest first) — more than one only when the candidate's spec set
	// max-parallel > 1.
	Running []CurrentCheck

	StartedAt time.Time
}

// CurrentCheck is one check running right now within an in-flight run.
type CurrentCheck struct {
	Name      string
	StartedAt time.Time // elapsed = snapshot.At.Sub(StartedAt)
}

// WaitingEntry is a queued-but-not-yet-picked candidate.
type WaitingEntry struct {
	Candidate core.Candidate
	Seq       int64 // FIFO sequence (Daemon.order); lower = earlier
}

// ParkedEntry is a candidate parked at its current SHA: it will not be
// re-tested until the ref's SHA changes, the ref vanishes, or a
// CommandRetry clears it.
type ParkedEntry struct {
	Candidate core.Candidate
	Outcome   core.Outcome // why it parked (rejected/conflict/error)
	Reason    string       // RunRecord.Detail at park time
	At        time.Time

	// RunID is the terminal run that parked this candidate — copied
	// straight from parkEntry.RunID — or "" when none is known (a boot
	// seed predating that field). The dashboard's /t/{target} Parked table
	// links its outcome tag to /run/{RunID} only when this is non-empty.
	RunID string
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

	// d.idleSince tracks the queue-idle transition across ticks (reconcile
	// goroutine only, like every other Daemon field here): the first tick
	// that finds every target idle stamps it, and it holds steady across
	// however many idle ticks follow, so Snapshot.IdleSince reports how long
	// the queue has been idle rather than just "is it idle this instant".
	// Any non-idle tick zeroes it, so the next idle stretch gets its own
	// fresh instant, never a stale one.
	if queueIdle(snap.Targets) {
		if d.idleSince.IsZero() {
			d.idleSince = snap.At
		}
		snap.IdleSince = d.idleSince
	} else {
		d.idleSince = time.Time{}
	}
	return snap
}

// queueIdle reports whether every target has no waiting candidates and no
// in-flight pipeline runs — the queue half of the daemon idleness signal
// (Snapshot.IdleSince's doc). Parked candidates
// don't count: they're dormant, not being worked on.
func queueIdle(targets []TargetSnapshot) bool {
	for _, ts := range targets {
		if len(ts.Waiting) > 0 || len(ts.Pipeline) > 0 {
			return false
		}
	}
	return true
}

// buildTargetSnapshot builds one target's TargetSnapshot.
func (d *Daemon) buildTargetSnapshot(t config.Target, refs map[string]string) TargetSnapshot {
	cands := discoverCandidates(t.Name, refs)
	ts := TargetSnapshot{
		Name:      t.Name,
		Branch:    t.Branch,
		TargetTip: refs[targetRefName(t)],
	}

	// Pipeline is every in-flight run for this target, head first;
	// InFlight mirrors its head element for
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
	// ts.Pipeline above, not just the head run's own head member: a filled
	// speculation window or a multi-member batch has members beyond
	// ts.InFlight.Candidate, and those must not be double-counted as
	// Waiting — that would inflate Waiting's count (and the depth series it
	// feeds, the tuning instrument for batch/speculate sizing) by however
	// many other in-flight members the pipeline happened to hold.
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
		ts.Parked = append(ts.Parked, ParkedEntry{Candidate: cand, Outcome: entry.Outcome, Reason: entry.Reason, At: entry.At, RunID: entry.RunID})
	}

	return ts
}

// buildRunSnapshot deep-copies r's observable state into a RunSnapshot:
// Done is built fresh from r.results in spec-declaration order (the
// member records themselves are only materialized at conclusion), and
// Running lists every in-flight check, earliest start first.
func buildRunSnapshot(r *run) *RunSnapshot {
	head := r.members[0]

	var done []core.CheckResult
	var running []CurrentCheck
	for i := range r.checks {
		name := r.checks[i].Name
		if res, ok := r.results[name]; ok {
			done = append(done, res)
		}
		if inf, ok := r.inflight[name]; ok {
			running = append(running, CurrentCheck{Name: inf.name, StartedAt: inf.start})
		}
	}
	sort.Slice(running, func(i, j int) bool { return running[i].StartedAt.Before(running[j].StartedAt) })
	var cur *CurrentCheck
	if len(running) > 0 {
		cur = &running[0]
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
		ChainTip:  r.chainTip, // ChainTip == MergeSHA (back-compat)
		MergeSHA:  r.chainTip,
		Predicted: r.predicted, // true iff this run's base is a predicted, unpushed predecessor chainTip (speculate, non-head)
		BatchID:   r.batchID,   // "" unless part of a batch
		Done:      done,
		Current:   cur,
		Running:   running,
		StartedAt: head.rec.StartedAt,
	}
}
