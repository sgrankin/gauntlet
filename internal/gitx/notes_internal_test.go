package gitx

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// notesTestRepo is this file's local equivalent of git_test.go's newRepo
// (unavailable here — that helper lives in package gitx_test, this file is
// white-box package gitx so it can reach the unexported publishNoteTestHook
// seam these two tests need for deterministic mid-call synchronization).
func notesTestRepo(t *testing.T) (*Repo, *testutil.Remote, string) {
	t.Helper()
	remote := testutil.NewRemote(t)
	dir := remote.BareClone()
	repo, err := New(context.Background(), dir, remote.Dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return repo, remote, dir
}

// rawPushNote attaches payload to sha directly on the bare repo at
// gitDir under ref (hash-object -w + notes add -C, the same byte-exact
// mechanics AddNote itself uses, invoked independently so a "second
// writer" push is a genuinely separate code path from PublishNote).
func rawPushNote(t *testing.T, gitDir, ref, sha string, payload []byte) {
	t.Helper()
	cmd := func(stdin *bytes.Reader, args ...string) string {
		full := append([]string{"--git-dir=" + gitDir}, args...)
		c := exec.Command("git", full...)
		if stdin != nil {
			c.Stdin = stdin
		}
		var out, errb bytes.Buffer
		c.Stdout, c.Stderr = &out, &errb
		if err := c.Run(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(full, " "), err, errb.String())
		}
		return out.String()
	}
	blob := strings.TrimSpace(cmd(bytes.NewReader(payload), "hash-object", "-w", "--stdin"))
	cmd(nil, "-c", "user.name=Second Writer", "-c", "user.email=second@example.com",
		"notes", "--ref="+ref, "add", "-C", blob, sha)
}

// TestPublishNoteDisjointAdvanceRetries drives issue #13's central
// contention scenario: a SECOND writer attaches a note for a DIFFERENT
// SHA to the same remote ref after our call's fetch has already
// completed, so our CAS goes stale on attempt 1. PublishNote must refetch
// and retry rather than treat this as a conflict (the brief's "a disjoint
// receipt advanced the ref; both must survive") — real git races at
// fetch-to-push granularity are too fast to land reliably from outside
// the package, so publishNoteTestHook (test-only, nil in production)
// lands the second writer's push deterministically inside the window.
func TestPublishNoteDisjointAdvanceRetries(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := notesTestRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	sha1 := remote.Ref("refs/heads/main")
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "new\n"})
	sha2 := remote.Ref(candRef)
	const ref = "refs/notes/gauntlet-receipts"

	pushed := false
	t.Cleanup(func() { publishNoteTestHook = nil })
	publishNoteTestHook = func(attempt int, tip string) {
		if attempt != 0 || pushed {
			return // only the first attempt races; later attempts must see it already merged
		}
		pushed = true
		rawPushNote(t, remote.Dir, ref, sha2, []byte("disjoint receipt for sha2"))
	}

	who := core.Identity{Name: "Gauntlet", Email: "gauntlet@ci.example"}
	res, err := repo.PublishNote(ctx, ref, sha1, []byte("our receipt for sha1"), who)
	if err != nil {
		t.Fatalf("PublishNote: %v", err)
	}
	if !res.Published {
		t.Errorf("Published = false, want true")
	}
	if !pushed {
		t.Fatalf("test hook never fired; the race wasn't exercised")
	}

	got1, err := exec.Command("git", "--git-dir="+remote.Dir, "notes", "--ref="+ref, "show", sha1).Output()
	if err != nil {
		t.Fatalf("show sha1 note: %v", err)
	}
	if string(got1) != "our receipt for sha1" {
		t.Errorf("sha1 note = %q, want %q", got1, "our receipt for sha1")
	}
	got2, err := exec.Command("git", "--git-dir="+remote.Dir, "notes", "--ref="+ref, "show", sha2).Output()
	if err != nil {
		t.Fatalf("show sha2 note: %v", err)
	}
	if string(got2) != "disjoint receipt for sha2" {
		t.Errorf("sha2 note = %q, want %q", got2, "disjoint receipt for sha2")
	}
}

// TestPublishNoteContentionExhaustion forces the remote to advance on
// EVERY attempt (a hostile/buggy peer racing us continuously), so
// PublishNote must give up after publishNoteMaxAttempts rather than loop
// forever or fall back to a force push. The remote must show only the
// racer's notes afterward — never a force-pushed note for our own SHA.
func TestPublishNoteContentionExhaustion(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := notesTestRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	sha := remote.Ref("refs/heads/main")
	const ref = "refs/notes/gauntlet-receipts"

	otherSHAs := make([]string, 0, publishNoteMaxAttempts)
	for i := 0; i < publishNoteMaxAttempts; i++ {
		r := remote.PushCandidate("main", "bob", "filler"+string(rune('a'+i)), map[string]string{
			"filler.txt": string(rune('a' + i)),
		})
		otherSHAs = append(otherSHAs, remote.Ref(r))
	}

	calls := 0
	t.Cleanup(func() { publishNoteTestHook = nil })
	publishNoteTestHook = func(attempt int, tip string) {
		rawPushNote(t, remote.Dir, ref, otherSHAs[calls%len(otherSHAs)], []byte("racer payload"))
		calls++
	}

	who := core.Identity{Name: "Gauntlet", Email: "gauntlet@ci.example"}
	_, err := repo.PublishNote(ctx, ref, sha, []byte("our receipt, should never land"), who)
	if err == nil {
		t.Fatalf("PublishNote under permanent contention: err = nil, want a bounded failure")
	}
	if calls != publishNoteMaxAttempts {
		t.Errorf("hook fired %d times, want exactly %d (one per attempt, no extra retries)", calls, publishNoteMaxAttempts)
	}

	// Our own SHA must never have gotten a note — no force push occurred.
	if out, err := exec.Command("git", "--git-dir="+remote.Dir, "notes", "--ref="+ref, "show", sha).CombinedOutput(); err == nil {
		t.Errorf("note exists for our SHA despite exhausted contention (force push?): %s", out)
	}
}
