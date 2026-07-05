// Batch-summary parallelization suite (S6, phase-6 audit synthesis): proves
// precomputeMergeBodies actually bounds concurrency and wall clock (direct
// unit tests, a fake summarizer with no queue.Daemon involved at all), and
// that startBatchRun's wiring of it lands every member's own precomputed
// body in its own merge commit message (an integration proof on the fake
// harness — batch_test.go's tier — with a real-time-sleeping fake
// summarizer, since the timing property this fixes is real wall clock, not
// the harness's injected logical clock).
package queue

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// fakeSummarizer is a scriptable Config.MergeBody stand-in that sleeps for a
// fixed duration per call (simulating a real Messages API round trip) and
// records the maximum number of calls ever in flight simultaneously — the
// property precomputeMergeBodies exists to bound.
type fakeSummarizer struct {
	sleep time.Duration

	mu      sync.Mutex
	inFlite int32 // current in-flight count (atomic)
	maxSeen int32 // high-water mark (atomic)

	calls int32 // total calls made (atomic)
}

func (f *fakeSummarizer) mergeBody(ctx context.Context, cand core.Candidate, base string) string {
	n := atomic.AddInt32(&f.inFlite, 1)
	for {
		max := atomic.LoadInt32(&f.maxSeen)
		if n <= max || atomic.CompareAndSwapInt32(&f.maxSeen, max, n) {
			break
		}
	}
	atomic.AddInt32(&f.calls, 1)
	time.Sleep(f.sleep)
	atomic.AddInt32(&f.inFlite, -1)
	return "summary of " + cand.Ref
}

// TestPrecomputeMergeBodies_BoundsConcurrencyAndWallClock is the direct
// timing proof: N requests, each sleeping `sleep`, must complete in roughly
// one `sleep` (bounded concurrency, not N*sleep serial), and the observed
// max-in-flight must never exceed maxConcurrentMergeBodies.
func TestPrecomputeMergeBodies_BoundsConcurrencyAndWallClock(t *testing.T) {
	const n = 10
	sleep := 40 * time.Millisecond
	fake := &fakeSummarizer{sleep: sleep}

	reqs := make([]mergeBodyRequest, n)
	for i := range reqs {
		reqs[i] = mergeBodyRequest{
			cand: core.Candidate{SHA: candSHA(i), Ref: candRef(i)},
			base: "base-tip",
		}
	}

	start := time.Now()
	got := precomputeMergeBodies(context.Background(), fake.mergeBody, reqs)
	elapsed := time.Since(start)

	if int(fake.calls) != n {
		t.Fatalf("mergeBody called %d times, want %d", fake.calls, n)
	}
	if got := atomic.LoadInt32(&fake.maxSeen); got > maxConcurrentMergeBodies {
		t.Fatalf("max concurrency observed = %d, want <= %d", got, maxConcurrentMergeBodies)
	}
	if atomic.LoadInt32(&fake.maxSeen) < 2 {
		t.Fatalf("max concurrency observed = %d, want > 1 (proves calls actually overlapped)", fake.maxSeen)
	}
	// N*sleep serial would be 10*40ms = 400ms; bounded concurrency of 4
	// needs ceil(10/4)=3 waves, ~120ms, plus scheduling slack. Assert well
	// under half the fully-serial time so a regression to serial execution
	// fails loudly without the test being flaky about exact wave counts.
	if elapsed >= time.Duration(n)*sleep/2 {
		t.Fatalf("elapsed = %v, want well under serial N*sleep=%v (proves calls ran concurrently)", elapsed, time.Duration(n)*sleep)
	}

	if len(got) != n {
		t.Fatalf("result map has %d entries, want %d", len(got), n)
	}
	for i, req := range reqs {
		want := "summary of " + req.cand.Ref
		if got[req.cand.SHA] != want {
			t.Errorf("result[%d] (sha=%s) = %q, want %q", i, req.cand.SHA, got[req.cand.SHA], want)
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

func candSHA(i int) string { return "sha-" + string(rune('a'+i)) }
func candRef(i int) string { return candidateRef("main", "user", string(rune('a'+i))) }

// TestBatchRun_PrecomputedBodiesLandInOwnMergeMessages is the end-to-end
// wiring proof, on the fake-git batch harness (batch_test.go's tier): a
// 4-member batch, each summarized by a real-time-sleeping fake, must (a)
// complete startBatchRun's chain-building in roughly one sleep's worth of
// wall clock, not four, and (b) land each member's own distinct precomputed
// body in its own merge commit message — proving buildChainLinkPrecomputed
// actually consumes precomputeMergeBodies' result keyed correctly per
// candidate, not just per position.
func TestBatchRun_PrecomputedBodiesLandInOwnMergeMessages(t *testing.T) {
	sleep := 30 * time.Millisecond
	fake := &fakeSummarizer{sleep: sleep}

	h := newMergeBodyBatchHarness(t, fake.mergeBody, 8)
	h.git.seed("main", checkSpecFile("test"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	refC := candidateRef("main", "carol", "c")
	refD := candidateRef("main", "dave", "d")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})
	h.git.pushCandidate(refC, "", map[string]string{"c.txt": "c\n"})
	h.git.pushCandidate(refD, "", map[string]string{"d.txt": "d\n"})

	start := time.Now()
	h.reconcile() // one refill: all four chain into one batch run
	elapsed := time.Since(start)

	if int(fake.calls) != 4 {
		t.Fatalf("mergeBody called %d times, want 4 (once per chained member)", fake.calls)
	}
	// 4*sleep serial would be 120ms; bounded concurrency finishes in ~1
	// wave (cap 4 >= 4 members). Generous bound to avoid flakiness while
	// still catching a regression to the old serial-in-the-loop behavior.
	if elapsed >= 3*sleep {
		t.Fatalf("startBatchRun took %v, want well under the serial bound %v (mergeBody must run concurrently)", elapsed, 4*sleep)
	}

	r := h.d.headRun("main")
	if r == nil || len(r.members) != 4 {
		t.Fatalf("headRun members = %+v, want 4 chained members", r)
	}
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // green: lands all four

	// Walk the landed chain tip back through its 4 merge links (first-parent),
	// asserting each carries the summary for its own candidate ref, not a
	// neighbor's (would fail if precompute mapped by position instead of by
	// candidate SHA once any concurrent completion order shuffled results).
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
	for _, line := range splitLines(haystack) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
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
