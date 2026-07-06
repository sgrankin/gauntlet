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

// outputCap bounds the combined stdout+stderr kept per check, per
// docs/plans/phase1.md §9.6. When output exceeds this, the head is
// discarded and the tail is kept.
const outputCap = 64 * 1024

// LocalExecutor runs checks as local OS processes: job.Command as argv (no
// shell), in job.Dir, with the check environment contract
// (docs/plans/phase1.md §5A) exported alongside the inherited environment.
type LocalExecutor struct {
	// BaseDir is the directory each check's ephemeral scratch dir
	// (gauntlet-check-*, holding only the result file) is created under via
	// os.MkdirTemp(BaseDir, ...) — S16 (phase-6 audit synthesis): rooting
	// this under -state/scratch, swept at daemon startup exactly like the
	// trial-tree export dir, closes the gap where these dirs used to escape
	// every sweep by defaulting to the OS temp dir. Empty preserves that
	// exact prior behavior (os.MkdirTemp's own "" -> os.TempDir()
	// fallback), which every existing caller/test that never sets this
	// field still gets unchanged.
	BaseDir string
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
	cmd.Env = append(os.Environ(),
		core.EnvBaseSHA+"="+job.BaseSHA,
		core.EnvMergeSHA+"="+job.MergeSHA,
		core.EnvCandidateSHA+"="+job.Candidate.SHA,
		core.EnvRef+"="+job.Candidate.Ref,
		core.EnvResultFile+"="+resultFile,
		core.EnvRunID+"="+job.RunID,
	)

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

	// Own process group so a cancel can kill the whole tree (§9.5): a
	// cancelled check must not leave grandchildren holding the export dir
	// open.
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
			// Nonzero exit is a verdict regardless of the result file
			// (§5A): the file only splits the exit-0 case, it is not an
			// exit-code convention.
			return core.CheckResult{
				Name:     job.Name,
				Status:   core.CheckFailed,
				Output:   out.String(),
				LogPath:  logPath,
				Duration: duration,
			}
		}
		// Exec-start failure (command missing/not executable/etc.): a
		// verdict, not Err (§9.2) — it's the check spec's problem to fix.
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
