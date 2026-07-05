package slack

import (
	"bytes"
	"context"
	"fmt"
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

// waitForStatsZero is waitForStats(t, s, timeout, 0, 0), plus the same §9.2
// leak-bound assertion for batchRecs — the third run-tracking map, which
// must also be empty once every run/batch has fully resolved.
func waitForStatsZero(t *testing.T, s *Slack, timeout time.Duration) {
	t.Helper()
	waitForStats(t, s, timeout, 0, 0)
	s.mu.Lock()
	n := len(s.batchRecs)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("batchRecs has %d entries after all runs resolved, want 0 (§9.2 leak bound)", n)
	}
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

// batchMembers is the 3-candidate fixture both batch Slack tests below
// share: a batch's chain-building fires one EventTrialClean per member, all
// sharing the batch's bare run ID (== BatchID); its landing/red-skip fires
// one terminal event per member, each carrying a distinct
// "<batchID>"/"<batchID>-m1"/"<batchID>-m2" RunID (queue.memberRunID's
// data-loss fix) but the same shared BatchID.
func batchMembers() (batchID string, cands []core.Candidate, runIDs []string) {
	batchID = "20260705T130000Z-1-abc123def456"
	cands = []core.Candidate{
		{Ref: "refs/heads/for/main/alice/a", Target: "main", User: "alice", Topic: "a", SHA: "aaaa"},
		{Ref: "refs/heads/for/main/bob/b", Target: "main", User: "bob", Topic: "b", SHA: "bbbb"},
		{Ref: "refs/heads/for/main/carol/c", Target: "main", User: "carol", Topic: "c", SHA: "cccc"},
	}
	runIDs = []string{batchID, batchID + "-m1", batchID + "-m2"}
	return batchID, cands, runIDs
}

// TestSlack_BatchLandingPostsOneRootOneEditOneSummary covers §3.3 addendum's
// batch-aware join for a green batch: 3 EventTrialClean events (chain
// building, one per member, all sharing batchID) must post exactly ONE root
// message — not one per member (postRoot's new idempotency guard) — then 3
// EventLanded terminal events (one per member, each with its own distinct
// RunID but the shared BatchID) must produce exactly one root edit (at the
// FIRST member processed) and exactly ONE threaded summary reply (posted
// once the LAST member, Position == BatchSize-1, arrives) that names every
// member — not one noisy reply per member — with both run-tracking maps
// back to zero afterward (§9.2).
func TestSlack_BatchLandingPostsOneRootOneEditOneSummary(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	batchID, cands, runIDs := batchMembers()

	for _, cand := range cands {
		mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: batchID})
	}
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]
	if root.method != "chat.postMessage" || root.threadTS != "" {
		t.Fatalf("root post = %+v, want a single top-level chat.postMessage despite 3 trial-clean events", root)
	}
	waitForStats(t, s, testTimeout, 1, 1)

	checks := []core.CheckResult{{Name: "test", Status: core.CheckPassed, Duration: 42 * time.Millisecond}}
	for i, cand := range cands {
		rec := &core.RunRecord{
			RunID:     runIDs[i],
			Target:    "main",
			Candidate: cand,
			Outcome:   core.OutcomeLanded,
			Checks:    checks,
			BatchID:   batchID,
			Position:  i,
			BatchSize: len(cands),
		}
		mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cand, RunID: runIDs[i], Record: rec})
	}

	posts = fake.waitForPosts(3, testTimeout) // root + one edit + ONE summary reply
	edit := posts[1]
	if edit.method != "chat.update" || edit.ts != root.ts {
		t.Fatalf("batch edit = %+v, want a single chat.update on the root", edit)
	}
	if !strings.Contains(edit.text, "✅") {
		t.Errorf("batch edit text = %q, want a ✅ verdict", edit.text)
	}

	summary := posts[2]
	if summary.method != "chat.postMessage" || summary.threadTS != root.ts {
		t.Fatalf("batch summary = %+v, want ONE threaded reply on the root", summary)
	}
	for _, want := range []string{"alice", "bob", "carol", batchID} {
		if !strings.Contains(summary.text, want) {
			t.Errorf("summary text = %q, want it to mention %q", summary.text, want)
		}
	}
	for _, want := range runIDs {
		if !strings.Contains(summary.text, want) {
			t.Errorf("summary text = %q, want it to list member run id %q", summary.text, want)
		}
	}

	waitForStatsZero(t, s, testTimeout)

	// No further posts after the last member's terminal event — proves the
	// summary really is posted once, not once per member.
	if got := fake.snapshotPosts(); len(got) != 3 {
		t.Fatalf("total posts = %d, want exactly 3 (root, edit, one summary)", len(got))
	}
}

// TestSlack_BatchRedSkippedPostsOneRootOneEditOneSummary mirrors the green
// case for a batch-red run (queue.finishBatchRed): every member's terminal
// event is EventSkipped, not EventLanded, but each still carries a Record
// with BatchID set (the join/forget logic doesn't branch on Outcome) — so
// the same one-root/one-edit/one-summary shape must hold, with a ⚠️ verdict
// instead of ✅.
func TestSlack_BatchRedSkippedPostsOneRootOneEditOneSummary(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	batchID, cands, runIDs := batchMembers()

	for _, cand := range cands {
		mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: batchID})
	}
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]
	waitForStats(t, s, testTimeout, 1, 1)

	checks := []core.CheckResult{{Name: "test", Status: core.CheckFailed, Duration: 42 * time.Millisecond}}
	detail := fmt.Sprintf("batch %s red on check %q; serializing", batchID, "test")
	for i, cand := range cands {
		rec := &core.RunRecord{
			RunID:     runIDs[i],
			Target:    "main",
			Candidate: cand,
			Outcome:   core.OutcomeSkipped,
			Checks:    checks,
			Detail:    detail,
			BatchID:   batchID,
			Position:  i,
			BatchSize: len(cands),
		}
		mustEmit(t, s, ctx, core.Event{Kind: core.EventSkipped, Target: "main", Candidate: cand, RunID: runIDs[i], Record: rec})
	}

	posts = fake.waitForPosts(3, testTimeout)
	edit := posts[1]
	if edit.method != "chat.update" || edit.ts != root.ts {
		t.Fatalf("batch edit = %+v, want a single chat.update on the root", edit)
	}
	if !strings.Contains(edit.text, "⚠️") {
		t.Errorf("batch edit text = %q, want a ⚠️ verdict", edit.text)
	}

	summary := posts[2]
	if summary.method != "chat.postMessage" || summary.threadTS != root.ts {
		t.Fatalf("batch summary = %+v, want ONE threaded reply on the root", summary)
	}
	for _, want := range []string{"alice", "bob", "carol", detail} {
		if !strings.Contains(summary.text, want) {
			t.Errorf("summary text = %q, want it to mention %q", summary.text, want)
		}
	}

	waitForStatsZero(t, s, testTimeout)
	if got := fake.snapshotPosts(); len(got) != 3 {
		t.Fatalf("total posts = %d, want exactly 3 (root, edit, one summary)", len(got))
	}
}
