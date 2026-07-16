package testutil

import (
	"os/exec"
	"strings"
	"testing"
)

// UnpushedMerge is the object topology every gauntlet run hands its checks:
// a bare clone of a remote holding a base branch ("main") and one
// candidate, plus a synthetic merge commit that exists ONLY in the local
// bare repo — no ref anywhere names it, and the remote has never seen it.
// Built with the same plumbing invocations the daemon's gitx runs
// (`merge-tree --write-tree`, `commit-tree`), so tests across packages
// (gitx's pin semantics, executor's GAUNTLET_GIT_DIR contract) exercise the
// object shape the daemon actually creates. Plain git commands rather than
// gitx itself only because gitx's own tests import this package — the
// invocations are the contract, not the Go wrapper.
type UnpushedMerge struct {
	Remote *Remote
	GitDir string // the local bare repo a gitx.Repo would be constructed against

	BaseSHA  string // the target tip the merge was built on
	CandSHA  string // the candidate tip (the merge's second parent)
	MergeSHA string // the unpushed synthetic merge commit
}

// NewUnpushedMerge builds an UnpushedMerge: seedFiles become the base
// branch's tree, candFiles the candidate's additions. The trial merge must
// come out clean, so the two file sets should not conflict.
func NewUnpushedMerge(t *testing.T, seedFiles, candFiles map[string]string) *UnpushedMerge {
	t.Helper()
	remote := NewRemote(t)
	remote.Seed("main", seedFiles)
	candRef := remote.PushCandidate("main", "alice", "feat", candFiles)

	gitDir := remote.BareClone()
	bare := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir=" + gitDir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("testutil: git --git-dir=%s %s: %v: %s", gitDir, strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	base := bare("rev-parse", "refs/heads/main")
	cand := bare("rev-parse", candRef)
	// merge-tree prints the tree OID on line 1; a conflict exits non-zero
	// here (bare fails the test), which is what the doc contract promises.
	tree := strings.SplitN(bare("merge-tree", "--write-tree", base, cand), "\n", 2)[0]
	merge := bare("-c", "user.name=Gauntlet", "-c", "user.email=gauntlet@ci.example",
		"commit-tree", "--no-gpg-sign", tree, "-p", base, "-p", cand, "-m", "trial merge")

	return &UnpushedMerge{
		Remote: remote, GitDir: gitDir,
		BaseSHA: base, CandSHA: cand, MergeSHA: merge,
	}
}

// GCPruneNow runs `git gc --prune=now` against gitDir — the most aggressive
// collection an operator can run, discarding the loose-object grace period
// entirely. This is the maintenance pass GC pins exist to survive.
func GCPruneNow(t *testing.T, gitDir string) {
	t.Helper()
	if out, err := exec.Command("git", "--git-dir="+gitDir, "gc", "--prune=now", "-q").CombinedOutput(); err != nil {
		t.Fatalf("testutil: gc --prune=now: %v: %s", err, out)
	}
}

// ObjectExists reports whether gitDir's object store can resolve oid.
func ObjectExists(gitDir, oid string) bool {
	return exec.Command("git", "--git-dir="+gitDir, "cat-file", "-e", oid).Run() == nil
}
