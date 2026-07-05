package channel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestRecordingChannel_CapturesEventsAndRecords(t *testing.T) {
	c := NewRecordingChannel()
	ev1 := core.Event{Kind: core.EventQueued, Target: "main"}
	ev2 := core.Event{Kind: core.EventLanded, Target: "main", Record: &core.RunRecord{RunID: "r1"}}

	if err := c.Emit(context.Background(), ev1); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := c.Emit(context.Background(), ev2); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	events := c.Events()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Kind != core.EventQueued || events[1].Kind != core.EventLanded {
		t.Fatalf("events out of order or wrong kind: %+v", events)
	}

	records := c.Records()
	if len(records) != 1 || records[0].RunID != "r1" {
		t.Fatalf("Records() = %+v, want one record with RunID r1", records)
	}
}

// TestRecordingChannel_EmitUnknownEventKindNoPanic is S14's universal
// contract test for RecordingChannel: unlike a real channel,
// RecordingChannel's whole job is to capture every event a test hands it
// verbatim, so its contract for an unrecognized core.EventKind isn't
// "ignore it" but "record it without panicking" — a future EventKind must
// be just as capturable by tests as any existing one.
func TestRecordingChannel_EmitUnknownEventKindNoPanic(t *testing.T) {
	c := NewRecordingChannel()
	ev := core.Event{Kind: core.EventKind(999), Target: "main"}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	events := c.Events()
	if len(events) != 1 || events[0].Kind != core.EventKind(999) {
		t.Fatalf("Events() = %+v, want the unrecognized-kind event recorded", events)
	}
}

func TestRecordingChannel_EventsSnapshotIsIndependent(t *testing.T) {
	c := NewRecordingChannel()
	if err := c.Emit(context.Background(), core.Event{Kind: core.EventQueued}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	snap := c.Events()
	if err := c.Emit(context.Background(), core.Event{Kind: core.EventLanded}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if len(snap) != 1 {
		t.Fatalf("snapshot mutated after later Emit: got %d events, want 1", len(snap))
	}
}

func TestRecordingChannel_ConcurrentEmit(t *testing.T) {
	c := NewRecordingChannel()
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = c.Emit(context.Background(), core.Event{Kind: core.EventQueued, Target: "main"})
		}()
	}
	wg.Wait()

	if got := len(c.Events()); got != n {
		t.Fatalf("got %d events, want %d", got, n)
	}
}

func TestRecordingChannel_WaitForKindBlocksThenWakes(t *testing.T) {
	c := NewRecordingChannel()

	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = c.Emit(context.Background(), core.Event{Kind: core.EventLanded, Target: "main"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ev, ok := c.WaitForKind(ctx, core.EventLanded)
	if !ok {
		t.Fatal("WaitForKind timed out")
	}
	if ev.Kind != core.EventLanded {
		t.Fatalf("got kind %v, want EventLanded", ev.Kind)
	}
}

func TestRecordingChannel_WaitForKindAlreadyArrived(t *testing.T) {
	c := NewRecordingChannel()
	if err := c.Emit(context.Background(), core.Event{Kind: core.EventRejected}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := c.WaitForKind(ctx, core.EventRejected); !ok {
		t.Fatal("expected immediate match for already-captured event")
	}
}

func TestRecordingChannel_WaitForKindTimesOut(t *testing.T) {
	c := NewRecordingChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, ok := c.WaitForKind(ctx, core.EventLanded); ok {
		t.Fatal("expected timeout, got a match")
	}
}

func TestRecordingChannel_CommandsNeverYields(t *testing.T) {
	c := NewRecordingChannel()
	select {
	case cmd, ok := <-c.Commands():
		t.Fatalf("expected no command, got %v (ok=%v)", cmd, ok)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRecordingChannel_SendCommandDeliversOnCommands(t *testing.T) {
	c := NewRecordingChannel()
	want := core.Command{Kind: core.CommandRetry, Target: "main", Ref: "refs/heads/for/main/alice/feat"}
	c.SendCommand(want)

	select {
	case got := <-c.Commands():
		if got != want {
			t.Fatalf("Commands() delivered %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("SendCommand'd command never arrived on Commands()")
	}
}
