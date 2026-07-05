package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestLocalExecutor_LogFile_FullContentTailCapped is F-a's core capture
// contract (DESIGN.md "Full per-check log files"): when job.LogPath is set,
// the executor tees the check's combined output to that file in full, while
// CheckResult.Output stays tail-capped at outputCap exactly as before. Both
// views must come from the same underlying write stream — the log file is
// simply the uncapped twin of the tail buffer.
func TestLocalExecutor_LogFile_FullContentTailCapped(t *testing.T) {
	dir := t.TempDir()
	const lines = 8000 // ~8000*11 bytes ~= 88KB > 64KiB
	cmd := script(t, dir, "check.sh", fmt.Sprintf(
		"#!/bin/sh\ni=0\nwhile [ $i -lt %d ]; do printf 'LINE-%%05d\\n' $i; i=$((i+1)); done\nexit 0\n",
		lines,
	))
	job := baseJob(t, cmd)
	job.LogPath = filepath.Join(t.TempDir(), "runs", "run1", "check.log")

	res := LocalExecutor{}.RunCheck(t.Context(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed", res.Status)
	}
	if res.LogPath != job.LogPath {
		t.Fatalf("LogPath = %q, want %q", res.LogPath, job.LogPath)
	}

	// Output stays tail-capped, same as TestLocalExecutor_OutputCap.
	if len(res.Output) > outputCap {
		t.Fatalf("Output len = %d, want <= %d", len(res.Output), outputCap)
	}
	firstLine := "LINE-00000"
	if strings.Contains(res.Output, firstLine) {
		t.Errorf("Output should NOT contain the head line %q (should have been discarded)", firstLine)
	}

	// The file has the complete record: every line, including the head
	// that the tail buffer discarded.
	data, err := os.ReadFile(job.LogPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), firstLine) {
		t.Errorf("log file missing head line %q; the file must hold the FULL output, not just the tail", firstLine)
	}
	lastLine := fmt.Sprintf("LINE-%05d", lines-1)
	if !strings.Contains(string(data), lastLine) {
		t.Errorf("log file missing tail line %q", lastLine)
	}
}

// TestLocalExecutor_LogFile_CreatesParentDirs asserts the executor creates
// LogPath's parent directories, since the queue's LogDir/<runID>/ layout
// depends on that (runID's directory doesn't otherwise exist yet).
func TestLocalExecutor_LogFile_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\necho hello\nexit 0\n")
	job := baseJob(t, cmd)
	job.LogPath = filepath.Join(t.TempDir(), "does", "not", "exist", "yet", "check.log")

	res := LocalExecutor{}.RunCheck(t.Context(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.LogPath != job.LogPath {
		t.Fatalf("LogPath = %q, want %q", res.LogPath, job.LogPath)
	}
	data, err := os.ReadFile(job.LogPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("log file content = %q, want to contain 'hello'", data)
	}
}

// TestLocalExecutor_LogFile_EmptyPathMeansNoFile asserts the pre-F-a
// behavior is preserved verbatim when job.LogPath == "": CheckResult.LogPath
// stays empty, and no file capture is attempted at all.
func TestLocalExecutor_LogFile_EmptyPathMeansNoFile(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\nexit 0\n")
	job := baseJob(t, cmd)
	// job.LogPath left at its zero value ("").

	res := LocalExecutor{}.RunCheck(t.Context(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.LogPath != "" {
		t.Fatalf("LogPath = %q, want empty when job.LogPath is empty", res.LogPath)
	}
}

// TestLocalExecutor_LogFile_OpenFailureFallsBack is F-a's explicit
// robustness contract: a LogPath whose directory can't be created/written to
// must never fail the check. The check still runs and passes/fails on its
// own merits (tail-only capture), and CheckResult.LogPath is left empty
// (the log-less fallback signal) rather than the executor returning Err.
func TestLocalExecutor_LogFile_OpenFailureFallsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based unwritable-dir simulation is POSIX-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}

	unwritable := t.TempDir()
	if err := os.Chmod(unwritable, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o755) }) // let t.TempDir() clean up

	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\necho still-runs\nexit 0\n")
	job := baseJob(t, cmd)
	job.LogPath = filepath.Join(unwritable, "subdir", "check.log")

	res := LocalExecutor{}.RunCheck(t.Context(), job)

	if res.Err != nil {
		t.Fatalf("a log-file-open failure must not become CheckResult.Err; got %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed (the check itself must still run and pass)", res.Status)
	}
	if res.LogPath != "" {
		t.Fatalf("LogPath = %q, want empty (log-less fallback) when the file can't be created", res.LogPath)
	}
	if !strings.Contains(res.Output, "still-runs") {
		t.Errorf("Output = %q, want to contain 'still-runs' (tail capture must still work)", res.Output)
	}
	if _, err := os.Stat(job.LogPath); err == nil {
		t.Errorf("log file unexpectedly exists at %q despite the unwritable parent", job.LogPath)
	}
}

// TestOpenCheckLog_EmptyPath asserts the shared helper's "nothing requested"
// shape: no file, no error.
func TestOpenCheckLog_EmptyPath(t *testing.T) {
	f, err := openCheckLog("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f != nil {
		t.Fatalf("file = %v, want nil for an empty path", f)
	}
}
