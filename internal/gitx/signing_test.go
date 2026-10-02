package gitx_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

func signingKey(t *testing.T) (string, string) {
	t.Helper()
	key := filepath.Join(t.TempDir(), "signing-key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, []byte("bot@example.com "+string(pub)), 0600); err != nil {
		t.Fatal(err)
	}
	return key, allowed
}
func verifySignedCommit(t *testing.T, dir, allowed, sha string) {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+dir, "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", sha).CombinedOutput()
	if err != nil {
		t.Fatalf("signature invalid: %v %s", err, out)
	}
}

func TestSSHSigningFinalCommitBytes(t *testing.T) {
	key, allowed := signingKey(t)
	_, remote, dir := newRepo(t)
	repo, err := gitx.New(context.Background(), dir, remote.Dir, gitx.WithSSHSigning(key, 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	remote.Seed("main", map[string]string{"a": "base"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "change", map[string]string{"a": "new"})
	source := remote.Ref(ref)
	if err := repo.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	tree := revParse(t, dir, source+"^{tree}")
	author := "author Original Author <author@example.com> 1234567890 +0530"
	raw := fmt.Sprintf("tree %s\nparent %s\n%s\ncommitter Old <old@example.com> 1234567890 +0530\nchange-id mzvtrksxklwp\ngpgsig invalid-source-signature\n\nOriginal\n", tree, base, author)
	cmd := exec.Command("git", "--git-dir="+dir, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	source = strings.TrimSpace(string(out))
	message := "PR title\n\nPR description\n\nGauntlet-Source: " + source + "\n"
	who := core.Identity{Name: "Gauntlet", Email: "bot@example.com"}
	landed, err := repo.LinearCommit(context.Background(), tree, base, source, message, who)
	if err != nil {
		t.Fatal(err)
	}
	result := catFile(t, dir, landed)
	if !strings.Contains(result, author+"\n") || !strings.Contains(result, "change-id mzvtrksxklwp\n") || strings.Contains(result, "invalid-source-signature") || !strings.HasSuffix(result, message) {
		t.Fatalf("lost final metadata: %s", result)
	}
	verifySignedCommit(t, dir, allowed, landed)
	legacy, err := repo.CommitTree(context.Background(), tree, []string{base, source}, message, who)
	if err != nil {
		t.Fatal(err)
	}
	verifySignedCommit(t, dir, allowed, legacy)
	noteRef := "refs/notes/test-signing"
	if _, err := repo.AddNote(context.Background(), noteRef, landed, []byte("receipt"), who); err != nil {
		t.Fatal(err)
	}
	verifySignedCommit(t, dir, allowed, revParse(t, dir, noteRef))
}

func TestSSHSigningDoesNotFallBack(t *testing.T) {
	_, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	repo, err := gitx.New(context.Background(), dir, remote.Dir, gitx.WithSSHSigning(filepath.Join(t.TempDir(), "missing"), time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := remote.Ref("refs/heads/main")
	tree := revParse(t, dir, base+"^{tree}")
	sha, err := repo.CommitTree(context.Background(), tree, []string{base}, "test", core.Identity{Name: "Gauntlet", Email: "bot@example.com"})
	if err == nil || sha != "" {
		t.Fatalf("unsigned fallback: %s %v", sha, err)
	}
}

func TestSSHSigningTimeout(t *testing.T) {
	_, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh-keygen"), []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo, err := gitx.New(context.Background(), dir, remote.Dir, gitx.WithSSHSigning("unused", 20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := remote.Ref("refs/heads/main")
	tree := revParse(t, dir, base+"^{tree}")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sha, err := repo.CommitTree(ctx, tree, []string{base}, "test", core.Identity{Name: "Gauntlet", Email: "bot@example.com"})
	if sha != "" || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("signing deadline was not enforced: %s %v (outer context: %v)", sha, err, ctx.Err())
	}
}

func TestSSHSigningAgentPublicKey(t *testing.T) {
	key, allowed := signingKey(t)
	out, err := exec.Command("ssh-agent", "-s").Output()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok && (name == "SSH_AUTH_SOCK" || name == "SSH_AGENT_PID") {
			values[name], _, _ = strings.Cut(value, ";")
		}
	}
	pid, err := strconv.Atoi(values["SSH_AGENT_PID"])
	if err != nil {
		t.Fatalf("agent PID: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	t.Setenv("SSH_AUTH_SOCK", values["SSH_AUTH_SOCK"])
	if out, err := exec.Command("ssh-add", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v %s", err, out)
	}
	_, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"a": "base"})
	repo, err := gitx.New(context.Background(), dir, remote.Dir, gitx.WithSSHSigning(key+".pub", 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := remote.Ref("refs/heads/main")
	tree := revParse(t, dir, base+"^{tree}")
	sha, err := repo.CommitTree(context.Background(), tree, []string{base}, "test", core.Identity{Name: "Gauntlet", Email: "bot@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	verifySignedCommit(t, dir, allowed, sha)
}
