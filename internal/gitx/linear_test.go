package gitx_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestLinearCommitPreservesAuthorAndJJIdentity(t *testing.T) {
	ctx := context.Background()
	repo, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "change", map[string]string{"a": "new"})
	sha := remote.Ref(ref)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	tree := revParse(t, dir, sha+"^{tree}")
	author := "author Original Author <author@example.com> 1234567890 +0530"
	id := "change-id mzvtrksxklwp"
	raw := fmt.Sprintf("tree %s\nparent %s\n%s\ncommitter Old <old@example.com> 1234567890 +0530\n%s\ngpgsig invalid-old-signature\n\nOriginal\n", tree, base, author, id)
	cmd := exec.Command("git", "--git-dir", dir, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	source := strings.TrimSpace(string(out))
	message := "PR subject\n\nPR description\n\nGauntlet-Ref: " + ref + "\nGauntlet-Source: " + source + "\nGauntlet-Version: tested-version\n"
	landed, err := repo.LinearCommit(ctx, tree, base, source, message, core.Identity{Name: "Gauntlet", Email: "bot@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	result := catFile(t, dir, landed)
	if !strings.Contains(result, author+"\n") || !strings.Contains(result, id+"\n") || strings.Contains(result, "gpgsig") || strings.Count(result, "\nparent ") != 1 || !strings.HasSuffix(result, message) {
		t.Fatalf("incorrect linear object:\n%s", result)
	}
	if catFile(t, dir, source) != raw {
		t.Fatal("original object changed")
	}
	if got := revParse(t, dir, "refs/gauntlet/source/"+source); got != source {
		t.Fatalf("original not retained for delayed hooks: %s", got)
	}
	got, err := repo.FindLanding(ctx, landed, ref, source, "tested-version")
	if err != nil || got != landed {
		t.Fatalf("landing not recovered: %s %v", got, err)
	}
	for _, q := range []struct{ ref, sha, version string }{{ref, source, "changed"}, {ref + "-other", source, "tested-version"}, {ref, base, "tested-version"}} {
		got, err := repo.FindLanding(ctx, landed, q.ref, q.sha, q.version)
		if err != nil || got != "" {
			t.Fatalf("incorrect ledger match: %s %v", got, err)
		}
	}
}
