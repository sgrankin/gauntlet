package deploy_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/deploy"
)

// fakeRemote is an in-memory stand-in for the remote plus this daemon's
// local mirrors of it. It models the split faithfully, because the split
// is the thing under test: the tracker READS mirrors (refreshed only when
// the queue's fetch runs) and WRITES the remote (immediately, over the
// network). A fake that let a write appear in the read view instantly
// would quietly make every scenario one tick shorter than production.
//
//	remote   — ground truth, what CASUpdate mutates and assertions read
//	heads    — mirror of the remote's refs/heads/*, remote-named, which is
//	           what gitx.ListRefs reconstructs from refs/remotes/origin/*
//	observed — mirror of the remote's refs/gauntlet/deployed/*, which
//	           deploy.FetchRefspec brings down under identical names
//
// It also stands in for the daemon's OBJECT store, which the lane runner
// needs and the tracker never did: trees maps an OID to that revision's file
// contents, so a fake ReadFileFromTree can serve the deploy spec a scenario
// committed, and a fake ExportTree can materialize it on disk for real. Pins
// live here too, so "unpinned on every terminal path" is assertable.
type fakeRemote struct {
	mu       sync.Mutex
	remote   map[string]string
	heads    map[string]string
	observed map[string]string
	trees    map[string]map[string]string
	pins     map[string]bool
	nextOID  int
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		remote:   map[string]string{},
		heads:    map[string]string{},
		observed: map[string]string{},
		trees:    map[string]map[string]string{},
		pins:     map[string]bool{},
	}
}

// commit mints a fresh OID, records files as that revision's tree, and
// points branch at it — the fake's whole notion of "somebody landed
// something". The tracker only ever compares OIDs; the LANE RUNNER reads
// the tree, which is why contents are kept.
func (f *fakeRemote) commit(branch string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextOID++
	oid := fmt.Sprintf("%040x", f.nextOID)
	tree := make(map[string]string, len(files))
	maps.Copy(tree, files)
	f.trees[oid] = tree
	f.remote["refs/heads/"+branch] = oid
	return oid
}

// pinCount reports how many revisions are pinned right now.
func (f *fakeRemote) pinCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pins)
}

func (f *fakeRemote) setRef(ref, oid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if oid == "" {
		delete(f.remote, ref)
		return
	}
	f.remote[ref] = oid
}

func (f *fakeRemote) ref(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.remote[name]
}

// refresh is the fake's fetch: it re-derives both mirrors from the remote,
// pruning refs that vanished there.
func (f *fakeRemote) refresh() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads = map[string]string{}
	f.observed = map[string]string{}
	for name, oid := range f.remote {
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			f.heads[name] = oid
		case strings.HasPrefix(name, deploy.ObservedRefPrefix):
			f.observed[name] = oid
		}
	}
}

// fakeGit implements deploy.Git over a fakeRemote.
type fakeGit struct{ r *fakeRemote }

var _ deploy.Git = fakeGit{}

func (g fakeGit) ListRefs(context.Context) (map[string]string, error) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	return copyRefs(g.r.heads, ""), nil
}

func (g fakeGit) ListLocalRefs(_ context.Context, prefix string) (map[string]string, error) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	return copyRefs(g.r.observed, prefix), nil
}

func (g fakeGit) CASUpdate(_ context.Context, remoteRef, oldOID, newOID string) error {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	if g.r.remote[remoteRef] != oldOID {
		return fmt.Errorf("fake: cas update %s: %w", remoteRef, core.ErrCASStale)
	}
	if newOID == "" {
		delete(g.r.remote, remoteRef)
		return nil
	}
	g.r.remote[remoteRef] = newOID
	return nil
}

// --- the lane runner's half of deploy.Git ---

// Pin/Unpin record reachability the way gitx does: idempotent, and
// unpinning something never pinned is a no-op rather than an error, so a
// terminal path may unpin unconditionally.
func (g fakeGit) Pin(_ context.Context, oid string) error {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	g.r.pins[oid] = true
	return nil
}

func (g fakeGit) Unpin(_ context.Context, oid string) error {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	delete(g.r.pins, oid)
	return nil
}

// ReadFileFromTree serves the revision's own committed content — the seam
// that makes "the deploy graph comes from the tree being deployed" a real
// statement in the fake suite rather than a real-git-only one. A missing
// file is an error, exactly as `git cat-file` makes it.
func (g fakeGit) ReadFileFromTree(_ context.Context, tree, path string) ([]byte, error) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	files, ok := g.r.trees[tree]
	if !ok {
		return nil, fmt.Errorf("fake: no such tree %s", tree)
	}
	content, ok := files[path]
	if !ok {
		return nil, fmt.Errorf("fake: path %s does not exist in %s", path, tree)
	}
	return []byte(content), nil
}

// ExportTree writes the revision's files out for real: the export directory
// a deploy node runs in is a genuine directory under both harnesses, so
// nothing about the runner's export handling is fake-only.
func (g fakeGit) ExportTree(_ context.Context, tree, dir string) error {
	g.r.mu.Lock()
	files, ok := g.r.trees[tree]
	copied := make(map[string]string, len(files))
	maps.Copy(copied, files)
	g.r.mu.Unlock()
	if !ok {
		return fmt.Errorf("fake: no such tree %s", tree)
	}
	for path, content := range copied {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// RestoreMtimes is a no-op: the fake has no commit history to derive
// per-path times from, and the policy it implements (deterministic export
// metadata) is gitx's own contract, tested there.
func (g fakeGit) RestoreMtimes(context.Context, string, string) (core.MtimeStats, error) {
	return core.MtimeStats{}, nil
}

func copyRefs(in map[string]string, prefix string) map[string]string {
	out := make(map[string]string, len(in))
	for name, oid := range in {
		if strings.HasPrefix(name, prefix) {
			out[name] = oid
		}
	}
	return out
}

// racingGit decorates a deploy.Git with the ability to have another writer
// win a specific ref immediately before the tracker's CAS lands on it —
// the CAS-loss case, made deterministic. It sits ABOVE the Git interface
// on purpose, so the fake and real harnesses race identically instead of
// each faking a race their own way.
//
// It also counts CAS attempts, which is how a scenario proves the negative
// ("this tick did nothing") that a ref assertion alone cannot.
type racingGit struct {
	inner  deploy.Git
	setRef func(ref, oid string) // force-write on the remote, bypassing CAS

	mu       sync.Mutex
	armed    map[string]string // ref -> OID a racer writes before the next CAS
	attempts int
}

var _ deploy.Git = (*racingGit)(nil)

func newRacingGit(inner deploy.Git, setRef func(ref, oid string)) *racingGit {
	return &racingGit{inner: inner, setRef: setRef, armed: map[string]string{}}
}

func (g *racingGit) arm(ref, oid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed[ref] = oid
}

func (g *racingGit) casAttempts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts
}

func (g *racingGit) ListRefs(ctx context.Context) (map[string]string, error) {
	return g.inner.ListRefs(ctx)
}

func (g *racingGit) ListLocalRefs(ctx context.Context, prefix string) (map[string]string, error) {
	return g.inner.ListLocalRefs(ctx, prefix)
}

// The lane-runner methods pass straight through: nothing about a pin, a
// spec read, or an export is worth racing, and a decorator that forgot one
// would silently disable it.
func (g *racingGit) Pin(ctx context.Context, oid string) error   { return g.inner.Pin(ctx, oid) }
func (g *racingGit) Unpin(ctx context.Context, oid string) error { return g.inner.Unpin(ctx, oid) }

func (g *racingGit) ReadFileFromTree(ctx context.Context, tree, path string) ([]byte, error) {
	return g.inner.ReadFileFromTree(ctx, tree, path)
}

func (g *racingGit) ExportTree(ctx context.Context, tree, dir string) error {
	return g.inner.ExportTree(ctx, tree, dir)
}

func (g *racingGit) RestoreMtimes(ctx context.Context, commit, dir string) (core.MtimeStats, error) {
	return g.inner.RestoreMtimes(ctx, commit, dir)
}

func (g *racingGit) CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error {
	g.mu.Lock()
	oid, armed := g.armed[remoteRef]
	if armed {
		delete(g.armed, remoteRef)
	}
	g.attempts++
	g.mu.Unlock()
	if armed {
		g.setRef(remoteRef, oid)
	}
	return g.inner.CASUpdate(ctx, remoteRef, oldOID, newOID)
}
