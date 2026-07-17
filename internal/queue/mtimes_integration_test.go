package queue

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// pastDatedDirectPush commits files on top of branch with a CONTROLLED
// committer date and pushes directly, like testutil's DirectPush but with
// the date pinned far from the test's own wall clock — the property
// TestIntegration_HistoryMtimesEndToEnd needs, since a wall-clock seed
// commit and a wall-clock export could land in the same second and make
// history-derived and extraction-time mtimes indistinguishable.
func pastDatedDirectPush(t *testing.T, remote *testutil.Remote, branch string, at time.Time, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(nil, "clone", "-q", remote.Dir, ".")
	run(nil, "checkout", "-q", "-B", branch, "origin/"+branch)
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	run(nil, "add", "-A")
	date := at.UTC().Format(time.RFC3339)
	run([]string{
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date,
	}, "commit", "-q", "-m", "past-dated seed")
	run(nil, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
}

// TestIntegration_HistoryMtimesEndToEnd proves the queue↔gitx mtimes
// wiring against REAL git and a REAL check subprocess: with
// Config.HistoryMtimes on, the tree the check actually runs in carries
// history-derived mtimes. The fake-git suite can't catch divergences
// between what gitx stamps and what the export materializes (that's how
// the export-ignore bug slipped the first review), so this runs the whole
// path: fetch → merge-tree → commit-tree → export → RestoreMtimes → a
// /bin/sh check that stats the seeded file.
func TestIntegration_HistoryMtimesEndToEnd(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	h.d.cfg.HistoryMtimes = true
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	// seed.txt's one and only touching commit is pinned to 2026-01-01 —
	// far from both the candidate commit and the export, so only the
	// history walk can produce this mtime.
	seedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pastDatedDirectPush(t, remote, "main", seedTime, map[string]string{"seed.txt": "s\n"})

	outFile := filepath.Join(t.TempDir(), "observed-mtime")
	remote.PushCandidate("main", "alice", "mtimes", shellCheckSpec("record-mtime",
		"stat -c %Y seed.txt > "+outFile+"\n"))

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)
	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v (detail %q), want Landed", rec.Outcome, rec.Detail)
	}

	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("check never wrote its observation: %v", err)
	}
	got, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		t.Fatalf("parse observed mtime %q: %v", raw, err)
	}
	if got != seedTime.Unix() {
		t.Fatalf("seed.txt mtime observed by the check = %d, want the seeding commit's %d (history-derived, not export wall time)", got, seedTime.Unix())
	}
}
