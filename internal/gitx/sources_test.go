package gitx_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

func TestSourcePrunePreviewAndGC(t *testing.T) {
	ctx := context.Background()
	repo, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "author", "change", map[string]string{"a": "change"})
	source := remote.Ref(ref)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	remote.SetRef("refs/pull/7/head", source)
	if err := repo.FetchReview(ctx, "refs/pull/7/head", "refs/gauntlet/reviews/github/7", source); err != nil {
		t.Fatal(err)
	}
	tree := revParse(t, dir, source+"^{tree}")
	landed, err := repo.LinearCommit(ctx, tree, base, source, "Normalized", core.Identity{Name: "Queue", Email: "queue@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CASUpdate(ctx, "refs/heads/main", base, landed); err != nil {
		t.Fatal(err)
	}
	remote.DeleteCandidate(ref)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	stamp := filepath.Join(dir, "source-retention", source)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	result, err := repo.PruneSources(ctx, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Expired) != 1 || result.Expired[0] != source {
		t.Fatalf("preview: %+v", result)
	}
	if got := revParse(t, dir, "refs/gauntlet/source/"+source); got != source {
		t.Fatal("preview removed source")
	}
	result, err = repo.PruneSources(ctx, cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Expired) != 1 {
		t.Fatalf("apply: %+v", result)
	}
	if err := exec.Command("git", "--git-dir", dir, "gc", "--prune=now").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "--git-dir", dir, "cat-file", "-e", source).Run(); err == nil {
		t.Fatal("expired source still retained after GC")
	}
	if got := revParse(t, dir, "refs/remotes/origin/main"); got != landed {
		t.Fatal("pruning changed target")
	}
	if remote.Ref("refs/pull/7/head") != source {
		t.Fatal("pruning changed remote review")
	}
	if got := catFile(t, dir, landed); got == "" {
		t.Fatal("tested commit lost")
	}
}

func TestSourcePruneRefreshAndLegacyAge(t *testing.T) {
	ctx := context.Background()
	repo, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "author", "change", map[string]string{"a": "change"})
	source := remote.Ref(ref)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	tree := revParse(t, dir, source+"^{tree}")
	who := core.Identity{Name: "Queue", Email: "queue@example.com"}
	if _, err := repo.LinearCommit(ctx, tree, base, source, "Normalized", who); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(dir, "source-retention", source)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LinearCommit(ctx, tree, base, source, "Retried", who); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	result, err := repo.PruneSources(ctx, cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Expired) != 0 {
		t.Fatal("recent reuse did not renew retention")
	}
	if err := os.Remove(stamp); err != nil {
		t.Fatal(err)
	}
	result, err = repo.PruneSources(ctx, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unaged) != 1 {
		t.Fatalf("legacy archive: %+v", result)
	}
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatal("preview wrote age metadata")
	}
	result, err = repo.PruneSources(ctx, cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Expired) != 0 || len(result.Unaged) != 1 {
		t.Fatal("legacy archive was immediately pruned")
	}
	if info, err := os.Stat(stamp); err != nil || !info.ModTime().After(cutoff) {
		t.Fatalf("legacy clock not initialized: %v", err)
	}
	orphan := filepath.Join(dir, "source-retention", strings.Repeat("a", 40))
	if err := os.WriteFile(orphan, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	result, err = repo.PruneSources(ctx, cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Expired) != 1 {
		t.Fatalf("orphan clock not expired: %+v", result)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan clock remains")
	}

}

func TestSourceLeasesProtectAgainstLivePruningAndGC(t *testing.T) {
	ctx := t.Context()
	repo, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	ref := remote.PushCandidate("main", "author", "change", map[string]string{"a": "change"})
	source := remote.Ref(ref)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := repo.RetainSources(ctx, source, source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.RetainSources(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	remote.DeleteCandidate(ref)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "source-retention", source), old, old); err != nil {
		t.Fatal(err)
	}
	prune := func(want int) {
		t.Helper()
		result, err := repo.PruneSources(ctx, time.Now().Add(-time.Hour), true)
		if err != nil || len(result.Expired) != want {
			t.Fatalf("prune=%+v, %v; want %d expired", result, err, want)
		}
		testutil.GCPruneNow(t, dir)
	}
	prune(0)
	first()
	first() // duplicate release must not consume the other lease
	prune(0)
	if err := exec.Command("git", "--git-dir", dir, "cat-file", "-e", source).Run(); err != nil {
		t.Fatal("live source collected", err)
	}
	second()
	prune(1)
	if err := exec.Command("git", "--git-dir", dir, "cat-file", "-e", source).Run(); err == nil {
		t.Fatal("released source still retained")
	}
}

func TestSourceLeaseFailureDoesNotLeakProtection(t *testing.T) {
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	ref := remote.PushCandidate("main", "author", "change", map[string]string{"a": "change"})
	source := remote.Ref(ref)
	if err := repo.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RetainSources(t.Context(), source, strings.Repeat("f", 40)); err == nil {
		t.Fatal("retained missing object")
	}
	result, err := repo.PruneSources(t.Context(), time.Now().Add(time.Hour), true)
	if err != nil || len(result.Expired) != 1 {
		t.Fatalf("partial acquisition leaked: %+v %v", result, err)
	}
}
