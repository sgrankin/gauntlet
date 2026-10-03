package queue

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

func TestPrecomputeMergeBodiesBoundsConcurrencyAndSeparatesRefs(t *testing.T) {
	const n = 10
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, n)
	release := make(chan struct{}, n)
	var active, peak atomic.Int32
	reqs := make([]mergeBodyRequest, n)
	for i := range reqs {
		reqs[i] = mergeBodyRequest{cand: core.Candidate{SHA: "same-sha", Ref: candRef(i)}, base: "base"}
	}
	done := make(chan map[string]string, 1)
	go func() {
		done <- precomputeMergeBodies(ctx, func(ctx context.Context, c core.Candidate, base string) string {
			current := active.Add(1)
			for previous := peak.Load(); current > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, current) {
					break
				}
			}
			defer active.Add(-1)
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "summary of " + c.Ref
		}, reqs)
	}()
	// Each wave must fill all available slots before any call is released.
	// A serial implementation cannot fill a wave; an unbounded one exceeds peak.
	for remaining := n; remaining > 0; {
		wave := min(remaining, maxConcurrentMergeBodies)
		for range wave {
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("summary workers did not fill the wave")
			}
		}
		for range wave {
			release <- struct{}{}
		}
		remaining -= wave
	}
	var got map[string]string
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("summaries did not complete")
	}
	if peak.Load() != maxConcurrentMergeBodies {
		t.Fatalf("peak concurrency=%d", peak.Load())
	}
	if len(got) != n {
		t.Fatalf("distinct refs collapsed: %v", got)
	}
	for _, req := range reqs {
		if got[req.cand.Ref] != "summary of "+req.cand.Ref {
			t.Fatalf("wrong summary: %v", got)
		}
	}
}

// TestPrecomputeMergeBodies_NilMergeBodyReturnsNilMap covers the disabled
// case: no goroutines, no map, exactly as buildChainLinkPrecomputed's own
// nil-map fallback expects.
func TestPrecomputeMergeBodies_NilMergeBodyReturnsNilMap(t *testing.T) {
	got := precomputeMergeBodies(context.Background(), nil, []mergeBodyRequest{{cand: core.Candidate{SHA: "x"}}})
	if got != nil {
		t.Fatalf("got %v, want nil map when mergeBody is nil", got)
	}
}

// TestPrecomputeMergeBodies_EmptyRequestsReturnsNilMap covers the other
// degenerate input: nothing to summarize, nothing computed.
func TestPrecomputeMergeBodies_EmptyRequestsReturnsNilMap(t *testing.T) {
	got := precomputeMergeBodies(context.Background(), func(context.Context, core.Candidate, string) string { return "x" }, nil)
	if got != nil {
		t.Fatalf("got %v, want nil map for zero requests", got)
	}
}

func candRef(i int) string { return candidateRef("main", "user", string(rune('a'+i))) }

func TestBatchRun_PrecomputedBodiesLandInOwnMergeMessages(t *testing.T) {
	h := newMergeBodyBatchHarness(t, func(_ context.Context, c core.Candidate, _ string) string { return "summary of " + c.Ref }, 8)
	h.git.seed("main", checkSpecFile("test"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	refD := candidateRef("main", "dave", "d")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})
	h.git.pushCandidate(refD, "", map[string]string{"d.txt": "d\n"})

	h.reconcile()

	r := h.d.headRun("main")
	if r == nil || len(r.members) != 4 {
		t.Fatalf("headRun members = %+v, want 4 chained members", r)
	}
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // green: lands all four

	tip := h.git.ref("refs/heads/main")
	wantRefsTipFirst := []string{refD, refC, refB, refA}
	oid := tip
	for i, wantRef := range wantRefsTipFirst {
		msg := h.git.commitMessage(oid)
		wantBody := "summary of " + wantRef
		if !containsLine(msg, wantBody) {
			t.Fatalf("chain link %d (tip-first) message = %q, want it to contain body %q", i, msg, wantBody)
		}
		oid = h.git.commits[oid].parents[0]
	}
}

func containsLine(haystack, want string) bool {
	return slices.Contains(strings.Split(haystack, "\n"), want)
}

// newMergeBodyBatchHarness is newMergeBodyHarness (mergebody_test.go), but
// for batch mode with a caller-supplied MaxBatch — mergebody_test.go's own
// helper fixes the target to plain serial mode, which this file's batch
// wiring proof needs to vary.
func newMergeBodyBatchHarness(t *testing.T, mergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string, maxBatch int) *testHarness {
	t.Helper()
	git := newFakeGitRepo()
	exec := executor.NewGatedExecutor()
	ch := channel.NewRecordingChannel()
	h := &testHarness{t: t, git: git, exec: exec, ch: ch, clock: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)}

	d, err := New(git, exec, []core.Channel{ch}, Config{
		Targets:   []config.Target{batchTarget(maxBatch)},
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
