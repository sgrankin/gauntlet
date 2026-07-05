package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSweepAndRecreate_RemovesOrphanScratchDirs proves S16's scratch-dir
// sweep (phase-6 audit synthesis): orphaned gauntlet-check-*/
// gauntlet-container-* dirs left under -state/scratch by a prior crashed
// daemon must be gone after the sweep, and the directory itself must exist
// (empty) afterward so executors can create fresh scratch dirs under it
// immediately.
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

// fakeRuntimeScript writes an executable shell script named binName into
// dir that appends every invocation's argv (one line, space-joined) to
// logPath and always exits 0 — a fake container-runtime CLI standing in for
// docker/podman/`container`, so this test never depends on one actually
// being installed.
func fakeRuntimeScript(t *testing.T, dir, binName, logPath string) {
	t.Helper()
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, binName), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime script: %v", err)
	}
}

// TestSweepContainerOrphans_RemovesSurvivors proves the container-orphan
// half of S16: given a fake runtime whose `ps --filter name=gauntlet-`
// reports two survivor names, sweepContainerOrphans must issue exactly one
// `ps` call and one `rm -f <name>` per survivor — the documented "ps, then
// rm -f survivors" shape — without ever invoking a real container runtime.
func TestSweepContainerOrphans_RemovesSurvivors(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "invocations.log")

	// The fake script always echoes its args; for a `ps` call it also needs
	// to print the two survivor names as its "output" so
	// sweepContainerOrphans has something to parse. Layer a second script
	// body: log the call, then if $1 is "ps" print survivor names.
	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
if [ "$1" = "ps" ]; then
  echo "gauntlet-run1-check1"
  echo "gauntlet-run2-check1"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "container"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	t.Setenv("PATH", binDir)

	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "container", &logBuf)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("invocations = %v, want exactly 3 (1 ps + 2 rm -f)", lines)
	}
	if !strings.HasPrefix(lines[0], "ps ") {
		t.Errorf("invocation 0 = %q, want a `ps ...` call first", lines[0])
	}
	if !strings.Contains(lines[0], "name=gauntlet-") {
		t.Errorf("ps invocation = %q, want a name=gauntlet- filter", lines[0])
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

// TestSweepContainerOrphans_NoSurvivorsNoRM covers the common case: `ps`
// reports nothing, so no `rm -f` is ever issued.
func TestSweepContainerOrphans_NoSurvivorsNoRM(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "invocations.log")
	fakeRuntimeScript(t, binDir, "container", logPath)
	t.Setenv("PATH", binDir)

	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "container", &logBuf)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "ps ") {
		t.Fatalf("invocations = %v, want exactly 1 (`ps ...`), no rm -f calls", lines)
	}
}

// TestSweepContainerOrphans_EmptyRuntimeIsNoOp covers "no container
// executor configured": nothing runs, nothing is logged.
func TestSweepContainerOrphans_EmptyRuntimeIsNoOp(t *testing.T) {
	var logBuf strings.Builder
	sweepContainerOrphans(context.Background(), "", &logBuf)
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
	sweepContainerOrphans(context.Background(), "container", &logBuf)

	if !strings.Contains(logBuf.String(), "not found") {
		t.Fatalf("log = %q, want a message noting the runtime binary was not found", logBuf.String())
	}
}
