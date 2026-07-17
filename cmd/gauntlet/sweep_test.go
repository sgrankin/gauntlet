package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSweepAndRecreate_RemovesOrphanScratchDirs proves the scratch-dir
// sweep: orphaned gauntlet-check-*/gauntlet-container-* dirs left under
// -state/scratch by a prior crashed daemon must be gone after the sweep,
// and the directory itself must exist (empty) afterward so executors can
// create fresh scratch dirs under it immediately.
func TestSweepAndRecreate_RemovesOrphanScratchDirs(t *testing.T) {
	state := t.TempDir()
	scratch := filepath.Join(state, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("seed scratch dir: %v", err)
	}
	orphan1 := filepath.Join(scratch, "gauntlet-check-abc123")
	orphan2 := filepath.Join(scratch, "gauntlet-container-def456")
	for _, d := range []string{orphan1, orphan2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("seed orphan dir %s: %v", d, err)
		}
		if err := os.WriteFile(filepath.Join(d, "result"), []byte("stale"), 0o600); err != nil {
			t.Fatalf("seed orphan file: %v", err)
		}
	}

	if err := sweepAndRecreate(scratch); err != nil {
		t.Fatalf("sweepAndRecreate: %v", err)
	}

	if _, err := os.Stat(orphan1); !os.IsNotExist(err) {
		t.Errorf("orphan1 %s still exists after sweep (err=%v)", orphan1, err)
	}
	if _, err := os.Stat(orphan2); !os.IsNotExist(err) {
		t.Errorf("orphan2 %s still exists after sweep (err=%v)", orphan2, err)
	}
	info, err := os.Stat(scratch)
	if err != nil {
		t.Fatalf("scratch dir missing after sweep: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("scratch path is not a directory after sweep")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatalf("ReadDir scratch: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("scratch dir has %d entries after sweep, want 0", len(entries))
	}
}

// TestSweepAndRecreate_RemovesOrphanNodeWorkspaces is issue #9's startup
// orphan-sweep guarantee: isolated per-node workspaces are created as
// gauntlet-node-* dirs directly under WorkDir (== the trials dir), so the
// same wholesale trials-dir sweep that clears crashed run-level
// gauntlet-trial-* dirs must also clear crash-left node workspaces. The
// sweep is prefix-agnostic (it recreates the whole directory), so a
// gauntlet-node-* orphan is removed alongside a gauntlet-trial-* one.
func TestSweepAndRecreate_RemovesOrphanNodeWorkspaces(t *testing.T) {
	state := t.TempDir()
	trials := filepath.Join(state, "trials")
	if err := os.MkdirAll(trials, 0o755); err != nil {
		t.Fatalf("seed trials dir: %v", err)
	}
	nodeOrphan := filepath.Join(trials, "gauntlet-node-abc123")
	runOrphan := filepath.Join(trials, "gauntlet-trial-def456")
	for _, d := range []string{nodeOrphan, runOrphan} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("seed orphan dir %s: %v", d, err)
		}
		if err := os.WriteFile(filepath.Join(d, "stray.txt"), []byte("stale"), 0o600); err != nil {
			t.Fatalf("seed orphan file: %v", err)
		}
	}

	if err := sweepAndRecreate(trials); err != nil {
		t.Fatalf("sweepAndRecreate: %v", err)
	}

	if _, err := os.Stat(nodeOrphan); !os.IsNotExist(err) {
		t.Errorf("node workspace orphan %s still exists after sweep (err=%v)", nodeOrphan, err)
	}
	if _, err := os.Stat(runOrphan); !os.IsNotExist(err) {
		t.Errorf("run-level orphan %s still exists after sweep (err=%v)", runOrphan, err)
	}
	entries, err := os.ReadDir(trials)
	if err != nil {
		t.Fatalf("ReadDir trials: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("trials dir has %d entries after sweep, want 0", len(entries))
	}
}

// TestSweepAndRecreate_CreatesDirIfAbsent covers the first-run case: no
// prior scratch dir at all (fresh -state), same as trialsDir's existing
// startup behavior.
func TestSweepAndRecreate_CreatesDirIfAbsent(t *testing.T) {
	state := t.TempDir()
	scratch := filepath.Join(state, "scratch")

	if err := sweepAndRecreate(scratch); err != nil {
		t.Fatalf("sweepAndRecreate: %v", err)
	}
	info, err := os.Stat(scratch)
	if err != nil || !info.IsDir() {
		t.Fatalf("scratch dir not created: err=%v info=%v", err, info)
	}
}

// fakeRuntimeScript writes an executable shell script named binName into dir
// that appends every invocation's argv (one line, space-joined) to logPath
// and always exits 0 — a fake container-runtime CLI standing in for
// docker/podman/`container`, so this test never depends on one actually
// being installed. Its "ls" case prints an empty `container ls --format
// json` array (no survivors), matching listContainerNames' expected shape
// for the "container" runtime.
func fakeRuntimeScript(t *testing.T, dir, binName, logPath string) {
	t.Helper()
	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
if [ "$1" = "ls" ]; then
  echo '[]'
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, binName), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime script: %v", err)
	}
}

// TestSweepContainerOrphans_RemovesSurvivors proves the container-orphan
// half of S16/B1: given a fake `container` runtime whose `ls -a --format
// json` reports two survivor names, sweepContainerOrphans must issue exactly
// one `ls` call and one `rm -f <name>` per survivor — the "list, then rm -f
// matching survivors" shape — without ever invoking a real container
// runtime. An empty token means the host-global "gauntlet-" prefix matches
// both.
func TestSweepContainerOrphans_RemovesSurvivors(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "invocations.log")

	// The fake script always echoes its args; for an `ls` call it also
	// needs to print the two survivor names, `container ls --format
	// json`-shaped, as its "output" so sweepContainerOrphans has something
	// to parse.
	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
if [ "$1" = "ls" ]; then
  echo '[{"id":"gauntlet-run1-check1"},{"id":"gauntlet-run2-check1"}]'
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "container"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	t.Setenv("PATH", binDir)

	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "container", "", &logBuf)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("invocations = %v, want exactly 3 (1 ls + 2 rm -f)", lines)
	}
	if !strings.HasPrefix(lines[0], "ls ") {
		t.Errorf("invocation 0 = %q, want an `ls ...` call first", lines[0])
	}
	if strings.Contains(lines[0], "--filter") {
		t.Errorf("ls invocation = %q, want no --filter flag (container CLI's ps has no such flag/subcommand)", lines[0])
	}
	wantRM := map[string]bool{
		"rm -f gauntlet-run1-check1": false,
		"rm -f gauntlet-run2-check1": false,
	}
	for _, l := range lines[1:] {
		if _, ok := wantRM[l]; !ok {
			t.Errorf("unexpected invocation %q", l)
			continue
		}
		wantRM[l] = true
	}
	for want, seen := range wantRM {
		if !seen {
			t.Errorf("expected invocation %q was not made; got %v", want, lines[1:])
		}
	}
}

// TestSweepContainerOrphans_TokenScopesToOwnPrefix proves B1's fix directly:
// given two survivor names, one prefixed with this daemon's own token and
// one with a different (sibling daemon's) token, only the matching one is
// ever `rm -f`'d — a `gauntlet-<othertoken>-...` name must survive.
func TestSweepContainerOrphans_TokenScopesToOwnPrefix(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "invocations.log")

	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
if [ "$1" = "ls" ]; then
  echo '[{"id":"gauntlet-mytoken-run1-check1"},{"id":"gauntlet-othertoken-run2-check1"}]'
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "container"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	t.Setenv("PATH", binDir)

	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "container", "mytoken", &logBuf)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("invocations = %v, want exactly 2 (1 ls + 1 rm -f) — the sibling daemon's gauntlet-othertoken- container must survive", lines)
	}
	if lines[1] != "rm -f gauntlet-mytoken-run1-check1" {
		t.Errorf("invocation 1 = %q, want rm -f only for this daemon's own token-prefixed container", lines[1])
	}
}

// TestSweepContainerOrphans_DockerUsesPlainPSNoFilter proves docker/podman
// keep the `ps -a --format {{.Names}}` shape (no `--filter`, since prefix
// filtering now happens client-side in Go for every runtime — B1).
func TestSweepContainerOrphans_DockerUsesPlainPSNoFilter(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "invocations.log")

	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
if [ "$1" = "ps" ]; then
  echo "gauntlet-run1-check1"
  echo "other-container"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	t.Setenv("PATH", binDir)

	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "docker", "", &logBuf)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("invocations = %v, want exactly 2 (1 ps + 1 rm -f, non-gauntlet name skipped)", lines)
	}
	if lines[0] != "ps -a --format {{.Names}}" {
		t.Errorf("ps invocation = %q, want the plain (unfiltered) shape", lines[0])
	}
	if lines[1] != "rm -f gauntlet-run1-check1" {
		t.Errorf("rm invocation = %q, want only the gauntlet-prefixed name", lines[1])
	}
}

// TestSweepContainerOrphans_NoSurvivorsNoRM covers the common case: `ls`
// reports nothing, so no `rm -f` is ever issued.
func TestSweepContainerOrphans_NoSurvivorsNoRM(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "invocations.log")
	fakeRuntimeScript(t, binDir, "container", logPath)
	t.Setenv("PATH", binDir)

	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "container", "", &logBuf)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "ls ") {
		t.Fatalf("invocations = %v, want exactly 1 (`ls ...`), no rm -f calls", lines)
	}
}

// TestSweepContainerOrphans_EmptyRuntimeIsNoOp covers "no container
// executor configured": nothing runs, nothing is logged.
func TestSweepContainerOrphans_EmptyRuntimeIsNoOp(t *testing.T) {
	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "", "", &logBuf)
	if logBuf.String() != "" {
		t.Fatalf("log = %q, want empty (no-op for an unconfigured runtime)", logBuf.String())
	}
}

// TestSweepContainerOrphans_MissingBinaryLogsAndContinues proves the
// "tolerant of the runtime binary being absent" requirement: a runtime name
// that isn't on PATH at all must log a line and return without panicking or
// blocking startup.
func TestSweepContainerOrphans_MissingBinaryLogsAndContinues(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty directory: nothing resolves

	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "container", "", &logBuf)

	if !strings.Contains(logBuf.String(), "not found") {
		t.Fatalf("log = %q, want a message noting the runtime binary was not found", logBuf.String())
	}
}
