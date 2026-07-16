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

func TestLogReturnsCommitsOldestFirstWithBodies(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "1\n"})
	// PushCandidate's own commit message ("candidate feat") isn't
	// controllable, so add two more commits with known subjects/bodies
	// directly against a clone, bypassing testutil.
	commitOn(t, remote.Dir, candRef, map[string]string{"h.txt": "1\n"}, "Add h\n\nExplains why h exists.\nSecond body line.")
	commitOn(t, remote.Dir, candRef, map[string]string{"i.txt": "1\n"}, "Add i")
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, cand := refs["refs/heads/main"], refs[candRef]

	commits, err := repo.Log(ctx, base, cand)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(commits) != 3 {
		t.Fatalf("Log returned %d commits, want 3: %+v", len(commits), commits)
	}
	if commits[0].Subject != "candidate feat" {
		t.Errorf("commits[0].Subject = %q, want %q", commits[0].Subject, "candidate feat")
	}
	if commits[1].Subject != "Add h" {
		t.Errorf("commits[1].Subject = %q, want %q", commits[1].Subject, "Add h")
	}
	wantBody := "Explains why h exists.\nSecond body line."
	if commits[1].Body != wantBody {
		t.Errorf("commits[1].Body = %q, want %q", commits[1].Body, wantBody)
	}
	if commits[2].Subject != "Add i" {
		t.Errorf("commits[2].Subject = %q, want %q", commits[2].Subject, "Add i")
	}
	if commits[2].Body != "" {
		t.Errorf("commits[2].Body = %q, want empty", commits[2].Body)
	}
}

func TestLogEmptyRange(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tip := remote.Ref("refs/heads/main")

	commits, err := repo.Log(ctx, tip, tip)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("Log(tip, tip) = %+v, want empty", commits)
	}
}

func TestDiffStat(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "new file\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, cand := refs["refs/heads/main"], refs[candRef]

	stat, err := repo.DiffStat(ctx, base, cand)
	if err != nil {
		t.Fatalf("DiffStat: %v", err)
	}
	if !strings.Contains(stat, "g.txt") {
		t.Errorf("DiffStat = %q, want it to mention g.txt", stat)
	}
	if !strings.Contains(stat, "1 file changed") {
		t.Errorf("DiffStat = %q, want a changed-file summary line", stat)
	}
}

// commitOn adds one more commit with an arbitrary message onto ref's
// current tip and force-pushes it back, bypassing testutil.Remote (whose
// helpers hardcode their own commit messages) so Log's subject/body
// parsing can be exercised against known text.
func commitOn(t *testing.T, remoteDir, ref string, files map[string]string, message string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "clone", "-q", remoteDir, dir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	run("fetch", "-q", "origin", ref+":refs/heads/commit-on-work")
	run("checkout", "-q", "commit-on-work")
	run("config", "user.name", "Test Author")
	run("config", "user.email", "author@example.com")
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", message)
	run("push", "-q", "origin", "HEAD:"+ref)
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

// land drives repo's own MergeTree/CommitTree/CASUpdate exactly as the
// daemon's real land path does, producing a --no-ff merge of cand onto base
// and CAS-pushing it to ref — the real-git shape FindLandingMerge searches
// for.
func land(t *testing.T, ctx context.Context, repo *gitx.Repo, ref, base, cand string) string {
	t.Helper()
	tm, err := repo.MergeTree(ctx, base, cand)
	if err != nil || !tm.Clean {
		t.Fatalf("land: MergeTree(%s, %s): tm=%+v err=%v", base, cand, tm, err)
	}
	who := core.Identity{Name: "Gauntlet", Email: "gauntlet@ci.example"}
	commit, err := repo.CommitTree(ctx, tm.TreeOID, []string{base, cand}, "land\n", who)
	if err != nil {
		t.Fatalf("land: CommitTree: %v", err)
	}
	if err := repo.CASUpdate(ctx, ref, base, commit); err != nil {
		t.Fatalf("land: CASUpdate: %v", err)
	}
	return commit
}

func TestFindLandingMergeFindsEachCandidatesOwnMerge(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	aliceRef := remote.PushCandidate("main", "alice", "feat-a", map[string]string{"a.txt": "a\n"})
	bobRef := remote.PushCandidate("main", "bob", "feat-b", map[string]string{"b.txt": "b\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, aliceSHA, bobSHA := refs["refs/heads/main"], refs[aliceRef], refs[bobRef]

	aliceMerge := land(t, ctx, repo, "refs/heads/main", base, aliceSHA)
	bobMerge := land(t, ctx, repo, "refs/heads/main", aliceMerge, bobSHA)
	tip := bobMerge

	// bobMerge is branchTip itself: a direct parent-2 match, no walk needed.
	got, err := repo.FindLandingMerge(ctx, tip, bobSHA)
	if err != nil || got != bobMerge {
		t.Fatalf("FindLandingMerge(tip, bobSHA) = %q, %v, want %q, nil", got, err, bobMerge)
	}

	// aliceMerge is NOT branchTip — the walk must step past bob's link to
	// find it (this is the shape TestBatchCrashRecovery's non-head members
	// need: their own merge is behind the current tip).
	got, err = repo.FindLandingMerge(ctx, tip, aliceSHA)
	if err != nil || got != aliceMerge {
		t.Fatalf("FindLandingMerge(tip, aliceSHA) = %q, %v, want %q, nil", got, err, aliceMerge)
	}
}

func TestFindLandingMergeUnlandedReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	aliceRef := remote.PushCandidate("main", "alice", "feat-a", map[string]string{"a.txt": "a\n"})
	carolRef := remote.PushCandidate("main", "carol", "feat-c", map[string]string{"c.txt": "c\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, aliceSHA, carolSHA := refs["refs/heads/main"], refs[aliceRef], refs[carolRef]

	tip := land(t, ctx, repo, "refs/heads/main", base, aliceSHA)

	got, err := repo.FindLandingMerge(ctx, tip, carolSHA)
	if err != nil || got != "" {
		t.Fatalf("FindLandingMerge(tip, unlanded carolSHA) = %q, %v, want \"\", nil", got, err)
	}
}

// unpushedMerge builds exactly the object the daemon's runs live on — a
// synthetic merge commit no branch references (the shared testutil
// fixture) — plus a gitx.Repo over that same bare repo for the pin calls
// under test.
func unpushedMerge(t *testing.T) (*testutil.UnpushedMerge, *gitx.Repo) {
	t.Helper()
	um := testutil.NewUnpushedMerge(t,
		map[string]string{"f.txt": "line1\n"},
		map[string]string{"g.txt": "new\n"})
	repo, err := gitx.New(context.Background(), um.GitDir, um.Remote.Dir)
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	return um, repo
}

func TestPinSurvivesGCPruneNow(t *testing.T) {
	ctx := context.Background()
	um, repo := unpushedMerge(t)

	if err := repo.Pin(ctx, um.MergeSHA); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := repo.Pin(ctx, um.MergeSHA); err != nil {
		t.Fatalf("Pin (repeat): %v", err)
	}
	testutil.GCPruneNow(t, um.GitDir)
	if !testutil.ObjectExists(um.GitDir, um.MergeSHA) {
		t.Fatalf("pinned merge %s pruned by gc --prune=now", um.MergeSHA)
	}
	// The parents (base, candidate) must survive through the pin too even
	// once their remote-tracking anchors vanish: reachability through the
	// pinned tip is the whole point of pinning only the chain tip.
	for _, oid := range []string{um.BaseSHA, um.CandSHA} {
		if !testutil.ObjectExists(um.GitDir, oid) {
			t.Fatalf("parent %s of pinned merge pruned", oid)
		}
	}
}

func TestUnpinReleasesObjectAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	um, repo := unpushedMerge(t)

	if err := repo.Pin(ctx, um.MergeSHA); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := repo.Unpin(ctx, um.MergeSHA); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	// Unpinning again — and unpinning an OID that was never pinned — must
	// be a no-op: terminal paths unpin unconditionally.
	if err := repo.Unpin(ctx, um.MergeSHA); err != nil {
		t.Fatalf("Unpin (repeat): %v", err)
	}
	if err := repo.Unpin(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err != nil {
		t.Fatalf("Unpin (never pinned): %v", err)
	}
	testutil.GCPruneNow(t, um.GitDir)
	if testutil.ObjectExists(um.GitDir, um.MergeSHA) {
		t.Fatalf("unpinned, unreferenced merge %s survived gc --prune=now", um.MergeSHA)
	}
}

func TestSweepPins(t *testing.T) {
	ctx := context.Background()
	um, repo := unpushedMerge(t)

	n, err := repo.SweepPins(ctx)
	if err != nil || n != 0 {
		t.Fatalf("SweepPins (empty) = %d, %v, want 0, nil", n, err)
	}
	if err := repo.Pin(ctx, um.MergeSHA); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	n, err = repo.SweepPins(ctx)
	if err != nil || n != 1 {
		t.Fatalf("SweepPins = %d, %v, want 1, nil", n, err)
	}
	n, err = repo.SweepPins(ctx)
	if err != nil || n != 0 {
		t.Fatalf("SweepPins (after sweep) = %d, %v, want 0, nil", n, err)
	}
}

func TestFetchPruneLeavesPinsAlone(t *testing.T) {
	ctx := context.Background()
	um, repo := unpushedMerge(t)

	if err := repo.Pin(ctx, um.MergeSHA); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	// fetch --prune prunes remote-tracking refs whose remote counterpart
	// vanished; the pin namespace must be untouchable by it.
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := revParse(t, um.GitDir, "refs/gauntlet/pin/"+um.MergeSHA); got != um.MergeSHA {
		t.Fatalf("pin ref = %s after fetch --prune, want %s", got, um.MergeSHA)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	for name := range refs {
		if strings.Contains(name, "gauntlet/pin") {
			t.Fatalf("pin ref leaked into ListRefs: %s", name)
		}
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
