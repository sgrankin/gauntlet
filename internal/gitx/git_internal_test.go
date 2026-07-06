package gitx

import (
	"context"
	"fmt"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// TestFindLandingMergeRespectsBound is a white-box test (package gitx, not
// gitx_test) exercising the unexported maxCount parameter directly: building
// maxLandingMergeSearch's real bound (1000 merge commits) worth of history
// just to prove the walk stops would make this test itself the slow part of
// the suite, so the bound is parameterized here instead and exercised at a
// small depth.
func TestFindLandingMergeRespectsBound(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	dir := remote.BareClone()
	repo, err := New(ctx, dir, remote.Dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	oldCandRef := remote.PushCandidate("main", "alice", "old", map[string]string{"a.txt": "a\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base, oldCandSHA := refs["refs/heads/main"], refs[oldCandRef]

	tip := landFor(t, ctx, repo, "refs/heads/main", base, oldCandSHA)

	// Stack two more landings on top so oldCand's own merge sits 3
	// first-parent merge-commit steps behind the tip.
	for i := 0; i < 2; i++ {
		fillerRef := remote.PushCandidate("main", "bob", fmt.Sprintf("filler%d", i), map[string]string{fmt.Sprintf("b%d.txt", i): "b\n"})
		if err := repo.Fetch(ctx); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		refs, err := repo.ListRefs(ctx)
		if err != nil {
			t.Fatalf("ListRefs: %v", err)
		}
		tip = landFor(t, ctx, repo, "refs/heads/main", tip, refs[fillerRef])
	}

	got, err := repo.findLandingMerge(ctx, tip, oldCandSHA, 1)
	if err != nil || got != "" {
		t.Fatalf("findLandingMerge(maxCount=1) = %q, %v, want \"\", nil (bound exceeded before reaching oldCand's merge)", got, err)
	}

	got, err = repo.findLandingMerge(ctx, tip, oldCandSHA, 10)
	if err != nil || got == "" {
		t.Fatalf("findLandingMerge(maxCount=10) = %q, %v, want oldCand's landing merge, nil", got, err)
	}
}

// landFor drives repo's own MergeTree/CommitTree/CASUpdate exactly as the
// daemon's real land path does, producing a --no-ff merge of cand onto base
// and CAS-pushing it to ref.
func landFor(t *testing.T, ctx context.Context, repo *Repo, ref, base, cand string) string {
	t.Helper()
	tm, err := repo.MergeTree(ctx, base, cand)
	if err != nil || !tm.Clean {
		t.Fatalf("landFor: MergeTree(%s, %s): tm=%+v err=%v", base, cand, tm, err)
	}
	who := core.Identity{Name: "Gauntlet", Email: "gauntlet@ci.example"}
	commit, err := repo.CommitTree(ctx, tm.TreeOID, []string{base, cand}, "land\n", who)
	if err != nil {
		t.Fatalf("landFor: CommitTree: %v", err)
	}
	if err := repo.CASUpdate(ctx, ref, base, commit); err != nil {
		t.Fatalf("landFor: CASUpdate: %v", err)
	}
	return commit
}
