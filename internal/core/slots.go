package core

import (
	"context"
	"sync"
)

// Slots bounds all concurrent invocations, including hooks. A nil Slots is
// unlimited. Reserved capacity is available only to urgent queue work.
type Slots struct {
	mu                   sync.Mutex
	limit, reserve, used int
	changed              chan struct{}
}

func NewSlots(n int) *Slots { return NewReservedSlots(n, 0) }

func NewReservedSlots(n, reserve int) *Slots {
	if n <= 0 || reserve < 0 || reserve >= n {
		panic("core.NewReservedSlots: invalid capacity")
	}
	return &Slots{limit: n, reserve: reserve, changed: make(chan struct{})}
}

func (s *Slots) TryAcquire() bool { return s.TryAcquirePriority(false) }

func (s *Slots) TryAcquirePriority(urgent bool) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquire(urgent)
}

func (s *Slots) acquire(urgent bool) bool {
	limit := s.limit
	if !urgent {
		limit -= s.reserve
	}
	if s.used >= limit {
		return false
	}
	s.used++
	return true
}

func (s *Slots) Acquire(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for {
		s.mu.Lock()
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return err
		}
		if s.acquire(false) {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Slots) Release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used == 0 {
		panic("core.Slots.Release: release without matching acquire")
	}
	s.used--
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Slots) InUse() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// Saturated reports ordinary-capacity pressure without acquiring a slot.
func (s *Slots) Saturated() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used >= s.limit-s.reserve
}
