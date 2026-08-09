package deploy_test

import (
	"context"
	"fmt"
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
type fakeRemote struct {
	mu       sync.Mutex
	remote   map[string]string
	heads    map[string]string
	observed map[string]string
	nextOID  int
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		remote:   map[string]string{},
		heads:    map[string]string{},
		observed: map[string]string{},
	}
}

// commit mints a fresh OID and points branch at it — the fake's whole
// notion of "somebody landed something". Contents are irrelevant here: the
// tracker only ever compares OIDs.
func (f *fakeRemote) commit(branch string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextOID++
	oid := fmt.Sprintf("%040x", f.nextOID)
	f.remote["refs/heads/"+branch] = oid
	return oid
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
