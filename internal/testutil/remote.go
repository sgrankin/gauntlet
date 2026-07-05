// Package testutil provides a real-git test harness: a bare "remote"
// repository plus helpers that mimic what a human author or a second
// daemon does to it (push a candidate, move it, delete it, push directly
// to a target branch). Every helper takes a *testing.T and fails the test
// on error, keeping test bodies terse. No network is used; everything
// lives under t.TempDir().
package testutil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Remote is a local bare git repository standing in for the real remote
// gauntlet polls.
type Remote struct {
	t   *testing.T
	Dir string // bare repo path

	work string // lazily-created author working clone, reused across calls
}

// NewRemote creates an empty bare repository under t.TempDir().
func NewRemote(t *testing.T) *Remote {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if err := runPlain("init", "--bare", "-q", dir); err != nil {
		t.Fatalf("testutil: %v", err)
	}
	return &Remote{t: t, Dir: dir}
}

// BareClone creates a fresh bare clone of the remote under t.TempDir() and
// returns its path — the dir a daemon's gitx.Repo is constructed against.
func (r *Remote) BareClone() string {
	r.t.Helper()
	dir := filepath.Join(r.t.TempDir(), "gauntlet-local.git")
	if err := runPlain("clone", "--bare", "-q", r.Dir, dir); err != nil {
		r.t.Fatalf("testutil: %v", err)
	}
	return dir
}

// Seed commits files as the (possibly first) commit on branch and pushes
// it. If branch does not yet exist on the remote, it is created as an
// orphan; otherwise files are added on top of the branch's current tip.
func (r *Remote) Seed(branch string, files map[string]string) {
	r.t.Helper()
	dir := r.workDir()
	gitC(r.t, dir, "fetch", "-q", "origin")
	remoteRef := "refs/remotes/origin/" + branch
	if _, err := runQuiet(dir, "rev-parse", "--verify", "--quiet", remoteRef); err == nil {
		gitC(r.t, dir, "checkout", "-q", "-B", branch, remoteRef)
	} else {
		gitC(r.t, dir, "checkout", "-q", "--orphan", branch)
	}
	clearWorkTree(r.t, dir)
	writeFiles(r.t, dir, files)
	gitC(r.t, dir, "add", "-A")
	gitC(r.t, dir, "commit", "-q", "-m", "seed "+branch)
	gitC(r.t, dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
}

// PushCandidate commits files on top of target's current tip and pushes
// them to a new candidate ref, per the ref grammar
// refs/heads/for/<target>/<user>/<topic> (or .../<target>/<topic> if user
// is ""). It returns the ref name.
func (r *Remote) PushCandidate(target, user, topic string, files map[string]string) string {
	r.t.Helper()
	ref := candidateRef(target, user, topic)
	dir := r.workDir()
	gitC(r.t, dir, "fetch", "-q", "origin")
	gitC(r.t, dir, "checkout", "-q", "-B", "candidate-work", "refs/remotes/origin/"+target)
	writeFiles(r.t, dir, files)
	gitC(r.t, dir, "add", "-A")
	gitC(r.t, dir, "commit", "-q", "-m", "candidate "+topic)
	gitC(r.t, dir, "push", "-q", "-f", "origin", "HEAD:"+ref)
	return ref
}

func candidateRef(target, user, topic string) string {
	if user == "" {
		return fmt.Sprintf("refs/heads/for/%s/%s", target, topic)
	}
	return fmt.Sprintf("refs/heads/for/%s/%s/%s", target, user, topic)
}

// MoveCandidate commits files on top of ref's current content and
// force-pushes the result back to ref, simulating an author re-push
// (a new SHA at the same ref name). It returns the new SHA.
func (r *Remote) MoveCandidate(ref string, files map[string]string) string {
	r.t.Helper()
	dir := r.workDir()
	gitC(r.t, dir, "fetch", "-q", "origin", ref+":refs/candidate-tmp")
	gitC(r.t, dir, "checkout", "-q", "-B", "candidate-work", "refs/candidate-tmp")
	writeFiles(r.t, dir, files)
	gitC(r.t, dir, "add", "-A")
	gitC(r.t, dir, "commit", "-q", "-m", "update candidate")
	gitC(r.t, dir, "push", "-q", "-f", "origin", "HEAD:"+ref)
	return r.Ref(ref)
}

// DeleteCandidate deletes ref on the remote, simulating an author
// cancelling their submission.
func (r *Remote) DeleteCandidate(ref string) {
	r.t.Helper()
	gitC(r.t, r.workDir(), "push", "-q", "origin", ":"+ref)
}

// DirectPush commits files on top of branch's current tip and pushes them
// directly, simulating a human commit or a second daemon instance bypassing
// gauntlet.
func (r *Remote) DirectPush(branch string, files map[string]string) {
	r.t.Helper()
	dir := r.workDir()
	gitC(r.t, dir, "fetch", "-q", "origin")
	gitC(r.t, dir, "checkout", "-q", "-B", branch, "refs/remotes/origin/"+branch)
	writeFiles(r.t, dir, files)
	gitC(r.t, dir, "add", "-A")
	gitC(r.t, dir, "commit", "-q", "-m", "direct push to "+branch)
	gitC(r.t, dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
}

// Ref returns ref's current OID on the remote, or "" if it does not exist.
func (r *Remote) Ref(ref string) string {
	r.t.Helper()
	cmd := exec.Command("git", "-C", r.Dir, "rev-parse", "--verify", "--quiet", ref)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

func (r *Remote) workDir() string {
	r.t.Helper()
	if r.work == "" {
		dir := filepath.Join(r.t.TempDir(), "author-work")
		if err := runPlain("clone", "-q", r.Dir, dir); err != nil {
			r.t.Fatalf("testutil: %v", err)
		}
		gitC(r.t, dir, "config", "user.name", "Test Author")
		gitC(r.t, dir, "config", "user.email", "author@example.com")
		r.work = dir
	}
	return r.work
}

func clearWorkTree(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("testutil: read work dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatalf("testutil: clear work dir: %v", err)
		}
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("testutil: mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("testutil: write %s: %v", path, err)
		}
	}
}

// gitC runs git -C dir <args...>, failing the test on error.
func gitC(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("testutil: git -C %s %s: %v: %s", dir, strings.Join(args, " "), err, errb.String())
	}
	return out.String()
}

// runQuiet runs git -C dir <args...> without failing the test, for
// existence checks.
func runQuiet(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// runPlain runs git <args...> with no cwd change, for operations (like
// init and clone) whose target directory may not exist yet.
func runPlain(args ...string) error {
	cmd := exec.Command("git", args...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return nil
}
