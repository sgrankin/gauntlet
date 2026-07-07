package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// newMergeBodyHarness is newHarness (daemon_test.go), but with a caller-
// supplied Config.MergeBody — the one field newHarness's fixed Config
// construction doesn't expose, since only this file needs to vary it.
func newMergeBodyHarness(t *testing.T, mergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string) *testHarness {
	t.Helper()
	git := newFakeGitRepo()
	exec := executor.NewGatedExecutor()
	ch := channel.NewRecordingChannel()
	h := &testHarness{t: t, git: git, exec: exec, ch: ch, clock: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)}

	d, err := New(git, exec, []core.Channel{ch}, Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
		MergeBody: mergeBody,
	}, h.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
	t.Cleanup(func() { assertAllTerminalEventsHaveRecords(t, ch.Events()) })
	return h
}

// TestMergeBody_NilProducesExactPriorMessageShape locks in Config.MergeBody
// == nil (the zero value every test and config already relies on) as
// producing no body paragraph, ever.
func TestMergeBody_NilProducesExactPriorMessageShape(t *testing.T) {
	h := newMergeBodyHarness(t, nil)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	landed := h.git.ref("refs/heads/main")
	msg := h.git.commitMessage(landed)
	want := "Merge widget (alice)\n\nGauntlet-Ref: " + ref + "\nGauntlet-Run: " + runID + "\n"
	if msg != want {
		t.Fatalf("commit message = %q, want %q", msg, want)
	}
}

// TestMergeBody_SetInsertsBodyBetweenSubjectAndTrailers is the core wiring
// contract: a configured MergeBody hook is called once per trial
// (before the merge commit is built), and its return lands in the actual
// landed commit's message, between the subject and the trailers.
func TestMergeBody_SetInsertsBodyBetweenSubjectAndTrailers(t *testing.T) {
	var gotCand core.Candidate
	var gotBase string
	calls := 0
	h := newMergeBodyHarness(t, func(ctx context.Context, cand core.Candidate, baseOID string) string {
		calls++
		gotCand, gotBase = cand, baseOID
		return "  Adds widget rendering support.  \n"
	})
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	candSHA := h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	if calls != 1 {
		t.Fatalf("MergeBody called %d times, want exactly 1", calls)
	}
	if gotCand.SHA != candSHA {
		t.Errorf("MergeBody candidate SHA = %q, want %q", gotCand.SHA, candSHA)
	}
	if gotBase != base {
		t.Errorf("MergeBody baseOID = %q, want %q (the target tip, before the merge commit)", gotBase, base)
	}

	landed := h.git.ref("refs/heads/main")
	msg := h.git.commitMessage(landed)
	want := "Merge widget (alice)\n\nAdds widget rendering support.\n\nGauntlet-Ref: " + ref + "\nGauntlet-Run: " + runID + "\n"
	if msg != want {
		t.Fatalf("commit message = %q, want %q", msg, want)
	}
	// Trailers still parse: last two non-empty lines, still "Key: value".
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	if lines[len(lines)-2] != "Gauntlet-Ref: "+ref || lines[len(lines)-1] != "Gauntlet-Run: "+runID {
		t.Errorf("trailers not intact at the end of the message: %q", msg)
	}
}

// TestMergeBody_EmptyReturnDegradesSilently is the best-effort contract:
// MergeBody returning "" (a summarizer that degraded) never fails the
// trial or surfaces an error — the land proceeds with a plain message,
// exactly as if MergeBody were nil.
func TestMergeBody_EmptyReturnDegradesSilently(t *testing.T) {
	h := newMergeBodyHarness(t, func(ctx context.Context, cand core.Candidate, baseOID string) string {
		return ""
	})
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	landed := h.git.ref("refs/heads/main")
	msg := h.git.commitMessage(landed)
	want := "Merge widget (alice)\n\nGauntlet-Ref: " + ref + "\nGauntlet-Run: " + runID + "\n"
	if msg != want {
		t.Fatalf("commit message = %q, want %q (empty MergeBody must degrade to no body)", msg, want)
	}

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed despite the empty MergeBody return", last.Outcome)
	}
}
