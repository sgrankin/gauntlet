package gitx_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// deployRefspec is what internal/deploy hands WithFetchRefspecs: local and
// remote names identical, deliberately OUTSIDE refs/remotes/origin/* (see
// WithFetchRefspecs's doc). Spelled out here rather than imported so this
// package keeps its one-way dependency on nothing but core/testutil.
const deployRefspec = "+refs/gauntlet/deployed/*:refs/gauntlet/deployed/*"

// gitConfigAll returns every value of a multi-valued git config key in the
// bare repo at dir.
func gitConfigAll(t *testing.T, dir, key string) []string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+dir, "config", "--get-all", key).Output()
	if err != nil {
		t.Fatalf("git config --get-all %s: %v", key, err)
	}
	return strings.Fields(strings.TrimSpace(string(out)))
}

// TestFetchRefspecsMirrorsDeployedRefs is the whole point of the option:
// with it, one ordinary Fetch brings the remote's refs/gauntlet/deployed/*
// down under their own names — and ListRefs's answer is byte-identical to
// a control repo built without the option, so no queue state can shift.
func TestFetchRefspecsMirrorsDeployedRefs(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	remote.SetRef("refs/gauntlet/deployed/dev", remote.Ref("refs/heads/main"))

	repo, err := gitx.New(ctx, remote.BareClone(), remote.Dir, gitx.WithFetchRefspecs(deployRefspec))
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	local, err := repo.ListLocalRefs(ctx, "refs/gauntlet/deployed/")
	if err != nil {
		t.Fatalf("ListLocalRefs: %v", err)
	}
	if got := local["refs/gauntlet/deployed/dev"]; got != remote.Ref("refs/heads/main") {
		t.Fatalf("local refs/gauntlet/deployed/dev = %q, want main's tip %q (mirrored refs = %v)",
			got, remote.Ref("refs/heads/main"), local)
	}

	// Control: the same remote, a repo without the option.
	control, err := gitx.New(ctx, remote.BareClone(), remote.Dir)
	if err != nil {
		t.Fatalf("gitx.New (control): %v", err)
	}
	if err := control.Fetch(ctx); err != nil {
		t.Fatalf("Fetch (control): %v", err)
	}
	controlLocal, err := control.ListLocalRefs(ctx, "refs/gauntlet/deployed/")
	if err != nil {
		t.Fatalf("ListLocalRefs (control): %v", err)
	}
	if len(controlLocal) != 0 {
		t.Errorf("control repo mirrored %v; without the option nothing under that namespace should be fetched", controlLocal)
	}

	withRefs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	controlRefs, err := control.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs (control): %v", err)
	}
	if !reflect.DeepEqual(withRefs, controlRefs) {
		t.Fatalf("ListRefs differs with the extra refspec:\n with = %v\n without = %v", withRefs, controlRefs)
	}
}

// TestFetchRefspecsPruneStillWorks: the extra refspec must not disturb
// --prune's native semantics on the canonical one. A branch deleted on the
// remote still disappears from ListRefs.
func TestFetchRefspecsPruneStillWorks(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	remote.Seed("doomed", map[string]string{"g.txt": "1\n"})
	remote.SetRef("refs/gauntlet/deployed/dev", remote.Ref("refs/heads/main"))

	repo, err := gitx.New(ctx, remote.BareClone(), remote.Dir, gitx.WithFetchRefspecs(deployRefspec))
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if _, ok := refs["refs/heads/doomed"]; !ok {
		t.Fatalf("refs/heads/doomed missing before the delete: %v", refs)
	}

	remote.DeleteCandidate("refs/heads/doomed")
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch after delete: %v", err)
	}
	refs, err = repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs after delete: %v", err)
	}
	if _, ok := refs["refs/heads/doomed"]; ok {
		t.Fatalf("refs/heads/doomed survived --prune: %v", refs)
	}
	// The deploy mirror is untouched by the branch's disappearance.
	local, err := repo.ListLocalRefs(ctx, "refs/gauntlet/deployed/")
	if err != nil {
		t.Fatalf("ListLocalRefs: %v", err)
	}
	if len(local) != 1 {
		t.Errorf("deploy mirror = %v, want the one dev ref still present", local)
	}
}

// TestNewReplacesMultiValuedFetchRefspec is the --replace-all regression
// (see New): constructing WITH the extra refspec and then, on the same
// state dir, WITHOUT it must succeed and leave exactly the one canonical
// value — the "operator disabled deploy and restarted" path, which a plain
// `git config <name> <value>` cannot do against a multi-valued key.
func TestNewReplacesMultiValuedFetchRefspec(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	dir := remote.BareClone()

	if _, err := gitx.New(ctx, dir, remote.Dir, gitx.WithFetchRefspecs(deployRefspec)); err != nil {
		t.Fatalf("gitx.New (with refspec): %v", err)
	}
	got := gitConfigAll(t, dir, "remote.origin.fetch")
	want := []string{"+refs/heads/*:refs/remotes/origin/*", deployRefspec}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remote.origin.fetch = %v, want %v", got, want)
	}

	// Same dir, deploy now disabled: must not fail, must self-heal.
	if _, err := gitx.New(ctx, dir, remote.Dir); err != nil {
		t.Fatalf("gitx.New (without refspec, on a dir that had two values): %v", err)
	}
	got = gitConfigAll(t, dir, "remote.origin.fetch")
	if !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("remote.origin.fetch = %v, want just %v", got, want[:1])
	}

	// And re-enabling is stable, not cumulative.
	if _, err := gitx.New(ctx, dir, remote.Dir, gitx.WithFetchRefspecs(deployRefspec)); err != nil {
		t.Fatalf("gitx.New (re-enabled): %v", err)
	}
	if _, err := gitx.New(ctx, dir, remote.Dir, gitx.WithFetchRefspecs(deployRefspec)); err != nil {
		t.Fatalf("gitx.New (re-enabled twice): %v", err)
	}
	if got := gitConfigAll(t, dir, "remote.origin.fetch"); !reflect.DeepEqual(got, want) {
		t.Fatalf("remote.origin.fetch after two identical constructions = %v, want %v", got, want)
	}
}

// TestListLocalRefsScopedToPrefix: the answer is exactly the prefix's refs,
// named verbatim, and never any remote-tracking ref.
func TestListLocalRefsScopedToPrefix(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	tip := remote.Ref("refs/heads/main")
	remote.SetRef("refs/gauntlet/deployed/dev", tip)
	remote.SetRef("refs/gauntlet/deployed/prod", tip)
	remote.SetRef("refs/gauntlet/other/thing", tip)

	repo, err := gitx.New(ctx, remote.BareClone(), remote.Dir,
		gitx.WithFetchRefspecs(deployRefspec, "+refs/gauntlet/other/*:refs/gauntlet/other/*"))
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	local, err := repo.ListLocalRefs(ctx, "refs/gauntlet/deployed/")
	if err != nil {
		t.Fatalf("ListLocalRefs: %v", err)
	}
	want := map[string]string{
		"refs/gauntlet/deployed/dev":  tip,
		"refs/gauntlet/deployed/prod": tip,
	}
	if !reflect.DeepEqual(local, want) {
		t.Fatalf("ListLocalRefs = %v, want %v", local, want)
	}
	for name := range local {
		if strings.HasPrefix(name, "refs/remotes/") {
			t.Errorf("ListLocalRefs returned a remote-tracking ref %q", name)
		}
	}

	// An empty namespace is an empty map, not an error.
	empty, err := repo.ListLocalRefs(ctx, "refs/gauntlet/nothing/")
	if err != nil {
		t.Fatalf("ListLocalRefs (empty prefix): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListLocalRefs on an unused namespace = %v, want empty", empty)
	}
}

// TestCASUpdateObservedRef exercises the primitive the deploy tracker's
// observed-ref writes (D2) will use: a CAS-create from oldOID "" and a
// CAS-update of refs/gauntlet/deployed/<env> on the REMOTE, with a stale
// old value rejected as core.ErrCASStale.
func TestCASUpdateObservedRef(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	first := remote.Ref("refs/heads/main")
	remote.Seed("main", map[string]string{"f.txt": "2\n"})
	second := remote.Ref("refs/heads/main")

	repo, err := gitx.New(ctx, remote.BareClone(), remote.Dir, gitx.WithFetchRefspecs(deployRefspec))
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	const ref = "refs/gauntlet/deployed/dev"
	if err := repo.CASUpdate(ctx, ref, "", first); err != nil {
		t.Fatalf("CASUpdate create: %v", err)
	}
	if got := remote.Ref(ref); got != first {
		t.Fatalf("remote %s = %q, want %q after the create", ref, got, first)
	}

	if err := repo.CASUpdate(ctx, ref, first, second); err != nil {
		t.Fatalf("CASUpdate advance: %v", err)
	}
	if got := remote.Ref(ref); got != second {
		t.Fatalf("remote %s = %q, want %q after the advance", ref, got, second)
	}

	// A stale old value loses, and says so in the way callers branch on.
	if err := repo.CASUpdate(ctx, ref, first, first); !errors.Is(err, core.ErrCASStale) {
		t.Fatalf("CASUpdate with a stale old value = %v, want core.ErrCASStale", err)
	}
	if got := remote.Ref(ref); got != second {
		t.Fatalf("remote %s = %q, want it unchanged at %q after the lost CAS", ref, got, second)
	}
}

// TestCASCreateLosesToRacer covers the shape internal/deploy's race
// scenario depends on: a CAS-create (oldOID "") against a ref some other
// writer created first must FAIL rather than clobber it.
func TestCASCreateLosesToRacer(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	remote.Seed("other", map[string]string{"g.txt": "1\n"})

	repo, err := gitx.New(ctx, remote.BareClone(), remote.Dir)
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	const ref = "refs/heads/deploy/dev"
	racer := remote.Ref("refs/heads/other")
	remote.SetRef(ref, racer)

	err = repo.CASUpdate(ctx, ref, "", remote.Ref("refs/heads/main"))
	if err == nil {
		t.Fatal("CASUpdate create succeeded against an already-created ref; the racer's value was clobbered")
	}
	if got := remote.Ref(ref); got != racer {
		t.Fatalf("remote %s = %q, want the racer's %q", ref, got, racer)
	}
}

// TestFetchArgvUnchanged is a belt-and-braces read of the state dir: the
// extra refspec lives in config, so the fetch command line itself is the
// same one the queue has always run. Asserted indirectly — the canonical
// refspec is always value #1, whatever else is configured — since the argv
// is not observable from outside the package.
func TestFetchArgvUnchanged(t *testing.T) {
	ctx := context.Background()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f.txt": "1\n"})
	dir := filepath.Join(t.TempDir(), "fresh.git")

	if _, err := gitx.New(ctx, dir, remote.Dir, gitx.WithFetchRefspecs(deployRefspec)); err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	got := gitConfigAll(t, dir, "remote.origin.fetch")
	if len(got) == 0 || got[0] != "+refs/heads/*:refs/remotes/origin/*" {
		t.Fatalf("remote.origin.fetch = %v, want the canonical refspec first", got)
	}
}
