package gitx_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

// mtimeRepo is a hand-built working repo with CONTROLLED committer dates —
// the whole point of RestoreMtimes is per-commit times, so testutil's
// wall-clock commits (all within one second) can't drive these tests.
type mtimeRepo struct {
	t   *testing.T
	dir string
}

func newMtimeRepo(t *testing.T) *mtimeRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	r := &mtimeRepo{t: t, dir: dir}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	r.git("config", "user.name", "T")
	r.git("config", "user.email", "t@example.com")
	return r
}

func (r *mtimeRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitAt writes files (nil content = git rm) and commits at the given
// committer date, returning the commit SHA.
func (r *mtimeRepo) commitAt(at time.Time, msg string, files map[string]string) string {
	r.t.Helper()
	for path, content := range files {
		full := filepath.Join(r.dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			r.t.Fatalf("write: %v", err)
		}
	}
	r.git("add", "-A")
	cmd := exec.Command("git", "-C", r.dir, "commit", "-q", "--allow-empty", "-m", msg)
	date := at.UTC().Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git commit: %v: %s", err, out)
	}
	return r.git("rev-parse", "HEAD")
}

// open bare-clones the working repo and returns a gitx.Repo over the clone
// (the daemon's shape: RestoreMtimes runs against the bare repo's history).
func (r *mtimeRepo) open() (*gitx.Repo, string) {
	r.t.Helper()
	bare := filepath.Join(r.t.TempDir(), "bare.git")
	if out, err := exec.Command("git", "clone", "--bare", "-q", r.dir, bare).CombinedOutput(); err != nil {
		r.t.Fatalf("bare clone: %v: %s", err, out)
	}
	repo, err := gitx.New(context.Background(), bare, r.dir)
	if err != nil {
		r.t.Fatalf("gitx.New: %v", err)
	}
	return repo, bare
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.ModTime()
}

var (
	t1 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)
	t3 = time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)
)

func TestRestoreMtimes_HistoryDerivedAndDeterministic(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	w.commitAt(t1, "c1", map[string]string{"a.txt": "a1\n", "b.txt": "b1\n"})
	tip := w.commitAt(t2, "c2", map[string]string{"a.txt": "a2\n"})
	repo, _ := w.open()

	export := func() string {
		dir := t.TempDir()
		if err := repo.ExportTree(ctx, tip, dir); err != nil {
			t.Fatalf("ExportTree: %v", err)
		}
		stats, err := repo.RestoreMtimes(ctx, tip, dir)
		if err != nil {
			t.Fatalf("RestoreMtimes: %v", err)
		}
		if stats.Paths != 2 {
			t.Fatalf("stats.Paths = %d, want 2", stats.Paths)
		}
		return dir
	}

	d1 := export()
	if got := mtimeOf(t, filepath.Join(d1, "a.txt")); !got.Equal(t2) {
		t.Errorf("a.txt mtime = %v, want its last change %v", got, t2)
	}
	if got := mtimeOf(t, filepath.Join(d1, "b.txt")); !got.Equal(t1) {
		t.Errorf("b.txt mtime = %v, want its introducing commit %v (not the export wall clock)", got, t1)
	}

	// Same commit exported "later": metadata-identical.
	d2 := export()
	for _, f := range []string{"a.txt", "b.txt"} {
		if a, b := mtimeOf(t, filepath.Join(d1, f)), mtimeOf(t, filepath.Join(d2, f)); !a.Equal(b) {
			t.Errorf("%s differs across exports of the same commit: %v vs %v", f, a, b)
		}
	}
}

func TestRestoreMtimes_UnrelatedCommitLeavesPathAlone(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	w.commitAt(t1, "c1", map[string]string{"a.txt": "a1\n"})
	tip := w.commitAt(t2, "c2", map[string]string{"other.txt": "x\n"})
	repo, _ := w.open()

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, tip, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, tip, dir); err != nil {
		t.Fatalf("RestoreMtimes: %v", err)
	}
	if got := mtimeOf(t, filepath.Join(dir, "a.txt")); !got.Equal(t1) {
		t.Errorf("a.txt mtime = %v after an unrelated commit, want unchanged %v", got, t1)
	}
	if got := mtimeOf(t, filepath.Join(dir, "other.txt")); !got.Equal(t2) {
		t.Errorf("other.txt mtime = %v, want %v", got, t2)
	}
}

func TestRestoreMtimes_RenameIsAChangeAtTheNewPath(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	w.commitAt(t1, "c1", map[string]string{"old.txt": "same content, big enough to rename-detect\n"})
	w.git("mv", "old.txt", "new.txt")
	tip := w.commitAt(t2, "rename", nil)
	repo, _ := w.open()

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, tip, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, tip, dir); err != nil {
		t.Fatalf("RestoreMtimes: %v", err)
	}
	if got := mtimeOf(t, filepath.Join(dir, "new.txt")); !got.Equal(t2) {
		t.Errorf("new.txt mtime = %v, want the renaming commit's %v", got, t2)
	}
}

// TestRestoreMtimes_SyntheticMerge is the gauntlet case end-to-end: the
// trial merge commit created by MergeTree+CommitTree. Files inherited from
// either side keep their history times; auto-merged content (differing
// from BOTH parents) gets the merge's own time.
func TestRestoreMtimes_SyntheticMerge(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	shared := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	w.commitAt(t1, "base", map[string]string{"base.txt": "b\n", "shared.txt": shared})
	w.git("checkout", "-q", "-b", "cand")
	cand := w.commitAt(t2, "cand", map[string]string{
		"cand.txt":   "c\n",
		"shared.txt": strings.Replace(shared, "line1", "line1-cand", 1),
	})
	w.git("checkout", "-q", "main")
	base := w.commitAt(t3, "base2", map[string]string{
		"shared.txt": strings.Replace(shared, "line8", "line8-base", 1),
	})

	repo, bareDir := w.open()
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tm, err := repo.MergeTree(ctx, base, cand)
	if err != nil || !tm.Clean {
		t.Fatalf("MergeTree: clean=%v err=%v (the two shared.txt hunks must auto-merge)", tm.Clean, err)
	}
	merge, err := repo.CommitTree(ctx, tm.TreeOID, []string{base, cand}, "trial", core.Identity{Name: "G", Email: "g@x"})
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}
	mergeCT := commitTime(t, bareDir, merge)

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, merge, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, merge, dir); err != nil {
		t.Fatalf("RestoreMtimes: %v", err)
	}
	if got := mtimeOf(t, filepath.Join(dir, "base.txt")); !got.Equal(t1) {
		t.Errorf("base.txt = %v, want inherited %v", got, t1)
	}
	if got := mtimeOf(t, filepath.Join(dir, "cand.txt")); !got.Equal(t2) {
		t.Errorf("cand.txt = %v, want the candidate commit's %v", got, t2)
	}
	if got := mtimeOf(t, filepath.Join(dir, "shared.txt")); !got.Equal(mergeCT) {
		t.Errorf("shared.txt = %v, want the synthetic merge's own %v (auto-merged content differs from both parents)", got, mergeCT)
	}

	// Determinism across a retry of the SAME merge commit.
	dir2 := t.TempDir()
	if err := repo.ExportTree(ctx, merge, dir2); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, merge, dir2); err != nil {
		t.Fatalf("RestoreMtimes: %v", err)
	}
	for _, f := range []string{"base.txt", "cand.txt", "shared.txt"} {
		if a, b := mtimeOf(t, filepath.Join(dir, f)), mtimeOf(t, filepath.Join(dir2, f)); !a.Equal(b) {
			t.Errorf("%s differs across exports of the same merge: %v vs %v", f, a, b)
		}
	}
}

// TestRestoreMtimes_TreesameMergeInheritsCandidateTimes pins the MOST
// common trial-merge shape: the candidate is already based on the current
// tip, so the trial merge's tree is identical to the candidate's own tree.
// git log emits NO diff entry at all for a tree-identical parent — not an
// empty one — and a naive per-entry intersection then mistakes the merge
// for an ordinary commit and stamps every candidate path with the merge's
// own (wall-clock, per-trial) committer time: exactly the nondeterminism
// this feature exists to remove. The walker counts entries against %P's
// parent count instead, so the merge owns nothing and candidate paths
// keep the candidate commit's time across re-trials.
func TestRestoreMtimes_TreesameMergeInheritsCandidateTimes(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	base := w.commitAt(t1, "base", map[string]string{"base.txt": "b\n"})
	w.git("checkout", "-q", "-b", "cand")
	cand := w.commitAt(t2, "cand", map[string]string{"cand.txt": "c\n"})

	repo, _ := w.open()
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	tm, err := repo.MergeTree(ctx, base, cand)
	if err != nil || !tm.Clean {
		t.Fatalf("MergeTree: clean=%v err=%v", tm.Clean, err)
	}
	merge, err := repo.CommitTree(ctx, tm.TreeOID, []string{base, cand}, "trial", core.Identity{Name: "G", Email: "g@x"})
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, merge, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, merge, dir); err != nil {
		t.Fatalf("RestoreMtimes: %v", err)
	}
	if got := mtimeOf(t, filepath.Join(dir, "cand.txt")); !got.Equal(t2) {
		t.Errorf("cand.txt = %v, want the candidate commit's %v — a treesame trial merge must not own its paths", got, t2)
	}
	if got := mtimeOf(t, filepath.Join(dir, "base.txt")); !got.Equal(t1) {
		t.Errorf("base.txt = %v, want inherited %v", got, t1)
	}
}

// TestRestoreMtimes_ExportIgnoredPathsSkipped: the export (git archive)
// honors export-ignore attributes, so a tracked-but-unexported path must
// neither be stamped nor fail the pass — the pending set is what is on
// disk, never the raw tree listing.
func TestRestoreMtimes_ExportIgnoredPathsSkipped(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	tip := w.commitAt(t1, "c1", map[string]string{
		".gitattributes": "secret.txt export-ignore\n",
		"secret.txt":     "s\n",
		"a.txt":          "x\n",
	})
	repo, _ := w.open()

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, tip, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("precondition: the export should omit secret.txt (lstat err=%v)", err)
	}
	stats, err := repo.RestoreMtimes(ctx, tip, dir)
	if err != nil {
		t.Fatalf("RestoreMtimes on an export-ignore repo: %v", err)
	}
	if stats.Paths != 2 { // a.txt + .gitattributes — never the unexported path
		t.Errorf("stats.Paths = %d, want 2", stats.Paths)
	}
	if got := mtimeOf(t, filepath.Join(dir, "a.txt")); !got.Equal(t1) {
		t.Errorf("a.txt = %v, want %v", got, t1)
	}
}

// TestRestoreMtimes_ControlByteFilename: 0x01 — the header sentinel in the
// log format — is a legal filename byte, and -z emits paths raw. The
// NUL-token framing must treat it as data, in the tree and in history.
func TestRestoreMtimes_ControlByteFilename(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	w.commitAt(t1, "c1", map[string]string{"plain.txt": "p\n"})
	tip := w.commitAt(t2, "c2", map[string]string{"we\x01ird.txt": "w\n"})
	repo, _ := w.open()

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, tip, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, tip, dir); err != nil {
		t.Fatalf("RestoreMtimes with a 0x01 filename: %v", err)
	}
	if got := mtimeOf(t, filepath.Join(dir, "we\x01ird.txt")); !got.Equal(t2) {
		t.Errorf("0x01-named file = %v, want %v", got, t2)
	}
	if got := mtimeOf(t, filepath.Join(dir, "plain.txt")); !got.Equal(t1) {
		t.Errorf("plain.txt = %v, want %v", got, t1)
	}
}

func TestRestoreMtimes_SymlinkNotFollowed(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	w.commitAt(t1, "target", map[string]string{"target.txt": "x\n"})
	if err := os.Symlink("target.txt", filepath.Join(w.dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	tip := w.commitAt(t2, "link", nil)
	repo, _ := w.open()

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, tip, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, tip, dir); err != nil {
		t.Fatalf("RestoreMtimes: %v", err)
	}
	if got := mtimeOf(t, filepath.Join(dir, "link")); !got.Equal(t2) {
		t.Errorf("link lmtime = %v, want its committing time %v", got, t2)
	}
	if got := mtimeOf(t, filepath.Join(dir, "target.txt")); !got.Equal(t1) {
		t.Errorf("target.txt mtime = %v, want %v — stamping the link must not follow it", got, t1)
	}
}

func TestRestoreMtimes_FutureTimestampVerbatim(t *testing.T) {
	ctx := context.Background()
	w := newMtimeRepo(t)
	future := time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC)
	tip := w.commitAt(future, "from the future", map[string]string{"a.txt": "x\n"})
	repo, _ := w.open()

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, tip, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	if _, err := repo.RestoreMtimes(ctx, tip, dir); err != nil {
		t.Fatalf("RestoreMtimes: %v", err)
	}
	if got := mtimeOf(t, filepath.Join(dir, "a.txt")); !got.Equal(future) {
		t.Errorf("a.txt mtime = %v, want the future time %v verbatim (deterministic beats plausible)", got, future)
	}
}

func TestRestoreMtimes_CancellationFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := newMtimeRepo(t)
	tip := w.commitAt(t1, "c1", map[string]string{"a.txt": "x\n"})
	repo, _ := w.open()

	dir := t.TempDir()
	if err := repo.ExportTree(ctx, tip, dir); err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	cancel()
	if _, err := repo.RestoreMtimes(ctx, tip, dir); err == nil {
		t.Fatal("RestoreMtimes with a cancelled ctx = nil error, want a failure (never a silent wall-clock tree)")
	}
}

func commitTime(t *testing.T, gitDir, commit string) time.Time {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+gitDir, "log", "-1", "--format=%ct", commit).Output()
	if err != nil {
		t.Fatalf("log -1 %s: %v", commit, err)
	}
	ct, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("parse ct: %v", err)
	}
	return time.Unix(ct, 0)
}
