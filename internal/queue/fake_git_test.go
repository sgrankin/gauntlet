package queue

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
)

// fakeGitRepo is a real, tiny in-memory implementation of core.GitRepo — a
// ref map, a commit graph, and a content-addressed tree store — not a mock:
// it has no expectation recording, just enough working git semantics
// (CAS, ancestry, tree reads/exports, a scriptable trial merge) for the
// queue's state machine to be exercised end to end without touching a real
// git process. Real git plumbing is gitx's job (a different chunk); this
// fake exists solely so queue's tests can drive ReconcileOnce
// deterministically.
type fakeGitRepo struct {
	mu sync.Mutex

	refs    map[string]string            // ref name -> OID
	commits map[string]fakeCommit        // commit OID -> parents + tree
	trees   map[string]map[string]string // tree OID -> path -> content
	pins    map[string]bool              // pinned OIDs (Pin/Unpin), mirroring gitx's refs/gauntlet/pin/*

	// conflicts scripts a MergeTree outcome for a specific (base, candidate)
	// OID pair, overriding the default (always-clean, candidate-wins) merge.
	conflicts map[[2]string]core.TrialMerge

	// Injectable failures: each, when non-nil, makes its method return it
	// (persistently, until the test clears the field) — how tests exercise
	// the daemon-side infra-error paths a working in-memory git can never
	// produce on its own. Guard errors, not scripted call sequences: the
	// fake stays a fake.
	fetchErr      error
	mergeTreeErr  error
	commitTreeErr error
	isAncestorErr error
	exportErr     error
	pinErr        error
	mtimeErr      error

	// casErr, when non-nil, makes CASUpdate fail WITHOUT mutating refs and
	// WITHOUT being a stale lease — the ambiguous "client-visible error,
	// push may or may not have applied" failure landRun's Skip path exists
	// for. (This fake models the didn't-apply half; the did-apply half is a
	// beforeCAS-style ref mutation plus a plain error, which no current
	// test needs.)
	casErr      error
	lsRemoteErr error

	mergeTreeCalls  int
	mtimeCalls      int
	commitTreeCalls int
	exportCalls     int
	lsRemoteCalls   int

	// exportTrees records the exact tree-ish argument of every ExportTree
	// call, in order — so tests can assert WHICH object was archived (the
	// trial TREE OID, never the chain-tip merge COMMIT: issue #9's
	// exact-tree/export-subst invariant), not merely how many times.
	exportTrees []string

	// exportHook, when non-nil, is invoked inside ExportTree with the call's
	// (ctx, tree, dir) after exportCalls/exportTrees are recorded but before
	// any files are written, and OUTSIDE f.mu so it may block. A non-nil
	// return short-circuits the export with that error (no files written);
	// nil lets the default materialization proceed. This is how the
	// cap/cancellation test gates and counts concurrent materializations
	// deterministically without a real, slow git process.
	exportHook func(ctx context.Context, tree, dir string) error

	// casLog records every CASUpdate call in order, so tests can assert
	// ordering (e.g. land pushes the target before deleting the slot)
	// directly instead of inferring it indirectly.
	casLog []casCall

	// beforeCAS, if set, runs synchronously at the start of every
	// CASUpdate call, before the staleness check, for remoteRef —
	// simulating a concurrent mutation (a human push, a re-push) landing
	// in the narrow window between this tick's ListRefs snapshot and the
	// queue's own CAS attempt. That race is exactly what CAS exists to
	// catch; this hook is how a single-threaded fake can provoke it
	// deterministically without real concurrency.
	beforeCAS func(remoteRef string)

	// notes is fakeGitRepo's real (if tiny) git-notes store, keyed by
	// (remoteRef, sha): PublishNote's default behavior — fresh publish,
	// idempotent AlreadyPublished on an exact re-publish, ErrNoteConflict
	// on a differing re-publish — is computed against it exactly like
	// gitx's real fetch-check-add-CAS protocol, minus the actual git
	// plumbing. Tests that need a pre-existing note in place before their
	// own PublishNote call (an AlreadyPublished or a conflict fixture) use
	// seedNote to populate it directly, the same "real implementation,
	// scriptable from outside" style as seed/pushCandidate above.
	notes map[[2]string][]byte

	// publishNoteErr, when non-nil, makes every PublishNote call return it
	// (persistently, sticky like fetchErr/mergeTreeErr/... above) instead
	// of consulting notes — how tests exercise the transport-failure park
	// path without a real network.
	publishNoteErr error

	// publishNoteCalls records every PublishNote call's exact
	// (ref, sha, payload) — a defensive COPY of payload, so a caller that
	// mutates its buffer after the call can't retroactively corrupt what
	// the fake recorded — each stamped with the same opSeq sequence
	// casLog entries share, so a test can assert PublishNote fired
	// strictly before a specific CASUpdate (landRun's publish-then-CAS
	// gate) by comparing seq values directly, not merely slice position.
	publishNoteCalls []notePublishCall

	// opSeq is a single shared call counter, incremented (under f.mu) by
	// both CASUpdate and PublishNote — the ordering affordance issue #13's
	// landing-gate tests need: two independently-appended logs (casLog,
	// publishNoteCalls) would otherwise lose their relative interleaving.
	opSeq int
}

type casCall struct {
	ref, old, new string
	seq           int
}

// notePublishCall is one recorded PublishNote call (fakeGitRepo.
// publishNoteCalls) — see that field's doc.
type notePublishCall struct {
	remoteRef, sha string
	payload        []byte
	seq            int
}

type fakeCommit struct {
	tree    string
	parents []string
	message string // captured verbatim, for tests asserting on merge-message shape (e.g. the optional body)
}

func newFakeGitRepo() *fakeGitRepo {
	return &fakeGitRepo{
		refs:      make(map[string]string),
		commits:   make(map[string]fakeCommit),
		trees:     make(map[string]map[string]string),
		pins:      make(map[string]bool),
		conflicts: make(map[[2]string]core.TrialMerge),
	}
}

// --- core.GitRepo ---

func (f *fakeGitRepo) Fetch(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchErr
}

func (f *fakeGitRepo) ListRefs(ctx context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.refs))
	for k, v := range f.refs {
		out[k] = v
	}
	return out, nil
}

func (f *fakeGitRepo) MergeTree(ctx context.Context, base, candidate string) (core.TrialMerge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergeTreeCalls++
	if f.mergeTreeErr != nil {
		return core.TrialMerge{}, f.mergeTreeErr
	}

	if tm, ok := f.conflicts[[2]string{base, candidate}]; ok {
		return tm, nil
	}

	merged := make(map[string]string)
	for k, v := range f.trees[f.commits[base].tree] {
		merged[k] = v
	}
	for k, v := range f.trees[f.commits[candidate].tree] {
		merged[k] = v // candidate wins on overlap
	}
	return core.TrialMerge{Clean: true, TreeOID: f.internTree(merged)}, nil
}

func (f *fakeGitRepo) CommitTree(ctx context.Context, tree string, parents []string, message string, who core.Identity) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitTreeCalls++
	if f.commitTreeErr != nil {
		return "", f.commitTreeErr
	}

	oid := hashString("commit", tree, strings.Join(parents, ","), message, who.Name, who.Email, fmt.Sprintf("%d", f.commitTreeCalls))
	f.commits[oid] = fakeCommit{tree: tree, parents: append([]string(nil), parents...), message: message}
	return oid, nil
}

func (f *fakeGitRepo) ReadFileFromTree(ctx context.Context, tree, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files, ok := f.trees[tree]
	if !ok {
		// tree may be a commit-ish rather than a literal tree OID: real
		// git's ReadFileFromTree accepts either (it shells out to
		// "cat-file -p <commit-or-tree>:<path>"), a contract specChanged's
		// batch callers rely on directly (the batch boundary check passes
		// the target tip's own commit OID as the "before" side for a
		// batch's first member) — resolve a commit OID to its tree before
		// giving up.
		if c, isCommit := f.commits[tree]; isCommit {
			files, ok = f.trees[c.tree]
		}
	}
	if !ok {
		return nil, fmt.Errorf("fakeGitRepo: unknown tree %q", tree)
	}
	content, ok := files[path]
	if !ok {
		return nil, fmt.Errorf("fakeGitRepo: %s: not found in tree %q", path, tree)
	}
	return []byte(content), nil
}

func (f *fakeGitRepo) IsAncestor(ctx context.Context, maybeAncestor, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isAncestorErr != nil {
		return false, f.isAncestorErr
	}
	seen := make(map[string]bool)
	var walk func(oid string) bool
	walk = func(oid string) bool {
		if oid == "" || seen[oid] {
			return false
		}
		seen[oid] = true
		if oid == maybeAncestor {
			return true
		}
		c, ok := f.commits[oid]
		if !ok {
			return false
		}
		for _, p := range c.parents {
			if walk(p) {
				return true
			}
		}
		return false
	}
	return walk(ref), nil
}

// FindLandingMerge mirrors gitx.Repo's own logic (a real implementation, not
// a script, per this fake's own convention): walk branchTip's first-parent
// chain, newest first, and return the first 2-parent commit whose second
// parent equals candidateSHA exactly (see core.GitRepo.FindLandingMerge and
// gitx's own doc comment for why exact equality, not ancestry, is correct).
func (f *fakeGitRepo) FindLandingMerge(ctx context.Context, branchTip, candidateSHA string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	merges := 0
	for oid := branchTip; oid != ""; {
		c, ok := f.commits[oid]
		if !ok {
			return "", nil
		}
		if len(c.parents) >= 2 {
			if c.parents[1] == candidateSHA {
				return oid, nil
			}
			// Mirrors gitx.Repo's real `--max-count` behavior (verified
			// against real git): the bound counts merge commits found
			// along the first-parent walk, not every commit stepped over.
			merges++
			if merges >= maxLandingMergeSearchTest {
				return "", nil
			}
		}
		if len(c.parents) == 0 {
			return "", nil
		}
		oid = c.parents[0] // first-parent walk
	}
	return "", nil
}

// maxLandingMergeSearchTest bounds fakeGitRepo.FindLandingMerge's walk,
// mirroring gitx.Repo's own maxLandingMergeSearch bound so tests exercise
// the same "give up eventually" contract without needing a real, unexported
// constant shared across packages.
const maxLandingMergeSearchTest = 1000

func (f *fakeGitRepo) ExportTree(ctx context.Context, tree, dir string) error {
	f.mu.Lock()
	f.exportCalls++
	f.exportTrees = append(f.exportTrees, tree)
	if f.exportErr != nil {
		f.mu.Unlock()
		return f.exportErr
	}
	hook := f.exportHook
	files := make(map[string]string, len(f.trees[tree]))
	for k, v := range f.trees[tree] {
		files[k] = v
	}
	f.mu.Unlock()

	// Runs outside the lock so it may block (cap/cancellation test); a
	// non-nil return short-circuits before any files are written.
	if hook != nil {
		if err := hook(ctx, tree, dir); err != nil {
			return err
		}
	}

	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeGitRepo) RestoreMtimes(ctx context.Context, commit, dir string) (core.MtimeStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mtimeCalls++
	if f.mtimeErr != nil {
		return core.MtimeStats{}, f.mtimeErr
	}
	return core.MtimeStats{}, nil
}

func (f *fakeGitRepo) Pin(ctx context.Context, oid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pinErr != nil {
		return f.pinErr
	}
	f.pins[oid] = true
	return nil
}

// Unpin mirrors gitx's contract: removing an absent pin is a no-op, never
// an error, so terminal paths may unpin unconditionally.
func (f *fakeGitRepo) Unpin(ctx context.Context, oid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pins, oid)
	return nil
}

// ListRemoteRefs matches f.refs against a "<prefix>/*" or "<prefix>*"
// glob (the trailing-star form gitx passes), returning name -> OID. An
// injectable lsRemoteErr models a remote round-trip failure.
func (f *fakeGitRepo) ListRemoteRefs(ctx context.Context, pattern string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lsRemoteCalls++
	if f.lsRemoteErr != nil {
		return nil, f.lsRemoteErr
	}
	prefix := strings.TrimSuffix(pattern, "*")
	out := make(map[string]string)
	for ref, oid := range f.refs {
		if strings.HasPrefix(ref, prefix) {
			out[ref] = oid
		}
	}
	return out, nil
}

func (f *fakeGitRepo) CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error {
	if f.beforeCAS != nil {
		f.beforeCAS(remoteRef)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opSeq++
	f.casLog = append(f.casLog, casCall{ref: remoteRef, old: oldOID, new: newOID, seq: f.opSeq})
	if f.casErr != nil {
		return f.casErr
	}
	if f.refs[remoteRef] != oldOID {
		return core.ErrCASStale
	}
	if newOID == "" {
		delete(f.refs, remoteRef)
	} else {
		f.refs[remoteRef] = newOID
	}
	return nil
}

// PublishNote is a real (if tiny) implementation of core.GitRepo.
// PublishNote against f.notes, not a mock: fresh publish, idempotent
// AlreadyPublished on an exact re-publish, and fail-closed ErrNoteConflict
// on a differing one, mirroring gitx.Repo.PublishNote's own semantics
// closely enough for the queue's own tests (issue #13). publishNoteErr, when
// set, short-circuits every call — the transport-failure affordance.
//
// NoteBlobSHA is a deterministic content hash of payload alone (never of
// remoteRef/sha), matching real git's content-addressed blobs: two
// PublishNote calls with identical payload bytes — whether or not for the
// same sha — get the same NoteBlobSHA, exactly as gitx's blobID does.
func (f *fakeGitRepo) PublishNote(ctx context.Context, remoteRef, sha string, payload []byte, who core.Identity) (core.NotePublishResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opSeq++
	f.publishNoteCalls = append(f.publishNoteCalls, notePublishCall{
		remoteRef: remoteRef, sha: sha,
		payload: append([]byte(nil), payload...),
		seq:     f.opSeq,
	})
	if f.publishNoteErr != nil {
		return core.NotePublishResult{}, f.publishNoteErr
	}
	if len(payload) == 0 {
		return core.NotePublishResult{}, fmt.Errorf("fakeGitRepo: publish note %s on %s: empty payload", sha, remoteRef)
	}
	if f.notes == nil {
		f.notes = make(map[[2]string][]byte)
	}
	key := [2]string{remoteRef, sha}
	blobSHA := hashString("noteblob", string(payload))
	if existing, ok := f.notes[key]; ok {
		if bytes.Equal(existing, payload) {
			return core.NotePublishResult{Published: false, NoteBlobSHA: blobSHA}, nil
		}
		return core.NotePublishResult{}, fmt.Errorf("%w: sha %s on %s", core.ErrNoteConflict, sha, remoteRef)
	}
	f.notes[key] = append([]byte(nil), payload...)
	return core.NotePublishResult{Published: true, NoteBlobSHA: blobSHA}, nil
}

// seedNote plants a pre-existing note directly into f.notes, bypassing
// PublishNote/its call log entirely — for tests constructing an
// AlreadyPublished or ErrNoteConflict fixture without an extra scripted
// call of their own.
func (f *fakeGitRepo) seedNote(remoteRef, sha string, payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notes == nil {
		f.notes = make(map[[2]string][]byte)
	}
	f.notes[[2]string{remoteRef, sha}] = append([]byte(nil), payload...)
}

// --- test-only helpers: a tiny working git, scripted from the outside ---

// seed creates a root commit (no parents) with files on branch and points
// refs/heads/<branch> at it, returning the new commit OID.
func (f *fakeGitRepo) seed(branch string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tree := f.internTree(files)
	oid := hashString("seed", branch, tree)
	f.commits[oid] = fakeCommit{tree: tree}
	f.refs["refs/heads/"+branch] = oid
	return oid
}

// pushCandidate creates a new commit with files (parented on base, if
// given) and points ref at it — a fresh queue slot, or (called again on the
// same ref) a re-push (Move).
func (f *fakeGitRepo) pushCandidate(ref, base string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tree := f.internTree(files)
	var parents []string
	if base != "" {
		parents = []string{base}
	}
	oid := hashString("cand", ref, tree, fmt.Sprintf("%d", len(f.commits)))
	f.commits[oid] = fakeCommit{tree: tree, parents: parents}
	f.refs[ref] = oid
	return oid
}

// deleteCandidate removes ref entirely, as if the author deleted the slot.
func (f *fakeGitRepo) deleteCandidate(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refs, ref)
}

// directPush moves branch's ref straight to a new commit built by layering
// files on top of branch's current tree, bypassing CAS — simulating a human
// (or second daemon) push racing the queue. Additive (not a wholesale tree
// replacement), matching testutil.Remote.DirectPush's real-git semantics
// exactly (it checks out the branch's existing tip and writes files into
// that checkout rather than an empty one): a speculate scenario that
// direct-pushes twice — once to seed a check spec, again later to simulate a
// human push racing the target — needs the second push to preserve the
// first's content, exactly as it would against a real remote.
func (f *fakeGitRepo) directPush(branch string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.refs["refs/heads/"+branch]
	merged := make(map[string]string)
	if parent != "" {
		for k, v := range f.trees[f.commits[parent].tree] {
			merged[k] = v
		}
	}
	for k, v := range files {
		merged[k] = v
	}
	tree := f.internTree(merged)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	oid := hashString("direct", branch, tree, fmt.Sprintf("%d", len(f.commits)))
	f.commits[oid] = fakeCommit{tree: tree, parents: parents}
	f.refs["refs/heads/"+branch] = oid
	return oid
}

// commit records an arbitrary commit object directly (for constructing a
// commit graph shape a test needs, e.g. an already-landed merge for the
// IsAncestor recovery test) without going through pushCandidate/seed's
// single-parent assumptions.
func (f *fakeGitRepo) commit(files map[string]string, parents ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tree := f.internTree(files)
	oid := hashString("mkcommit", tree, strings.Join(parents, ","), fmt.Sprintf("%d", len(f.commits)))
	f.commits[oid] = fakeCommit{tree: tree, parents: append([]string(nil), parents...)}
	return oid
}

// setRef points ref directly at oid, bypassing CAS — for assembling test
// fixtures (e.g. planting an already-landed target tip).
func (f *fakeGitRepo) setRef(ref, oid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[ref] = oid
}

// ref returns the current OID of ref ("" if absent).
func (f *fakeGitRepo) ref(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refs[ref]
}

// hasRef reports whether ref currently exists.
func (f *fakeGitRepo) hasRef(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.refs[ref]
	return ok
}

// pinned reports whether oid is currently pinned.
func (f *fakeGitRepo) pinned(oid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pins[oid]
}

// pinCount returns how many OIDs are currently pinned.
func (f *fakeGitRepo) pinCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pins)
}

// commitMessage returns the exact message CommitTree was given for oid, for
// tests asserting on merge-message shape (e.g. the optional body).
func (f *fakeGitRepo) commitMessage(oid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits[oid].message
}

// scriptConflict makes a future MergeTree(base, candidate) call return a
// conflict on the given paths instead of the default clean merge.
func (f *fakeGitRepo) scriptConflict(base, candidate string, paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conflicts[[2]string{base, candidate}] = core.TrialMerge{Clean: false, Conflicts: paths}
}

// internTree registers files as a tree and returns its content-addressed
// OID. Caller must hold f.mu.
func (f *fakeGitRepo) internTree(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha1.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, files[k])
	}
	oid := fmt.Sprintf("%x", h.Sum(nil))
	if _, ok := f.trees[oid]; !ok {
		cp := make(map[string]string, len(files))
		for k, v := range files {
			cp[k] = v
		}
		f.trees[oid] = cp
	}
	return oid
}

func hashString(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%s\x00", p)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
