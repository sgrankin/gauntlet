package slack

import (
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestSlack_CheckFinishedWithCheckIncludesVerdictAndDuration is F-a's
// (DESIGN.md "Full per-check log files") channel-side contract: once
// core.Event.Check is populated on EventCheckFinished, the threaded reply
// must show the check's verdict and duration immediately, not just its
// name.
func TestSlack_CheckFinishedWithCheckIncludesVerdictAndDuration(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/alice/widgets", Target: "main", User: "alice", Topic: "widgets", SHA: "deadbeef"}
	runID := "run-check-verdict"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	fake.waitForPosts(1, testTimeout)

	check := &core.CheckResult{Name: "lint", Status: core.CheckFailed, Duration: 1500 * time.Millisecond}
	mustEmit(t, s, ctx, core.Event{Kind: core.EventCheckFinished, Target: "main", Candidate: cand, RunID: runID, CheckName: "lint", Check: check})
	posts := fake.waitForPosts(2, testTimeout)

	reply := posts[1]
	if !strings.Contains(reply.text, "lint") {
		t.Errorf("reply text = %q, want it to mention the check name", reply.text)
	}
	if !strings.Contains(reply.text, "1.5s") {
		t.Errorf("reply text = %q, want it to mention the duration (1.5s)", reply.text)
	}
	if !strings.Contains(reply.text, checkEmoji(core.CheckFailed)) {
		t.Errorf("reply text = %q, want it to carry the failed-check verdict glyph %q", reply.text, checkEmoji(core.CheckFailed))
	}
}

// TestSlack_CheckFinishedWithoutCheckFallsBackToNameOnly asserts the
// pre-F-a rendering survives verbatim when Event.Check is nil (an older
// event, or a channel-carrying producer that hasn't been updated yet).
func TestSlack_CheckFinishedWithoutCheckFallsBackToNameOnly(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	cand := core.Candidate{Ref: "refs/heads/for/main/alice/widgets", Target: "main", User: "alice", Topic: "widgets", SHA: "deadbeef"}
	runID := "run-check-nil"

	mustEmit(t, s, ctx, core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: cand, RunID: runID})
	fake.waitForPosts(1, testTimeout)

	mustEmit(t, s, ctx, core.Event{Kind: core.EventCheckFinished, Target: "main", Candidate: cand, RunID: runID, CheckName: "lint"})
	posts := fake.waitForPosts(2, testTimeout)

	reply := posts[1]
	if reply.text != "◾ lint finished" {
		t.Fatalf("reply text = %q, want the name-only fallback %q", reply.text, "◾ lint finished")
	}
}
