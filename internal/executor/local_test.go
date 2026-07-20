package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	// A nonzero exit is a verdict regardless of the result file: the file
	// only splits the exit-0 case.
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

func TestLocalExecutor_GitDirEnv(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf(`#!/bin/sh
test "$%s" = "/state/repos/origin.git" || { echo "bad git dir: $%s"; exit 1; }
exit 0
`, core.EnvGitDir, core.EnvGitDir)
	cmd := script(t, dir, "check.sh", body)
	job := baseJob(t, cmd)

	res := LocalExecutor{GitDir: "/state/repos/origin.git"}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
}

func TestLocalExecutor_EmptyGitDirOmitsEnvVar(t *testing.T) {
	// Empty GitDir must leave the variable UNSET (the pre-GitDir contract),
	// not set-but-empty: a check script telling the two apart with
	// ${GAUNTLET_GIT_DIR+x} should see the var absent.
	dir := t.TempDir()
	body := fmt.Sprintf(`#!/bin/sh
test -z "${%s+x}" || { echo "git dir var unexpectedly set: $%s"; exit 1; }
exit 0
`, core.EnvGitDir, core.EnvGitDir)
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

func TestLocalExecutor_ServiceEnvAppended(t *testing.T) {
	dir := t.TempDir()
	body := `#!/bin/sh
test "$GAUNTLET_SVC_PG_HOST" = "127.0.0.1" || { echo "bad host: $GAUNTLET_SVC_PG_HOST"; exit 1; }
test "$GAUNTLET_SVC_PG_PORT" = "54321" || { echo "bad port: $GAUNTLET_SVC_PG_PORT"; exit 1; }
exit 0
`
	cmd := script(t, dir, "check.sh", body)
	job := baseJob(t, cmd)
	job.ServiceEnv = []string{"GAUNTLET_SVC_PG_HOST=127.0.0.1", "GAUNTLET_SVC_PG_PORT=54321"}
	// Networks is ModeNetwork-only and meaningless to a local subprocess;
	// setting it here proves LocalExecutor ignores it rather than erroring.
	job.Networks = []string{"gauntlet-svc-abcd1234"}

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
}

// TestLocalExecutor_SecretEnvStrippedFromCandidateJob covers issue #13 Gap
// 1's core mechanism directly: a job with OperatorOwned left at its zero
// value (false — candidate code: an ordinary check, image build, or
// receipt producer) must not see a variable named in SecretEnv, even
// though it's present in this test process's own environment (t.Setenv)
// and would otherwise pass straight through via os.Environ().
func TestLocalExecutor_SecretEnvStrippedFromCandidateJob(t *testing.T) {
	const secretVar = "GAUNTLET_TEST_SECRET_VAR"
	const secretValue = "must-not-leak-to-candidate-code"
	t.Setenv(secretVar, secretValue)

	dir := t.TempDir()
	body := fmt.Sprintf(`#!/bin/sh
test -z "${%s+x}" || { echo "secret var visible to candidate job: $%s"; exit 1; }
exit 0
`, secretVar, secretVar)
	cmd := script(t, dir, "check.sh", body)
	job := baseJob(t, cmd) // OperatorOwned: false (zero value) — candidate code

	res := LocalExecutor{SecretEnv: []string{secretVar}}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed (SecretEnv var must be stripped from a candidate job); output=%q", res.Status, res.Output)
	}
}

// TestLocalExecutor_SecretEnvVisibleToOperatorOwnedJob covers the
// deliberate exemption's other half: a job with OperatorOwned true (what
// internal/hooks's Runner sets on every post-land hook job) MUST still see
// a SecretEnv-named variable — hooks are operator-owned daemon config, not
// candidate code, and legitimately use the same credentials the daemon
// itself holds (e.g. a deploy hook driving `gh`). Same SecretEnv, same
// variable, opposite job flag, opposite outcome — proving the exemption is
// live, not merely that filtering is off by default.
func TestLocalExecutor_SecretEnvVisibleToOperatorOwnedJob(t *testing.T) {
	const secretVar = "GAUNTLET_TEST_SECRET_VAR"
	const secretValue = "hook-may-legitimately-see-this"
	t.Setenv(secretVar, secretValue)

	dir := t.TempDir()
	body := fmt.Sprintf(`#!/bin/sh
test "$%s" = %q || { echo "secret var missing or wrong for operator-owned job: $%s"; exit 1; }
exit 0
`, secretVar, secretValue, secretVar)
	cmd := script(t, dir, "hook.sh", body)
	job := baseJob(t, cmd)
	job.OperatorOwned = true // the hooks exemption (core.CheckJob.OperatorOwned's doc)

	res := LocalExecutor{SecretEnv: []string{secretVar}}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed (an operator-owned job must still see its configured secret); output=%q", res.Status, res.Output)
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
	// be dead too, not just the direct child.
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

// TestLocalExecutor_BaseDirRootsScratchDir proves that a non-empty BaseDir
// roots the check's ephemeral scratch dir under it, rather than the OS
// default temp dir — the property that lets
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

// --- resource usage capture (issue #14) ---

// requireTac skips the test when GNU coreutils' tac isn't on PATH — house
// CI (ubuntu-latest, see .github/workflows) always has it, but a developer
// running tests on a machine without GNU coreutils (e.g. a bare macOS
// install, which ships BSD tools and no tac at all) shouldn't see a
// spurious failure from a test whose only job is to allocate a known-size
// buffer.
func requireTac(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tac"); err != nil {
		t.Skip("tac not on PATH, skipping (see requireTac's doc)")
	}
}

// TestLocalExecutor_PeakRSS_BoundedRange allocates a known-order-of-magnitude
// buffer and asserts PeakRSS lands in a bounded range around it, not an
// exact value (peak RSS is inherently a little fuzzy: allocator overhead,
// page rounding, the shell and dd's own small footprint all add a bit on
// top of the pure buffer).
//
// The shape: `dd if=/dev/zero bs=1M count=32 | tac > /dev/null`, piped
// (not through a regular file) so tac cannot mmap its input — a pipe isn't
// seekable, so tac is forced to read the whole 32MB into a malloc'd
// buffer before it can emit anything in reverse, which is exactly the
// large, deterministic, portable allocation this test wants. (Tried
// piping through a real file first: tac mmaps a seekable regular file
// instead of copying it, so touched-but-mapped pages don't reliably show
// up in ru_maxrss — that variant measured ~2.7MB, not ~32MB, on this
// host. The piped form measured a stable ~34MB across five repeated
// runs — see the investigation notes accompanying this change.)
//
// Bounds: > 24MB rules out "nothing was captured" (a bug regressing to
// near-zero); < 400MB gives generous headroom above the ~34MB observed
// value for allocator/page-rounding variance across kernels and libc
// versions without risking a flake — see CLAUDE.md: bounded-range
// assertions must not be flaky, widen before shipping a flake.
func TestLocalExecutor_PeakRSS_BoundedRange(t *testing.T) {
	requireTac(t)
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\ndd if=/dev/zero bs=1M count=32 2>/dev/null | tac > /dev/null\n")
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
	const minBytes = 24 * 1024 * 1024
	const maxBytes = 400 * 1024 * 1024
	if res.PeakRSS < minBytes || res.PeakRSS > maxBytes {
		t.Errorf("PeakRSS = %d bytes (%.1fMB), want in [%d, %d] (~24-400MB)", res.PeakRSS, float64(res.PeakRSS)/(1024*1024), minBytes, maxBytes)
	}
}

// TestLocalExecutor_CPUSpin_UserCPUDominatesAndClearsFloor runs a pure
// busy-loop (no syscalls in the hot path beyond arithmetic and the
// condition test) and asserts UserCPU is both well above SysCPU (the
// command does no I/O) and comfortably above a floor that rules out "CPU
// time wasn't actually captured".
func TestLocalExecutor_CPUSpin_UserCPUDominatesAndClearsFloor(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\ni=0\nwhile [ $i -lt 2000000 ]; do i=$((i+1)); done\n")
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
	const floor = 100 * time.Millisecond
	if res.UserCPU < floor {
		t.Errorf("UserCPU = %v, want >= %v (a 2M-iteration busy loop)", res.UserCPU, floor)
	}
	if res.UserCPU <= res.SysCPU {
		t.Errorf("UserCPU = %v, SysCPU = %v, want UserCPU well above SysCPU (a pure busy loop does no I/O)", res.UserCPU, res.SysCPU)
	}
}

// TestLocalExecutor_TrivialCommand_ResourceFieldsPresentButSmall asserts
// the opposite corner from the two tests above: a command that does
// essentially nothing still gets a real (nonzero) PeakRSS — every process
// has SOME resident footprint just from being loaded and started — but
// every field stays small, nowhere near the buffer/spin tests' values.
func TestLocalExecutor_TrivialCommand_ResourceFieldsPresentButSmall(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", "#!/bin/sh\nexit 0\n")
	job := baseJob(t, cmd)

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}
	if res.PeakRSS <= 0 {
		t.Errorf("PeakRSS = %d, want > 0 (every started process has some resident footprint)", res.PeakRSS)
	}
	const smallCeiling = 50 * 1024 * 1024 // 50MB: generous ceiling for a bare `sh -c exit 0`
	if res.PeakRSS > smallCeiling {
		t.Errorf("PeakRSS = %d bytes, want < %d (a trivial command should have a small footprint)", res.PeakRSS, smallCeiling)
	}
	if res.UserCPU < 0 || res.UserCPU > time.Second {
		t.Errorf("UserCPU = %v, want in [0, 1s] for a trivial command", res.UserCPU)
	}
	if res.SysCPU < 0 || res.SysCPU > time.Second {
		t.Errorf("SysCPU = %v, want in [0, 1s] for a trivial command", res.SysCPU)
	}
}

// TestLocalExecutor_CtxCancel_StillCapturesPartialUsage confirms the
// investigation finding behind captureRusage's doc: cmd.Cancel's SIGKILL
// still lets exec.Cmd.Wait populate ProcessState (and its rusage) before
// Run returns, so a cancelled check reports whatever CPU/RSS it burned
// before being killed rather than all zeros. Uses a CPU-spinning script
// (not a mere `sleep`, which does no CPU work and so would round to zero
// either way) so a nonzero UserCPU is a meaningful assertion.
func TestLocalExecutor_CtxCancel_StillCapturesPartialUsage(t *testing.T) {
	dir := t.TempDir()
	// A very long spin loop, killed well before it could finish on its own.
	cmd := script(t, dir, "check.sh", "#!/bin/sh\ni=0\nwhile [ $i -lt 100000000 ]; do i=$((i+1)); done\n")
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
		if res.UserCPU <= 0 {
			t.Errorf("UserCPU = %v, want > 0 (the loop ran for ~200ms before being killed, and ProcessState is populated even after cmd.Cancel's SIGKILL)", res.UserCPU)
		}
		if res.PeakRSS <= 0 {
			t.Errorf("PeakRSS = %d, want > 0 (same reasoning as UserCPU)", res.PeakRSS)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunCheck did not return after ctx cancel")
	}
}

// TestLocalExecutor_CommandNotFound_ResourceFieldsZero confirms the other
// half of captureRusage's nil-ProcessState branch: when exec itself never
// gets a process started (command missing), there is nothing to have
// measured, so all three resource fields stay at their zero
// "not measured" value rather than some stale or fabricated number.
func TestLocalExecutor_CommandNotFound_ResourceFieldsZero(t *testing.T) {
	job := baseJob(t, []string{filepath.Join(t.TempDir(), "does-not-exist")})

	res := LocalExecutor{}.RunCheck(context.Background(), job)

	if res.PeakRSS != 0 {
		t.Errorf("PeakRSS = %d, want 0 (exec-start failure never started a process)", res.PeakRSS)
	}
	if res.UserCPU != 0 {
		t.Errorf("UserCPU = %v, want 0", res.UserCPU)
	}
	if res.SysCPU != 0 {
		t.Errorf("SysCPU = %v, want 0", res.SysCPU)
	}
}
