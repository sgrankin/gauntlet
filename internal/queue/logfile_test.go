// Tests for F-a (DESIGN.md "Full per-check log files"): the queue's half of
// the contract — Config.LogDir assigning CheckJob.LogPath, path
// sanitization/containment, and Event.Check being populated on
// EventCheckFinished. The executor's half (actually teeing output to the
// file) is covered in internal/executor's own tests; here we only need to
// prove the queue computes and threads the right path and event field.
package queue

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

// capturingExecutor is a queue-test-local core.Executor (a real, minimal
// implementation, not a mock) that records the CheckJob it was last asked
// to run per check name, actually writes a small marker file at
// job.LogPath when one is assigned (mirroring what LocalExecutor/
// ContainerExecutor really do), and returns a fixed-shape CheckResult that
// threads job.LogPath through — exactly what the executor contract
// promises (CheckResult.LogPath set iff a log file was written), without
// needing a real subprocess or container runtime to prove the queue
// assigned the path correctly and never cleans the file up itself.
type capturingExecutor struct {
	mu   sync.Mutex
	jobs map[string]core.CheckJob
}

func newCapturingExecutor() *capturingExecutor {
	return &capturingExecutor{jobs: make(map[string]core.CheckJob)}
}

func (c *capturingExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	c.mu.Lock()
	c.jobs[job.Name] = job
	c.mu.Unlock()

	logPath := ""
	if job.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(job.LogPath), 0o755); err == nil {
			if err := os.WriteFile(job.LogPath, []byte("captured output for "+job.Name+"\n"), 0o644); err == nil {
				logPath = job.LogPath
			}
		}
	}
	return core.CheckResult{
		Name:     job.Name,
		Status:   core.CheckPassed,
		LogPath:  logPath,
		Duration: time.Millisecond,
	}
}

func (c *capturingExecutor) job(name string) (core.CheckJob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j, ok := c.jobs[name]
	return j, ok
}

// newLogDirHarness builds a Daemon wired to a fakeGitRepo, a
// capturingExecutor, and a RecordingChannel, with cfg.LogDir set — the
// minimum needed to exercise CheckJob.LogPath assignment end to end.
func newLogDirHarness(t *testing.T, logDir string) (*fakeGitRepo, *capturingExecutor, *channel.RecordingChannel, *Daemon) {
	t.Helper()
	git := newFakeGitRepo()
	exec := newCapturingExecutor()
	ch := channel.NewRecordingChannel()
	d, err := New(git, exec, []core.Channel{ch}, Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
		LogDir:    logDir,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return git, exec, ch, d
}

// reconcileUntilRecord spins ReconcileOnce (yielding the scheduler between
// attempts) until at least one new terminal RunRecord has landed — needed
// because capturingExecutor's goroutine (started by startCheck) resolves
// asynchronously; nothing rendezvous with the tick that reads its result.
func reconcileUntilRecord(t *testing.T, d *Daemon, ch *channel.RecordingChannel, before int) *core.RunRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := d.ReconcileOnce(context.Background()); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}
		if recs := ch.Records(); len(recs) > before {
			return recs[len(recs)-1]
		}
		if time.Now().After(deadline) {
			t.Fatal("no new run record after repeated ReconcileOnce calls")
		}
		runtime.Gosched()
	}
}

// TestReconcile_LogDir_AssignsJobLogPath is F-a's core queue-side contract:
// when Config.LogDir is set, CheckJob.LogPath must be
// <LogDir>/<runID>/<seq>-<check>.log.zst — seq is the check's 1-based position
// in the spec (closing-review FIX 3, so colliding sanitized names don't
// alias onto the same file) — and the resulting RunRecord's CheckResult
// (threaded back from the executor) must carry the same path.
func TestReconcile_LogDir_AssignsJobLogPath(t *testing.T) {
	logDir := t.TempDir()
	git, exec, ch, d := newLogDirHarness(t, logDir)
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("test"))

	before := len(ch.Records())
	rec := reconcileUntilRecord(t, d, ch, before)

	job, ok := exec.job("test")
	if !ok {
		t.Fatal("check \"test\" never ran")
	}
	want := filepath.Join(logDir, rec.RunID, "1-test.log.zst")
	if job.LogPath != want {
		t.Fatalf("CheckJob.LogPath = %q, want %q", job.LogPath, want)
	}
	if len(rec.Checks) != 1 || rec.Checks[0].LogPath != want {
		t.Fatalf("RunRecord.Checks = %+v, want a single check with LogPath %q", rec.Checks, want)
	}
}

// TestReconcile_LogDir_Empty_LeavesJobLogPathEmpty asserts the pre-F-a
// behavior is preserved verbatim when Config.LogDir == "": no path is ever
// assigned, so the executor (whichever one is configured) never opens a
// log file at all.
func TestReconcile_LogDir_Empty_LeavesJobLogPathEmpty(t *testing.T) {
	git, exec, ch, d := newLogDirHarness(t, "") // LogDir empty
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("test"))

	before := len(ch.Records())
	reconcileUntilRecord(t, d, ch, before)

	job, ok := exec.job("test")
	if !ok {
		t.Fatal("check \"test\" never ran")
	}
	if job.LogPath != "" {
		t.Fatalf("CheckJob.LogPath = %q, want empty when Config.LogDir is empty", job.LogPath)
	}
}

// TestReconcile_LogDir_SanitizesCheckNamePathTraversal is F-a's path-safety
// requirement: a check name that looks like a path-traversal attempt
// ("../evil") must not let the resulting LogPath escape
// <LogDir>/<runID>/.
func TestReconcile_LogDir_SanitizesCheckNamePathTraversal(t *testing.T) {
	logDir := t.TempDir()
	git, exec, ch, d := newLogDirHarness(t, logDir)
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("../evil"))

	before := len(ch.Records())
	rec := reconcileUntilRecord(t, d, ch, before)

	job, ok := exec.job("../evil")
	if !ok {
		t.Fatal("check \"../evil\" never ran")
	}
	if job.LogPath == "" {
		t.Fatal("LogPath not assigned for a check with an unusual name")
	}

	wantDir := filepath.Join(logDir, rec.RunID)
	rel, err := filepath.Rel(wantDir, job.LogPath)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q): %v", wantDir, job.LogPath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("LogPath %q escaped %q (rel=%q)", job.LogPath, wantDir, rel)
	}
	if strings.ContainsRune(filepath.Base(job.LogPath), filepath.Separator) {
		t.Fatalf("sanitized log file name %q still contains a path separator", filepath.Base(job.LogPath))
	}
}

// TestReconcile_LogDir_CollidingSanitizedNamesGetDistinctFiles is
// closing-review FIX 3's regression test: two check names that sanitize to
// the same string ("lint go" and "lint/go" both -> "lint-go") must not
// alias onto the same O_TRUNC'd log file — each check's history row would
// otherwise point at whichever happened to write last. The 1-based seq
// prefix (this check's position in the spec) keeps them distinct.
func TestReconcile_LogDir_CollidingSanitizedNamesGetDistinctFiles(t *testing.T) {
	logDir := t.TempDir()
	git, exec, ch, d := newLogDirHarness(t, logDir)
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("lint go", "lint/go"))

	before := len(ch.Records())
	rec := reconcileUntilRecord(t, d, ch, before)

	first, ok := exec.job("lint go")
	if !ok {
		t.Fatal("check \"lint go\" never ran")
	}
	second, ok := exec.job("lint/go")
	if !ok {
		t.Fatal("check \"lint/go\" never ran")
	}

	if first.LogPath == second.LogPath {
		t.Fatalf("both colliding checks got the same LogPath %q, want distinct files", first.LogPath)
	}
	wantFirst := filepath.Join(logDir, rec.RunID, "1-lint-go.log.zst")
	wantSecond := filepath.Join(logDir, rec.RunID, "2-lint-go.log.zst")
	if first.LogPath != wantFirst {
		t.Errorf("first check LogPath = %q, want %q", first.LogPath, wantFirst)
	}
	if second.LogPath != wantSecond {
		t.Errorf("second check LogPath = %q, want %q", second.LogPath, wantSecond)
	}

	if len(rec.Checks) != 2 {
		t.Fatalf("RunRecord.Checks = %+v, want 2 checks", rec.Checks)
	}
	for _, cr := range rec.Checks {
		if _, err := os.Stat(cr.LogPath); err != nil {
			t.Errorf("log file %q for check %q must exist: %v", cr.LogPath, cr.Name, err)
		}
	}
}

// TestReconcile_EventCheckFinished_CarriesCheck is F-a's Event contract:
// EventCheckFinished must carry the just-finished CheckResult (verdict +
// duration), not just the check's name, so channels can render a per-check
// line mid-run.
func TestReconcile_EventCheckFinished_CarriesCheck(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: 42 * time.Millisecond})

	var found bool
	for _, e := range h.ch.Events() {
		if e.Kind != core.EventCheckFinished {
			continue
		}
		found = true
		if e.Check == nil {
			t.Fatal("Event.Check is nil on EventCheckFinished")
		}
		if e.Check.Name != "test" || e.Check.Status != core.CheckPassed || e.Check.Duration != 42*time.Millisecond {
			t.Fatalf("Event.Check = %+v, want {test CheckPassed 42ms}", e.Check)
		}
	}
	if !found {
		t.Fatal("no EventCheckFinished event captured")
	}
}

// TestReconcile_EventCheckStarted_HasNoCheck asserts Event.Check stays nil
// for EventCheckStarted, which fires before any result exists to carry —
// the nil-check contract channels must honor.
func TestReconcile_EventCheckStarted_HasNoCheck(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()

	var found bool
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventCheckStarted {
			found = true
			if e.Check != nil {
				t.Fatalf("Event.Check = %+v, want nil on EventCheckStarted", e.Check)
			}
		}
	}
	if !found {
		t.Fatal("no EventCheckStarted event captured")
	}
}

// TestReconcile_FinishRun_DoesNotDeleteLogFiles asserts finishRun's cleanup
// (reconcile.go) only removes the trial export dir (WorkDir), never a
// check's log file under LogDir: log files outlive the run by design
// (DESIGN.md "Full per-check log files"), so a landed run must leave its
// checks' log files exactly where capturingExecutor wrote them.
func TestReconcile_FinishRun_DoesNotDeleteLogFiles(t *testing.T) {
	logDir := t.TempDir()
	git, exec, ch, d := newLogDirHarness(t, logDir)
	git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	git.pushCandidate(ref, "", checkSpecFile("test"))

	before := len(ch.Records())
	rec := reconcileUntilRecord(t, d, ch, before)
	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed; Detail=%q", rec.Outcome, rec.Detail)
	}

	job, ok := exec.job("test")
	if !ok {
		t.Fatal("check \"test\" never ran")
	}
	if job.LogPath == "" {
		t.Fatal("LogPath not assigned")
	}
	if _, err := os.Stat(job.LogPath); err != nil {
		t.Fatalf("log file %q must still exist after the run finished (finishRun cleanup must not touch LogDir): %v", job.LogPath, err)
	}
}
