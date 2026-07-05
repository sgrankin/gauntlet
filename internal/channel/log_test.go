package channel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestLogChannel_EmitEventKinds(t *testing.T) {
	cases := []struct {
		name string
		ev   core.Event
		want []string // substrings that must appear in the rendered line
	}{
		{
			name: "queued with candidate",
			ev: core.Event{
				Kind:   core.EventQueued,
				Target: "main",
				Candidate: core.Candidate{
					Ref: "refs/heads/for/main/alice/feat",
					SHA: "deadbeefcafef00d",
				},
			},
			want: []string{"kind=queued", "target=main", "ref=refs/heads/for/main/alice/feat", "sha=deadbeef"},
		},
		{
			name: "trial clean",
			ev:   core.Event{Kind: core.EventTrialClean, Target: "main"},
			want: []string{"kind=trial_clean", "target=main"},
		},
		{
			name: "trial conflict with detail",
			ev: core.Event{
				Kind:   core.EventTrialConflict,
				Target: "main",
				Detail: "conflict in foo.go",
			},
			want: []string{"kind=trial_conflict", `detail="conflict in foo.go"`},
		},
		{
			name: "check started",
			ev: core.Event{
				Kind:      core.EventCheckStarted,
				Target:    "main",
				RunID:     "run-1",
				CheckName: "lint",
			},
			want: []string{"kind=check_started", "run=run-1", "check=lint"},
		},
		{
			name: "check finished",
			ev: core.Event{
				Kind:      core.EventCheckFinished,
				Target:    "main",
				RunID:     "run-1",
				CheckName: "lint",
			},
			want: []string{"kind=check_finished", "run=run-1", "check=lint"},
		},
		{
			name: "rejected",
			ev:   core.Event{Kind: core.EventRejected, Target: "main", Detail: "missing .gauntlet.kdl"},
			want: []string{"kind=rejected", `detail="missing .gauntlet.kdl"`},
		},
		{
			name: "skipped",
			ev:   core.Event{Kind: core.EventSkipped, Target: "main"},
			want: []string{"kind=skipped"},
		},
		{
			name: "error",
			ev:   core.Event{Kind: core.EventError, Target: "main", Detail: "tempdir failed"},
			want: []string{"kind=error", `detail="tempdir failed"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := NewLogChannel(&buf)
			if err := c.Emit(context.Background(), tc.ev); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
		})
	}
}

func TestLogChannel_EmitRunRecordSummary(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := &core.RunRecord{
		RunID:  "20260101T000000Z-abc123",
		Target: "main",
		Checks: []core.CheckResult{
			{Name: "lint", Status: core.CheckPassed, Duration: 1200 * time.Millisecond},
			{Name: "test", Status: core.CheckFailed, Duration: 3400 * time.Millisecond},
		},
		Outcome:   core.OutcomeRejected,
		StartedAt: start,
		EndedAt:   start.Add(4600 * time.Millisecond),
	}
	ev := core.Event{
		Kind:   core.EventRejected,
		Target: "main",
		Record: rec,
	}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"run=20260101T000000Z-abc123",
		"outcome=rejected",
		"lint=passed(1.2s)",
		"test=failed(3.4s)",
		"wall=4.6s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestLogChannel_EmitSwallowsWriteFailure(t *testing.T) {
	c := NewLogChannel(failingWriter{})
	err := c.Emit(context.Background(), core.Event{Kind: core.EventQueued, Target: "main"})
	if err != nil {
		t.Fatalf("Emit must swallow write errors, got: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("boom") }

func TestLogChannel_NilWriterDefaultsToStderr(t *testing.T) {
	c := NewLogChannel(nil)
	if c.w != os.Stderr {
		t.Fatalf("expected default writer os.Stderr, got %v", c.w)
	}
}

func TestLogChannel_CommandsNeverYields(t *testing.T) {
	c := NewLogChannel(io.Discard)
	select {
	case cmd, ok := <-c.Commands():
		t.Fatalf("expected no command, got %v (ok=%v)", cmd, ok)
	case <-time.After(20 * time.Millisecond):
		// expected: nothing arrived
	}
}
