package gitx_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

func TestWorkerObjectTransportPreservesExactTreeAndHistory(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"base": "base"})
	base := remote.Ref("refs/heads/main")
	source, err := gitx.New(t.Context(), remote.BareClone(), remote.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	worker, err := gitx.OpenObjects(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	var pack bytes.Buffer
	if err = source.WritePack(t.Context(), &pack, []string{base}, nil); err != nil {
		t.Fatal(err)
	}
	if err = worker.ImportPack(t.Context(), &pack); err != nil {
		t.Fatal(err)
	}
	ref := remote.PushCandidate("main", "alice", "fix", map[string]string{"added": "new"})
	head := remote.Ref(ref)
	if err = source.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = source.WritePack(t.Context(), &pack, []string{head}, []string{base}); err != nil {
		t.Fatal(err)
	}
	if err = worker.ImportPack(t.Context(), &pack); err != nil {
		t.Fatal(err)
	}
	if yes, err := worker.IsAncestor(t.Context(), base, head); err != nil || !yes {
		t.Fatalf("history missing: %v %v", yes, err)
	}
	tree, err := worker.ObjectTree(t.Context(), head)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := worker.ReadFileFromTree(t.Context(), tree, "added"); err != nil || string(data) != "new" {
		t.Fatalf("wrong source: %q %v", data, err)
	}
	release, err := worker.RetainSources(t.Context(), head)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err = source.WritePack(t.Context(), &pack, []string{"--all"}, nil); err == nil {
		t.Fatal("accepted revision expression from worker")
	}
}
