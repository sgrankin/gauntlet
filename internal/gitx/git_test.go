package gitx_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// newRepo returns a gitx.Repo constructed against a fresh bare clone of a
// fresh remote, plus the remote and the local bare dir (for tests that need
// to inspect objects gitx created but hasn't pushed anywhere).
func newRepo(t *testing.T) (*gitx.Repo, *testutil.Remote, string) {
	t.Helper()
	remote := testutil.NewRemote(t)
	dir := remote.BareClone()
	repo, err := gitx.New(context.Background(), dir, remote.Dir)
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	return repo, remote, dir
}

func TestNewInitsMissingDir(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})

	dir := filepath.Join(t.TempDir(), "does-not-exist-yet.git")
	repo, err := gitx.New(ctx, dir, remote.Dir)
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
	if _, ok := refs["refs/heads/main"]; !ok {
		t.Fatalf("expected refs/heads/main after fetch, got %v", refs)
	}
}

func TestMergeTreeClean(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "line1\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "new file\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, cand := refs["refs/heads/main"], refs[candRef]

	tm, err := repo.MergeTree(ctx, base, cand)
	if err != nil {
		t.Fatalf("MergeTree: %v", err)
	}
	if !tm.Clean {
		t.Fatalf("expected clean merge, conflicts=%v", tm.Conflicts)
	}
	if tm.TreeOID == "" {
		t.Fatalf("expected a tree OID")
	}
}

func TestMergeTreeConflict(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "line1\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"f.txt": "line1\nalice\n"})
	remote.DirectPush("main", map[string]string{"f.txt": "line1\nbob\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, cand := refs["refs/heads/main"], refs[candRef]

	tm, err := repo.MergeTree(ctx, base, cand)
	if err != nil {
		t.Fatalf("MergeTree: %v", err)
	}
	if tm.Clean {
		t.Fatalf("expected a conflict")
	}
	if len(tm.Conflicts) != 1 || tm.Conflicts[0] != "f.txt" {
		t.Fatalf("expected conflict on f.txt, got %v", tm.Conflicts)
	}
}

func TestMergeTreeInvalidRefIsError(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newRepo(t)
	if _, err := repo.MergeTree(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatalf("expected an error for nonexistent objects, not a conflict")
	}
}

func TestCommitTreeTwoParentsWithTrailers(t *testing.T) {
	ctx := context.Background()
	repo, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "line1\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "new\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, cand := refs["refs/heads/main"], refs[candRef]

	tm, err := repo.MergeTree(ctx, base, cand)
	if err != nil || !tm.Clean {
		t.Fatalf("MergeTree: tm=%+v err=%v", tm, err)
	}

	who := core.Identity{Name: "Gauntlet", Email: "gauntlet@ci.example"}
	msg := "Merge feat (alice)\n\nGauntlet-Ref: for/main/alice/feat\nGauntlet-Run: run-123\n"
	commit, err := repo.CommitTree(ctx, tm.TreeOID, []string{base, cand}, msg, who)
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}

	raw := catFile(t, dir, commit)
	if !strings.Contains(raw, "Gauntlet-Ref: for/main/alice/feat") {
		t.Fatalf("expected Gauntlet-Ref trailer in commit, got:\n%s", raw)
	}
	if !strings.Contains(raw, "Gauntlet-Run: run-123") {
		t.Fatalf("expected Gauntlet-Run trailer in commit, got:\n%s", raw)
	}
	if !strings.Contains(raw, "author Gauntlet <gauntlet@ci.example>") {
		t.Fatalf("expected author identity in commit, got:\n%s", raw)
	}

	if got := revParse(t, dir, commit+"^2"); got != cand {
		t.Fatalf("parent[1] = %s, want candidate SHA %s verbatim", got, cand)
	}
	if got := revParse(t, dir, commit+"^1"); got != base {
		t.Fatalf("parent[0] = %s, want base SHA %s", got, base)
	}
}

func TestReadFileFromTree(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{".gauntlet.kdl": "check \"test\" {}\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	base := remote.Ref("refs/heads/main")

	got, err := repo.ReadFileFromTree(ctx, base, ".gauntlet.kdl")
	if err != nil {
		t.Fatalf("ReadFileFromTree present: %v", err)
	}
	if string(got) != "check \"test\" {}\n" {
		t.Fatalf("content mismatch: %q", got)
	}

	if _, err := repo.ReadFileFromTree(ctx, base, "missing.kdl"); err == nil {
		t.Fatalf("expected an error for a missing path")
	}
}

func TestIsAncestor(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	base := remote.Ref("refs/heads/main")
	remote.DirectPush("main", map[string]string{"f.txt": "1\n2\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tip := remote.Ref("refs/heads/main")

	ok, err := repo.IsAncestor(ctx, base, tip)
	if err != nil || !ok {
		t.Fatalf("expected base to be an ancestor of tip: ok=%v err=%v", ok, err)
	}
	ok, err = repo.IsAncestor(ctx, tip, base)
	if err != nil || ok {
		t.Fatalf("expected tip NOT to be an ancestor of base: ok=%v err=%v", ok, err)
	}
}

func TestExportTree(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{
		"f.txt":     "hello\n",
		"sub/g.txt": "world\n",
	})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tip := remote.Ref("refs/heads/main")

	exportDir := filepath.Join(t.TempDir(), "export")
	if err := repo.ExportTree(ctx, tip, exportDir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(exportDir, "f.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("f.txt: got %q, err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(exportDir, "sub", "g.txt"))
	if err != nil || string(got) != "world\n" {
		t.Fatalf("sub/g.txt: got %q, err=%v", got, err)
	}
}

func TestCASUpdateSet(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	base := remote.Ref("refs/heads/main")

	tm, err := repo.MergeTree(ctx, base, base)
	if err != nil || !tm.Clean {
		t.Fatalf("MergeTree: tm=%+v err=%v", tm, err)
	}
	who := core.Identity{Name: "Gauntlet", Email: "gauntlet@ci.example"}
	commit, err := repo.CommitTree(ctx, tm.TreeOID, []string{base}, "test commit\n", who)
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}

	const zero = "0000000000000000000000000000000000000000"
	err = repo.CASUpdate(ctx, "refs/heads/main", zero, commit)
	if !errors.Is(err, core.ErrCASStale) {
		t.Fatalf("expected ErrCASStale for a wrong old-OID, got %v", err)
	}
	if got := remote.Ref("refs/heads/main"); got != base {
		t.Fatalf("target moved despite stale CAS: %s", got)
	}

	if err := repo.CASUpdate(ctx, "refs/heads/main", base, commit); err != nil {
		t.Fatalf("CASUpdate with correct old-OID: %v", err)
	}
	if got := remote.Ref("refs/heads/main"); got != commit {
		t.Fatalf("target = %s, want %s", got, commit)
	}
}

func TestCASUpdateDelete(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "new\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	candSHA := remote.Ref(candRef)

	const zero = "0000000000000000000000000000000000000000"
	err := repo.CASUpdate(ctx, candRef, zero, "")
	if !errors.Is(err, core.ErrCASStale) {
		t.Fatalf("expected ErrCASStale for a wrong old-OID delete, got %v", err)
	}
	if got := remote.Ref(candRef); got != candSHA {
		t.Fatalf("candidate ref moved/deleted despite stale CAS: %q", got)
	}

	if err := repo.CASUpdate(ctx, candRef, candSHA, ""); err != nil {
		t.Fatalf("CASUpdate delete with correct old-OID: %v", err)
	}
	if got := remote.Ref(candRef); got != "" {
		t.Fatalf("expected candidate ref deleted, still = %q", got)
	}
}

func TestFetchPrunesDeletedRefs(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "new\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if _, ok := refs[candRef]; !ok {
		t.Fatalf("expected candidate ref present before delete")
	}

	remote.DeleteCandidate(candRef)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err = repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if _, ok := refs[candRef]; ok {
		t.Fatalf("expected candidate ref pruned after delete, still present")
	}
}

// catFile and revParse inspect objects directly (bypassing the GitRepo
// interface, which has no generic object-inspection method) to verify what
// CommitTree actually produced.

func catFile(t *testing.T, gitDir, obj string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+gitDir, "cat-file", "-p", obj).CombinedOutput()
	if err != nil {
		t.Fatalf("cat-file %s: %v: %s", obj, err, out)
	}
	return string(out)
}

func revParse(t *testing.T, gitDir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+gitDir, "rev-parse", rev).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v: %s", rev, err, out)
	}
	return strings.TrimSpace(string(out))
}
