package slack

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// testTimeout bounds every wait in this file. Generous for CI, but every
// wait synchronizes on a channel (fakeSlack's postCh/connCh, Slack's own
// notify, or Commands()) rather than a wall-clock sleep loop.
const testTimeout = 5 * time.Second

// newTestSlack builds a Slack wired to a fresh fakeSlack, starts Run in the
// background, waits for the socket-mode connection to be established, and
// registers cleanup that cancels ctx and waits for Run to return.
func newTestSlack(t *testing.T, log io.Writer) (*Slack, *fakeSlack, context.Context) {
	t.Helper()
	fake := newFakeSlack(t)
	if log == nil {
		log = io.Discard
	}

	s := New(Params{
		Channel:  "C_TARGET",
		AppToken: "xapp-test",
		BotToken: "xoxb-test",
		APIURL:   fake.apiURL(),
		Log:      log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v after ctx cancel, want nil", err)
			}
		case <-time.After(testTimeout):
			t.Fatal("Run did not return after ctx cancel")
		}
	})

	fake.waitForConn(testTimeout)
	return s, fake, ctx
}

func mustEmit(t *testing.T, s *Slack, ctx context.Context, ev core.Event) {
	t.Helper()
	if err := s.Emit(ctx, ev); err != nil {
		t.Fatalf("Emit(kind=%d): %v", ev.Kind, err)
	}
}

// waitForStats blocks until Stats() reports (wantRuns, wantRoots),
// synchronizing on s.notify (closed and replaced every time the drainer
// finishes handling one outbound event, including any map mutation)
// rather than polling with a sleep: a post landing at the fake server
// happens before the client has processed the response and updated its
// maps, so tests must wait for "processed", not just "posted".
func waitForStats(t *testing.T, s *Slack, timeout time.Duration, wantRuns, wantRoots int) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		runs, roots := len(s.runRoot), len(s.roots)
		wake := s.notify
		s.mu.Unlock()
		if runs == wantRuns && roots == wantRoots {
			return
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("Stats() = (%d,%d) after %s, want (%d,%d)", runs, roots, timeout, wantRuns, wantRoots)
		}
	}
}

// waitForStatsZero is waitForStats(t, s, timeout, 0, 0).
func waitForStatsZero(t *testing.T, s *Slack, timeout time.Duration) {
	t.Helper()
	waitForStats(t, s, timeout, 0, 0)
}

func TestSlack_TrialCleanPostsRootCheckThreadsAndTerminalEdit(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)

	cand := core.Candidate{Ref: "refs/heads/for/main/alice/widgets", Target: "main", User: "alice", Topic: "widgets", SHA: "deadbeef"}
	runID := "run-1"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]
	if root.method != "chat.postMessage" || root.threadTS != "" {
		t.Fatalf("root post = %+v, want a top-level chat.postMessage", root)
	}
	for _, want := range []string{"widgets", "alice", "main"} {
		if !strings.Contains(root.text, want) {
			t.Errorf("root text = %q, want it to contain %q", root.text, want)
		}
	}
	waitForStats(t, s, testTimeout, 1, 1)

	mustEmit(t, s, ctx, core.Event{Kind: core.EventCheckStarted, Target: "main", Candidate: cand, RunID: runID, CheckName: "lint"})
	posts = fake.waitForPosts(2, testTimeout)
	reply := posts[1]
	if reply.method != "chat.postMessage" || reply.threadTS != root.ts {
		t.Fatalf("check-started reply = %+v, want threaded on %q", reply, root.ts)
	}
	if !strings.Contains(reply.text, "lint") {
		t.Errorf("reply text = %q, want it to mention the check name", reply.text)
	}

	mustEmit(t, s, ctx, core.Event{Kind: core.EventCheckFinished, Target: "main", Candidate: cand, RunID: runID, CheckName: "lint"})
	fake.waitForPosts(3, testTimeout)

	rec := &core.RunRecord{
		RunID:   runID,
		Target:  "main",
		Outcome: core.OutcomeLanded,
		Checks:  []core.CheckResult{{Name: "lint", Status: core.CheckPassed, Duration: 2 * time.Second}},
	}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cand, RunID: runID, Record: rec})
	posts = fake.waitForPosts(5, testTimeout)

	update := posts[3]
	if update.method != "chat.update" || update.ts != root.ts {
		t.Fatalf("terminal edit = %+v, want a chat.update on %q", update, root.ts)
	}
	if !strings.Contains(update.text, "✅") {
		t.Errorf("terminal edit text = %q, want a ✅ verdict", update.text)
	}

	summary := posts[4]
	if summary.method != "chat.postMessage" || summary.threadTS != root.ts {
		t.Fatalf("final summary = %+v, want threaded on %q", summary, root.ts)
	}
	for _, want := range []string{"lint", runID} {
		if !strings.Contains(summary.text, want) {
			t.Errorf("final summary text = %q, want it to contain %q", summary.text, want)
		}
	}

	waitForStatsZero(t, s, testTimeout)
}

func TestSlack_TerminalOutcomesEditRootWithExpectedEmojiAndClearMaps(t *testing.T) {
	cases := []struct {
		name      string
		kind      core.EventKind
		outcome   core.Outcome
		wantEmoji string
	}{
		{"landed", core.EventLanded, core.OutcomeLanded, "✅"},
		{"rejected", core.EventRejected, core.OutcomeRejected, "❌"},
		{"trial_conflict", core.EventTrialConflict, core.OutcomeConflict, "❌"},
		{"skipped", core.EventSkipped, core.OutcomeSkipped, "⚠️"},
		{"error", core.EventError, core.OutcomeError, "❌"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, ctx := newTestSlack(t, nil)
			cand := core.Candidate{Ref: "refs/heads/for/main/bob/" + tc.name, Target: "main", User: "bob", Topic: tc.name, SHA: "cafef00d"}
			runID := "run-" + tc.name

			mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
			posts := fake.waitForPosts(1, testTimeout)
			root := posts[0]

			rec := &core.RunRecord{RunID: runID, Target: "main", Outcome: tc.outcome, Detail: "detail-" + tc.name}
			mustEmit(t, s, ctx, core.Event{Kind: tc.kind, Target: "main", Candidate: cand, RunID: runID, Record: rec})
			posts = fake.waitForPosts(3, testTimeout)

			update := posts[1]
			if update.method != "chat.update" || update.ts != root.ts {
				t.Fatalf("terminal edit = %+v, want a chat.update on %q", update, root.ts)
			}
			if !strings.Contains(update.text, tc.wantEmoji) {
				t.Errorf("terminal edit text = %q, want emoji %q", update.text, tc.wantEmoji)
			}

			waitForStatsZero(t, s, testTimeout)
		})
	}
}

func TestSlack_RejectedFinalSummaryIncludesFailingCheckOutput(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/erin/thing", Target: "main", User: "erin", Topic: "thing", SHA: "f00dcafe"}
	runID := "run-rejected-output"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]

	rec := &core.RunRecord{
		RunID:  runID,
		Target: "main",
		Checks: []core.CheckResult{
			{Name: "test", Status: core.CheckFailed, Duration: 148 * time.Millisecond,
				Output: "airbag_test.go:18: deploy at 148ms, want <= 25ms\n"},
		},
		Outcome: core.OutcomeRejected,
	}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventRejected, Target: "main", Candidate: cand, RunID: runID, Record: rec})
	posts = fake.waitForPosts(3, testTimeout)

	summary := posts[2]
	if summary.method != "chat.postMessage" || summary.threadTS != root.ts {
		t.Fatalf("final summary = %+v, want threaded on %q", summary, root.ts)
	}
	wantBlock := "```\nairbag_test.go:18: deploy at 148ms, want <= 25ms\n```"
	if !strings.Contains(summary.text, wantBlock) {
		t.Errorf("final summary text = %q, want it to contain the code block %q", summary.text, wantBlock)
	}

	waitForStatsZero(t, s, testTimeout)
}

func TestSlack_LandedFinalSummaryHasNoCodeBlock(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/frank/thing", Target: "main", User: "frank", Topic: "thing", SHA: "abad1dea"}
	runID := "run-landed-no-block"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	fake.waitForPosts(1, testTimeout)

	rec := &core.RunRecord{
		RunID:   runID,
		Target:  "main",
		Checks:  []core.CheckResult{{Name: "test", Status: core.CheckPassed, Duration: 148 * time.Millisecond}},
		Outcome: core.OutcomeLanded,
	}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cand, RunID: runID, Record: rec})
	posts := fake.waitForPosts(3, testTimeout)

	summary := posts[2]
	if strings.Contains(summary.text, "```") {
		t.Errorf("final summary text = %q, want no code block for a landed run", summary.text)
	}

	waitForStatsZero(t, s, testTimeout)
}

func TestSlack_HookFailedPostsStandaloneMessageWithTail(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/grace/deploy", Target: "main", User: "grace", Topic: "deploy", SHA: "feedface"}

	check := &core.CheckResult{Name: "deploy", Status: core.CheckFailed, Duration: 3 * time.Second, Output: "connection refused: 10.0.0.1:443\n"}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventHookFinished, Target: "main", Candidate: cand, RunID: "run-hook-fail", CheckName: "deploy", Check: check})

	posts := fake.waitForPosts(1, testTimeout)
	post := posts[0]
	if post.method != "chat.postMessage" || post.threadTS != "" {
		t.Fatalf("hook-failed post = %+v, want a standalone (non-threaded) chat.postMessage", post)
	}
	for _, want := range []string{"deploy", "deploy", "grace", "main"} {
		if !strings.Contains(post.text, want) {
			t.Errorf("hook-failed text = %q, want it to contain %q", post.text, want)
		}
	}
	wantBlock := "```\nconnection refused: 10.0.0.1:443\n```"
	if !strings.Contains(post.text, wantBlock) {
		t.Errorf("hook-failed text = %q, want it to contain the code block %q", post.text, wantBlock)
	}
}

func TestSlack_HookPassedPostsNothing(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/henry/deploy", Target: "main", User: "henry", Topic: "deploy", SHA: "0ff1ce"}

	check := &core.CheckResult{Name: "deploy", Status: core.CheckPassed, Duration: time.Second}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventHookFinished, Target: "main", Candidate: cand, RunID: "run-hook-pass", CheckName: "deploy", Check: check})

	// Synchronize on notify (fires once the drainer has fully handled the
	// event) rather than a wall-clock sleep before asserting absence.
	s.mu.Lock()
	wake := s.notify
	s.mu.Unlock()
	select {
	case <-wake:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the event to be processed")
	}

	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no Slack post for a passed hook, got %+v", got)
	}
}

func TestSlack_TerminalWithoutKnownRootIsANoOp(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)

	rec := &core.RunRecord{RunID: "unknown-run", Target: "main", Outcome: core.OutcomeLanded}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "unknown-run", Record: rec})

	// Give the drainer a chance to process the event, synchronizing on
	// notify rather than a sleep: signalProcessed fires once per handled
	// event regardless of whether it did anything.
	s.mu.Lock()
	wake := s.notify
	s.mu.Unlock()
	select {
	case <-wake:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the event to be processed")
	}

	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no Slack calls for an unknown run, got %+v", got)
	}
}

func TestSlack_RecycleReactionOnOwnedRootProducesRetryCommand(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/carol/thing", Target: "main", User: "carol", Topic: "thing", SHA: "beadface"}

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: "run-recycle"})
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", root.ts)

	select {
	case cmd := <-s.Commands():
		if cmd.Kind != core.CommandRetry || cmd.Target != "main" || cmd.Ref != cand.Ref {
			t.Fatalf("Command = %+v, want retry for target=main ref=%s", cmd, cand.Ref)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a retry Command")
	}
}

// TestSlack_XReactionOnOwnedRootProducesCancelCommand mirrors
// TestSlack_RecycleReactionOnOwnedRootProducesRetryCommand exactly (Feature
// 1: manual operator cancellation) — same wiring, differing only in the
// reaction name and the resulting Command.Kind.
func TestSlack_XReactionOnOwnedRootProducesCancelCommand(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/carol/thing", Target: "main", User: "carol", Topic: "thing", SHA: "beadface"}

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: "run-cancel"})
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "x", root.ts)

	select {
	case cmd := <-s.Commands():
		if cmd.Kind != core.CommandCancel || cmd.Target != "main" || cmd.Ref != cand.Ref {
			t.Fatalf("Command = %+v, want cancel for target=main ref=%s", cmd, cand.Ref)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a cancel Command")
	}
}

func TestSlack_ReactionOnUnownedTimestampIgnored(t *testing.T) {
	s, fake, _ := newTestSlack(t, nil)
	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", "9999999999.000000")

	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("expected no Command for a reaction on an unowned ts, got %v (ok=%v)", cmd, ok)
	case <-time.After(150 * time.Millisecond):
		// expected: nothing arrived
	}
}

func TestSlack_UnknownReactionOnOwnedRootIgnored(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/dave/thing", Target: "main", User: "dave", Topic: "thing", SHA: "b16b00b5"}

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: "run-other-reaction"})
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "thumbsup", root.ts)

	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("expected no Command for a non-recycle reaction, got %v (ok=%v)", cmd, ok)
	case <-time.After(150 * time.Millisecond):
		// expected: nothing arrived
	}
}

func TestSlack_EmitNeverBlocksWhenDrainerNotRunning(t *testing.T) {
	fake := newFakeSlack(t)
	var logBuf bytes.Buffer
	s := New(Params{Channel: "C1", AppToken: "xapp", BotToken: "xoxb", APIURL: fake.apiURL(), Log: &logBuf})

	ctx := context.Background()
	for i := range outboxBuffer {
		if err := s.Emit(ctx, core.Event{Kind: core.EventQueued, Target: "main"}); err != nil {
			t.Fatalf("Emit #%d: %v", i, err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Emit(ctx, core.Event{Kind: core.EventQueued, Target: "main", RunID: "overflow"}); err != nil {
			t.Errorf("Emit past a full outbox: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Emit blocked with a full outbox and no drainer running")
	}

	if logBuf.Len() == 0 {
		t.Error("expected a dropped-event log line, got nothing")
	}
}

func TestSlack_ContextCancelStopsRunCleanly(t *testing.T) {
	fake := newFakeSlack(t)
	s := New(Params{Channel: "C1", AppToken: "xapp", BotToken: "xoxb", APIURL: fake.apiURL(), Log: io.Discard})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	fake.waitForConn(testTimeout)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after ctx cancel, want nil", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestSlack_NilLogDefaultsToStderr(t *testing.T) {
	s := New(Params{Channel: "C1", AppToken: "xapp", BotToken: "xoxb"})
	if s.log != os.Stderr {
		t.Fatalf("log = %v, want os.Stderr", s.log)
	}
}
