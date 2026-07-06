package queue

// This file is the command drain (docs/plans/phase23.md §2.2): the first
// consumer of core.Channel.Commands(), and the sanctioned phase-2 mechanism
// for clearing a park explicitly (phase1 §9.1: "a channel `retry` command
// will clear parks explicitly"), extended by Feature 1 (manual operator
// cancellation, core.CommandCancel).

import (
	"context"
	"fmt"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

// drainCommands applies every currently-buffered command from every
// configured channel, then returns. Non-blocking: draining one channel is a
// for/select/default loop, not a range, since a channel's Commands() is
// expected to stay open (and un-signaled) for the daemon's entire lifetime
// — ranging over it would block the reconcile pass forever the instant its
// buffer ran dry. No fan-in goroutine, no inbox mutex: this runs at the top
// of the reconcile pass (ReconcileOnce, after ListRefs) on the reconcile
// goroutine, so command application is already serialized with everything
// else the pass does.
//
// refs is the tick's own ListRefs snapshot (ReconcileOnce's, taken just
// before this runs): CommandCancel needs it to learn a WAITING candidate's
// current SHA (there is no in-flight run to read it from in that case) —
// see applyCancel.
func (d *Daemon) drainCommands(ctx context.Context, refs map[string]string) {
	for _, ch := range d.chans {
		d.drainOne(ctx, ch, refs)
	}
}

// drainOne drains ch's Commands() until it would block.
func (d *Daemon) drainOne(ctx context.Context, ch core.Channel, refs map[string]string) {
	cmds := ch.Commands()
	for {
		select {
		case cmd := <-cmds:
			d.applyCommand(ctx, cmd, refs)
		default:
			return
		}
	}
}

// applyCommand handles one inbound Command (Invariant 8: channels produce
// commands, the queue applies them — this is the entire application
// surface, so adding a channel never touches core logic). Unrecognized
// Kinds are ignored, symmetric with core.Channel implementations being
// required to ignore event kinds they don't recognize (channel/log.go).
func (d *Daemon) applyCommand(ctx context.Context, cmd core.Command, refs map[string]string) {
	switch cmd.Kind {
	case core.CommandRetry:
		d.applyRetry(ctx, cmd)
	case core.CommandCancel:
		d.applyCancel(ctx, cmd, refs)
	}
}

// applyRetry clears the park for (cmd.Target, cmd.Ref) at its current SHA,
// if one exists, and emits EventQueued with detail "retry: park cleared" so
// the next pick re-tests it, plus EventRetryRequested (S3: persisted retry
// intent) so history.Store can durably record that this retry happened —
// see that event's own doc and history.Store.LatestTerminalPerRef's
// seed-park query for why: without it, a daemon crash between this retry
// and the retried run's own terminal event would have restart's park-seed
// query re-read the stale pre-retry verdict and silently re-park the ref at
// its old rejection, undoing the operator's action. Idempotent in-memory:
// retrying a ref that isn't parked (already cleared by an earlier retry, a
// re-push, or because it was never parked at all) is a silent no-op —
// touches no ref, no CAS, nothing but this in-memory bookkeeping (a
// redundant EventRetryRequested for an already-cleared park is never
// emitted, since clearParkAndRetry returns early in that case).
//
// The actual clear+emit is clearParkAndRetry (below), shared verbatim with
// the phase-B automatic retry (maybeAutoRetry, autoretry.go) — this
// operator path's own Detail strings ("retry: park cleared" on EventQueued,
// unset on EventRetryRequested) are unchanged from before that sharing.
func (d *Daemon) applyRetry(ctx context.Context, cmd core.Command) {
	d.clearParkAndRetry(ctx, cmd.Target, cmd.Ref, "retry: park cleared", "")
}

// clearParkAndRetry clears (target, ref)'s park, if one currently exists,
// and emits EventQueued (carrying queuedDetail) plus EventRetryRequested
// (carrying retryDetail) — the shared machinery behind both an operator's
// explicit CommandRetry (applyRetry, above) and the phase-B automatic retry
// (maybeAutoRetry, autoretry.go), so Slack threading, history's
// retry_intents-based stale-park suppression, and the dashboard all treat
// the two identically; only the Detail text on the two events tells them
// apart. Reports whether a park actually existed and was cleared — false
// (unknown target, or ref not currently parked) is a no-op that touches
// nothing else, matching applyRetry's pre-existing idempotence contract.
func (d *Daemon) clearParkAndRetry(ctx context.Context, target, ref, queuedDetail, retryDetail string) bool {
	done := d.done[target]
	if done == nil {
		return false
	}
	entry, ok := done[ref]
	if !ok {
		return false
	}
	delete(done, ref)

	now := d.now()
	cand := core.Candidate{Ref: ref, Target: target, SHA: entry.SHA}
	d.emit(ctx, core.Event{
		Kind:      core.EventQueued,
		At:        now,
		Target:    target,
		Candidate: cand,
		Detail:    queuedDetail,
	})
	d.emit(ctx, core.Event{
		Kind:      core.EventRetryRequested,
		At:        now,
		Target:    target,
		Candidate: cand,
		Detail:    retryDetail,
	})
	return true
}

// cancelDetail is the Detail every CommandCancel-caused park/skip carries —
// how an operator (or a channel rendering the event) tells a manual
// cancellation apart from a genuine check failure or infra error.
const cancelDetail = "cancelled by operator"

// applyCancel implements manual operator cancellation (Feature 1,
// core.CommandCancel): stop whatever is currently happening to
// (cmd.Target, cmd.Ref) and park it at its current SHA, exactly like a red
// verdict — using the same park machinery a rejection uses (d.park +
// eventKindForOutcome), just with cancelDetail distinguishing the cause.
//
// Three cases, checked in order:
//
//  1. The ref is a member of an in-flight run in this target's lane: hand
//     off to cancelInFlight, which cancels that run (the same invalidation
//     machinery a ref move uses) and parks/re-queues per mode.
//  2. The ref is only WAITING — present in this tick's refs but not
//     in-flight: park it directly at its current SHA (cancel-before-start:
//     it will never be picked up). refs is this tick's own ListRefs
//     snapshot (drainCommands runs before syncBookkeeping, so there is no
//     other source for "this ref's current SHA" yet this tick).
//  3. Unknown to refs (deleted, or never a well-formed candidate for this
//     target), or already parked at its current SHA: a no-op — idempotent,
//     touches nothing.
func (d *Daemon) applyCancel(ctx context.Context, cmd core.Command, refs map[string]string) {
	if l := d.lanes[cmd.Target]; l != nil {
		for i, r := range l.runs {
			for _, m := range r.members {
				if m.cand.Ref == cmd.Ref {
					d.cancelInFlight(ctx, config.Target{Name: cmd.Target}, l, i, cmd.Ref)
					return
				}
			}
		}
	}

	d.cancelWaiting(ctx, cmd.Target, cmd.Ref, refs)
}

// cancelInFlight cancels lane.runs[i] — which is currently testing ref, one
// of its members — and parks/re-queues per mode (Feature 1). Mirrors
// advanceLane's bubble step (§2.1c) almost exactly, but with an
// operator-chosen park instead of a real verdict's:
//
//   - serial/speculate (len(members)==1, the only shape either mode ever
//     builds): the run's sole member parks at its current SHA
//     (OutcomeRejected, cancelDetail) via finishRun's normal park=true path.
//   - batch (len(members)>1): cancelBatchMember parks ONLY the named
//     member; every other member of the run Skips unparked and re-queues
//     (§10's own "batch member cancelled" wording) — unlike finishBatchRed,
//     there is no ambiguity about who's guilty here (the operator named the
//     ref explicitly), so there is no reason to force serial fallback the
//     way a genuine batch-red verdict does.
//
// Either way, any run behind this one in the lane (a speculation window's
// suffix, built on this run's now-invalid predicted chainTip) bubbles via
// the existing invalidateSuffix, exactly as a real bubble or move would.
//
// Timing note: unlike a real bubble (which runs inside advanceLane, whose
// "true" return makes reconcileTarget defer that tick's refill to the next
// tick's fresh Fetch/ListRefs — see its own doc comment) or a land (which
// mutates git refs the tick's cands/targetTip snapshot would then be stale
// against), this runs during drainCommands, BEFORE reconcileTarget even
// starts for this target. A cancel never mutates any git ref, so there is no
// staleness hazard to defer a tick for: any sibling this empties the lane's
// room for (a re-queued batch member, a bubbled speculation-window run) is
// safe to re-pick against this very same tick's already-snapshotted cands —
// and reconcileTarget's own refillLane does exactly that, immediately,
// once drainCommands returns. Observable in cancel_test.go: a cancel that
// re-queues siblings shows them already re-picked by the time the
// cancel-draining ReconcileOnce call returns, not a tick later.
func (d *Daemon) cancelInFlight(ctx context.Context, t config.Target, lane *lane, i int, ref string) {
	r := lane.runs[i]
	d.cancelRun(r)

	if len(r.members) > 1 {
		d.cancelBatchMember(ctx, t, r, ref)
	} else {
		d.finishRun(ctx, t, r, core.OutcomeRejected, cancelDetail, true)
	}

	d.invalidateSuffix(ctx, t, lane, i+1, "pipeline bubble: operator cancelled a run ahead of it")
	lane.runs = lane.runs[:i]
}

// cancelBatchMember finishes r — a genuine multi-member batch run (§2.6) —
// after ref was cancelled mid-run: ref's own member parks at its current SHA
// (OutcomeRejected, cancelDetail); every OTHER member Skips unparked with a
// batch-scoped detail naming ref, so it re-queues on the very next refill,
// FIFO order preserved. This mirrors finishBatchRed's per-member-record
// shape (one terminal event each, in member order) but — unlike
// finishBatchRed — never sets d.batchFallback: the culprit here is already
// known (the operator named it), so there is nothing for a one-at-a-time
// serial walk to discover that isn't already established.
func (d *Daemon) cancelBatchMember(ctx context.Context, t config.Target, r *run, ref string) {
	now := d.now()
	requeueDetail := fmt.Sprintf("batch member cancelled (%s)", ref)

	for i := range r.members {
		m := &r.members[i]
		m.rec.EndedAt = now
		if m.cand.Ref == ref {
			m.rec.Outcome = core.OutcomeRejected
			m.rec.Detail = cancelDetail
		} else {
			m.rec.Outcome = core.OutcomeSkipped
			m.rec.Detail = requeueDetail
		}
	}

	d.finalizeRun(r)

	for i := range r.members {
		m := &r.members[i]
		if m.cand.Ref == ref {
			d.park(t.Name, m.cand, m.rec.Outcome, m.rec.Detail, m.rec.RunID)
		}
		d.emit(ctx, core.Event{
			Kind:      eventKindForOutcome(m.rec.Outcome),
			At:        now,
			Target:    t.Name,
			Candidate: m.cand,
			RunID:     r.runID,
			Record:    m.rec,
			Detail:    m.rec.Detail,
		})
	}
}

// cancelWaiting handles a CommandCancel naming a ref that names no in-flight
// run (applyCancel's fallback): if ref exists in this tick's refs snapshot
// and isn't already parked at its current SHA, park it there directly
// (cancel-before-start — the ref is simply never picked up); otherwise a
// no-op (ref doesn't exist, or is already parked at this SHA — idempotent).
func (d *Daemon) cancelWaiting(ctx context.Context, target, ref string, refs map[string]string) {
	sha, ok := refs[ref]
	if !ok {
		return // unknown to this tick's refs: nothing to cancel
	}
	if entry, ok := d.done[target][ref]; ok && entry.SHA == sha {
		return // already parked at this SHA: idempotent no-op
	}

	cand := core.Candidate{Ref: ref, Target: target, SHA: sha}
	if t, user, topic, ok := parseCandidateRef(ref); ok && t == target {
		cand.User, cand.Topic = user, topic
	}

	now := d.now()
	runID := newRunID(now, sha)
	rec := &core.RunRecord{
		RunID: runID, Target: target, Candidate: cand,
		Outcome: core.OutcomeRejected, Detail: cancelDetail,
		StartedAt: now, EndedAt: now,
	}
	d.park(target, cand, core.OutcomeRejected, cancelDetail, runID)
	d.emit(ctx, core.Event{
		Kind: eventKindForOutcome(core.OutcomeRejected), At: now, Target: target,
		Candidate: cand, RunID: runID, Record: rec, Detail: cancelDetail,
	})
}
