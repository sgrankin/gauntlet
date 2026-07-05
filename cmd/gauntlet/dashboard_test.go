package main

import (
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/queue"
)

// TestShouldRecord exercises the pure decision behind the depth sampler's
// change-only recording (chunk E1): record on a tuple change, or when the
// heartbeat interval has elapsed since the last recording, and never
// otherwise.
func TestShouldRecord(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		last, current depthTuple
		lastAt, now   time.Time
		want          bool
	}{
		{
			name:    "first sample ever (lastAt zero) always records",
			last:    depthTuple{},
			current: depthTuple{},
			lastAt:  time.Time{},
			now:     base,
			want:    true,
		},
		{
			name:    "changed tuple records immediately",
			last:    depthTuple{Waiting: 1, InFlight: 0, Parked: 0},
			current: depthTuple{Waiting: 2, InFlight: 0, Parked: 0},
			lastAt:  base,
			now:     base.Add(time.Second),
			want:    true,
		},
		{
			name:    "unchanged tuple, well within heartbeat, does not record",
			last:    depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			current: depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			lastAt:  base,
			now:     base.Add(time.Minute),
			want:    false,
		},
		{
			name:    "unchanged tuple, exactly at heartbeat, records",
			last:    depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			current: depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			lastAt:  base,
			now:     base.Add(depthHeartbeat),
			want:    true,
		},
		{
			name:    "unchanged tuple, past heartbeat, records",
			last:    depthTuple{Waiting: 3, InFlight: 0, Parked: 2},
			current: depthTuple{Waiting: 3, InFlight: 0, Parked: 2},
			lastAt:  base,
			now:     base.Add(depthHeartbeat + time.Hour),
			want:    true,
		},
		{
			name:    "only parked differs still counts as changed",
			last:    depthTuple{Waiting: 0, InFlight: 0, Parked: 1},
			current: depthTuple{Waiting: 0, InFlight: 0, Parked: 2},
			lastAt:  base,
			now:     base.Add(time.Second),
			want:    true,
		},
		{
			name:    "unchanged zero tuple just under heartbeat does not record",
			last:    depthTuple{},
			current: depthTuple{},
			lastAt:  base,
			now:     base.Add(depthHeartbeat - time.Nanosecond),
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRecord(tc.last, tc.current, tc.lastAt, tc.now)
			if got != tc.want {
				t.Errorf("shouldRecord(%+v, %+v, lastAt=%v, now=%v) = %v, want %v",
					tc.last, tc.current, tc.lastAt, tc.now, got, tc.want)
			}
		})
	}
}

// TestBuildDepthTuple_InFlightIsPipelineDepth exercises the depth sampler's
// tuple extraction (docs/plans/phase5.md §10 amendment 5, chunk P5-H): the
// InFlight component is len(TargetSnapshot.Pipeline), not a 0/1
// InFlight!=nil flag. Today (before speculation/batching land elsewhere)
// Pipeline never exceeds 1 element, so the idle (0) and serial-busy (1)
// cases must come out byte-identical to the sampler's old InFlight!=nil
// values — no series discontinuity — while a hand-built depth-3 pipeline
// (a fixture standing in for what speculate mode will publish) proves the
// tuple now reflects actual pipeline occupancy rather than a boolean.
func TestBuildDepthTuple_InFlightIsPipelineDepth(t *testing.T) {
	cases := []struct {
		name string
		ts   queue.TargetSnapshot
		want depthTuple
	}{
		{
			name: "idle: no pipeline, no waiting/parked",
			ts:   queue.TargetSnapshot{},
			want: depthTuple{Waiting: 0, InFlight: 0, Parked: 0},
		},
		{
			name: "serial-busy: one run in the pipeline, unchanged from today's InFlight!=nil=1",
			ts: queue.TargetSnapshot{
				Pipeline: []queue.RunSnapshot{{RunID: "run-1"}},
				Waiting:  []queue.WaitingEntry{{}, {}},
			},
			want: depthTuple{Waiting: 2, InFlight: 1, Parked: 0},
		},
		{
			name: "pipeline depth 3 (speculation): InFlight reflects lane depth, not a 0/1 flag",
			ts: queue.TargetSnapshot{
				Pipeline: []queue.RunSnapshot{{RunID: "run-1"}, {RunID: "run-2"}, {RunID: "run-3"}},
				Parked:   []queue.ParkedEntry{{}},
			},
			want: depthTuple{Waiting: 0, InFlight: 3, Parked: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDepthTuple(tc.ts)
			if got != tc.want {
				t.Errorf("buildDepthTuple(%+v) = %+v, want %+v", tc.ts, got, tc.want)
			}
		})
	}
}
