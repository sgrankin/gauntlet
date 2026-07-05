package queue

import (
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

	mergeTreeCalls  int
	commitTreeCalls int
	exportCalls     int

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
}

type casCall struct {
	ref, old, new string
}

type fakeCommit struct {
	tree    string
	parents []string
	message string // captured verbatim, for tests asserting on merge-message shape (e.g. phase-4's optional body)
}

func newFakeGitRepo() *fakeGitRepo {
	return &fakeGitRepo{
		refs:      make(map[string]string),
		commits:   make(map[string]fakeCommit),
		trees:     make(map[string]map[string]string),
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

func (f *fakeGitRepo) ExportTree(ctx context.Context, tree, dir string) error {
	f.mu.Lock()
	if f.exportErr != nil {
		f.exportCalls++
		f.mu.Unlock()
		return f.exportErr
	}
	files := make(map[string]string, len(f.trees[tree]))
	for k, v := range f.trees[tree] {
		files[k] = v
	}
	f.exportCalls++
	f.mu.Unlock()

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

func (f *fakeGitRepo) CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error {
	if f.beforeCAS != nil {
		f.beforeCAS(remoteRef)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.casLog = append(f.casLog, casCall{ref: remoteRef, old: oldOID, new: newOID})
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

// directPush moves branch's ref straight to a new commit built from files,
// bypassing CAS — simulating a human (or second daemon) push racing the
// queue.
func (f *fakeGitRepo) directPush(branch string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.refs["refs/heads/"+branch]
	tree := f.internTree(files)
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

// commitMessage returns the exact message CommitTree was given for oid, for
// tests asserting on merge-message shape (e.g. phase-4's optional body).
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
