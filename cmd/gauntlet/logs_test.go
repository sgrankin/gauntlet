package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkLogDir creates dir/name (a stand-in for one run's log directory,
// <logDir>/<runID>/) with a file inside it, then backdates the directory's
// modtime to at — pruneLogFiles keys off the directory's modtime, not the
// file's, since that's what a run-log directory gets once (on first check
// log file creation) and never again (see pruneLogFiles' doc).
func mkLogDir(t *testing.T, dir, name string, at time.Time) string {
	t.Helper()
	runDir := filepath.Join(dir, name)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", runDir, err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "check.log"), []byte("log content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(runDir, at, at); err != nil {
		t.Fatalf("Chtimes(%s): %v", runDir, err)
	}
	return runDir
}

// TestPruneLogFiles_DeletesOldKeepsFresh confirms pruneLogFiles removes
// exactly the run-log directories whose modtime is at or before cutoff,
// leaving fresher directories (and their contents) untouched.
func TestPruneLogFiles_DeletesOldKeepsFresh(t *testing.T) {
	logDir := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)

	oldDir := mkLogDir(t, logDir, "run-old", cutoff.Add(-time.Hour))
	boundaryDir := mkLogDir(t, logDir, "run-boundary", cutoff)
	freshDir := mkLogDir(t, logDir, "run-fresh", cutoff.Add(time.Hour))

	if err := pruneLogFiles(logDir, cutoff); err != nil {
		t.Fatalf("pruneLogFiles: %v", err)
	}

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("old run dir %s still exists (err=%v), want deleted", oldDir, err)
	}
	if _, err := os.Stat(boundaryDir); !os.IsNotExist(err) {
		t.Errorf("boundary run dir %s (modtime == cutoff) still exists (err=%v), want deleted", boundaryDir, err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh run dir %s missing: %v, want kept", freshDir, err)
	}
	if data, err := os.ReadFile(filepath.Join(freshDir, "check.log")); err != nil || string(data) != "log content" {
		t.Errorf("fresh run dir's log content = %q, %v, want intact", data, err)
	}
}

// TestPruneLogFiles_MissingLogDirIsNotAnError confirms a logDir that
// doesn't exist yet (fresh state dir, or nothing has ever assigned a log
// path) is treated as "nothing to prune", not an error — mirroring every
// other degrade-gracefully path in this codebase.
func TestPruneLogFiles_MissingLogDirIsNotAnError(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := pruneLogFiles(logDir, time.Now()); err != nil {
		t.Errorf("pruneLogFiles(missing dir) = %v, want nil", err)
	}
}

// TestPruneLogFiles_IgnoresNonDirectoryEntries confirms a stray non-directory
// file directly under logDir (not the expected <runID>/ layout) is left
// alone rather than deleted — pruneLogFiles only ever removes directories.
func TestPruneLogFiles_IgnoresNonDirectoryEntries(t *testing.T) {
	logDir := t.TempDir()
	strayFile := filepath.Join(logDir, "stray.txt")
	if err := os.WriteFile(strayFile, []byte("not a run dir"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	old := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(strayFile, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := pruneLogFiles(logDir, time.Now()); err != nil {
		t.Fatalf("pruneLogFiles: %v", err)
	}
	if _, err := os.Stat(strayFile); err != nil {
		t.Errorf("stray file %s removed (err=%v), want left alone (not a directory)", strayFile, err)
	}
}
