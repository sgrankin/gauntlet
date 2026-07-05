package hooks

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// fakeGitRepo is a minimal core.GitRepo test double: every method but
// ExportTree is unused by Runner and returns a zero value; ExportTree
// records the tree-ish it was asked to export and, unless exportErr is set,
// writes a marker file into dir so tests can assert the dir was actually
// populated and later cleaned up.
type fakeGitRepo struct {
	mu        sync.Mutex
	exported  []string // tree-ish args, in call order
	dirs      []string // dirs passed, in call order
	exportErr error
}

func (f *fakeGitRepo) Fetch(ctx context.Context) error { return nil }
func (f *fakeGitRepo) ListRefs(ctx context.Context) (map[string]string, error) {
	return nil, nil
}
func (f *fakeGitRepo) MergeTree(ctx context.Context, base, candidate string) (core.TrialMerge, error) {
	return core.TrialMerge{}, nil
}
func (f *fakeGitRepo) CommitTree(ctx context.Context, tree string, parents []string, message string, who core.Identity) (string, error) {
	return "", nil
}
func (f *fakeGitRepo) ReadFileFromTree(ctx context.Context, tree, path string) ([]byte, error) {
	return nil, nil
}
func (f *fakeGitRepo) IsAncestor(ctx context.Context, maybeAncestor, ref string) (bool, error) {
	return false, nil
}

func (f *fakeGitRepo) ExportTree(ctx context.Context, tree, dir string) error {
	f.mu.Lock()
	f.exported = append(f.exported, tree)
	f.dirs = append(f.dirs, dir)
	f.mu.Unlock()
	if f.exportErr != nil {
		return f.exportErr
	}
	return os.WriteFile(filepath.Join(dir, "marker"), []byte(tree), 0o644)
}

func (f *fakeGitRepo) CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error {
	return nil
}

func (f *fakeGitRepo) exportedDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dirs) == 0 {
		return ""
	}
	return f.dirs[len(f.dirs)-1]
}

var _ core.GitRepo = (*fakeGitRepo)(nil)

// fakeExecutor is a recording core.Executor test double: it captures every
// CheckJob it's asked to run, in order, and returns a per-job-name result
// from results (falling back to a passing result) so tests can script
// individual hooks to fail.
type fakeExecutor struct {
	mu      sync.Mutex
	jobs    []core.CheckJob
	results map[string]core.CheckResult // job.Name -> canned result
}

func (f *fakeExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	res, ok := f.results[job.Name]
	f.mu.Unlock()
	if !ok {
		res = core.CheckResult{Status: core.CheckPassed}
	}
	res.Name = job.Name
	return res
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

func (f *fakeExecutor) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.jobs))
	for i, j := range f.jobs {
		names[i] = j.Name
	}
	return names
}

var _ core.Executor = (*fakeExecutor)(nil)

// recordingEmit is a Params.Emit test double: it captures every event
// handed to it, safe for concurrent use.
type recordingEmit struct {
	mu     sync.Mutex
	events []core.Event
}

func (e *recordingEmit) fn(ctx context.Context, ev core.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *recordingEmit) snapshot() []core.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]core.Event, len(e.events))
	copy(out, e.events)
	return out
}

func landedEvent(target string, rec *core.RunRecord) core.Event {
	return core.Event{
		Kind:      core.EventLanded,
		At:        time.Now(),
		Target:    target,
		Candidate: rec.Candidate,
		RunID:     rec.RunID,
		Record:    rec,
	}
}

func TestRunner_RunsHooksInOrderWithLandedCoordinates(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	emit := &recordingEmit{}

	r := New(Params{
		Hooks: map[string][]Hook{
			"main": {
				{Name: "deploy", Command: []string{"echo", "deploy"}},
				{Name: "notify", Command: []string{"echo", "notify"}},
			},
		},
		Git:     git,
		Exec:    exec,
		Emit:    emit.fn,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	rec := &core.RunRecord{
		RunID:  "run-1",
		Target: "main",
		Candidate: core.Candidate{
			Ref: "refs/heads/for/main/alice/feat", Target: "main", User: "alice", Topic: "feat", SHA: "cand-sha",
		},
		BaseOID:  "base-sha",
		MergeSHA: "merge-sha",
	}

	r.runLanding(context.Background(), landedEvent("main", rec))

	if got := exec.callNames(); len(got) != 2 || got[0] != "hook:deploy" || got[1] != "hook:notify" {
		t.Fatalf("callNames = %v, want [hook:deploy hook:notify]", got)
	}

	for _, job := range exec.jobs {
		if job.RunID != "run-1" {
			t.Errorf("job %s: RunID = %q, want run-1", job.Name, job.RunID)
		}
		if job.Target != "main" {
			t.Errorf("job %s: Target = %q, want main", job.Name, job.Target)
		}
		if job.BaseSHA != "base-sha" {
			t.Errorf("job %s: BaseSHA = %q, want base-sha", job.Name, job.BaseSHA)
		}
		if job.MergeSHA != "merge-sha" {
			t.Errorf("job %s: MergeSHA = %q, want merge-sha", job.Name, job.MergeSHA)
		}
		if job.Candidate.SHA != "cand-sha" {
			t.Errorf("job %s: Candidate.SHA = %q, want cand-sha", job.Name, job.Candidate.SHA)
		}
		if job.Dir == "" {
			t.Errorf("job %s: Dir is empty", job.Name)
		}
	}
	if exec.jobs[0].Command[0] != "echo" || exec.jobs[0].Command[1] != "deploy" {
		t.Errorf("jobs[0].Command = %v", exec.jobs[0].Command)
	}

	if got := git.exported; len(got) != 1 || got[0] != "merge-sha" {
		t.Fatalf("ExportTree called with %v, want [merge-sha] once", got)
	}

	events := emit.snapshot()
	if len(events) != 2 {
		t.Fatalf("emitted %d events, want 2", len(events))
	}
	for i, want := range []string{"deploy", "notify"} {
		ev := events[i]
		if ev.Kind != core.EventHookFinished {
			t.Errorf("events[%d].Kind = %v, want EventHookFinished", i, ev.Kind)
		}
		if ev.CheckName != want {
			t.Errorf("events[%d].CheckName = %q, want %q", i, ev.CheckName, want)
		}
		if ev.Check == nil || ev.Check.Status != core.CheckPassed {
			t.Errorf("events[%d].Check = %+v, want a passing result", i, ev.Check)
		}
		if ev.RunID != "run-1" {
			t.Errorf("events[%d].RunID = %q, want run-1", i, ev.RunID)
		}
	}
}

func TestRunner_FailureStopsRemainingHooks(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{
		results: map[string]core.CheckResult{
			"hook:deploy": {Status: core.CheckFailed, Output: "deploy exploded"},
		},
	}
	emit := &recordingEmit{}

	r := New(Params{
		Hooks: map[string][]Hook{
			"main": {
				{Name: "deploy", Command: []string{"false"}},
				{Name: "notify", Command: []string{"echo", "notify"}},
			},
		},
		Git:     git,
		Exec:    exec,
		Emit:    emit.fn,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	rec := &core.RunRecord{RunID: "run-2", Target: "main", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("main", rec))

	if n := exec.callCount(); n != 1 {
		t.Fatalf("exec called %d times, want 1 (should stop after deploy fails)", n)
	}

	events := emit.snapshot()
	if len(events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(events))
	}
	if events[0].CheckName != "deploy" || events[0].Check.Status != core.CheckFailed {
		t.Fatalf("events[0] = %+v, want a failed deploy result", events[0])
	}
}

func TestRunner_NonLandedEventsIgnored(t *testing.T) {
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     &fakeGitRepo{},
		Exec:    &fakeExecutor{},
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	cases := []core.Event{
		{Kind: core.EventQueued, Target: "main"},
		{Kind: core.EventCheckFinished, Target: "main", Record: nil},
		{Kind: core.EventRejected, Target: "main", Record: &core.RunRecord{}}, // terminal, but not a landing
	}
	for _, ev := range cases {
		if err := r.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit(%v) returned error: %v", ev.Kind, err)
		}
	}
	if n := len(r.queue); n != 0 {
		t.Fatalf("queue length = %d, want 0 (non-landing events must be ignored)", n)
	}
}

func TestRunner_LandedWithoutRecordIgnored(t *testing.T) {
	r := New(Params{Git: &fakeGitRepo{}, Exec: &fakeExecutor{}, WorkDir: t.TempDir(), Log: io.Discard})
	if err := r.Emit(context.Background(), core.Event{Kind: core.EventLanded, Record: nil}); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if n := len(r.queue); n != 0 {
		t.Fatalf("queue length = %d, want 0", n)
	}
}

func TestRunner_DropWhenFull(t *testing.T) {
	var logBuf boundedLogBuf
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     &fakeGitRepo{},
		Exec:    &fakeExecutor{},
		WorkDir: t.TempDir(),
		Log:     &logBuf,
	})

	// Fill the queue to capacity without a drainer running.
	for i := 0; i < queueBuffer; i++ {
		ev := landedEvent("main", &core.RunRecord{RunID: "r", MergeSHA: "m"})
		if err := r.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit #%d: %v", i, err)
		}
	}
	if n := len(r.queue); n != queueBuffer {
		t.Fatalf("queue length = %d, want %d", n, queueBuffer)
	}

	// One more must be dropped, not block.
	overflow := landedEvent("main", &core.RunRecord{RunID: "overflow", MergeSHA: "m"})
	done := make(chan struct{})
	go func() {
		_ = r.Emit(context.Background(), overflow)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a full queue instead of dropping")
	}
	if n := len(r.queue); n != queueBuffer {
		t.Fatalf("queue length after overflow = %d, want unchanged %d", n, queueBuffer)
	}
	if !logBuf.contains("queue full") {
		t.Errorf("log = %q, want a message mentioning \"queue full\"", logBuf.String())
	}
}

func TestRunner_ExportDirCleanedUp(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     git,
		Exec:    exec,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	rec := &core.RunRecord{RunID: "run-3", Target: "main", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("main", rec))

	dir := git.exportedDir()
	if dir == "" {
		t.Fatal("ExportTree was never called")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("export dir %s still exists after runLanding, want cleaned up", dir)
	}
}

func TestRunner_RecoveredLandingWithoutMergeSHASkipped(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	var logBuf boundedLogBuf
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     git,
		Exec:    exec,
		WorkDir: t.TempDir(),
		Log:     &logBuf,
	})

	// Mirrors queue/reconcile.go's recoverLanded: a synthesized landing
	// with no MergeSHA (the merge already happened in an earlier pass).
	rec := &core.RunRecord{RunID: "run-4", Target: "main"}
	r.runLanding(context.Background(), landedEvent("main", rec))

	if exec.callCount() != 0 {
		t.Fatalf("exec called %d times, want 0 (no tree to export)", exec.callCount())
	}
	if len(git.exported) != 0 {
		t.Fatalf("ExportTree called %d times, want 0", len(git.exported))
	}
}

func TestRunner_ExportFailureSkipsHooks(t *testing.T) {
	git := &fakeGitRepo{exportErr: errors.New("boom")}
	exec := &fakeExecutor{}
	var logBuf boundedLogBuf
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     git,
		Exec:    exec,
		WorkDir: t.TempDir(),
		Log:     &logBuf,
	})

	rec := &core.RunRecord{RunID: "run-5", Target: "main", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("main", rec))

	if exec.callCount() != 0 {
		t.Fatalf("exec called %d times, want 0 (export failed)", exec.callCount())
	}
}

func TestRunner_NoHooksConfiguredForTargetIsNoop(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     git,
		Exec:    exec,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	rec := &core.RunRecord{RunID: "run-6", Target: "other", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("other", rec))

	if exec.callCount() != 0 || len(git.exported) != 0 {
		t.Fatalf("expected no export/exec for a target with no configured hooks")
	}
}

func TestRunner_CommandsNeverYields(t *testing.T) {
	r := New(Params{Git: &fakeGitRepo{}, Exec: &fakeExecutor{}, WorkDir: t.TempDir(), Log: io.Discard})
	select {
	case cmd, ok := <-r.Commands():
		t.Fatalf("expected no command, got %v (ok=%v)", cmd, ok)
	case <-time.After(20 * time.Millisecond):
		// expected: nothing arrived
	}
}

func TestRunner_RunDrainsQueueInOrder(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	emit := &recordingEmit{}
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     git,
		Exec:    exec,
		Emit:    emit.fn,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	for i := 0; i < 3; i++ {
		rec := &core.RunRecord{RunID: "run", Target: "main", MergeSHA: "merge-sha"}
		if err := r.Emit(ctx, landedEvent("main", rec)); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	deadline := time.After(2 * time.Second)
	for {
		if exec.callCount() >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("exec called %d times after timeout, want 3", exec.callCount())
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func TestRunner_CtxShutdownDrainsCleanly(t *testing.T) {
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     &fakeGitRepo{},
		Exec:    &fakeExecutor{},
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	rec := &core.RunRecord{RunID: "run-7", Target: "main", MergeSHA: "merge-sha"}
	if err := r.Emit(ctx, landedEvent("main", rec)); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// boundedLogBuf is a tiny concurrency-safe log sink for tests that need to
// assert on a dropped/failed message without racing Runner's internal
// goroutine against the test goroutine reading a bytes.Buffer directly.
type boundedLogBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *boundedLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *boundedLogBuf) contains(substr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(string(b.buf), substr)
}
