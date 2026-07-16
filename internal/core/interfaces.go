package core

import (
	"context"
	"errors"
)

// ErrCASStale is returned by GitRepo.CASUpdate when the ref's actual value at
// push time did not match the expected old OID: a direct human push, a
// second daemon instance, or a re-push all surface this way. Callers must
// treat it as a signal to re-derive state and retry, never as corruption.
var ErrCASStale = errors.New("gitx: CAS failed, ref moved")

// GitRepo is gauntlet's entire VCS surface: plumbing only, no working copy.
// Every mutating method is compare-and-swap or additive-only, per Invariants
// 1, 2, 3, and 6.
type GitRepo interface {
	// Fetch updates the local view of the remote (fetch --prune). Every
	// reconcile tick starts with a Fetch; its result is the tick's
	// snapshot of ground truth.
	Fetch(ctx context.Context) error

	// ListRefs returns every remote-tracking ref as name -> OID, as of the
	// most recent Fetch.
	ListRefs(ctx context.Context) (map[string]string, error)

	// MergeTree trial-merges candidate onto base without touching any
	// working copy or branch, returning the resulting tree (if clean) or
	// the conflicted paths (if not).
	MergeTree(ctx context.Context, base, candidate string) (TrialMerge, error)

	// CommitTree creates a commit object from tree and parents with the
	// given message and identity. This is the only object gauntlet ever
	// creates (Invariant 6): candidate commits are never rewritten.
	CommitTree(ctx context.Context, tree string, parents []string, message string, who Identity) (string, error)

	// ReadFileFromTree reads path out of tree without a checkout. Used to
	// read the candidate's own check spec out of the trial tree, so a
	// candidate that changes its checks is tested by its own definition.
	ReadFileFromTree(ctx context.Context, tree, path string) ([]byte, error)

	// IsAncestor reports whether maybeAncestor is an ancestor of ref.
	// Used at crash recovery to detect a run that already landed before a
	// crash interrupted slot cleanup (Invariant 4).
	IsAncestor(ctx context.Context, maybeAncestor, ref string) (bool, error)

	// FindLandingMerge identifies the merge commit that landed candidateSHA
	// onto the target branch, for a candidate crash-recovery already found
	// to be an ancestor of branchTip (Invariant 4's recoverLanded). Every
	// gauntlet land is a --no-ff merge whose second parent is the landed
	// candidate's own SHA verbatim (Invariant 6: candidate commits are
	// never rewritten), so this walks branchTip's first-parent chain,
	// newest first, and returns the first merge commit whose second parent
	// equals candidateSHA exactly. (Ancestry, not exact equality, would be
	// the wrong test: candidateSHA is trivially an ancestor of any later
	// candidate rebased onto main after candidateSHA's own landing, which
	// would wrongly match that later, unrelated merge instead.)
	//
	// Returns ("", nil) — never an error — if no such merge commit is found
	// within the walk's bound: callers must treat "" as "unknown", the same
	// as a genuinely unlanded candidate, not as a failure. A non-nil error
	// means the underlying plumbing itself failed (e.g. branchTip does not
	// resolve), which callers may treat as a soft failure too — the merge
	// SHA is enrichment for recovered landings, not something recovery
	// itself depends on.
	FindLandingMerge(ctx context.Context, branchTip, candidateSHA string) (mergeSHA string, err error)

	// ExportTree materializes tree's contents into dir for checks to run
	// against.
	ExportTree(ctx context.Context, tree, dir string) error

	// Pin anchors oid — an active run's chain-tip merge commit — against
	// garbage collection until Unpin. The merge commits gauntlet creates
	// are referenced by no branch until (unless) they land, yet checks and
	// hooks resolve them through GAUNTLET_GIT_DIR for the whole run, so
	// their reachability is part of the check contract, not a GC-timing
	// accident. Pinning the chain tip covers the entire chain: a commit
	// reaches its parents, so every link, every member SHA, and the base
	// stay live behind one pin. Idempotent per OID.
	Pin(ctx context.Context, oid string) error

	// Unpin releases oid's pin. Unpinning an OID that was never pinned is
	// a no-op, not an error, so terminal paths may unpin unconditionally.
	Unpin(ctx context.Context, oid string) error

	// CASUpdate compare-and-swaps remoteRef from oldOID to newOID.
	// newOID == "" deletes the ref. Returns ErrCASStale if the ref's
	// actual value did not match oldOID (Invariants 2 and 3).
	CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error
}

// Executor runs one named check and reports its verdict. The queue owns
// sequencing, aggregation, per-check spans, and the run record, so per-check
// observability lives in core, not in every Executor implementation.
type Executor interface {
	RunCheck(ctx context.Context, job CheckJob) CheckResult
}

// Channel is a duplex notification/command transport: events flow out,
// commands flow in. Slack, GitHub commit status, a web dashboard, the CLI,
// and stdout are all implementations of this one interface (Invariant 8).
type Channel interface {
	// Emit reports an Event. Implementations must not block the reconcile
	// loop.
	Emit(ctx context.Context, ev Event) error

	// Commands yields inbound Command values. The LogChannel implementation
	// never yields on this channel.
	Commands() <-chan Command
}
