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

	// SecretEnv names the config-named operator secret environment
	// variables (config.Daemon.SecretEnvNames — github's token-env in
	// static mode, slack's app-token-env/bot-token-env, summarize's
	// api-key-env) that must never enter a CANDIDATE-CODE command's
	// environment (issue #13 Gap 1, docs/checks.md's environment
	// reference): a check, image build, or receipt producer is a
	// candidate's own repo code, and the daemon's operator secrets are not
	// its business — the daemon reads the GitHub token itself, in-process,
	// and never hands it to a child. Only NAMES, never values; matched by
	// exact env-var name, not prefix or pattern.
	//
	// Applied in RunCheck to the BASE os.Environ() only, before Env/the
	// GAUNTLET_* contract/ServiceEnv are appended — those layers are
	// gauntlet's own or the operator's own, never a candidate's, so
	// nothing in them is ever a filter target. Skipped entirely when
	// job.OperatorOwned is true (internal/hooks sets this on every
	// post-land hook job): a hook is itself operator-owned daemon config,
	// not candidate code, and legitimately uses these same credentials
	// (e.g. a deploy hook driving `gh`) — see core.CheckJob.OperatorOwned's
	// doc for the full boundary. Nil (the default, and every hand-built
	// executor in tests that doesn't set it) filters nothing, byte-identical
	// to the pre-Gap-1 environment.
	//
	// This is NOT a sandbox: DESIGN.md's issue #6 same-UID /proc caveat
	// still stands unchanged — a same-UID process can still read another
	// process's environment off /proc on platforms that allow it,
	// regardless of what this executor puts in argv/envp. What this DOES
	// close is the ordinary, by-design channel: a candidate command no
	// longer receives the credential in its own env at all.
	SecretEnv []string
}

// filterSecretEnv returns env with every entry whose NAME (the part before
// "=") appears in secret dropped, preserving relative order of what
// remains. Matches by exact name only. A nil/empty secret returns env
// unchanged (same backing array, no allocation) — the common case for
// every daemon with no configured integrations and for every hand-built
// executor in tests.
func filterSecretEnv(env []string, secret []string) []string {
	if len(secret) == 0 {
		return env
	}
	drop := make(map[string]bool, len(secret))
	for _, name := range secret {
		drop[name] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
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
	// Base environment: the daemon's own os.Environ(), with every
	// config-named operator secret stripped by exact name — UNLESS this
	// job is operator-owned (a post-land hook), which is exempt (see
	// SecretEnv's and core.CheckJob.OperatorOwned's docs). Filtering here,
	// before Env/the GAUNTLET_* contract/ServiceEnv are appended below,
	// means none of gauntlet's own or the operator's own values can ever
	// be a filter target — only the inherited ambient environment is.
	base := os.Environ()
	if !job.OperatorOwned {
		base = filterSecretEnv(base, e.SecretEnv)
	}
	// Fixed profile env sits between the inherited environment and the
	// GAUNTLET_* contract, so gauntlet's own values win any collision
	// (last entry wins for exec.Cmd.Env).
	cmd.Env = append(base, e.Env...)
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
	if job.ReceiptCapture {
		// A receipt has no skipped verdict either; exit 0 hands the result
		// file's RAW bytes back verbatim (bounded to job.ReceiptMaxBytes+1
		// so the queue can detect an oversized result) for the queue to
		// validate — empty/unreadable/oversized are all rejected there,
		// with one root cause on the receipt node itself, never a check
		// running against a payload that was never actually captured.
		receipt := readReceiptResult(resultFile, job.ReceiptMaxBytes)
		return core.CheckResult{
			Name:     job.Name,
			Status:   core.CheckPassed,
			Receipt:  receipt,
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
// image builds get EnvImageResultFile INSTEAD of EnvResultFile, and receipt
// jobs get EnvReceiptResultFile INSTEAD of either — each protocol is
// distinct and must never be conflated (core.EnvImageResultFile's and
// core.EnvReceiptResultFile's docs).
func resultFileEnv(job core.CheckJob) string {
	switch {
	case job.ImageBuild:
		return core.EnvImageResultFile
	case job.ReceiptCapture:
		return core.EnvReceiptResultFile
	default:
		return core.EnvResultFile
	}
}

// maxReceiptResultBytesFallback bounds readReceiptResult's read when a
// ReceiptCapture job somehow carries no ReceiptMaxBytes (defense in depth —
// SpecRejectReason's symmetric gate means a receipt node only ever runs
// with a configured policy, so job.ReceiptMaxBytes should always be
// positive by construction). Matches config's maxAllowedReceiptBytes hard
// ceiling (internal/config/daemon.go), not duplicated by import — this
// package doesn't depend on internal/config — so a fallback read can never
// exceed what a legitimately configured max-bytes could ever have allowed
// anyway.
const maxReceiptResultBytesFallback = 1 << 20

// readReceiptResult reads a receipt node's captured result file, bounded to
// maxBytes+1 bytes so the QUEUE can tell a legitimate payload from an
// oversized one (len(result) > maxBytes) without this executor ever loading
// more than one byte past the configured limit into memory. maxBytes<=0
// falls back to maxReceiptResultBytesFallback.
//
// Returns nil when the file cannot be opened or read at all (missing, a
// permission error, or the command left something unreadable in its
// place) — deliberately distinct from a non-nil EMPTY slice, which means
// the file was opened and read successfully but is genuinely zero bytes;
// core.CheckResult.Receipt's doc and the queue's validReceipt tell the two
// apart with different one-line messages.
func readReceiptResult(path string, maxBytes int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	bound := maxBytes
	if bound <= 0 {
		bound = maxReceiptResultBytesFallback
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(bound)+1))
	if err != nil {
		return nil
	}
	if data == nil {
		data = []byte{} // guarantee non-nil for "successfully read", even for a genuinely empty file
	}
	return data
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
