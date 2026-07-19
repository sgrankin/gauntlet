package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
)

// notesWorkPrefix is the local ref namespace FetchNotesRef mirrors a
// remote notes ref into. It deliberately sits under refs/notes/: git's
// notes porcelain (`git notes --ref=<ref>`) silently reprefixes any --ref
// argument that does not already start with "refs/notes/" onto
// "refs/notes/<ref>" (empirically confirmed while building this — passing
// "refs/gauntlet/notes-work/x" lands the ref at
// "refs/notes/refs/gauntlet/notes-work/x", not the name given). Naming our
// own working ref anywhere else would make every --ref= call below target
// something other than what FetchNotesRef actually wrote. Namespaced by
// remoteRef itself so distinct receipt refs (e.g. one per target) never
// collide in one local bare repo.
const notesWorkPrefix = "refs/notes/gauntlet/work/"

// NotesWorkRef derives the local ref FetchNotesRef fetches remoteRef into
// and that ReadNote, AddNote, and PublishNote all address. Pure function
// of remoteRef, no I/O, so a caller can compute the mapping without a
// round trip.
func NotesWorkRef(remoteRef string) string {
	return notesWorkPrefix + strings.TrimPrefix(remoteRef, "refs/")
}

// FetchNotesRef force-fetches remoteRef into NotesWorkRef(remoteRef) — the
// same "local ref is a cache of the remote" pattern Fetch uses for
// refs/heads/* via +refspec. Rides the same authenticated transport as
// every other remote operation (runRemote). Returns the fetched tip OID,
// or "" if remoteRef does not exist on the remote yet. That "no ref" case
// is distinguished from a transport failure (bad remote, network,
// permission) by matching git's specific "couldn't find remote ref"
// fetch-refspec error; anything else is returned as a non-nil error. On
// "no ref", any stale local work ref left over from an earlier fetch of
// this same remoteRef (e.g. the remote note was since deleted, or a
// different process's partial attempt) is explicitly cleared so
// ReadNote/AddNote never operate against local history that no longer
// corresponds to the "" tip being reported — leaving it would let a
// PublishNote retry silently resurrect stale notes under a CAS-create
// (old == "") that the actual remote state would otherwise reject.
func (r *Repo) FetchNotesRef(ctx context.Context, remoteRef string) (string, error) {
	localRef := NotesWorkRef(remoteRef)
	refspec := "+" + remoteRef + ":" + localRef
	if _, err := r.runRemote(ctx, "fetch", "origin", refspec); err != nil {
		if isNoSuchRemoteRef(err) {
			_, _ = r.run(ctx, "update-ref", "-d", localRef) // no-op if it never existed
			return "", nil
		}
		return "", fmt.Errorf("gitx: fetch notes ref %s: %w", remoteRef, err)
	}
	out, err := r.run(ctx, "rev-parse", localRef)
	if err != nil {
		return "", fmt.Errorf("gitx: resolve fetched notes ref %s: %w", localRef, err)
	}
	return strings.TrimSpace(out), nil
}

func isNoSuchRemoteRef(err error) bool {
	var ge *gitError
	return errors.As(err, &ge) && strings.Contains(ge.stderr, "couldn't find remote ref")
}

// ReadNote returns the payload attached to sha on localWorkRef (as
// produced by FetchNotesRef), byte-exact. exists is false, err nil, when
// sha carries no note — including when localWorkRef does not exist
// locally at all (no note has ever been fetched/published on the
// corresponding remote ref): git's own error message doesn't distinguish
// the two ("no note found for object <sha>" either way), and callers
// don't need to either — FetchNotesRef's returned tip already tells them
// whether the remote ref itself exists.
func (r *Repo) ReadNote(ctx context.Context, localWorkRef, sha string) ([]byte, bool, error) {
	out, err := r.run(ctx, "notes", "--ref="+localWorkRef, "show", sha)
	if err != nil {
		if isNoNoteFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("gitx: read note %s on %s: %w", sha, localWorkRef, err)
	}
	return []byte(out), true, nil
}

func isNoNoteFound(err error) bool {
	var ge *gitError
	return errors.As(err, &ge) && strings.Contains(ge.stderr, "no note found for object")
}

// AddNote attaches payload to sha on localWorkRef, creating a notes commit
// (blob + tree + commit) with who as both author and committer identity —
// the same -c user.name/-c user.email pattern CommitTree uses, so the
// notes commit carries the daemon's identity regardless of the process's
// ambient git config or GIT_AUTHOR_*/GIT_COMMITTER_* environment. This is
// the second deliberate object-creation site DESIGN.md's Invariant 6 now
// names (issue #13; CommitTree remains the first, and unchanged).
//
// The blob is created with `git hash-object -w` from the raw payload
// bytes and attached via `git notes add -C <blob>` (reusing an existing
// object's content verbatim), never via notes add's `-m`/`-F` (a message
// string): empirically, `git notes add -m`/`-F` appends a trailing
// newline to the note's blob whenever payload doesn't already end in one
// — confirmed with a byte-for-byte cmp against the written blob object,
// not just against `git notes show`'s output — so those flags cannot be
// used for a byte-exact receipt payload. `-C` sidesteps the mangling
// entirely (it copies an object's content as-is) while still using git's
// own notes-tree/fanout machinery, so no hand-rolled mktree/read-tree is
// needed. See notes_test.go's round-trip tests for the empirical proof.
//
// Empty payload is rejected here as defense in depth (the queue enforces
// it too). Returns the note's blob SHA — content identity for provenance.
func (r *Repo) AddNote(ctx context.Context, localWorkRef, sha string, payload []byte, who core.Identity) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("gitx: add note %s on %s: empty payload", sha, localWorkRef)
	}
	blobOut, err := runGit(ctx, r.dir, bytes.NewReader(payload), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", fmt.Errorf("gitx: add note %s on %s: hash-object: %w", sha, localWorkRef, err)
	}
	blobSHA := strings.TrimSpace(blobOut)

	args := []string{
		"-c", "user.name=" + who.Name,
		"-c", "user.email=" + who.Email,
		"notes", "--ref=" + localWorkRef, "add", "-C", blobSHA, sha,
	}
	if _, err := r.run(ctx, args...); err != nil {
		return "", fmt.Errorf("gitx: add note %s on %s: %w", sha, localWorkRef, err)
	}
	return blobSHA, nil
}

// blobID computes the content-addressed object ID payload would hash to
// as a git blob, without requiring it to already be written under
// localWorkRef's history — used by PublishNote's AlreadyPublished path,
// where the note (and its blob) were created by a call this process did
// not make (a previous run, or a disjoint writer), so there is no
// just-created blobSHA to hand back. Blob IDs are pure content hashes
// (independent of any tree/commit they're referenced from), so this
// agrees with AddNote's returned blobSHA for identical payload bytes —
// verified by TestPublishNoteBlobSHAAgreesWithTreeLookup.
func (r *Repo) blobID(ctx context.Context, payload []byte) (string, error) {
	out, err := runGit(ctx, r.dir, bytes.NewReader(payload), "hash-object", "--stdin")
	if err != nil {
		return "", fmt.Errorf("gitx: compute blob id: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// publishNoteMaxAttempts bounds PublishNote's fetch/CAS retry loop. A CAS
// rejection means remoteRef advanced concurrently since the fetch — most
// often a disjoint receipt (a different SHA) that must survive right
// alongside this one, so the loop refetches and retries rather than
// giving up on the first collision. Bounded so persistent contention
// (concurrent publishers hammering the same ref every attempt) surfaces
// as an error instead of looping forever; it never escalates to a force
// push.
const publishNoteMaxAttempts = 4

// publishNoteTestHook, when non-nil, is invoked once per PublishNote loop
// iteration right after that iteration's fetch (attempt, the fetched tip)
// and before ReadNote/AddNote/CASUpdate. It exists purely as a
// synchronization seam for tests that need to land a concurrent write
// deterministically inside PublishNote's fetch-to-push window: real git
// subprocesses race at a granularity too fast to pin down reliably from
// outside the package. It does not stub or fake git — every call it makes
// is still a real git invocation against a real repo — it only sequences
// them. nil in production; only ever set by this package's own tests.
var publishNoteTestHook func(attempt int, tip string)

// PublishNote is the composed, idempotent publish protocol the queue
// drives (issue #13, requirement 8), implementing core.GitRepo.PublishNote:
// fetch remoteRef, check whether sha already carries this exact payload
// (success, no-op — AlreadyPublished), and if not, add a note commit and
// CAS-push it — retrying on CAS staleness (a concurrent writer moved
// remoteRef) up to publishNoteMaxAttempts times. who is used only when a
// new note commit is actually created.
//
// A pre-existing note for sha with DIFFERENT payload is fail-closed:
// core.ErrNoteConflict, remote untouched, no retry — this is an invariant
// violation for the caller to surface, never something to force through.
func (r *Repo) PublishNote(ctx context.Context, remoteRef, sha string, payload []byte, who core.Identity) (core.NotePublishResult, error) {
	if len(payload) == 0 {
		return core.NotePublishResult{}, fmt.Errorf("gitx: publish note %s on %s: empty payload", sha, remoteRef)
	}
	localRef := NotesWorkRef(remoteRef)

	var lastErr error
	for attempt := 0; attempt < publishNoteMaxAttempts; attempt++ {
		tip, err := r.FetchNotesRef(ctx, remoteRef)
		if err != nil {
			return core.NotePublishResult{}, fmt.Errorf("gitx: publish note: %w", err)
		}
		if publishNoteTestHook != nil {
			publishNoteTestHook(attempt, tip)
		}

		existing, exists, err := r.ReadNote(ctx, localRef, sha)
		if err != nil {
			return core.NotePublishResult{}, fmt.Errorf("gitx: publish note: %w", err)
		}
		if exists {
			if bytes.Equal(existing, payload) {
				blobSHA, err := r.blobID(ctx, payload)
				if err != nil {
					return core.NotePublishResult{}, fmt.Errorf("gitx: publish note: %w", err)
				}
				return core.NotePublishResult{Published: false, NoteBlobSHA: blobSHA}, nil
			}
			return core.NotePublishResult{}, fmt.Errorf("%w: sha %s on %s", core.ErrNoteConflict, sha, remoteRef)
		}

		blobSHA, err := r.AddNote(ctx, localRef, sha, payload, who)
		if err != nil {
			return core.NotePublishResult{}, fmt.Errorf("gitx: publish note: %w", err)
		}
		newTipOut, err := r.run(ctx, "rev-parse", localRef)
		if err != nil {
			return core.NotePublishResult{}, fmt.Errorf("gitx: publish note: resolve new tip: %w", err)
		}
		newTip := strings.TrimSpace(newTipOut)

		err = r.CASUpdate(ctx, remoteRef, tip, newTip)
		if err == nil {
			return core.NotePublishResult{Published: true, NoteBlobSHA: blobSHA}, nil
		}
		if errors.Is(err, core.ErrCASStale) {
			lastErr = err
			continue // remoteRef moved (likely a disjoint receipt); refetch and retry
		}
		return core.NotePublishResult{}, fmt.Errorf("gitx: publish note: %w", err)
	}
	return core.NotePublishResult{}, fmt.Errorf("gitx: publish note %s on %s: exhausted %d attempts under contention: %w",
		sha, remoteRef, publishNoteMaxAttempts, lastErr)
}
