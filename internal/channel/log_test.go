package channel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
			name: "check finished with Check",
			ev: core.Event{
				Kind:      core.EventCheckFinished,
				Target:    "main",
				RunID:     "run-1",
				CheckName: "lint",
				Check:     &core.CheckResult{Name: "lint", Status: core.CheckFailed, Duration: 1500 * time.Millisecond},
			},
			want: []string{"kind=check_finished", "run=run-1", "check=lint", "status=failed", "duration=1.5s"},
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
		{
			name: "ignored ref",
			ev: core.Event{
				Kind:   core.EventIgnoredRef,
				Target: "staging",
				Candidate: core.Candidate{
					Ref: "refs/heads/for/staging/alice/feat",
					SHA: "deadbeefcafef00d",
				},
				Detail: `target "staging" is not configured`,
			},
			want: []string{"kind=ignored_ref", "ref=refs/heads/for/staging/alice/feat", `detail="target \"staging\" is not configured"`},
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

// TestLogChannel_CheckFinishedWithoutCheckOmitsStatusDuration asserts the
// nil-Check fallback: an EventCheckFinished with Check == nil must not
// render a status/duration field at all (only kind/run/check, as before
// F-a — DESIGN.md "Full per-check log files").
func TestLogChannel_CheckFinishedWithoutCheckOmitsStatusDuration(t *testing.T) {
	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	ev := core.Event{Kind: core.EventCheckFinished, Target: "main", RunID: "run-1", CheckName: "lint"}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "status=") || strings.Contains(out, "duration=") {
		t.Errorf("output %q should have no status/duration field when Event.Check is nil", out)
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

func TestLogChannel_EmitFailureBlock(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 15 numbered lines plus the interesting failure line: with
	// failureTailMaxLines=10, only the last 10 (dropping "line 1".."line
	// 5") should make it into the rendered block.
	var lines []string
	for i := 1; i <= 15; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines = append(lines, "airbag_test.go:18: deploy at 148ms, want <= 25ms")
	output := strings.Join(lines, "\n") + "\n"

	rec := &core.RunRecord{
		RunID:  "run-1",
		Target: "main",
		Checks: []core.CheckResult{
			{Name: "lint", Status: core.CheckPassed, Duration: time.Second},
			{Name: "test", Status: core.CheckFailed, Duration: time.Second, Output: output},
		},
		Outcome:   core.OutcomeRejected,
		StartedAt: start,
		EndedAt:   start.Add(2 * time.Second),
	}
	ev := core.Event{Kind: core.EventRejected, Target: "main", Record: rec}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "    | airbag_test.go:18: deploy at 148ms, want <= 25ms") {
		t.Errorf("output %q missing indented failure block", out)
	}
	if strings.Contains(out, "    | line 5") {
		t.Errorf("output %q should be capped to the last 10 lines, not include line 5", out)
	}
	if !strings.Contains(out, "    | line 15") {
		t.Errorf("output %q should retain lines within the last-10-line cap", out)
	}
}

func TestLogChannel_EmitOmitsFailureBlockWhenNoFailure(t *testing.T) {
	rec := &core.RunRecord{
		RunID:  "run-2",
		Target: "main",
		Checks: []core.CheckResult{
			{Name: "lint", Status: core.CheckPassed, Duration: time.Second},
		},
		Outcome: core.OutcomeLanded,
	}
	ev := core.Event{Kind: core.EventLanded, Target: "main", Record: rec}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(buf.String(), "    | ") {
		t.Errorf("output %q should have no failure block for a landed run", buf.String())
	}
}

func TestLogChannel_EmitHookFinished(t *testing.T) {
	ev := core.Event{
		Kind:      core.EventHookFinished,
		Target:    "main",
		RunID:     "run-1",
		CheckName: "deploy",
		Check: &core.CheckResult{
			Name:     "deploy",
			Status:   core.CheckPassed,
			Duration: 800 * time.Millisecond,
		},
	}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"kind=hook_finished", "check=deploy", "hook=deploy", "status=passed", "duration=800ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestLogChannel_EmitHookFinishedFailure(t *testing.T) {
	ev := core.Event{
		Kind:      core.EventHookFinished,
		Target:    "main",
		RunID:     "run-1",
		CheckName: "deploy",
		Check: &core.CheckResult{
			Name:     "deploy",
			Status:   core.CheckFailed,
			Duration: 400 * time.Millisecond,
			Output:   "rsync: connection refused\n",
		},
	}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"status=failed", "    | rsync: connection refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestLogChannel_EmitHookFinishedNilCheckOmitsBlock(t *testing.T) {
	ev := core.Event{Kind: core.EventHookFinished, Target: "main", RunID: "run-1", CheckName: "deploy", Check: nil}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "kind=hook_finished") {
		t.Errorf("output %q missing kind=hook_finished", out)
	}
	if strings.Contains(out, "hook=") {
		t.Errorf("output %q should have no hook= line when Check is nil, got %q", out, out)
	}
}

// TestLogChannel_EmitHookStarted confirms EventHookStarted renders its own
// distinct greppable marker line, in addition to formatEvent's generic
// kind=/target=/check= fields.
func TestLogChannel_EmitHookStarted(t *testing.T) {
	ev := core.Event{Kind: core.EventHookStarted, Target: "main", RunID: "run-1", CheckName: "deploy"}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"kind=hook_started", "check=deploy", "▶ hook deploy"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

// TestLogChannel_EmitHookSkipped confirms EventHookSkipped (a
// recovery-skipped landing's hooks never ran at all) renders a distinct
// marker line including Detail.
func TestLogChannel_EmitHookSkipped(t *testing.T) {
	ev := core.Event{Kind: core.EventHookSkipped, Target: "main", RunID: "run-1", Detail: "recovered landing; hooks not run"}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"kind=hook_skipped", "⚠ hooks skipped (recovery)", "recovered landing; hooks not run"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

// TestLogChannel_EmitRetryRequested confirms EventRetryRequested (a
// history-only durability signal) still renders via formatEvent's generic
// fields — LogChannel logs every event
// unconditionally (unlike Slack, which filters), so no special-case block is
// needed beyond the eventKindString name.
func TestLogChannel_EmitRetryRequested(t *testing.T) {
	ev := core.Event{
		Kind:      core.EventRetryRequested,
		Target:    "main",
		Candidate: core.Candidate{Ref: "refs/heads/for/main/alice/feat", SHA: "deadbeefcafef00d"},
	}

	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"kind=retry_requested", "ref=refs/heads/for/main/alice/feat", "sha=deadbeef"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

// TestLogChannel_EmitUnknownEventKindNoPanic is S14's universal contract
// test for LogChannel: core.EventKind(999) (a future kind eventKindString's
// switch has never heard of) must not panic Emit, matching the package doc's
// own claim ("every core.Channel implementation is expected to hold the
// same contract — ignore event kinds it doesn't understand, never fail
// Emit over one"). Unlike a silent consumer, LogChannel's own contract is
// to still render the line, tagged "unknown(N)" rather than dropped, so
// that (not "no output") is what this asserts.
func TestLogChannel_EmitUnknownEventKindNoPanic(t *testing.T) {
	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), core.Event{Kind: core.EventKind(999), Target: "main"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(buf.String(), "unknown(999)") {
		t.Errorf("output %q missing kind=unknown(999) rendering for an unrecognized EventKind", buf.String())
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

// TestLogChannel_EventKindNames: every kind gauntlet actually emits has a
// real name here. eventKindString's default renders "unknown(N)" so an
// unnamed kind can never break Emit — which is precisely why one would
// otherwise ship silently, greppable only as a number. The three deploy
// kinds are this slice's addition; trial_merged and verified were genuinely
// missing and are named here too.
func TestLogChannel_EventKindNames(t *testing.T) {
	want := map[core.EventKind]string{
		core.EventQueued:             "queued",
		core.EventTrialClean:         "trial_clean",
		core.EventTrialConflict:      "trial_conflict",
		core.EventTrialMerged:        "trial_merged",
		core.EventCheckStarted:       "check_started",
		core.EventCheckFinished:      "check_finished",
		core.EventVerified:           "verified",
		core.EventLanded:             "landed",
		core.EventRejected:           "rejected",
		core.EventSkipped:            "skipped",
		core.EventError:              "error",
		core.EventIgnoredRef:         "ignored_ref",
		core.EventHookStarted:        "hook_started",
		core.EventHookFinished:       "hook_finished",
		core.EventHookSkipped:        "hook_skipped",
		core.EventRetryRequested:     "retry_requested",
		core.EventDeployStarted:      "deploy_started",
		core.EventDeployNodeFinished: "deploy_node_finished",
		core.EventDeployFinished:     "deploy_finished",
	}
	for kind, name := range want {
		if got := eventKindString(kind); got != name {
			t.Errorf("eventKindString(%d) = %q, want %q", int(kind), got, name)
		}
	}
}

// TestLogChannel_EmitDeployEvents: a deploy event is lane-addressed, so its
// line is greppable by env= and sha= rather than by ref=/target=.
func TestLogChannel_EmitDeployEvents(t *testing.T) {
	cases := []struct {
		name string
		ev   core.Event
		want []string
	}{
		{
			name: "started",
			ev: core.Event{
				Kind: core.EventDeployStarted, RunID: "deploy-1",
				DeployEnv: "prod", DeploySHA: "e5f6a7b8c9d0e1f2", DeployedSHA: "9c0d1e2f3a4b5c6d",
			},
			want: []string{"kind=deploy_started", "env=prod", "sha=e5f6a7b8", "run=deploy-1"},
		},
		{
			name: "node finished",
			ev: core.Event{
				Kind: core.EventDeployNodeFinished, RunID: "deploy-1", CheckName: "migrate",
				DeployEnv: "prod", DeploySHA: "e5f6a7b8c9d0e1f2",
				Check: &core.CheckResult{Name: "migrate", Status: core.CheckPassed, Duration: 22 * time.Second},
			},
			want: []string{"kind=deploy_node_finished", "env=prod", "check=migrate", "status=passed", "duration=22s"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := NewLogChannel(&buf)
			if err := c.Emit(context.Background(), tc.ev); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("line %q missing %q", buf.String(), want)
				}
			}
		})
	}
}

// TestLogChannel_EmitDeployRecordSummary: the terminal deploy event renders
// its carried record as a compact summary line — every declared node,
// blocked rows included — followed by the failing node's output tail, the
// same two-part treatment a run's terminal record gets.
func TestLogChannel_EmitDeployRecordSummary(t *testing.T) {
	started := time.Now().Add(-2 * time.Minute)
	rec := &core.DeployRecord{
		RunID: "deploy-9", Env: "prod2",
		DeploySHA: "e5f6a7b8c9d0", DeployedSHA: "9c0d1e2f3a4b",
		Nodes: []core.CheckResult{
			{Name: "migrate", Status: core.CheckFailed, Duration: 24 * time.Second, Output: "line one\nlock wait timeout"},
			{Name: "app1", Status: core.CheckBlocked, BlockedBy: []string{"migrate"}},
		},
		Outcome: core.OutcomeRejected, Culprit: "migrate", Detail: "migrate failed",
		StartedAt: started, EndedAt: started.Add(24 * time.Second),
	}
	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), core.Event{
		Kind: core.EventDeployFinished, RunID: rec.RunID,
		DeployEnv: rec.Env, DeploySHA: rec.DeploySHA, DeployedSHA: rec.DeployedSHA,
		Deploy: rec,
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"kind=deploy_finished", "deploy=deploy-9", "env=prod2", "outcome=rejected",
		"sha=e5f6a7b8", "from=9c0d1e2f",
		"migrate=failed(24s)", "app1=blocked(0s)",
		"culprit=migrate", `detail="migrate failed"`,
		"    | lock wait timeout",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered deploy record %q missing %q", out, want)
		}
	}
}

// TestLogChannel_EmitDeployRecordGreenHasNoFailureBlock: a green graph run
// summarizes and stops — there is no failing node to tail.
func TestLogChannel_EmitDeployRecordGreenHasNoFailureBlock(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	rec := &core.DeployRecord{
		RunID: "deploy-ok", Env: "dev", DeploySHA: "abcdef012345",
		Nodes: []core.CheckResult{
			{Name: "migrate", Status: core.CheckPassed, Duration: time.Second, Output: "noisy but fine"},
			{Name: "app1", Status: core.CheckSkipped, Output: "no diff"},
		},
		Outcome: core.OutcomeLanded, StartedAt: started, EndedAt: started.Add(time.Second),
	}
	var buf bytes.Buffer
	c := NewLogChannel(&buf)
	if err := c.Emit(context.Background(), core.Event{
		Kind: core.EventDeployFinished, RunID: rec.RunID,
		DeployEnv: rec.Env, DeploySHA: rec.DeploySHA, Deploy: rec,
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "outcome=landed") {
		t.Errorf("rendered %q, want outcome=landed", out)
	}
	if strings.Contains(out, "    | ") {
		t.Errorf("rendered %q, want no failure block for an all-green graph", out)
	}
	if strings.Contains(out, "from=") {
		t.Errorf("rendered %q, want no from= for a first-ever deploy (empty DeployedSHA)", out)
	}
}
