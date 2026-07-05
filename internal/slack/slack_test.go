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

// waitForBatchArrived blocks until batchID's buffered batchEntry has
// recorded at least want arrivals, synchronizing on s.notify (as
// waitForStats does) rather than polling with a sleep. Needed whenever a
// test cares that a SPECIFIC member's terminal event has been fully
// processed but that event produces no post of its own to wait on (no
// edit: Position != 0; no flush: neither trigger fires) — waitForPosts/
// waitForStats alone can be satisfied by an EARLIER member's processing
// and race ahead of a later one still sitting in the outbox.
func waitForBatchArrived(t *testing.T, s *Slack, batchID string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		var got int
		if be := s.batchRecs[batchID]; be != nil {
			got = be.arrived
		}
		wake := s.notify
		s.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("batch %s arrived = %d after %s, want %d", batchID, got, timeout, want)
		}
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

// TestSlack_BatchSummaryToleratesMiddleMemberHole proves F1a/F1b (the
// phase-5 review): a batch member's terminal event can be silently lost to
// Emit's outbox-full drop (§9.2's "if the outbox is full, the event is
// logged and dropped") — nothing ever re-delivers it. The old
// summarizeBatch assumed every Position slot was filled and nil-dereferenced
// on recs[0] the instant any slot was empty (a drainer-goroutine panic,
// i.e. a process crash). Skipping bob's (Position 1, the middle member's)
// Emit entirely must not panic; the LAST member's arrival (Position ==
// BatchSize-1) still triggers the flush, the summary explicitly notes the
// gap instead of silently omitting bob, and every map still ends up clean.
func TestSlack_BatchSummaryToleratesMiddleMemberHole(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	batchID, cands, runIDs := batchMembers()

	for _, cand := range cands {
		mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: batchID})
	}
	root := fake.waitForPosts(1, testTimeout)[0]
	waitForStats(t, s, testTimeout, 1, 1)

	checks := []core.CheckResult{{Name: "test", Status: core.CheckPassed, Duration: 42 * time.Millisecond}}
	recAt := func(i int) *core.RunRecord {
		return &core.RunRecord{
			RunID: runIDs[i], Target: "main", Candidate: cands[i],
			Outcome: core.OutcomeLanded, Checks: checks,
			BatchID: batchID, Position: i, BatchSize: len(cands),
		}
	}

	// Position 0 (alice) and Position 2 (carol, == BatchSize-1) arrive;
	// Position 1 (bob) never does.
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cands[0], RunID: runIDs[0], Record: recAt(0)})
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cands[2], RunID: runIDs[2], Record: recAt(2)})

	posts := fake.waitForPosts(3, testTimeout) // root + edit + summary — no panic despite the hole
	edit := posts[1]
	if edit.method != "chat.update" || edit.ts != root.ts {
		t.Fatalf("batch edit = %+v, want a single chat.update on the root", edit)
	}

	summary := posts[2]
	if summary.method != "chat.postMessage" || summary.threadTS != root.ts {
		t.Fatalf("batch summary = %+v, want ONE threaded reply on the root", summary)
	}
	for _, want := range []string{"alice", "carol", "1 member(s) whose events were dropped"} {
		if !strings.Contains(summary.text, want) {
			t.Errorf("summary text = %q, want it to mention %q", summary.text, want)
		}
	}
	if strings.Contains(summary.text, "bob") {
		t.Errorf("summary text = %q, must not mention bob (his event never arrived)", summary.text)
	}

	waitForStatsZero(t, s, testTimeout) // maps clean despite the hole
	if got := fake.snapshotPosts(); len(got) != 3 {
		t.Fatalf("total posts = %d, want exactly 3 (root, edit, one summary)", len(got))
	}
}

// TestSlack_BatchStaleEntryFlushedOnAnotherBatchArrival proves F1c: a batch
// whose flush-triggering arrival is ITSELF the event Emit dropped (here,
// carol's Position == BatchSize-1 terminal event, the old code's ONLY flush
// trigger) would buffer — and leak its runRoot/roots/batchRecs entries —
// forever without the staleness sweep. The sweep runs opportunistically, on
// some OTHER batch's terminal arrival (no dedicated goroutine or timer): a
// second, unrelated batch's own first terminal event, arriving more than
// batchStaleTimeout after the stuck batch was last touched, force-flushes
// the stuck batch (with a hole noted for its missing member) and forgets
// it, leaving the second batch's own (still mid-flight) state untouched.
func TestSlack_BatchStaleEntryFlushedOnAnotherBatchArrival(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	batchID, cands, runIDs := batchMembers()

	for _, cand := range cands {
		mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: batchID})
	}
	root1 := fake.waitForPosts(1, testTimeout)[0]
	waitForStats(t, s, testTimeout, 1, 1)

	checks := []core.CheckResult{{Name: "test", Status: core.CheckPassed, Duration: 42 * time.Millisecond}}
	recAt := func(i int) *core.RunRecord {
		return &core.RunRecord{
			RunID: runIDs[i], Target: "main", Candidate: cands[i],
			Outcome: core.OutcomeLanded, Checks: checks,
			BatchID: batchID, Position: i, BatchSize: len(cands),
		}
	}
	// Only Position 0 (alice) and Position 1 (bob) arrive; Position 2
	// (carol, == BatchSize-1) never does — simulating THAT specific event
	// being the one Emit dropped, so neither of the two flush triggers
	// (arrived == BatchSize, or Position == BatchSize-1) is ever satisfied.
	// Neither arrival posts anything of its own (the root headline edit is
	// now produced at flush time, alongside the summary — see
	// postBatchTerminal's own doc comment), so nothing here adds to
	// fake's post count; waitForBatchArrived is what actually pins "both
	// arrivals are fully processed" before this test starts fiddling with
	// s.now below (a race that would otherwise let bob's own
	// be.lastTouched write land AFTER the override, corrupting the very
	// staleness this test means to provoke).
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cands[0], RunID: runIDs[0], Record: recAt(0)})
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cands[1], RunID: runIDs[1], Record: recAt(1)})
	waitForBatchArrived(t, s, batchID, 2, testTimeout)
	waitForStats(t, s, testTimeout, 1, 1) // batch1's root/run-tracking entries still present; no flush yet

	// Advance the clock 11 minutes (past batchStaleTimeout) relative to
	// real wall-clock time, directly — the sweep has no timer of its own,
	// so nothing observes this until some batch's own next terminal
	// arrival checks it.
	frozen := time.Now().Add(11 * time.Minute)
	s.mu.Lock()
	s.now = func() time.Time { return frozen }
	s.mu.Unlock()

	batchID2 := "20260705T130000Z-2-def456abc123"
	cands2 := []core.Candidate{
		{Ref: "refs/heads/for/main/dave/x", Target: "main", User: "dave", Topic: "x", SHA: "dddd"},
		{Ref: "refs/heads/for/main/erin/y", Target: "main", User: "erin", Topic: "y", SHA: "eeee"},
	}
	for _, cand := range cands2 {
		mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: batchID2})
	}
	fake.waitForPosts(2, testTimeout) // root1, batch2's own root (batch2's own edit is deferred to ITS flush, which never happens in this test)

	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cands2[0], RunID: batchID2, Record: &core.RunRecord{
		RunID: batchID2, Target: "main", Candidate: cands2[0], Outcome: core.OutcomeLanded, Checks: checks,
		BatchID: batchID2, Position: 0, BatchSize: 2,
	}})

	// batch2's own Position-0 arrival (not itself a flush trigger: 1 of 2
	// members in, so it posts nothing of its own) piggybacks the staleness
	// sweep, which finds batch1 stale and force-flushes it — headline edit
	// + summary, together — with a hole noted for its missing (carol)
	// member.
	posts := fake.waitForPosts(4, testTimeout) // root1, root2, batch1's stale-flush edit + summary
	var staleSummary *postedMessage
	for i := range posts {
		if posts[i].threadTS == root1.ts && posts[i].method == "chat.postMessage" {
			staleSummary = &posts[i]
		}
	}
	if staleSummary == nil {
		t.Fatalf("no stale-flush summary posted on batch1's root among %+v", posts)
	}
	for _, want := range []string{"alice", "bob", "1 member(s) whose events were dropped"} {
		if !strings.Contains(staleSummary.text, want) {
			t.Errorf("stale summary text = %q, want it to mention %q", staleSummary.text, want)
		}
	}

	// batch1 is fully forgotten; batch2 is untouched (still mid-batch, 1 of
	// 2 members in).
	waitForStats(t, s, testTimeout, 1, 1) // only batch2's root/run-tracking entries remain
	s.mu.Lock()
	_, batch1Present := s.batchRecs[batchID]
	_, batch2Present := s.batchRecs[batchID2]
	n := len(s.batchRecs)
	s.mu.Unlock()
	if batch1Present {
		t.Fatal("batch1 must be forgotten after the staleness sweep")
	}
	if !batch2Present || n != 1 {
		t.Fatalf("batchRecs = %d entries (batch2 present=%v), want exactly batch2's", n, batch2Present)
	}
}

// TestSlack_SingleMemberBatchRendersLikeSerial proves the live-Slack
// addendum to F1 (the phase-5 review): a batch formed with exactly one
// member — max-batch 1, or a queue that only ever offered one candidate;
// §4.1 promises this "degrades to serial behavior" byte for byte — must
// render its root headline edit and threaded summary IDENTICALLY to a
// genuine serial run's own messages, not "batch <runID> (1 members) →
// target" (the live bug: broken grammar, and it drops the topic/user
// entirely). This drives the exact same candidate/checks/outcome through
// both the serial path (BatchID == "") and the batch-of-one path (BatchID
// set, Position 0, BatchSize 1) on two independent Slack instances, and
// diffs the resulting message text byte for byte.
func TestSlack_SingleMemberBatchRendersLikeSerial(t *testing.T) {
	cand := core.Candidate{Ref: "refs/heads/for/main/alice/widget", Target: "main", User: "alice", Topic: "widget", SHA: "deadbeef"}
	runID := "20260705T130000Z-1-abc123def456"
	checks := []core.CheckResult{{Name: "test", Status: core.CheckPassed, Duration: 42 * time.Millisecond}}

	serialS, serialFake, serialCtx := newTestSlack(t, nil)
	mustEmit(t, serialS, serialCtx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	serialFake.waitForPosts(1, testTimeout)
	mustEmit(t, serialS, serialCtx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cand, RunID: runID, Record: &core.RunRecord{
		RunID: runID, Target: "main", Candidate: cand, Outcome: core.OutcomeLanded, Checks: checks,
	}})
	serialPosts := serialFake.waitForPosts(3, testTimeout) // root, edit, summary

	batchS, batchFake, batchCtx := newTestSlack(t, nil)
	mustEmit(t, batchS, batchCtx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	batchFake.waitForPosts(1, testTimeout)
	mustEmit(t, batchS, batchCtx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cand, RunID: runID, Record: &core.RunRecord{
		RunID: runID, Target: "main", Candidate: cand, Outcome: core.OutcomeLanded, Checks: checks,
		BatchID: runID, Position: 0, BatchSize: 1,
	}})
	batchPosts := batchFake.waitForPosts(3, testTimeout) // root, edit, summary

	serialEdit, batchEdit := serialPosts[1], batchPosts[1]
	if batchEdit.text != serialEdit.text {
		t.Fatalf("batch-of-one root edit = %q, want byte-identical to serial's %q", batchEdit.text, serialEdit.text)
	}
	if strings.Contains(batchEdit.text, "batch") || strings.Contains(batchEdit.text, "member") {
		t.Errorf("batch-of-one root edit = %q, must not mention batch/members at all", batchEdit.text)
	}

	serialSummary, batchSummary := serialPosts[2], batchPosts[2]
	if batchSummary.text != serialSummary.text {
		t.Fatalf("batch-of-one summary = %q, want byte-identical to serial's %q", batchSummary.text, serialSummary.text)
	}

	waitForStatsZero(t, serialS, testTimeout)
	waitForStatsZero(t, batchS, testTimeout)
}
