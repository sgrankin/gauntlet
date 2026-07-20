package core

import "context"

// Slots is the daemon-wide execution-capacity semaphore behind the operator's
// `max-executions` cap: every bounded invocation through an Executor — a
// candidate check, a post-land hook, a future candidate-image build — holds
// one slot from just before it starts until the executor has finished child
// cleanup (RunCheck returning), so a released slot never represents a still
// running process or container. Long-lived pooled service containers do NOT
// take slots; their own instance limits remain authoritative.
//
// A nil *Slots means unlimited — every method is nil-safe and the zero
// config never constructs one, preserving pre-cap behavior exactly.
//
// The two acquire forms match the two callers' constraints: the queue's
// reconcile loop must never block (TryAcquire — a check that finds no slot
// simply stays ready and accrues CheckResult.Waited until a later tick),
// while the hooks runner is a dedicated goroutine that may block (Acquire).
type Slots struct {
	ch chan struct{}
}

// NewSlots returns a semaphore admitting n concurrent holders. n <= 0 is a
// caller bug (use a nil *Slots for "unlimited"); it panics rather than
// minting a semaphore nothing can ever acquire.
func NewSlots(n int) *Slots {
	if n <= 0 {
		panic("core.NewSlots: n must be positive; use nil for unlimited")
	}
	return &Slots{ch: make(chan struct{}, n)}
}

// TryAcquire takes a slot if one is free, reporting whether it did. Always
// true on a nil *Slots.
func (s *Slots) TryAcquire() bool {
	if s == nil {
		return true
	}
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Acquire blocks until a slot is free or ctx is done. Immediate nil on a
// nil *Slots.
func (s *Slots) Acquire(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a slot taken by TryAcquire/Acquire. No-op on nil.
// Releasing without a matching acquire is a caller bug and panics loudly —
// silently absorbing it would quietly widen the cap.
func (s *Slots) Release() {
	if s == nil {
		return
	}
	select {
	case <-s.ch:
	default:
		panic("core.Slots.Release: release without matching acquire")
	}
}

// InUse reports how many slots are currently held. A cheap, racy-by-design
// read (len on a channel) meant for observability sampling (issue #14's
// gauntlet.slots.in_use gauge), never for a correctness decision — nothing
// here synchronizes with a concurrent Acquire/Release, so a caller wanting
// an authoritative admission decision must keep using TryAcquire/Acquire.
// Always 0 on a nil *Slots; that's indistinguishable from "cap configured
// but idle", so an observability caller that needs to tell "no cap" apart
// from "zero held" must check for nil itself rather than trust this return
// alone.
func (s *Slots) InUse() int {
	if s == nil {
		return 0
	}
	return len(s.ch)
}
