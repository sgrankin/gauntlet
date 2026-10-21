package core

import (
	"context"
	"testing"
)

func TestUrgentReservation(t *testing.T) {
	slots := NewReservedSlots(3, 1)
	if !slots.TryAcquire() || !slots.TryAcquire() || slots.TryAcquire() {
		t.Fatal("ordinary work consumed reserve")
	}
	if !slots.TryAcquirePriority(true) || slots.TryAcquirePriority(true) {
		t.Fatal("urgent work did not respect total cap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if slots.Acquire(ctx) != context.Canceled {
		t.Fatal("blocked acquisition ignored cancellation")
	}
	slots.Release()
	slots.Release()
	slots.Release()
	if slots.InUse() != 0 || !slots.TryAcquire() {
		t.Fatal("released slots unavailable")
	}
}
