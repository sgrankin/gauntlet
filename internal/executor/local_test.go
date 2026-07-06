package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// script writes a shell script to dir/name and returns argv to run it via
// /bin/sh (job.Command is argv, no shell interpolation of a single string —
// /bin/sh here is simply the program, the script file its argument).
func script(t *testing.T, dir, name, body string) []string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return []string{"/bin/sh", path}
}

func baseJob(t *testing.T, command []string) core.CheckJob {
	t.Helper()
	return core.CheckJob{
		RunID:    "run1",
		Target:   "main",
		Name:     "check",
		Command:  command,
		Dir:      t.TempDir(),
		BaseSHA:  "base-sha",
		MergeSHA: "merge-sha",
		Candidate: core.Candidate{
			Ref:    "refs/heads/for/main/alice/topic",
			Target: "main",
			User:   "alice",
			Topic:  "topic",
			SHA:    "cand-sha",
		},
	}
}

func TestLocalExecutor_Passed(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\nexit 0\n")
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", res.Duration)
	}
}

func TestLocalExecutor_Failed(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\necho boom\nexit 1\n")
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckFailed {
		t.Fatalf("Status = %v, want CheckFailed", res.Status)
	}
	if !strings.Contains(res.Output, "boom") {
		t.Errorf("Output = %q, want to contain 'boom'", res.Output)
	}
}

func TestLocalExecutor_SkippedViaResultFile(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", fmt.Sprintf(
		"#!/bin/sh\nprintf 'skipped' > \"$%s\"\nexit 0\n", core.EnvResultFile))
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckSkipped {
		t.Fatalf("Status = %v, want CheckSkipped", res.Status)
	}
}

func TestLocalExecutor_FailedWinsOverResultFile(t *testing.T) {
	// A nonzero exit is a verdict regardless of the result file (§5A): the
	// file only splits the exit-0 case.
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", fmt.Sprintf(
		"#!/bin/sh\nprintf 'skipped' > \"$%s\"\nexit 1\n", core.EnvResultFile))
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckFailed {
		t.Fatalf("Status = %v, want CheckFailed (result file must not override nonzero exit)", res.Status)
	}
}

func TestLocalExecutor_CommandNotFound(t *testing.T) {
	job := baseJob(t, []string{filepath.Join(t.TempDir(), "does-not-exist")})

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("exec-start failure must be a verdict (CheckFailed), not Err; got Err=%v", res.Err)
	}
	if res.Status != core.CheckFailed {
		t.Fatalf("Status = %v, want CheckFailed", res.Status)
	}
	if res.Output == "" {
		t.Errorf("Output should explain the exec-start failure, got empty")
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", res.Duration)
	}
}

func TestLocalExecutor_CtxCancel(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\nsleep 300\n")
	job := baseJob(t, cmd)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan core.CheckResult, 1)
	go func() {
		done <- LocalExecutor{}.RunCheck(ctx, job)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if res.Err == nil {
			t.Fatalf("Err = nil, want ctx cancellation error; status=%v", res.Status)
		}
		if res.Duration <= 0 {
			t.Errorf("Duration = %v, want > 0", res.Duration)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunCheck did not return after ctx cancel")
	}
}

func TestLocalExecutor_EnvVars(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf(`#!/bin/sh
test "$%s" = "base-sha" || { echo "bad base sha: $%s"; exit 1; }
test "$%s" = "merge-sha" || { echo "bad merge sha: $%s"; exit 1; }
test "$%s" = "cand-sha" || { echo "bad candidate sha: $%s"; exit 1; }
test "$%s" = "refs/heads/for/main/alice/topic" || { echo "bad ref: $%s"; exit 1; }
test -n "$%s" || { echo "result file var unset"; exit 1; }
test "$%s" = "run1" || { echo "bad run id: $%s"; exit 1; }
exit 0
`,
		core.EnvBaseSHA, core.EnvBaseSHA,
		core.EnvMergeSHA, core.EnvMergeSHA,
		core.EnvCandidateSHA, core.EnvCandidateSHA,
		core.EnvRef, core.EnvRef,
		core.EnvResultFile,
		core.EnvRunID, core.EnvRunID,
	)
	cmd := script(t, dir, "check.sh", body)
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
}

func TestLocalExecutor_OutputCap(t *testing.T) {
	dir := t.TempDir()
	// Write well more than outputCap bytes of distinguishable output, then
	// assert only the tail survives.
	const lines = 8000 // ~8000*11 bytes ~= 88KB > 64KiB
	cmd := script(t, dir, "check.sh", fmt.Sprintf(
		"#!/bin/sh\ni=0\nwhile [ $i -lt %d ]; do printf 'LINE-%%05d\\n' $i; i=$((i+1)); done\nexit 0\n",
		lines,
	))
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed", res.Status)
	}
	if len(res.Output) > outputCap {
		t.Fatalf("Output len = %d, want <= %d", len(res.Output), outputCap)
	}
	lastLine := fmt.Sprintf("LINE-%05d", lines-1)
	if !strings.Contains(res.Output, lastLine) {
		t.Errorf("Output should contain the tail line %q; got %d bytes, head=%q", lastLine, len(res.Output), res.Output[:min(200, len(res.Output))])
	}
	firstLine := "LINE-00000"
	if strings.Contains(res.Output, firstLine) {
		t.Errorf("Output should NOT contain the head line %q (should have been discarded)", firstLine)
	}
}

func TestLocalExecutor_ProcessGroupKill(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cmd := script(t, dir, "check.sh", fmt.Sprintf(`#!/bin/sh
sleep 300 &
echo $! > %q
sleep 300
`, pidFile))
	job := baseJob(t, cmd)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan core.CheckResult, 1)
	go func() {
		done <- LocalExecutor{}.RunCheck(ctx, job)
	}()

	// Wait for the background child's pid to be written.
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &childPID)
			if childPID > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("background child never wrote its pid")
	}

	cancel()

	select {
	case res := <-done:
		if res.Err == nil {
			t.Fatalf("Err = nil, want ctx cancellation error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunCheck did not return after ctx cancel")
	}

	// The background child (grandchild of the process-group leader) must
	// be dead too, not just the direct child (§9.5).
	deadline = time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if err == syscall.ESRCH {
			return // dead, as expected
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child pid %d still alive after group kill", childPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLocalExecutor_BaseDirRootsScratchDir proves S16's fix (phase-6 audit
// synthesis): a non-empty BaseDir roots the check's ephemeral scratch dir
// under it, rather than the OS default temp dir — the property that lets
// cmd/gauntlet's startup sweep of -state/scratch actually catch it. The
// scratch dir is removed (defer os.RemoveAll) before RunCheck returns, so
// the check script itself captures its own $GAUNTLET_RESULT_FILE's
// directory into a file outside the scratch dir for the test to inspect
// afterward.
func TestLocalExecutor_BaseDirRootsScratchDir(t *testing.T) {
	base := t.TempDir()
	capture := filepath.Join(t.TempDir(), "captured-scratch-dir")
	cmd := script(t, filepath.Dir(capture), "check.sh", fmt.Sprintf(
		"#!/bin/sh\ndirname \"$GAUNTLET_RESULT_FILE\" > %q\nexit 0\n", capture,
	))
	job := baseJob(t, cmd)

	res := LocalExecutor{BaseDir: base}.RunCheck(context.Background(), job)
	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}

	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured scratch dir: %v", err)
	}
	scratchDir := strings.TrimSpace(string(got))
	if !strings.HasPrefix(scratchDir, base+string(os.PathSeparator)) {
		t.Fatalf("scratch dir = %q, want it rooted under BaseDir %q", scratchDir, base)
	}
}

// TestLocalExecutor_EmptyBaseDirPreservesOSDefaultTempDir locks in the
// empty-BaseDir fallback every pre-S16 test (and this file's every other
// test) already relies on: os.MkdirTemp("", ...) behavior, unchanged.
func TestLocalExecutor_EmptyBaseDirPreservesOSDefaultTempDir(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "captured-scratch-dir")
	cmd := script(t, dir, "check.sh", fmt.Sprintf(
		"#!/bin/sh\ndirname \"$GAUNTLET_RESULT_FILE\" > %q\nexit 0\n", capture,
	))
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)
	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}

	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured scratch dir: %v", err)
	}
	scratchDir := strings.TrimSpace(string(got))

	// Reference: os.MkdirTemp("", ...), called the same way an empty
	// BaseDir resolves to, right here — both share the same parent dir
	// (os.TempDir()) without needing any symlink resolution to compare.
	refDir, err := os.MkdirTemp("", "reference-")
	if err != nil {
		t.Fatalf("os.MkdirTemp reference: %v", err)
	}
	defer os.RemoveAll(refDir)

	if filepath.Dir(scratchDir) != filepath.Dir(refDir) {
		t.Fatalf("scratch dir = %q (parent %q), want parent %q (empty BaseDir must preserve os.MkdirTemp(\"\", ...) prior behavior)", scratchDir, filepath.Dir(scratchDir), filepath.Dir(refDir))
	}
}
