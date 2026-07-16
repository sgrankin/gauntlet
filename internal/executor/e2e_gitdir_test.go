// End-to-end GAUNTLET_GIT_DIR suite: the argv/env-shape tests elsewhere in
// this package prove what the executors *pass*; these prove the contract a
// check script actually experiences — real `git` queries resolving an
// UNPUSHED synthetic merge (an object that exists nowhere but the daemon's
// bare repo) out of $GAUNTLET_GIT_DIR, and, for the container executor, the
// fixed /gauntlet-git mount being readable but not writable.
package executor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// chmodR toggles the write bits on every file and directory under root:
// writable=false strips them (owner/group/other), true restores owner
// write so t.TempDir cleanup can remove the tree.
func chmodR(t *testing.T, root string, writable bool) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if writable {
			mode |= 0o200
		} else {
			mode &^= 0o222
		}
		return os.Chmod(path, mode)
	})
	if err != nil {
		t.Fatalf("chmodR %s: %v", root, err)
	}
}

// bareRepoWithUnpushedMerge builds the exact object topology a live run
// hands checks: a bare clone of a remote whose base ("main") and candidate
// refs were fetched, plus a synthetic merge commit created by CommitTree
// that exists ONLY in the local bare repo — no ref anywhere names it, and
// the remote has never seen it. Returns the bare repo path and the three
// SHAs of the env contract.
func bareRepoWithUnpushedMerge(t *testing.T) (gitDir, baseSHA, candSHA, mergeSHA string) {
	t.Helper()
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"sub/f.txt": "base\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"web/g.txt": "new\n"})

	gitDir = remote.BareClone()
	repo, err := gitx.New(ctx, gitDir, remote.Dir)
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	baseSHA, candSHA = refs["refs/heads/main"], refs[candRef]
	tm, err := repo.MergeTree(ctx, baseSHA, candSHA)
	if err != nil || !tm.Clean {
		t.Fatalf("MergeTree: clean=%v err=%v", tm.Clean, err)
	}
	mergeSHA, err = repo.CommitTree(ctx, tm.TreeOID, []string{baseSHA, candSHA}, "trial merge", core.Identity{Name: "Gauntlet", Email: "g@ci.example"})
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}
	return gitDir, baseSHA, candSHA, mergeSHA
}

func TestLocalExecutor_GitDirEndToEndQueries(t *testing.T) {
	gitDir, baseSHA, candSHA, mergeSHA := bareRepoWithUnpushedMerge(t)

	// The three query shapes docs/checks.md documents, run by a real shell
	// against a real bare repo, resolving a merge commit no remote has:
	// content identity (rev-parse of a subtree), affected-paths diff, and
	// last-changing-commit provenance. All name explicit SHAs, never HEAD.
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", fmt.Sprintf(`#!/bin/sh
set -eu
g() { git --git-dir="$GAUNTLET_GIT_DIR" "$@"; }

g cat-file -e "$GAUNTLET_MERGE_SHA"
g cat-file -e "$GAUNTLET_BASE_SHA"
g cat-file -e "$GAUNTLET_CANDIDATE_SHA"

subtree=$(g rev-parse "$GAUNTLET_MERGE_SHA:web")
[ -n "$subtree" ] || { echo "empty subtree id"; exit 1; }

changed=$(g diff --name-only "$GAUNTLET_BASE_SHA" "$GAUNTLET_MERGE_SHA")
[ "$changed" = %q ] || { echo "diff --name-only = $changed"; exit 1; }

last=$(g log -1 --format=%%H "$GAUNTLET_MERGE_SHA" -- web/)
[ "$last" = "$GAUNTLET_CANDIDATE_SHA" ] || { echo "log -1 -- web/ = $last, want the candidate commit that introduced web/"; exit 1; }
`, "web/g.txt"))

	job := baseJob(t, cmd)
	job.BaseSHA = baseSHA
	job.MergeSHA = mergeSHA
	job.Candidate.SHA = candSHA

	res := LocalExecutor{GitDir: gitDir}.RunCheck(context.Background(), job)
	if res.Err != nil {
		t.Fatalf("unexpected Err: %v (output=%q)", res.Err, res.Output)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
}

// TestLocalExecutor_GitDirQueriesSurviveReadOnly re-runs the same queries
// against a repo whose files were made read-only — the cooperative local
// analogue of the container's :ro mount (docs/checks.md documents the
// local boundary as trust-based; this proves read-only *suffices* for the
// query contract, so a stricter deployment loses nothing).
func TestLocalExecutor_GitDirQueriesSurviveReadOnly(t *testing.T) {
	gitDir, baseSHA, _, mergeSHA := bareRepoWithUnpushedMerge(t)
	chmodR(t, gitDir, false)
	t.Cleanup(func() { chmodR(t, gitDir, true) }) // so TempDir cleanup can delete it

	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", `#!/bin/sh
set -eu
git --git-dir="$GAUNTLET_GIT_DIR" diff --name-only "$1" "$2" >/dev/null
git --git-dir="$GAUNTLET_GIT_DIR" log -1 --format=%H "$2" >/dev/null
`)
	job := baseJob(t, append(cmd, baseSHA, mergeSHA))
	job.BaseSHA = baseSHA
	job.MergeSHA = mergeSHA

	res := LocalExecutor{GitDir: gitDir}.RunCheck(context.Background(), job)
	if res.Err != nil {
		t.Fatalf("unexpected Err: %v (output=%q)", res.Err, res.Output)
	}
	if res.Status != core.CheckPassed {
		t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
	}
}
