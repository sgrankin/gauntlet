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
	return newTestSlackWith(t, log, nil)
}

// newTestSlackWith is newTestSlack with a Params hook for the handful of
// tests that need a non-default option (e.g. AllowedUsers).
func newTestSlackWith(t *testing.T, log io.Writer, mut func(*Params)) (*Slack, *fakeSlack, context.Context) {
	t.Helper()
	fake := newFakeSlack(t)
	if log == nil {
		log = io.Discard
	}

	p := Params{
		Channel:  "C_TARGET",
		AppToken: "xapp-test",
		BotToken: "xoxb-test",
		APIURL:   fake.apiURL(),
		Log:      log,
	}
	if mut != nil {
		mut(&p)
	}
	s := New(p)

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

// emitAndDrain emits ev and blocks until the drainer has fully processed
// it — the synchronization the posts-nothing tests need before asserting
// absence. The wake channel MUST be grabbed before the emit: signalProcessed
// closes-and-replaces s.notify, so grabbing after the emit races the drainer
// — on a loaded machine (CI runners, not laptops) the drainer can process
// the event first, leaving the test waiting testTimeout on a replacement
// channel that only a second, never-sent event would close. That exact lost
// wakeup flaked TestSlack_HookStartedPostsNothing on the v0.1.0-era CI run.
// Grabbing first makes it impossible: processing our event necessarily
// closes the channel we hold. (The waitFor* helpers don't need this — they
// re-check their condition under the same lock that grabs the channel, so a
// missed signal is caught by the next iteration's check.)
func emitAndDrain(t *testing.T, s *Slack, ctx context.Context, ev core.Event) {
	t.Helper()
	s.mu.Lock()
	wake := s.notify
	s.mu.Unlock()
	mustEmit(t, s, ctx, ev)
	select {
	case <-wake:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the event to be processed")
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

// waitForStatsZero is waitForStats(t, s, timeout, 0, 0), plus the same
// leak-bound assertion for batchRecs — the third run-tracking map, which
// must also be empty once every run/batch has fully resolved.
func waitForStatsZero(t *testing.T, s *Slack, timeout time.Duration) {
	t.Helper()
	waitForStats(t, s, timeout, 0, 0)
	s.mu.Lock()
	n := len(s.batchRecs)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("batchRecs has %d entries after all runs resolved, want 0 (leak bound)", n)
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
	// Durable-ownership metadata: every root carries
	// event_type gauntlet_run and a {target, ref} payload, so a reaction
	// arriving after this run has terminated can still be traced back to
	// its owner.
	if !root.metadataSet || root.eventType != gauntletRunEventType {
		t.Fatalf("root metadata = (set=%v, type=%q), want set with type %q", root.metadataSet, root.eventType, gauntletRunEventType)
	}
	if got := root.payload["target"]; got != "main" {
		t.Errorf("root metadata payload[target] = %v, want %q", got, "main")
	}
	if got := root.payload["ref"]; got != cand.Ref {
		t.Errorf("root metadata payload[ref] = %v, want %q", got, cand.Ref)
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
	// The terminal edit re-attaches the same {target, ref} metadata,
	// confirming the single-run shape as authoritative now that the run
	// finished solo (postTerminal's doc comment).
	if !update.metadataSet || update.eventType != gauntletRunEventType || update.payload["ref"] != cand.Ref {
		t.Errorf("terminal edit metadata = (set=%v, type=%q, payload=%v), want type %q with ref %q", update.metadataSet, update.eventType, update.payload, gauntletRunEventType, cand.Ref)
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
	// emitAndDrain (not a wall-clock sleep) before asserting absence.
	emitAndDrain(t, s, ctx, core.Event{Kind: core.EventHookFinished, Target: "main", Candidate: cand, RunID: "run-hook-pass", CheckName: "deploy", Check: check})

	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no Slack post for a passed hook, got %+v", got)
	}
}

// TestSlack_HookFinishedNilCheckPostsNothing is a regression test,
// mirroring TestLogChannel_EmitHookFinishedNilCheckOmitsBlock
// (internal/channel/log_test.go): postHookFinished (slack.go) branches on
// ev.Check first, before looking at its Status/Err — a nil Check (an
// EventHookFinished with no result, however that might arise) must not
// panic on that nil dereference, and must post nothing, exactly like a
// passed hook does. Every other Slack hook test (e.g.
// TestSlack_HookPassedPostsNothing, TestSlack_HookFailedPostsStandaloneMessageWithTail)
// passes a non-nil Check, so this branch was previously unexercised.
func TestSlack_HookFinishedNilCheckPostsNothing(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/ivy/deploy", Target: "main", User: "ivy", Topic: "deploy", SHA: "0bad0bad"}

	emitAndDrain(t, s, ctx, core.Event{Kind: core.EventHookFinished, Target: "main", Candidate: cand, RunID: "run-hook-nil-check", CheckName: "deploy", Check: nil})

	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no Slack post for a nil-Check hook-finished event, got %+v", got)
	}
}

// TestSlack_HookStartedPostsNothing confirms EventHookStarted produces no
// Slack post: live hook-in-progress state is the
// dashboard/API's job; standalone start+finish messages per hook would be
// ~2N posts per landing, pure noise — mirrors the existing ignored-kind tests
// (e.g. TestSlack_RetryRequestedPostsNothing) rather than asserting a post.
func TestSlack_HookStartedPostsNothing(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/jill/deploy", Target: "main", User: "jill", Topic: "deploy", SHA: "cafefeed"}

	emitAndDrain(t, s, ctx, core.Event{Kind: core.EventHookStarted, Target: "main", Candidate: cand, RunID: "run-hook-started", CheckName: "deploy"})

	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no Slack post for EventHookStarted, got %+v", got)
	}
}

// TestSlack_HookSkippedPostsStandaloneMessage confirms EventHookSkipped
// (a recovery-skipped landing's hooks never ran at all) posts a
// standalone message including Detail, distinct from a normal hook result.
func TestSlack_HookSkippedPostsStandaloneMessage(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)

	mustEmit(t, s, ctx, core.Event{Kind: core.EventHookSkipped, Target: "main", RunID: "run-hook-skipped", Detail: "recovered landing; hooks not run"})

	posts := fake.waitForPosts(1, testTimeout)
	post := posts[0]
	if post.method != "chat.postMessage" || post.threadTS != "" {
		t.Fatalf("hook-skipped post = %+v, want a standalone (non-threaded) chat.postMessage", post)
	}
	for _, want := range []string{"⚠", "skipped", "main", "recovered landing; hooks not run"} {
		if !strings.Contains(post.text, want) {
			t.Errorf("hook-skipped text = %q, want it to contain %q", post.text, want)
		}
	}
}

// TestSlack_RetryRequestedPostsNothing confirms EventRetryRequested (a
// history-only durability signal: other channels default-ignore it; only
// history acts on it) produces no Slack post —
// Slack falls through handleOutbound's switch exactly like any other kind it
// doesn't render.
func TestSlack_RetryRequestedPostsNothing(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/kim/topic", Target: "main", SHA: "abc123"}

	emitAndDrain(t, s, ctx, core.Event{Kind: core.EventRetryRequested, Target: "main", Candidate: cand})

	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no Slack post for EventRetryRequested, got %+v", got)
	}
}

// TestSlack_UnknownEventKindPostsNothing is the universal contract test
// for Slack: handleOutbound's switch falls through to no-op for any Kind it
// doesn't recognize (mirroring core.Channel's "ignore unknown kinds"
// contract, internal/channel/log.go), so core.EventKind(999) — a future
// kind this build has never heard of — must produce no post and no panic,
// exactly like EventQueued/EventCheckStarted-without-thread today.
func TestSlack_UnknownEventKindPostsNothing(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	emitAndDrain(t, s, ctx, core.Event{Kind: core.EventKind(999), Target: "main"})

	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no Slack post for an unrecognized EventKind, got %+v", got)
	}
}

func TestSlack_TerminalWithoutKnownRootIsANoOp(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)

	rec := &core.RunRecord{RunID: "unknown-run", Target: "main", Outcome: core.OutcomeLanded}
	// signalProcessed fires once per handled event regardless of whether it
	// did anything, so emitAndDrain synchronizes even for this no-op path.
	emitAndDrain(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "unknown-run", Record: rec})

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

// TestSlack_ReactionFromUnauthorizedUserIgnored: with allowed-users
// configured, a command reaction from anyone off the list is dropped before
// ANY ownership resolution — no Command, no ack reaction, no metadata fetch
// side effects. The channel-visible silence is deliberate (don't invite
// probing); only the daemon log records it.
func TestSlack_ReactionFromUnauthorizedUserIgnored(t *testing.T) {
	s, fake, ctx := newTestSlackWith(t, nil, func(p *Params) {
		p.AllowedUsers = []string{"U_BOSS"}
	})
	cand := core.Candidate{Ref: "refs/heads/for/main/carol/thing", Target: "main", User: "carol", Topic: "thing", SHA: "beadface"}

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: "run-authz-deny"})
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U_MALLORY", "recycle", root.ts)

	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("expected no Command from an unauthorized user, got %v (ok=%v)", cmd, ok)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing arrived
	}
	if got := fake.snapshotReactions(); len(got) != 0 {
		t.Fatalf("expected no ack reaction for an unauthorized user, got %+v", got)
	}
}

// TestSlack_ReactionFromAllowedUserMintsCommand is the positive half: the
// same configuration, but the reactor IS on the list — full normal flow,
// command + 👀 ack.
func TestSlack_ReactionFromAllowedUserMintsCommand(t *testing.T) {
	s, fake, ctx := newTestSlackWith(t, nil, func(p *Params) {
		p.AllowedUsers = []string{"U_BOSS", "U_OTHER"}
	})
	cand := core.Candidate{Ref: "refs/heads/for/main/carol/thing", Target: "main", User: "carol", Topic: "thing", SHA: "beadface"}

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: "run-authz-allow"})
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U_BOSS", "recycle", root.ts)

	select {
	case cmd := <-s.Commands():
		if cmd.Kind != core.CommandRetry || cmd.Target != "main" || cmd.Ref != cand.Ref {
			t.Fatalf("Command = %+v, want retry for target=main ref=%s", cmd, cand.Ref)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a retry Command from an allowed user")
	}
	if got := fake.waitForReactions(1, testTimeout); got[0].name != ackEyes {
		t.Fatalf("ack reaction = %+v, want %s", got[0], ackEyes)
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

// TestSlack_BatchLandingPostsOneRootOneEditOneSummary covers the
// batch-aware join for a green batch: 3 EventTrialClean events (chain
// building, one per member, all sharing batchID) must post exactly ONE root
// message — not one per member (postRoot's idempotency guard) — then 3
// EventLanded terminal events (one per member, each with its own distinct
// RunID but the shared BatchID) must produce exactly one root edit (at the
// FIRST member processed) and exactly ONE threaded summary reply (posted
// once the LAST member, Position == BatchSize-1, arrives) that names every
// member — not one noisy reply per member — with both run-tracking maps
// back to zero afterward.
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
	// A genuine multi-member batch's final metadata omits ref:
	// a reaction can't name which member it means, so handleForeignReaction
	// must be able to tell this apart from a single-run root by payload
	// shape alone.
	if !edit.metadataSet || edit.eventType != gauntletRunEventType {
		t.Fatalf("batch edit metadata = (set=%v, type=%q), want set with type %q", edit.metadataSet, edit.eventType, gauntletRunEventType)
	}
	if got := edit.payload["target"]; got != "main" {
		t.Errorf("batch edit metadata payload[target] = %v, want %q", got, "main")
	}
	if _, hasRef := edit.payload["ref"]; hasRef {
		t.Errorf("batch edit metadata payload = %v, must not carry ref for a multi-member batch", edit.payload)
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

// TestSlack_BatchSummaryToleratesMiddleMemberHole proves a batch member's
// terminal event can be silently lost to Emit's outbox-full drop ("if the
// outbox is full, the event is logged and dropped") — nothing ever
// re-delivers it — and summarizeBatch must never assume every Position
// slot is filled: nil-dereferencing on recs[0] the instant any slot is
// empty would crash the drainer goroutine. Skipping bob's (Position 1, the
// middle member's) Emit entirely must not panic; the LAST member's arrival
// (Position == BatchSize-1) still triggers the flush, the summary
// explicitly notes the gap instead of silently omitting bob, and every map
// still ends up clean.
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

// TestSlack_BatchStaleEntryFlushedOnAnotherBatchArrival proves a batch
// whose flush-triggering arrival is ITSELF the event Emit dropped (here,
// carol's Position == BatchSize-1 terminal event, which a Position-only
// flush trigger would depend on exclusively) would buffer — and leak its
// runRoot/roots/batchRecs entries — forever without the staleness sweep.
// The sweep runs opportunistically, on
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

// TestSlack_SingleMemberBatchRendersLikeSerial proves that a batch formed
// with exactly one member — max-batch 1, or a queue that only ever offered
// one candidate — degrades to serial behavior byte for byte in Slack too:
// its root headline edit and threaded summary must render IDENTICALLY to a
// genuine serial run's own messages, never "batch <runID> (1 members) →
// target", which would have broken grammar and drop the topic/user
// entirely. This drives the exact same candidate/checks/outcome through
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
	// The "degrades to serial byte for byte" invariant extends to metadata
	// shape too (finishBatch's doc comment): a batch of exactly one member
	// gets the single-run payload (ref included), not the batch-only shape
	// — there's no ambiguity about which member a reaction on it means.
	if batchEdit.payload["ref"] != cand.Ref {
		t.Errorf("batch-of-one edit metadata payload[ref] = %v, want %q (must not omit ref like a genuine multi-member batch)", batchEdit.payload["ref"], cand.Ref)
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

// TestSlack_ReactionAfterTerminationMintsCommandViaMetadataFetchAndAcks is
// the load-bearing proof that reaction-retry works end-to-end even though
// roots/runRoot are (correctly) forgotten the instant a run terminates
// (the leak-bound cleanup), while a human reacting to a terminal ❌ root
// does so AFTER that point — never mid-run, the only case the in-memory
// map's fast path can resolve. This drives a run all the way to a
// forgotten, rejected terminal
// state, THEN reacts on its (now-untracked) root, and checks the command is
// still minted — via the conversations.history metadata fetch, not the
// roots map — and the reaction is acknowledged with a 👀 (eyes) reaction.
func TestSlack_ReactionAfterTerminationMintsCommandViaMetadataFetchAndAcks(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/ivan/thing", Target: "main", User: "ivan", Topic: "thing", SHA: "0ddba11"}
	runID := "run-post-terminal-reaction"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	root := fake.waitForPosts(1, testTimeout)[0]

	rec := &core.RunRecord{RunID: runID, Target: "main", Outcome: core.OutcomeRejected, Detail: "red"}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventRejected, Target: "main", Candidate: cand, RunID: runID, Record: rec})
	fake.waitForPosts(3, testTimeout) // root, terminal edit, summary

	// The run is now fully terminal: both run-tracking maps have forgotten
	// it — exactly the shape a real, unattended ❌ root has by the
	// time a human gets around to reacting to it.
	waitForStatsZero(t, s, testTimeout)

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", root.ts)

	select {
	case cmd := <-s.Commands():
		if cmd.Kind != core.CommandRetry || cmd.Target != "main" || cmd.Ref != cand.Ref {
			t.Fatalf("Command = %+v, want retry for target=main ref=%s", cmd, cand.Ref)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a retry Command minted via the metadata-fetch fallback")
	}

	acks := fake.waitForReactions(1, testTimeout)
	if acks[0].name != ackEyes || acks[0].ts != root.ts || acks[0].channel != "C_TARGET" {
		t.Fatalf("reactions.add = %+v, want a single %q ack on channel=C_TARGET ts=%q", acks[0], ackEyes, root.ts)
	}
}

// TestSlack_RetryContinuityThreadsUnderOldRootAndReEditsIt proves retry continuity:
// once a reaction-retry has been minted from root T for (target, ref), the
// NEXT trial-clean for that same (target, ref) — the re-queued run the retry
// itself produces — must thread under T rather than starting a fresh root:
// a threaded "retesting" notice under T, T's own text re-edited to show the
// retry in flight, and every subsequent event for the new run (checks, the
// terminal edit) landing on T too, exactly as if T had been that run's own
// root all along.
func TestSlack_RetryContinuityThreadsUnderOldRootAndReEditsIt(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/judy/thing", Target: "main", User: "judy", Topic: "thing", SHA: "5ca1ab1e"}
	runID := "run-retry-continuity-1"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	root := fake.waitForPosts(1, testTimeout)[0]

	rec := &core.RunRecord{RunID: runID, Target: "main", Outcome: core.OutcomeRejected, Detail: "red"}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventRejected, Target: "main", Candidate: cand, RunID: runID, Record: rec})
	fake.waitForPosts(3, testTimeout)
	waitForStatsZero(t, s, testTimeout)

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", root.ts)
	// Receiving the Command is itself the fence: mintCommand records
	// refRetry BEFORE the cmds-channel send (its doc comment — the write
	// must be ordered before whatever the daemon does in response, exactly
	// the ordering this receive relies on), so the entry is guaranteed
	// visible from here on.
	select {
	case cmd := <-s.Commands():
		if cmd.Kind != core.CommandRetry {
			t.Fatalf("Command = %+v, want retry", cmd)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the retry Command")
	}

	// The re-queue's own trial-clean: same (target, ref), a fresh RunID
	// (the queue mints a new run id per attempt).
	retryRunID := "run-retry-continuity-2"
	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: retryRunID})

	posts := fake.waitForPosts(4, testTimeout) // root, edit, summary, + the retry-continuity threaded notice
	notice := posts[3]
	if notice.method != "chat.postMessage" || notice.threadTS != root.ts {
		t.Fatalf("retry-continuity notice = %+v, want a threaded reply on %q", notice, root.ts)
	}
	if !strings.Contains(notice.text, "retry") {
		t.Errorf("retry-continuity notice text = %q, want it to mention retry", notice.text)
	}

	posts = fake.waitForPosts(5, testTimeout)
	reEdit := posts[4]
	if reEdit.method != "chat.update" || reEdit.ts != root.ts {
		t.Fatalf("retry-continuity re-edit = %+v, want a chat.update on %q", reEdit, root.ts)
	}
	for _, want := range []string{"retry", "thing", "judy", "main"} {
		if !strings.Contains(reEdit.text, want) {
			t.Errorf("retry-continuity re-edit text = %q, want it to contain %q", reEdit.text, want)
		}
	}

	// No FRESH root was posted for the retried run: its tracking entry
	// points at the very same ts as the original root.
	waitForStats(t, s, testTimeout, 1, 1)
	s.mu.Lock()
	gotRoot := s.runRoot[retryRunID]
	s.mu.Unlock()
	if gotRoot != root.ts {
		t.Fatalf("runRoot[%q] = %q, want the original root %q", retryRunID, gotRoot, root.ts)
	}

	mustEmit(t, s, ctx, core.Event{Kind: core.EventCheckStarted, Target: "main", Candidate: cand, RunID: retryRunID, CheckName: "test"})
	posts = fake.waitForPosts(6, testTimeout)
	checkReply := posts[5]
	if checkReply.threadTS != root.ts {
		t.Fatalf("check-started reply for the retried run = %+v, want threaded on the original root %q", checkReply, root.ts)
	}

	rec2 := &core.RunRecord{RunID: retryRunID, Target: "main", Outcome: core.OutcomeLanded}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cand, RunID: retryRunID, Record: rec2})
	posts = fake.waitForPosts(8, testTimeout)
	finalEdit := posts[6]
	if finalEdit.method != "chat.update" || finalEdit.ts != root.ts {
		t.Fatalf("final edit for the retried run = %+v, want a chat.update on the original root %q", finalEdit, root.ts)
	}
	if !strings.Contains(finalEdit.text, "✅") {
		t.Errorf("final edit text = %q, want a ✅ verdict", finalEdit.text)
	}

	waitForStatsZero(t, s, testTimeout)
}

// TestSlack_BatchRootReactionAfterTerminationGetsQuestionAckAndGuidance
// proves the deliberate blunt-instrument refusal: a reaction on a
// finished, multi-member batch's root can't name which member it means, so
// rather than retrying/cancelling ALL members (rejected as "too blunt" by
// the design) it mints NO command at all — just a ❓ (question) ack and a
// threaded reply pointing at the API/CLI for member-level commands.
func TestSlack_BatchRootReactionAfterTerminationGetsQuestionAckAndGuidance(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	batchID, cands, runIDs := batchMembers()

	for _, cand := range cands {
		mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: batchID})
	}
	root := fake.waitForPosts(1, testTimeout)[0]
	waitForStats(t, s, testTimeout, 1, 1)

	checks := []core.CheckResult{{Name: "test", Status: core.CheckPassed, Duration: 42 * time.Millisecond}}
	for i, cand := range cands {
		rec := &core.RunRecord{
			RunID: runIDs[i], Target: "main", Candidate: cand,
			Outcome: core.OutcomeLanded, Checks: checks,
			BatchID: batchID, Position: i, BatchSize: len(cands),
		}
		mustEmit(t, s, ctx, core.Event{Kind: core.EventLanded, Target: "main", Candidate: cand, RunID: runIDs[i], Record: rec})
	}
	fake.waitForPosts(3, testTimeout) // root, batch edit, batch summary
	waitForStatsZero(t, s, testTimeout)

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", root.ts)

	// No command: a batch root reaction never mints one.
	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("expected no Command for a batch-root reaction, got %v (ok=%v)", cmd, ok)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing arrived
	}

	acks := fake.waitForReactions(1, testTimeout)
	if acks[0].name != ackQuestion || acks[0].ts != root.ts {
		t.Fatalf("reactions.add = %+v, want a single %q ack on %q", acks[0], ackQuestion, root.ts)
	}

	posts := fake.waitForPosts(4, testTimeout) // + the guidance reply
	guidance := posts[3]
	if guidance.method != "chat.postMessage" || guidance.threadTS != root.ts {
		t.Fatalf("batch-guidance reply = %+v, want a threaded reply on %q", guidance, root.ts)
	}
	if !strings.Contains(guidance.text, "batch") {
		t.Errorf("batch-guidance reply text = %q, want it to explain this is a batch root", guidance.text)
	}
}

// TestSlack_ForeignMessageReactionAfterTerminationIgnored proves
// handleForeignReaction's authorship check is load-bearing: a reaction on a
// message this bot never posted (a human's message, or another app's) —
// even one that happens to carry a gauntlet_run-shaped metadata payload,
// simulating a spoofing attempt — must never be trusted into minting a
// Command or acknowledged with a reaction.
func TestSlack_ForeignMessageReactionAfterTerminationIgnored(t *testing.T) {
	s, fake, _ := newTestSlack(t, nil)

	ts := "1700000123.000001"
	fake.injectForeignMessage(ts, "U_SOMEONE_ELSE", gauntletRunEventType, map[string]any{"target": "main", "ref": "refs/heads/for/main/mallory/spoof"})

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", ts)

	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("expected no Command for a foreign message's reaction, got %v (ok=%v)", cmd, ok)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing arrived
	}

	if got := fake.snapshotReactions(); len(got) != 0 {
		t.Fatalf("expected no reactions.add for a foreign message, got %+v", got)
	}
	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no posts for a foreign message's reaction, got %+v", got)
	}
}

// TestSlack_ForeignBotMessageReactionAfterTerminationIgnored is the other
// half of the spoofing surface: another APP's message — bot_message shape,
// bot_id set but not ours, no user field — stamped with gauntlet-lookalike
// metadata. isOwnMessage accepts a bot_id match (real bot posts may omit
// user), so this proves that acceptance compares the id, not merely "is a
// bot message with the right event_type".
func TestSlack_ForeignBotMessageReactionAfterTerminationIgnored(t *testing.T) {
	s, fake, _ := newTestSlack(t, nil)

	ts := "1700000123.000002"
	fake.injectForeignBotMessage(ts, "B_EVIL_APP", gauntletRunEventType, map[string]any{"target": "main", "ref": "refs/heads/for/main/mallory/spoof"})

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", ts)

	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("expected no Command for a foreign bot message's reaction, got %v (ok=%v)", cmd, ok)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing arrived
	}

	if got := fake.snapshotReactions(); len(got) != 0 {
		t.Fatalf("expected no reactions.add for a foreign bot message, got %+v", got)
	}
	if got := fake.snapshotPosts(); len(got) != 0 {
		t.Fatalf("expected no posts for a foreign bot message's reaction, got %+v", got)
	}
}

// TestSlack_ReactionResolvesOwnershipViaUserFieldToo pins the OTHER accepted
// authorship shape: a message whose history form carries user == the bot's
// own user id and no bot_id. Live Slack varies in which field it populates
// for bot posts (workspace/app-config dependent), so isOwnMessage accepts
// either; the fake's default is the bot_id shape, and this test keeps the
// user-match path from silently rotting.
func TestSlack_ReactionResolvesOwnershipViaUserFieldToo(t *testing.T) {
	s, fake, _ := newTestSlack(t, nil)

	ts := "1700000123.000003"
	ref := "refs/heads/for/main/kim/userfield"
	fake.injectForeignMessage(ts, fakeBotUserID, gauntletRunEventType, map[string]any{"target": "main", "ref": ref})

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", ts)

	select {
	case cmd := <-s.Commands():
		if cmd.Kind != core.CommandRetry || cmd.Target != "main" || cmd.Ref != ref {
			t.Fatalf("Command = %+v, want retry main %s", cmd, ref)
		}
	case <-time.After(testTimeout):
		t.Fatal("expected a retry Command via the user-field ownership match, got none")
	}
}

// TestSlack_ReactionOnOwnNonRootMessageAfterTerminationIgnored covers the
// most likely real operator misfire: reacting `:recycle:` on the bot's OWN
// threaded final summary (or any other bot message that isn't a root) after
// the run has finished. Such a message passes the authorship check — the bot
// really did post it — but carries no gauntlet_run metadata, so
// handleForeignReaction's event_type check must reject it: no Command, no
// ack, no extra posts.
func TestSlack_ReactionOnOwnNonRootMessageAfterTerminationIgnored(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/kim/thing", Target: "main", User: "kim", Topic: "thing", SHA: "deadd00d"}
	runID := "run-own-nonroot-reaction"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	fake.waitForPosts(1, testTimeout)

	rec := &core.RunRecord{RunID: runID, Target: "main", Outcome: core.OutcomeRejected, Detail: "red"}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventRejected, Target: "main", Candidate: cand, RunID: runID, Record: rec})
	posts := fake.waitForPosts(3, testTimeout) // root, terminal edit, threaded summary
	waitForStatsZero(t, s, testTimeout)

	summary := posts[2]
	if summary.threadTS == "" || summary.metadataSet {
		t.Fatalf("test setup: posts[2] = %+v, want the metadata-less threaded summary", summary)
	}

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", summary.ts)

	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("expected no Command for a reaction on the bot's own non-root message, got %v (ok=%v)", cmd, ok)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing arrived
	}

	if got := fake.snapshotReactions(); len(got) != 0 {
		t.Fatalf("expected no reactions.add for the bot's own non-root message, got %+v", got)
	}
	if got := fake.snapshotPosts(); len(got) != 3 {
		t.Fatalf("expected no posts beyond the original 3, got %d: %+v", len(got), got)
	}
}
