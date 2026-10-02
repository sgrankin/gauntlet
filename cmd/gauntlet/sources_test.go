package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

func TestAutomaticSourcePrunerPreservesLiveLeaseThenExpires(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"a": "base"})
	ref := remote.PushCandidate("main", "author", "change", map[string]string{"a": "change"})
	source := remote.Ref(ref)
	dir := remote.BareClone()
	repo, err := gitx.New(t.Context(), dir, remote.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	release, err := repo.RetainSources(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	stamp := filepath.Join(dir, "source-retention", source)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time)
	close(ticks)
	runSourcePruner(t.Context(), repo, time.Hour, ticks)
	if _, err := os.Stat(stamp); err != nil {
		t.Fatal("automatic prune removed live source", err)
	}
	release()
	runSourcePruner(t.Context(), repo, time.Hour, ticks)
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatalf("automatic prune did not expire released source: %v", err)
	}
}
