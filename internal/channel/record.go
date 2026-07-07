package channel

import (
	"context"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
)

var _ core.Channel = (*RecordingChannel)(nil)

// RecordingChannel is a core.Channel test double that captures every Event
// emitted to it, for assertions in queue and integration tests. It is safe
// for concurrent use.
type RecordingChannel struct {
	mu     sync.Mutex
	events []core.Event
	notify chan struct{} // closed and replaced on every Emit, to wake WaitForKind waiters

	// cmds is buffered so SendCommand can enqueue test-injected commands
	// (e.g. CommandRetry) without a reader present yet; ReconcileOnce's
	// drainCommands drains it non-blockingly at the top of the next
	// reconcile pass. A test that never calls SendCommand still observes
	// Commands() as never yielding.
	cmds chan core.Command
}

// commandBuffer bounds RecordingChannel's inbound command queue. Generous
// for a test double: no test needs to enqueue more commands than this
// between drains.
const commandBuffer = 64

// NewRecordingChannel returns an empty RecordingChannel.
func NewRecordingChannel() *RecordingChannel {
	return &RecordingChannel{
		notify: make(chan struct{}),
		cmds:   make(chan core.Command, commandBuffer),
	}
}

// SendCommand enqueues cmd for delivery on Commands() — a test affordance
// letting tests inject inbound commands (e.g. core.CommandRetry) the way a
// real duplex channel implementation (Slack's :recycle: reaction) would. It
// does not block: the buffer (commandBuffer) is sized generously
// for test use; a test that needs to enqueue more than that before a drain
// is doing something unusual enough to warrant a look.
func (c *RecordingChannel) SendCommand(cmd core.Command) {
	c.cmds <- cmd
}

// Emit records ev.
func (c *RecordingChannel) Emit(ctx context.Context, ev core.Event) error {
	c.mu.Lock()
	c.events = append(c.events, ev)
	old := c.notify
	c.notify = make(chan struct{})
	c.mu.Unlock()
	close(old)
	return nil
}

// Commands returns the channel SendCommand enqueues onto. A test that never
// calls SendCommand observes it as never yielding, matching LogChannel's
// behavior (Invariant 8: no built-in channel produces a Command on its own).
func (c *RecordingChannel) Commands() <-chan core.Command {
	return c.cmds
}

// Events returns a snapshot of every Event captured so far, in arrival
// order. The returned slice is independent of later Emit calls.
func (c *RecordingChannel) Events() []core.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]core.Event, len(c.events))
	copy(out, c.events)
	return out
}

// Records returns the RunRecord carried by every terminal event captured so
// far, in arrival order.
func (c *RecordingChannel) Records() []*core.RunRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*core.RunRecord
	for _, ev := range c.events {
		if ev.Record != nil {
			out = append(out, ev.Record)
		}
	}
	return out
}

// WaitForKind blocks until an Event of the given kind has been captured or
// ctx is done, whichever comes first — bound ctx with context.WithTimeout to
// give it a deadline. If a matching event was already captured before the
// call, it returns immediately. ok is false only if ctx ended first.
func (c *RecordingChannel) WaitForKind(ctx context.Context, kind core.EventKind) (ev core.Event, ok bool) {
	for {
		c.mu.Lock()
		for _, e := range c.events {
			if e.Kind == kind {
				c.mu.Unlock()
				return e, true
			}
		}
		wake := c.notify
		c.mu.Unlock()

		select {
		case <-wake:
			// An Emit landed; loop around and re-scan.
		case <-ctx.Done():
			return core.Event{}, false
		}
	}
}
