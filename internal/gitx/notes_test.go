package gitx_test

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

const testNotesRef = "refs/notes/gauntlet-receipts"

var testWho = core.Identity{Name: "Gauntlet", Email: "gauntlet@ci.example"}

// rawGit runs git --git-dir=gitDir <args...> directly against a bare repo
// (bypassing gitx entirely), failing the test on error. Used to simulate
// what a stock `git notes` user, or a second daemon instance, would do
// straight against the remote — no working copy needed, notes/refs
// plumbing works fine against a bare repo.
func rawGit(t *testing.T, gitDir string, stdin *bytes.Reader, args ...string) string {
	t.Helper()
	full := append([]string{"--git-dir=" + gitDir}, args...)
	cmd := exec.Command("git", full...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(full, " "), err, errb.String())
	}
	return out.String()
}

func TestPublishNoteFreshPublish(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	sha := remote.Ref("refs/heads/main")

	if got := remote.Ref(testNotesRef); got != "" {
		t.Fatalf("precondition: notes ref already exists: %s", got)
	}

	res, err := repo.PublishNote(ctx, testNotesRef, sha, []byte("receipt payload"), testWho)
	if err != nil {
		t.Fatalf("PublishNote: %v", err)
	}
	if !res.Published {
		t.Errorf("Published = false, want true (fresh publish)")
	}
	if res.NoteBlobSHA == "" {
		t.Errorf("NoteBlobSHA empty")
	}

	tip := remote.Ref(testNotesRef)
	if tip == "" {
		t.Fatalf("remote notes ref does not exist after publish")
	}

	got := rawGit(t, remote.Dir, nil, "notes", "--ref="+testNotesRef, "show", sha)
	if got != "receipt payload" {
		t.Errorf("remote note content = %q, want %q", got, "receipt payload")
	}
}

func TestPublishNoteByteExactRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"no trailing newline", []byte("no trailing newline here")},
		{"with trailing newline", []byte("has a trailing newline\n")},
		{"binary bytes", []byte{0x01, 0xff, 0x00, 'b', 'i', 'n', 0x0a, 0xfe}},
	}
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	base := remote.Ref("refs/heads/main")

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A distinct commit (via a distinct file) per case, so each
			// gets its own note-able SHA.
			candRef := remote.PushCandidate("main", "alice", "topic"+string(rune('a'+i)), map[string]string{
				"g.txt": tc.name,
			})
			if err := repo.Fetch(ctx); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			sha := remote.Ref(candRef)
			if sha == "" || sha == base {
				t.Fatalf("precondition: candidate SHA not resolved: %q", sha)
			}

			res, err := repo.PublishNote(ctx, testNotesRef, sha, tc.payload, testWho)
			if err != nil {
				t.Fatalf("PublishNote: %v", err)
			}
			if !res.Published {
				t.Errorf("Published = false, want true")
			}

			tip, err := repo.FetchNotesRef(ctx, testNotesRef)
			if err != nil {
				t.Fatalf("FetchNotesRef: %v", err)
			}
			if tip == "" {
				t.Fatalf("expected a tip after publish")
			}
			localRef := gitx.NotesWorkRef(testNotesRef)
			got, exists, err := repo.ReadNote(ctx, localRef, sha)
			if err != nil {
				t.Fatalf("ReadNote: %v", err)
			}
			if !exists {
				t.Fatalf("ReadNote: exists = false, want true")
			}
			if !bytes.Equal(got, tc.payload) {
				t.Errorf("round-trip mismatch:\n got  %q\n want %q", got, tc.payload)
			}
		})
	}
}

func TestPublishNoteIdempotentRepublish(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	sha := remote.Ref("refs/heads/main")
	payload := []byte("same receipt")

	res1, err := repo.PublishNote(ctx, testNotesRef, sha, payload, testWho)
	if err != nil {
		t.Fatalf("PublishNote (first): %v", err)
	}
	if !res1.Published {
		t.Fatalf("first publish: Published = false, want true")
	}
	tipAfterFirst := remote.Ref(testNotesRef)

	res2, err := repo.PublishNote(ctx, testNotesRef, sha, payload, testWho)
	if err != nil {
		t.Fatalf("PublishNote (repeat): %v", err)
	}
	if res2.Published {
		t.Errorf("repeat publish: Published = true, want false (AlreadyPublished)")
	}
	if res2.NoteBlobSHA != res1.NoteBlobSHA {
		t.Errorf("repeat publish blob SHA = %s, want %s (same content)", res2.NoteBlobSHA, res1.NoteBlobSHA)
	}

	tipAfterSecond := remote.Ref(testNotesRef)
	if tipAfterSecond != tipAfterFirst {
		t.Errorf("remote tip changed on idempotent re-publish: %s -> %s (an empty commit was pushed)", tipAfterFirst, tipAfterSecond)
	}
}

func TestPublishNoteConflictFailsClosed(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	sha := remote.Ref("refs/heads/main")

	if _, err := repo.PublishNote(ctx, testNotesRef, sha, []byte("payload A"), testWho); err != nil {
		t.Fatalf("PublishNote (A): %v", err)
	}
	tipAfterA := remote.Ref(testNotesRef)

	_, err := repo.PublishNote(ctx, testNotesRef, sha, []byte("payload B (different)"), testWho)
	if !errors.Is(err, core.ErrNoteConflict) {
		t.Fatalf("PublishNote (B, conflicting): err = %v, want ErrNoteConflict", err)
	}

	tipAfterB := remote.Ref(testNotesRef)
	if tipAfterB != tipAfterA {
		t.Errorf("remote notes ref moved on a conflicting publish: %s -> %s, want untouched", tipAfterA, tipAfterB)
	}
	got := rawGit(t, remote.Dir, nil, "notes", "--ref="+testNotesRef, "show", sha)
	if got != "payload A" {
		t.Errorf("remote note content after rejected conflict = %q, want original %q", got, "payload A")
	}
}

func TestFetchNotesRefNoRefVsTransportFailure(t *testing.T) {
	ctx := context.Background()

	// No ref: a real, reachable remote that simply has never had this
	// notes ref published.
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	tip, err := repo.FetchNotesRef(ctx, testNotesRef)
	if err != nil {
		t.Fatalf("FetchNotesRef (no ref): unexpected error %v", err)
	}
	if tip != "" {
		t.Fatalf("FetchNotesRef (no ref): tip = %q, want \"\"", tip)
	}

	// Transport failure: the remote itself does not exist.
	badRepo, err := gitx.New(ctx, t.TempDir()+"/local.git", "/nonexistent/path/to/nowhere.git")
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	tip, err = badRepo.FetchNotesRef(ctx, testNotesRef)
	if err == nil {
		t.Fatalf("FetchNotesRef against a nonexistent remote: err = nil, want a transport error")
	}
	if tip != "" {
		t.Errorf("FetchNotesRef on transport failure: tip = %q, want \"\"", tip)
	}
}

// TestPublishNoteFanoutInterop proves interop both directions with stock
// `git notes` porcelain (issue #13's requirement): a remote notes ref
// already carrying a note written entirely by stock `git notes add -m`
// (not through gitx) is correctly read and appended to via PublishNote,
// and a THIRD, independent clone using stock `git notes show` can read
// back byte-exact what PublishNote wrote.
func TestPublishNoteFanoutInterop(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	sha1 := remote.Ref("refs/heads/main")
	candRef := remote.PushCandidate("main", "alice", "feat", map[string]string{"g.txt": "new\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	sha2 := remote.Ref(candRef)

	// Stock `git notes add -m` writes directly to the remote (bare repo),
	// simulating a human or another tool, not gauntlet.
	rawGit(t, remote.Dir, nil, "-c", "user.name=Human", "-c", "user.email=human@example.com",
		"notes", "--ref="+testNotesRef, "add", "-m", "stock note for sha1", sha1)
	stockTip := remote.Ref(testNotesRef)
	if stockTip == "" {
		t.Fatalf("precondition: stock note push failed")
	}

	// PublishNote reads through the pre-existing stock history and
	// appends its own note for a different SHA.
	res, err := repo.PublishNote(ctx, testNotesRef, sha2, []byte("gauntlet receipt, no trailing nl"), testWho)
	if err != nil {
		t.Fatalf("PublishNote: %v", err)
	}
	if !res.Published {
		t.Errorf("Published = false, want true")
	}

	// The original stock note must survive untouched.
	got1 := rawGit(t, remote.Dir, nil, "notes", "--ref="+testNotesRef, "show", sha1)
	if got1 != "stock note for sha1\n" {
		t.Errorf("stock note for sha1 = %q, want %q (stock -m appends a newline)", got1, "stock note for sha1\n")
	}

	// A third, wholly independent clone using stock git notes can read
	// what PublishNote wrote, byte-exact.
	thirdDir := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote.Dir, thirdDir).CombinedOutput(); err != nil {
		t.Fatalf("clone third: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", thirdDir, "fetch", "-q", "origin",
		testNotesRef+":"+testNotesRef).CombinedOutput(); err != nil {
		t.Fatalf("fetch notes ref in third clone: %v: %s", err, out)
	}
	out, err := exec.Command("git", "-C", thirdDir, "notes", "--ref="+testNotesRef, "show", sha2).Output()
	if err != nil {
		t.Fatalf("stock notes show sha2 in third clone: %v", err)
	}
	if string(out) != "gauntlet receipt, no trailing nl" {
		t.Errorf("third clone stock read = %q, want %q", string(out), "gauntlet receipt, no trailing nl")
	}
}

// TestPublishNoteBlobSHAAgreesWithTreeLookup verifies PublishNote's
// returned NoteBlobSHA (computed via hash-object, independent of the
// notes tree's fan-out layout) names the exact same object git's own
// notes tree stores the payload under — resolved here via ls-tree/rev-
// parse against the local work ref's tree, not by assuming a flat vs.
// fanned path layout.
func TestPublishNoteBlobSHAAgreesWithTreeLookup(t *testing.T) {
	ctx := context.Background()
	repo, remote, dir := newRepo(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	sha := remote.Ref("refs/heads/main")

	res, err := repo.PublishNote(ctx, testNotesRef, sha, []byte("payload for tree lookup"), testWho)
	if err != nil {
		t.Fatalf("PublishNote: %v", err)
	}

	localRef := gitx.NotesWorkRef(testNotesRef)
	out := rawGit(t, dir, nil, "ls-tree", "-r", localRef)
	found := ""
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		// "<mode> blob <sha>\t<path>"
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		path := line[strings.Index(line, "\t")+1:]
		if strings.HasSuffix(path, sha) {
			found = fields[2]
			break
		}
	}
	if found == "" {
		t.Fatalf("no notes-tree entry found for sha %s in ls-tree output:\n%s", sha, out)
	}
	if found != res.NoteBlobSHA {
		t.Errorf("tree-resolved blob = %s, PublishNote returned %s, want equal", found, res.NoteBlobSHA)
	}
}

// TestPublishNoteAuthedTransport proves PublishNote rides the same
// authenticated transport as every other gitx remote operation (fetch,
// CASUpdate) — reusing auth_test.go's smart-HTTP-plus-basic-auth harness
// in this same package.
func TestPublishNoteAuthedTransport(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	ft := &fakeTokens{seq: []string{"ghs_FAKENOTES"}}
	srv := newAuthedRemote(t, upstream, 0, func() string { return "ghs_FAKENOTES" })

	local := t.TempDir() + "/local.git"
	repo, err := gitx.New(ctx, local, srv.URL+"/upstream.git",
		gitx.WithTokenSource(ft, strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	sha := refs["refs/heads/main"]
	if sha == "" {
		t.Fatalf("refs/heads/main missing: %v", refs)
	}

	res, err := repo.PublishNote(ctx, testNotesRef, sha, []byte("authed receipt"), testWho)
	if err != nil {
		t.Fatalf("PublishNote through authed transport: %v", err)
	}
	if !res.Published {
		t.Errorf("Published = false, want true")
	}

	out, err := exec.Command("git", "-C", upstream, "notes", "--ref="+testNotesRef, "show", sha).Output()
	if err != nil {
		t.Fatalf("read note on upstream: %v", err)
	}
	if string(out) != "authed receipt" {
		t.Errorf("upstream note = %q, want %q", string(out), "authed receipt")
	}
	if _, invalidated := ft.stats(); len(invalidated) != 0 {
		t.Errorf("invalidated = %v, want none on the happy path", invalidated)
	}
}
