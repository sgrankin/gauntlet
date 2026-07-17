// Package executor implements core.Executor: running one named check
// against an exported trial tree.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// outputCap bounds the combined stdout+stderr kept per check. When output
// exceeds this, the head is discarded and the tail is kept.
const outputCap = 64 * 1024

// LocalExecutor runs checks as local OS processes: job.Command as argv (no
// shell), in job.Dir, with the check environment contract exported
// alongside the inherited environment.
type LocalExecutor struct {
	// BaseDir is the directory each check's ephemeral scratch dir
	// (gauntlet-check-*, holding only the result file) is created under via
	// os.MkdirTemp(BaseDir, ...) — rooting this under -state/scratch, swept
	// at daemon startup exactly like the trial-tree export dir, closes the
	// gap where these dirs used to escape every sweep by defaulting to the
	// OS temp dir. Empty preserves that
	// exact prior behavior (os.MkdirTemp's own "" -> os.TempDir()
	// fallback), which every existing caller/test that never sets this
	// field still gets unchanged.
	BaseDir string

	// GitDir, when non-empty, is the daemon's bare repo path (absolute —
	// checks run with cwd set to the trial tree, so a relative path would
	// resolve wrong), exported to every check as GAUNTLET_GIT_DIR so
	// affected-only scripts can `git diff`/`git log` the SHAs the env
	// contract hands them without their own object store (core.EnvGitDir).
	// Empty omits the variable entirely — the pre-GitDir contract,
	// unchanged, which every hand-built executor in tests still gets.
	GitDir string

	// Env is fixed operator-owned environment ("NAME=VALUE" strings) from
	// the executor profile, appended BEFORE the GAUNTLET_* contract so
	// gauntlet's own values win any collision (last entry wins for
	// exec.Cmd.Env). Config validation already rejects GAUNTLET_-prefixed
	// names outright. Nil for the profile-less default, byte-identical to
	// the pre-profiles environment.
	Env []string
}

// RunCheck implements core.Executor.
func (e LocalExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	start := time.Now()

	tmpDir, err := os.MkdirTemp(e.BaseDir, "gauntlet-check-")
	if err != nil {
		return core.CheckResult{
			Name:     job.Name,
			Err:      fmt.Errorf("executor: create temp dir: %w", err),
			Duration: time.Since(start),
		}
	}
	defer os.RemoveAll(tmpDir)

	resultFile := filepath.Join(tmpDir, "result")
	if err := os.WriteFile(resultFile, nil, 0o600); err != nil {
		return core.CheckResult{
			Name:     job.Name,
			Err:      fmt.Errorf("executor: create result file: %w", err),
			Duration: time.Since(start),
		}
	}

	cmd := exec.CommandContext(ctx, job.Command[0], job.Command[1:]...)
	cmd.Dir = job.Dir
	// Fixed profile env sits between the inherited environment and the
	// GAUNTLET_* contract, so gauntlet's own values win any collision
	// (last entry wins for exec.Cmd.Env).
	cmd.Env = append(os.Environ(), e.Env...)
	cmd.Env = append(cmd.Env,
		core.EnvBaseSHA+"="+job.BaseSHA,
		core.EnvMergeSHA+"="+job.MergeSHA,
		core.EnvCandidateSHA+"="+job.Candidate.SHA,
		core.EnvRef+"="+job.Candidate.Ref,
		resultFileEnv(job)+"="+resultFile,
		core.EnvRunID+"="+job.RunID,
	)
	if e.GitDir != "" {
		cmd.Env = append(cmd.Env, core.EnvGitDir+"="+e.GitDir)
	}
	// Shared-services env: appended after the built-ins, nil for
	// checks with no `needs`. Networks is ModeNetwork-only (a shared
	// runtime network) and has no meaning for a local subprocess, so it's
	// deliberately ignored here.
	cmd.Env = append(cmd.Env, job.ServiceEnv...)

	out := &tailBuffer{cap: outputCap}

	// logFile, when non-nil, is teed alongside the tail buffer: the full,
	// uncapped combined output (DESIGN.md "Full per-check log files"). Its
	// open error is deliberately swallowed here — see openCheckLog's doc —
	// so a bad/unwritable job.LogPath degrades to the tail-only capture
	// this executor already had, never to a failed check.
	logFile, _ := openCheckLog(job.LogPath)
	var combined io.Writer = out
	if logFile != nil {
		defer logFile.Close()
		combined = io.MultiWriter(out, logFile)
	}
	cmd.Stdout = combined
	cmd.Stderr = combined

	// Own process group so a cancel can kill the whole tree: a cancelled
	// check must not leave grandchildren holding the export dir open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Bounds how long Wait can be stuck on inherited pipes held open by a
	// grandchild that escaped the group kill (or if Cancel itself fails).
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	duration := time.Since(start)

	// The command has now been attempted regardless of outcome, so every
	// return from here on reports whether the full log file actually got
	// written: LogPath is set iff logFile was successfully opened above,
	// empty otherwise (no file requested, or the open/mkdir fallback).
	logPath := ""
	if logFile != nil {
		logPath = job.LogPath
	}

	// ctx cancellation takes precedence over any run error: the process
	// may exit with a signalled/non-zero status as a side effect of the
	// group kill, but that is not a verdict.
	if ctx.Err() != nil {
		return core.CheckResult{
			Name:     job.Name,
			Err:      ctx.Err(),
			Output:   out.String(),
			LogPath:  logPath,
			Duration: duration,
		}
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// Nonzero exit is a verdict regardless of the result file: the
			// file only splits the exit-0 case, it is not an exit-code
			// convention.
			return core.CheckResult{
				Name:     job.Name,
				Status:   core.CheckFailed,
				Output:   out.String(),
				LogPath:  logPath,
				Duration: duration,
			}
		}
		// Exec-start failure (command missing/not executable/etc.): a
		// verdict, not Err — it's the check spec's problem to fix.
		output := out.String()
		if output != "" {
			output += "\n"
		}
		output += "executor: failed to start command: " + runErr.Error()
		return core.CheckResult{
			Name:     job.Name,
			Status:   core.CheckFailed,
			Output:   output,
			LogPath:  logPath,
			Duration: duration,
		}
	}

	if job.ImageBuild {
		// An image build has no skipped verdict; exit 0 hands the result
		// file's content back verbatim for the QUEUE to validate (a
		// missing/empty/mutable result is a build failure there, with one
		// root cause — never N consumer failures here).
		image := readImageResult(resultFile)
		return core.CheckResult{
			Name:     job.Name,
			Status:   core.CheckPassed,
			Image:    image,
			Output:   out.String(),
			LogPath:  logPath,
			Duration: duration,
		}
	}
	status := core.CheckPassed
	if data, err := os.ReadFile(resultFile); err == nil && strings.TrimSpace(string(data)) == "skipped" {
		status = core.CheckSkipped
	}
	return core.CheckResult{
		Name:     job.Name,
		Status:   status,
		Output:   out.String(),
		LogPath:  logPath,
		Duration: duration,
	}
}

// maxImageResultBytes bounds how much of an image-build result file is
// ever read back: a legitimate reference is well under 1 KiB, and a build
// that misdirects its whole log into the file (`build > "$FILE"` instead
// of --iidfile) must not ride megabytes of it into the result, every
// channel, and history — the truncated content still fails the queue's
// validation, which is all it needs to do.
const maxImageResultBytes = 4096

// readImageResult reads an image build's captured result, bounded and
// trimmed. Read errors read as "" (the queue rejects an empty result with
// a pointed message either way).
func readImageResult(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImageResultBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resultFileEnv picks which result-file variable a job's command sees:
// image builds get EnvImageResultFile INSTEAD of EnvResultFile — a build
// has no skipped verdict, and the two protocols must never be conflated
// (core.EnvImageResultFile's doc).
func resultFileEnv(job core.CheckJob) string {
	if job.ImageBuild {
		return core.EnvImageResultFile
	}
	return core.EnvResultFile
}

// tailBuffer is an io.Writer that keeps only the last cap bytes written to
// it, discarding the head as more arrives.
type tailBuffer struct {
	cap int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if t.cap > 0 && n > t.cap {
		p = p[n-t.cap:]
	}
	t.buf = append(t.buf, p...)
	if excess := len(t.buf) - t.cap; t.cap > 0 && excess > 0 {
		copy(t.buf, t.buf[excess:])
		t.buf = t.buf[:t.cap]
	}
	return n, nil
}

func (t *tailBuffer) String() string {
	return string(t.buf)
}
