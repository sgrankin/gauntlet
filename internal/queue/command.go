package queue

// This file is the command drain (docs/plans/phase23.md §2.2): the first
// consumer of core.Channel.Commands(), and the sanctioned phase-2 mechanism
// for clearing a park explicitly (phase1 §9.1: "a channel `retry` command
// will clear parks explicitly").

import (
	"context"

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
func (d *Daemon) drainCommands(ctx context.Context) {
	for _, ch := range d.chans {
		d.drainOne(ctx, ch)
	}
}

// drainOne drains ch's Commands() until it would block.
func (d *Daemon) drainOne(ctx context.Context, ch core.Channel) {
	cmds := ch.Commands()
	for {
		select {
		case cmd := <-cmds:
			d.applyCommand(ctx, cmd)
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
func (d *Daemon) applyCommand(ctx context.Context, cmd core.Command) {
	switch cmd.Kind {
	case core.CommandRetry:
		d.applyRetry(ctx, cmd)
	}
}

// applyRetry clears the park for (cmd.Target, cmd.Ref) at its current SHA,
// if one exists, and emits EventQueued with detail "retry: park cleared" so
// the next pick re-tests it. Idempotent: retrying a ref that isn't parked
// (already cleared by an earlier retry, a re-push, or because it was never
// parked at all) is a silent no-op — touches no ref, no CAS, nothing but
// this in-memory bookkeeping.
func (d *Daemon) applyRetry(ctx context.Context, cmd core.Command) {
	done := d.done[cmd.Target]
	if done == nil {
		return
	}
	entry, ok := done[cmd.Ref]
	if !ok {
		return
	}
	delete(done, cmd.Ref)

	const detail = "retry: park cleared"
	d.emit(ctx, core.Event{
		Kind:      core.EventQueued,
		At:        d.now(),
		Target:    cmd.Target,
		Candidate: core.Candidate{Ref: cmd.Ref, Target: cmd.Target, SHA: entry.SHA},
		Detail:    detail,
	})
}
