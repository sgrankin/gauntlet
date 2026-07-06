package hooks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
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

	r.runLanding(context.Background(), landedEvent("main", rec), nil)

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

	// Every hook now also fires an EventHookStarted immediately before its
	// EventHookFinished (S1-C/S5); filter down to just the Finished events
	// for this assertion's original per-hook-outcome shape.
	var finished []core.Event
	for _, ev := range emit.snapshot() {
		if ev.Kind == core.EventHookFinished {
			finished = append(finished, ev)
		}
	}
	events := finished
	if len(events) != 2 {
		t.Fatalf("emitted %d EventHookFinished events, want 2", len(events))
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

// TestRunner_EmitsHookStartedBeforeEachHook is S1-C/S5's producer-side
// contract: EventHookStarted must fire, in order, immediately before each
// hook's RunCheck — interleaved with the existing EventHookFinished per
// hook, not batched at the start or end. HookIndex/HookCount must be
// correct for a 2-hook landing.
func TestRunner_EmitsHookStartedBeforeEachHook(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	emit := &recordingEmit{}

	r := New(Params{
		Hooks: map[string][]Hook{
			"main": {
				{Name: "deploy", Command: []string{"true"}},
				{Name: "notify", Command: []string{"true"}},
			},
		},
		Git:     git,
		Exec:    exec,
		Emit:    emit.fn,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	rec := &core.RunRecord{RunID: "run-started", Target: "main", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

	events := emit.snapshot()
	wantKinds := []core.EventKind{
		core.EventHookStarted, core.EventHookFinished,
		core.EventHookStarted, core.EventHookFinished,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("emitted %d events, want %d: %+v", len(events), len(wantKinds), events)
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("events[%d].Kind = %v, want %v", i, events[i].Kind, want)
		}
	}

	started0, started1 := events[0], events[2]
	if started0.CheckName != "deploy" || started0.HookIndex != 0 || started0.HookCount != 2 {
		t.Errorf("events[0] (hook 0 started) = %+v, want CheckName=deploy HookIndex=0 HookCount=2", started0)
	}
	if started1.CheckName != "notify" || started1.HookIndex != 1 || started1.HookCount != 2 {
		t.Errorf("events[2] (hook 1 started) = %+v, want CheckName=notify HookIndex=1 HookCount=2", started1)
	}
	for i, ev := range []core.Event{started0, started1} {
		if ev.RunID != "run-started" {
			t.Errorf("started event %d RunID = %q, want run-started", i, ev.RunID)
		}
	}
}

// TestRunner_RecoveredLandingEmitsHookSkipped confirms the
// MergeSHA=="" recovery branch emits EventHookSkipped (S1-C's durable
// marker) carrying Detail and HookCount, in addition to (not instead of)
// the existing stderr log line, and never calls RunCheck.
func TestRunner_RecoveredLandingEmitsHookSkipped(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	emit := &recordingEmit{}
	var logBuf boundedLogBuf

	r := New(Params{
		Hooks: map[string][]Hook{"main": {
			{Name: "deploy", Command: []string{"true"}},
			{Name: "notify", Command: []string{"true"}},
		}},
		Git:     git,
		Exec:    exec,
		Emit:    emit.fn,
		WorkDir: t.TempDir(),
		Log:     &logBuf,
	})

	rec := &core.RunRecord{RunID: "run-skip", Target: "main"} // MergeSHA == ""
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

	if exec.callCount() != 0 {
		t.Fatalf("exec called %d times, want 0 (recovered landing must never run hooks)", exec.callCount())
	}
	if !logBuf.contains("recovered landing") {
		t.Errorf("log = %q, want the existing stderr line preserved", logBuf.String())
	}

	events := emit.snapshot()
	if len(events) != 1 {
		t.Fatalf("emitted %d events, want 1 (EventHookSkipped)", len(events))
	}
	ev := events[0]
	if ev.Kind != core.EventHookSkipped {
		t.Fatalf("events[0].Kind = %v, want EventHookSkipped", ev.Kind)
	}
	if ev.RunID != "run-skip" || ev.Target != "main" {
		t.Errorf("EventHookSkipped RunID/Target = %q/%q, want run-skip/main", ev.RunID, ev.Target)
	}
	if ev.Detail == "" {
		t.Error("EventHookSkipped.Detail is empty, want a human-readable reason")
	}
	if ev.HookCount != 2 {
		t.Errorf("EventHookSkipped.HookCount = %d, want 2 (target's configured hook count)", ev.HookCount)
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
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

	if n := exec.callCount(); n != 1 {
		t.Fatalf("exec called %d times, want 1 (should stop after deploy fails)", n)
	}

	// deploy fires both EventHookStarted (before RunCheck) and
	// EventHookFinished (S1-C/S5); notify never runs at all since deploy
	// failed, so it fires neither.
	events := emit.snapshot()
	if len(events) != 2 {
		t.Fatalf("emitted %d events, want 2 (deploy's Started+Finished)", len(events))
	}
	if events[0].Kind != core.EventHookStarted || events[0].CheckName != "deploy" {
		t.Fatalf("events[0] = %+v, want EventHookStarted for deploy", events[0])
	}
	if events[1].Kind != core.EventHookFinished || events[1].CheckName != "deploy" || events[1].Check.Status != core.CheckFailed {
		t.Fatalf("events[1] = %+v, want a failed deploy EventHookFinished", events[1])
	}
}

// TestRunner_NonLandedEventsIgnored also covers S14's universal contract —
// core.EventKind(999) (a future kind this version of Runner has never heard
// of) must be ignored exactly like any other non-landing event, not panic
// or otherwise misbehave (internal/channel/log.go's "channels ignore
// unknown kinds" contract, which Runner.Emit's own doc cites).
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
		{Kind: core.EventKind(999), Target: "main"},                           // unrecognized kind (S14)
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
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

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
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

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
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

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
	r.runLanding(context.Background(), landedEvent("other", rec), nil)

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

// --- full per-check log files (hooks/history parity) --------------------

// TestRunner_HookLogPathAssignedAndSanitized exercises hookLogPath's
// contract directly through runLanding: each hook's CheckJob.LogPath must
// land under <LogDir>/<runID>/hook-<1-based seq>-<sanitized name>.log.zst,
// with a free-form hook name (spaces, slashes) sanitized the same way
// check names are (core.SanitizeName).
func TestRunner_HookLogPathAssignedAndSanitized(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	logDir := filepath.Join(t.TempDir(), "logs") // need not exist; fakeExecutor never touches disk

	r := New(Params{
		Hooks: map[string][]Hook{
			"main": {
				{Name: "deploy/prod", Command: []string{"true"}},
				{Name: "notify team", Command: []string{"true"}},
			},
		},
		Git:     git,
		Exec:    exec,
		WorkDir: t.TempDir(),
		LogDir:  logDir,
		Log:     io.Discard,
	})

	rec := &core.RunRecord{RunID: "run-log-path", Target: "main", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

	want := []string{
		filepath.Join(logDir, "run-log-path", "hook-1-deploy-prod.log.zst"),
		filepath.Join(logDir, "run-log-path", "hook-2-notify-team.log.zst"),
	}
	if len(exec.jobs) != len(want) {
		t.Fatalf("exec.jobs = %d, want %d", len(exec.jobs), len(want))
	}
	for i, job := range exec.jobs {
		if job.LogPath != want[i] {
			t.Errorf("jobs[%d].LogPath = %q, want %q", i, job.LogPath, want[i])
		}
	}
}

// TestRunner_HookLogPathEmptyWhenLogDirUnset confirms the pre-parity
// default survives: a Runner built with no Params.LogDir (the zero value)
// assigns no LogPath at all, exactly as if full hook logging never
// existed.
func TestRunner_HookLogPathEmptyWhenLogDirUnset(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &fakeExecutor{}
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     git,
		Exec:    exec,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	rec := &core.RunRecord{RunID: "run-no-logdir", Target: "main", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

	if len(exec.jobs) != 1 {
		t.Fatalf("exec.jobs = %d, want 1", len(exec.jobs))
	}
	if exec.jobs[0].LogPath != "" {
		t.Errorf("LogPath = %q, want empty when LogDir is unset", exec.jobs[0].LogPath)
	}
}

// TestRunner_HookLogFileActuallyWritten is the one integration-style test
// through a real core.Executor (executor.LocalExecutor, no fake): proves
// the tee machinery that already exists for checks (internal/executor/
// logfile.go's openCheckLog, driven purely off CheckJob.LogPath) needs no
// executor-side change at all to also cover hooks — assigning LogPath in
// hooks.go is sufficient. Reads back the hook's real zstd-compressed log
// file from disk and confirms it holds the command's actual output.
func TestRunner_HookLogFileActuallyWritten(t *testing.T) {
	git := &fakeGitRepo{}
	logDir := t.TempDir()

	r := New(Params{
		Hooks: map[string][]Hook{
			"main": {{Name: "deploy echo", Command: []string{"echo", "hello from hook"}}},
		},
		Git:     git,
		Exec:    executor.LocalExecutor{},
		WorkDir: t.TempDir(),
		LogDir:  logDir,
		Log:     io.Discard,
	})

	rec := &core.RunRecord{RunID: "run-real-log", Target: "main", MergeSHA: "merge-sha"}
	r.runLanding(context.Background(), landedEvent("main", rec), nil)

	wantPath := filepath.Join(logDir, "run-real-log", "hook-1-deploy-echo.log.zst")
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read hook log %s: %v", wantPath, err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer dec.Close()
	content, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decompress hook log: %v", err)
	}
	if !strings.Contains(string(content), "hello from hook") {
		t.Errorf("hook log content = %q, want to contain %q", content, "hello from hook")
	}
}

// --- backlog policies (hooks v2) ---------------------------------------

// blockingExecutor is a fakeExecutor variant for backlog-policy tests: the
// job named blockName belonging to run blockRun blocks — after recording
// itself and closing started, so a test can synchronize on "the blocking
// hook is now in flight" — until either release is closed (simulating a
// hook that eventually finishes on its own, for coalesce tests) or ctx is
// cancelled (simulating PolicyCancel's mid-hook cancellation), mirroring
// LocalExecutor's ctx.Err()-takes-precedence rule
// (internal/executor/local.go) so a cancelled RunCheck returns promptly
// with Err set. Every other job passes immediately.
type blockingExecutor struct {
	mu        sync.Mutex
	jobs      []core.CheckJob
	blockName string
	blockRun  string
	started   chan struct{}
	release   chan struct{}
}

func (f *blockingExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.mu.Unlock()

	if job.Name == f.blockName && job.RunID == f.blockRun {
		close(f.started)
		select {
		case <-ctx.Done():
			return core.CheckResult{Name: job.Name, Err: ctx.Err()}
		case <-f.release:
		}
	}
	return core.CheckResult{Name: job.Name, Status: core.CheckPassed}
}

func (f *blockingExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

func (f *blockingExecutor) runIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.jobs))
	for i, j := range f.jobs {
		out[i] = j.RunID
	}
	return out
}

var _ core.Executor = (*blockingExecutor)(nil)

// waitFor polls cond until it's true or t fails after 2s — used instead of
// a fixed sleep to synchronize with Run's background goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met after timeout")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestApplyBacklogPolicy_PerTargetIndependence exercises applyBacklogPolicy
// directly (no Run goroutine, no timing): a batch mixing two targets, one
// PolicyQueue and one PolicyCoalesce, must apply each target's policy
// independently — the queue target's landings all survive, in order and
// uncollapsed, regardless of the other target's landings interleaved
// alongside them in the same batch; the coalesce target's landings
// collapse to only the newest of the three queued for it.
func TestApplyBacklogPolicy_PerTargetIndependence(t *testing.T) {
	var logBuf boundedLogBuf
	r := New(Params{
		Hooks: map[string][]Hook{
			"main":  {{Name: "deploy", Command: []string{"true"}}},
			"other": {{Name: "deploy", Command: []string{"true"}}},
		},
		Policies: map[string]Policy{
			"main":  PolicyQueue,
			"other": PolicyCoalesce,
		},
		Git:     &fakeGitRepo{},
		Exec:    &fakeExecutor{},
		WorkDir: t.TempDir(),
		Log:     &logBuf,
	})

	ev := func(target, run, sha string) core.Event {
		return landedEvent(target, &core.RunRecord{
			RunID: run, Target: target, MergeSHA: "merge-" + run,
			Candidate: core.Candidate{Target: target, Topic: "t", SHA: sha},
		})
	}

	batch := []core.Event{
		ev("main", "main-1", "sha-m1"),
		ev("other", "other-1", "sha-o1"),
		ev("main", "main-2", "sha-m2"),
		ev("other", "other-2", "sha-o2"),
		ev("other", "other-3", "sha-o3"),
	}

	got := r.applyBacklogPolicy(batch)

	var gotRuns []string
	for _, e := range got {
		gotRuns = append(gotRuns, e.RunID)
	}
	want := []string{"main-1", "main-2", "other-3"}
	if len(gotRuns) != len(want) {
		t.Fatalf("applyBacklogPolicy runIDs = %v, want %v", gotRuns, want)
	}
	for i := range want {
		if gotRuns[i] != want[i] {
			t.Fatalf("applyBacklogPolicy runIDs = %v, want %v", gotRuns, want)
		}
	}
	if !logBuf.contains("coalesced landing t@sha-o1") {
		t.Errorf("log = %q, want a coalesce line for other-1", logBuf.String())
	}
	if !logBuf.contains("coalesced landing t@sha-o2") {
		t.Errorf("log = %q, want a coalesce line for other-2", logBuf.String())
	}
	if logBuf.contains("coalesced landing t@sha-m1") || logBuf.contains("coalesced landing t@sha-m2") {
		t.Errorf("log = %q, want no coalesce line for the queue-policy target", logBuf.String())
	}
}

// TestRunner_CoalescePolicy_DropsBacklogKeepsNewest drives the whole
// Runner (Run goroutine + real queue) for PolicyCoalesce: three landings
// for the same target, with the first's single hook held open until the
// second and third are both already queued behind it (deterministic via
// blockingExecutor's started/release synchronization, not sleeps). The
// first must run to completion undisturbed (coalesce never touches what's
// already running); the second, still only queued when the third arrives,
// must be dropped with a log line; the third must run.
func TestRunner_CoalescePolicy_DropsBacklogKeepsNewest(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &blockingExecutor{
		blockName: "hook:deploy",
		blockRun:  "run-1",
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	emit := &recordingEmit{}
	var logBuf boundedLogBuf

	r := New(Params{
		Hooks:    map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Policies: map[string]Policy{"main": PolicyCoalesce},
		Git:      git,
		Exec:     exec,
		Emit:     emit.fn,
		WorkDir:  t.TempDir(),
		Log:      &logBuf,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	rec1 := &core.RunRecord{RunID: "run-1", Target: "main", MergeSHA: "merge-1",
		Candidate: core.Candidate{Target: "main", Topic: "t1", SHA: "sha1"}}
	if err := r.Emit(ctx, landedEvent("main", rec1)); err != nil {
		t.Fatalf("Emit run-1: %v", err)
	}

	<-exec.started // run-1's deploy hook is now blocked in flight

	rec2 := &core.RunRecord{RunID: "run-2", Target: "main", MergeSHA: "merge-2",
		Candidate: core.Candidate{Target: "main", Topic: "t2", SHA: "sha2"}}
	rec3 := &core.RunRecord{RunID: "run-3", Target: "main", MergeSHA: "merge-3",
		Candidate: core.Candidate{Target: "main", Topic: "t3", SHA: "sha3"}}
	if err := r.Emit(ctx, landedEvent("main", rec2)); err != nil {
		t.Fatalf("Emit run-2: %v", err)
	}
	if err := r.Emit(ctx, landedEvent("main", rec3)); err != nil {
		t.Fatalf("Emit run-3: %v", err)
	}

	close(exec.release) // let run-1 finish

	waitFor(t, func() bool { return exec.callCount() >= 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}

	gotRuns := exec.runIDs()
	want := []string{"run-1", "run-3"}
	if len(gotRuns) != len(want) || gotRuns[0] != want[0] || gotRuns[1] != want[1] {
		t.Fatalf("executed runIDs = %v, want %v (run-2 must be dropped, never executed)", gotRuns, want)
	}
	if !logBuf.contains("coalesced landing t2@sha2, superseded by t3@sha3") {
		t.Errorf("log = %q, want a coalesce line naming run-2 superseded by run-3", logBuf.String())
	}

	events := emit.snapshot()
	for _, ev := range events {
		if ev.RunID == "run-2" {
			t.Errorf("run-2 must never get an EventHookFinished (coalesced away before it ran), got %+v", ev)
		}
	}
}

// TestRunner_CancelPolicy_CancelsRunningLandingMidHook drives the whole
// Runner for PolicyCancel: run-1's first hook blocks; while it's in
// flight, run-2 for the same target arrives. That must cancel run-1's
// hook execution immediately (mid-hook, not after it finishes) — its
// remaining hook ("notify") must never run — and the cancelled hook's
// EventHookFinished must carry the Err the executor returns on
// cancellation plus a "superseded by" Detail naming run-2. run-2 must
// then run both of its own hooks normally.
func TestRunner_CancelPolicy_CancelsRunningLandingMidHook(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &blockingExecutor{
		blockName: "hook:deploy",
		blockRun:  "run-1",
		started:   make(chan struct{}),
		release:   make(chan struct{}), // never closed: run-1 must be cancelled, not released
	}
	emit := &recordingEmit{}
	var logBuf boundedLogBuf

	r := New(Params{
		Hooks: map[string][]Hook{"main": {
			{Name: "deploy", Command: []string{"true"}},
			{Name: "notify", Command: []string{"true"}},
		}},
		Policies: map[string]Policy{"main": PolicyCancel},
		Git:      git,
		Exec:     exec,
		Emit:     emit.fn,
		WorkDir:  t.TempDir(),
		Log:      &logBuf,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	rec1 := &core.RunRecord{RunID: "run-1", Target: "main", MergeSHA: "merge-1",
		Candidate: core.Candidate{Target: "main", Topic: "t1", SHA: "sha1"}}
	if err := r.Emit(ctx, landedEvent("main", rec1)); err != nil {
		t.Fatalf("Emit run-1: %v", err)
	}

	<-exec.started // run-1's deploy hook is now blocked in flight, ctx not yet cancelled

	rec2 := &core.RunRecord{RunID: "run-2", Target: "main", MergeSHA: "merge-2",
		Candidate: core.Candidate{Target: "main", Topic: "t2", SHA: "sha2"}}
	if err := r.Emit(ctx, landedEvent("main", rec2)); err != nil {
		t.Fatalf("Emit run-2: %v", err)
	}

	// run-2's own hooks (deploy+notify) both pass immediately, so once
	// they've run the whole batch (run-1 cancelled, run-2 completed) is
	// done draining.
	waitFor(t, func() bool { return exec.callCount() >= 3 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}

	// run-1's "notify" must never have run: cancellation stopped it after
	// "deploy", the same stop-at-first-failure path an ordinary failure
	// takes.
	for _, j := range exec.jobs {
		if j.RunID == "run-1" && j.Name == "hook:notify" {
			t.Fatal("run-1's notify hook ran; cancellation must skip remaining hooks")
		}
	}

	// EventHookStarted (S1-C) also carries RunID=run-1/CheckName=deploy, so
	// filter specifically for the Finished event rather than any match.
	events := emit.snapshot()
	var run1Deploy *core.Event
	for i, ev := range events {
		if ev.Kind == core.EventHookFinished && ev.RunID == "run-1" && ev.CheckName == "deploy" {
			run1Deploy = &events[i]
		}
	}
	if run1Deploy == nil {
		t.Fatal("no EventHookFinished for run-1's deploy hook")
	}
	if run1Deploy.Check == nil || run1Deploy.Check.Err == nil {
		t.Fatalf("run-1 deploy event = %+v, want a Check with Err set (the cancellation)", run1Deploy)
	}
	if !errors.Is(run1Deploy.Check.Err, context.Canceled) {
		t.Errorf("run-1 deploy Check.Err = %v, want context.Canceled", run1Deploy.Check.Err)
	}
	if run1Deploy.Detail != "superseded by t2@sha2" {
		t.Errorf("run-1 deploy Detail = %q, want %q", run1Deploy.Detail, "superseded by t2@sha2")
	}

	if !logBuf.contains("cancelling in-flight landing run=run-1, superseded by t2@sha2") {
		t.Errorf("log = %q, want a cancellation line naming run-1 superseded by run-2", logBuf.String())
	}

	// run-2 must have run both its own hooks normally afterward.
	var run2Names []string
	for _, j := range exec.jobs {
		if j.RunID == "run-2" {
			run2Names = append(run2Names, j.Name)
		}
	}
	want := []string{"hook:deploy", "hook:notify"}
	if len(run2Names) != len(want) || run2Names[0] != want[0] || run2Names[1] != want[1] {
		t.Fatalf("run-2 hook calls = %v, want %v", run2Names, want)
	}
}

// TestRunner_CancelCurrent_CancelsRunningLandingMidHook drives Runner for
// PolicyCancel and cancels run-1's in-flight hook via CancelCurrent
// (Feature 1's operator-triggered counterpart to PolicyCancel's own
// automatic supersede-cancel) instead of a superseding landing: the blocked
// "deploy" hook must be interrupted immediately (mid-hook), its remaining
// "notify" hook must never run, and its EventHookFinished must carry the
// Err the executor returns on cancellation plus a Detail naming the
// operator, not another landing.
func TestRunner_CancelCurrent_CancelsRunningLandingMidHook(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &blockingExecutor{
		blockName: "hook:deploy",
		blockRun:  "run-1",
		started:   make(chan struct{}),
		release:   make(chan struct{}), // never closed: must be cancelled, not released
	}
	emit := &recordingEmit{}
	var logBuf boundedLogBuf

	r := New(Params{
		Hooks: map[string][]Hook{"main": {
			{Name: "deploy", Command: []string{"true"}},
			{Name: "notify", Command: []string{"true"}},
		}},
		Policies: map[string]Policy{"main": PolicyCancel},
		Git:      git,
		Exec:     exec,
		Emit:     emit.fn,
		WorkDir:  t.TempDir(),
		Log:      &logBuf,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	rec1 := &core.RunRecord{RunID: "run-1", Target: "main", MergeSHA: "merge-1",
		Candidate: core.Candidate{Target: "main", Topic: "t1", SHA: "sha1"}}
	if err := r.Emit(ctx, landedEvent("main", rec1)); err != nil {
		t.Fatalf("Emit run-1: %v", err)
	}
	<-exec.started // run-1's deploy hook is now blocked in flight, ctx not yet cancelled

	if !r.CancelCurrent("main") {
		t.Fatal("CancelCurrent = false, want true (run-1's landing is running right now)")
	}

	// Wait for run-1's own EventHookFinished specifically — NOT a plain
	// callCount check (jobs already recorded 1 entry the moment "deploy"
	// blocked, before CancelCurrent ever ran, so that would return
	// immediately and prove nothing), NOT the outer ctx cancel (cancelling
	// the Run loop's own parent context would race the causality chain this
	// test is verifying: inbox send -> supersede send -> execLanding's own
	// per-landing cancel -> RunCheck unblocks -> runLanding reads supersede),
	// and NOT a bare RunID+CheckName match (S1-C's EventHookStarted now also
	// carries RunID=run-1/CheckName=deploy, and fires BEFORE RunCheck even
	// blocks — it would already be present the instant exec.started closes,
	// proving nothing about the cancellation). Waiting for the Finished
	// event specifically is the one signal that's synchronized with
	// runLanding having already computed Detail.
	waitFor(t, func() bool {
		for _, ev := range emit.snapshot() {
			if ev.Kind == core.EventHookFinished && ev.RunID == "run-1" && ev.CheckName == "deploy" {
				return true
			}
		}
		return false
	})

	for _, j := range exec.jobs {
		if j.RunID == "run-1" && j.Name == "hook:notify" {
			t.Fatal("run-1's notify hook ran; CancelCurrent must skip remaining hooks")
		}
	}

	events := emit.snapshot()
	var run1Deploy *core.Event
	for i, ev := range events {
		if ev.Kind == core.EventHookFinished && ev.RunID == "run-1" && ev.CheckName == "deploy" {
			run1Deploy = &events[i]
		}
	}
	if run1Deploy == nil {
		t.Fatal("no EventHookFinished for run-1's deploy hook")
	}
	if run1Deploy.Check == nil || run1Deploy.Check.Err == nil {
		t.Fatalf("run-1 deploy event = %+v, want a Check with Err set (the cancellation)", run1Deploy)
	}
	if !errors.Is(run1Deploy.Check.Err, context.Canceled) {
		t.Errorf("run-1 deploy Check.Err = %v, want context.Canceled", run1Deploy.Check.Err)
	}
	if run1Deploy.Detail != "superseded by operator-cancel@manual" {
		t.Errorf("run-1 deploy Detail = %q, want %q", run1Deploy.Detail, "superseded by operator-cancel@manual")
	}

	if !logBuf.contains("cancelling in-flight landing run=run-1, superseded by operator-cancel@manual") {
		t.Errorf("log = %q, want a cancellation line naming the operator-cancel", logBuf.String())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// TestRunner_CancelCurrent_NoRunningLandingIsFalse covers the common,
// expected-not-an-error case: nothing is running for target right now (no
// landing has arrived at all), so CancelCurrent has nothing to signal.
func TestRunner_CancelCurrent_NoRunningLandingIsFalse(t *testing.T) {
	r := New(Params{
		Hooks:    map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Policies: map[string]Policy{"main": PolicyCancel},
		Git:      &fakeGitRepo{},
		Exec:     &fakeExecutor{},
		WorkDir:  t.TempDir(),
	})
	if r.CancelCurrent("main") {
		t.Fatal("CancelCurrent = true, want false (no landing is running for this target)")
	}
	if r.CancelCurrent("unconfigured-target") {
		t.Fatal("CancelCurrent = true for an unconfigured target, want false")
	}
}

// TestRunner_CancelCurrent_NonCancelPolicyIsFalse covers CancelCurrent's
// documented scope: it only ever cancels a PolicyCancel landing's mid-hook
// execution, since that's the only Policy execLanding registers a monitor
// for. A PolicyQueue target's currently-running landing has no
// cancellation mechanism to wrap at all, so CancelCurrent reports false —
// not an error, just "there's nothing this can do here" — and the running
// landing's hooks are left to finish undisturbed.
func TestRunner_CancelCurrent_NonCancelPolicyIsFalse(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &blockingExecutor{
		blockName: "hook:deploy",
		blockRun:  "run-1",
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}

	r := New(Params{
		Hooks:    map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Policies: map[string]Policy{"main": PolicyQueue},
		Git:      git,
		Exec:     exec,
		WorkDir:  t.TempDir(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	rec1 := &core.RunRecord{RunID: "run-1", Target: "main", MergeSHA: "merge-1",
		Candidate: core.Candidate{Target: "main", Topic: "t1", SHA: "sha1"}}
	if err := r.Emit(ctx, landedEvent("main", rec1)); err != nil {
		t.Fatalf("Emit run-1: %v", err)
	}
	<-exec.started // run-1's deploy hook is blocked in flight

	if r.CancelCurrent("main") {
		t.Fatal("CancelCurrent = true, want false (PolicyQueue registers no monitor to cancel)")
	}

	close(exec.release) // let it finish normally so Run can drain cleanly
	waitFor(t, func() bool { return exec.callCount() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// --- live state (S5) ------------------------------------------------------

// TestRunner_Snapshot_NoLandingRunningIsFalse covers the common idle case:
// no landing has ever run for target, so Snapshot reports false with a
// zero-valued LiveState (BacklogDepth still populated, since the queue
// length is target-agnostic — meaningful regardless of whether target
// itself has anything running).
func TestRunner_Snapshot_NoLandingRunningIsFalse(t *testing.T) {
	r := New(Params{
		Hooks:   map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}},
		Git:     &fakeGitRepo{},
		Exec:    &fakeExecutor{},
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	live, ok := r.Snapshot("main")
	if ok {
		t.Fatalf("Snapshot(main) ok = true, want false (nothing has ever run)")
	}
	if live.Running {
		t.Errorf("Snapshot(main) = %+v, want Running=false", live)
	}
	if all := r.SnapshotAll(); all != nil {
		t.Errorf("SnapshotAll() = %+v, want nil (nothing running anywhere)", all)
	}
}

// TestRunner_Snapshot_ReflectsInFlightHookAndBacklog drives the whole
// Runner (Run goroutine + real queue) to prove Snapshot/SnapshotAll report
// live state accurately while a landing's hook is blocked mid-flight:
// Running, CurrentHook, HookIndex/HookCount, and a non-zero BacklogDepth
// once further landings queue up behind the blocked one — then confirms
// Snapshot clears back to false once everything concludes.
func TestRunner_Snapshot_ReflectsInFlightHookAndBacklog(t *testing.T) {
	git := &fakeGitRepo{}
	exec := &blockingExecutor{
		blockName: "hook:deploy",
		blockRun:  "run-1",
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}

	r := New(Params{
		Hooks: map[string][]Hook{"main": {
			{Name: "deploy", Command: []string{"true"}},
			{Name: "notify", Command: []string{"true"}},
		}},
		Git:     git,
		Exec:    exec,
		WorkDir: t.TempDir(),
		Log:     io.Discard,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	rec1 := &core.RunRecord{RunID: "run-1", Target: "main", MergeSHA: "merge-1",
		Candidate: core.Candidate{Target: "main", Topic: "t1", SHA: "sha1"}}
	if err := r.Emit(ctx, landedEvent("main", rec1)); err != nil {
		t.Fatalf("Emit run-1: %v", err)
	}
	<-exec.started // run-1's deploy hook is now blocked in flight

	live, ok := r.Snapshot("main")
	if !ok {
		t.Fatal("Snapshot(main) ok = false, want true (run-1's deploy hook is in flight)")
	}
	if !live.Running || live.CurrentHook != "deploy" || live.HookIndex != 0 || live.HookCount != 2 {
		t.Errorf("Snapshot(main) = %+v, want Running=true CurrentHook=deploy HookIndex=0 HookCount=2", live)
	}
	if live.StartedAt.IsZero() {
		t.Error("Snapshot(main).StartedAt is zero, want set")
	}

	if _, ok := r.Snapshot("other"); ok {
		t.Error("Snapshot(other) ok = true, want false (only main has anything running)")
	}

	all := r.SnapshotAll()
	if len(all) != 1 || all[0].Target != "main" || !all[0].Running {
		t.Errorf("SnapshotAll() = %+v, want exactly one running entry for main", all)
	}

	// Queue two more landings for the same target while run-1 is still
	// blocked: they land directly in r.queue (Run is busy inside
	// execLanding for run-1, not back at the top select reading/draining
	// it), so BacklogDepth must reflect them.
	rec2 := &core.RunRecord{RunID: "run-2", Target: "main", MergeSHA: "merge-2",
		Candidate: core.Candidate{Target: "main", Topic: "t2", SHA: "sha2"}}
	rec3 := &core.RunRecord{RunID: "run-3", Target: "main", MergeSHA: "merge-3",
		Candidate: core.Candidate{Target: "main", Topic: "t3", SHA: "sha3"}}
	if err := r.Emit(ctx, landedEvent("main", rec2)); err != nil {
		t.Fatalf("Emit run-2: %v", err)
	}
	if err := r.Emit(ctx, landedEvent("main", rec3)); err != nil {
		t.Fatalf("Emit run-3: %v", err)
	}

	live, ok = r.Snapshot("main")
	if !ok || live.BacklogDepth != 2 {
		t.Errorf("Snapshot(main).BacklogDepth = %d (ok=%v), want 2 (run-2 and run-3 queued behind run-1)", live.BacklogDepth, ok)
	}

	close(exec.release) // let run-1 finish; run-2/run-3 drain after
	// run-1, run-2, run-3 each run both hooks: 6 total RunCheck calls.
	waitFor(t, func() bool { return exec.callCount() >= 6 })

	live, ok = r.Snapshot("main")
	if ok || live.Running {
		t.Errorf("Snapshot(main) after everything drained = %+v (ok=%v), want Running=false", live, ok)
	}
	if all := r.SnapshotAll(); all != nil {
		t.Errorf("SnapshotAll() after everything drained = %+v, want nil", all)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}
