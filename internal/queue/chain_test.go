// Chain-builder integration suite: proves buildChainLink and specChanged
// against REAL git (internal/gitx + internal/testutil), the same tier as
// integration_test.go (see that file's package doc). These tests call
// buildChainLink directly, repeatedly, with the base advancing to the prior
// call's mergeOID — exactly the shape batch and speculate drive it in (see
// docs/design/queue-modes.md, "The merge-commit chain"). Nothing here goes
// through ReconcileOnce/reconcileTarget: no run/lane is ever created, no ref
// is ever mutated — the chain is pure trial-merge/commit-tree plumbing that
// exists before any run does.
package queue

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// newChainHarness builds a Daemon wired to a real gitx.Repo, mirroring
// newIntegrationHarness (integration_test.go), additionally returning the
// local bare clone's directory. Chain links built by buildChainLink are
// unreferenced commits that live only as loose objects in this local clone
// until a land push — a test asserting
// --first-parent history on one of them (never pushed anywhere, and never
// will be here) must inspect this directory directly with raw git, which
// newIntegrationHarness's harness type has no need to expose. remote may be
// nil to create a fresh one; mergeBody may be nil (Config.MergeBody
// disabled, the common case — matches every test here except the one that
// asserts on it).
func newChainHarness(t *testing.T, remote *testutil.Remote, mergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string) (*Daemon, *gitx.Repo, string, *testutil.Remote) {
	t.Helper()
	if remote == nil {
		remote = testutil.NewRemote(t)
	}
	dir := remote.BareClone()
	repo, err := gitx.New(context.Background(), dir, remote.Dir)
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	d, err := New(repo, executor.NewGatedExecutor(), nil, Config{
		Targets:   []config.Target{{Name: "main", Branch: "main"}},
		CheckSpec: testCheckSpecPath,
		Committer: testCommitter,
		MergeBody: mergeBody,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, repo, dir, remote
}

// chainOnClean is a stand-in for buildChainLink's onClean callback for
// tests that don't care about the minted run ID (every test here: run IDs
// only matter to EventTrialClean/the run record, neither of which exists at
// this layer).
func chainOnClean(core.TrialMerge) string { return "test-run" }

// chainCommit is one commit's SHA and parent list, as inspected directly out
// of the local bare clone's object store (firstParentChain).
type chainCommit struct {
	sha     string
	parents []string
}

// firstParentChain walks tip's first-parent history in the local bare repo
// at gitDir (never the testutil.Remote — the chain's link commits are
// unreferenced and were never pushed there), returning the n newest commits,
// tip first.
func firstParentChain(t *testing.T, gitDir, tip string, n int) []chainCommit {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+gitDir, "log", "--first-parent", "--format=%H\x1f%P", "-n", strconv.Itoa(n), tip)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log --first-parent %s: %v", tip, err)
	}
	var commits []chainCommit
	for _, ln := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if ln == "" {
			continue
		}
		sha, parents, _ := strings.Cut(ln, "\x1f")
		commits = append(commits, chainCommit{sha: sha, parents: strings.Fields(parents)})
	}
	return commits
}

// TestChainBuild_TipTreeIsCumulative builds a 3-link chain via repeated
// buildChainLink calls, each one's base the previous call's mergeOID: the
// tip tree must contain every member's file (plus the pre-chain content),
// and --first-parent from the tip must show exactly one merge per member,
// each merge's parent[1] equal to that member's candidate SHA verbatim
// (Invariant 1/6 for the whole chain, proved here through buildChainLink
// itself rather than raw git).
func TestChainBuild_TipTreeIsCumulative(t *testing.T) {
	ctx := context.Background()
	d, repo, dir, remote := newChainHarness(t, nil, nil)

	remote.Seed("main", map[string]string{"base.txt": "base\n"})
	refA := remote.PushCandidate("main", "alice", "a", map[string]string{"a.txt": "a\n"})
	refB := remote.PushCandidate("main", "bob", "b", map[string]string{"b.txt": "b\n"})
	refC := remote.PushCandidate("main", "carol", "c", map[string]string{"c.txt": "c\n"})

	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base := refs["refs/heads/main"]
	cands := []core.Candidate{
		{Ref: refA, Target: "main", User: "alice", Topic: "a", SHA: refs[refA]},
		{Ref: refB, Target: "main", User: "bob", Topic: "b", SHA: refs[refB]},
		{Ref: refC, Target: "main", User: "carol", Topic: "c", SHA: refs[refC]},
	}

	cur := base
	var links []chainLink
	for _, cand := range cands {
		link, trial, err := d.buildChainLink(ctx, ctx, "main", cur, cand, chainOnClean)
		if err != nil {
			t.Fatalf("buildChainLink(%s): %v", cand.Topic, err)
		}
		if !trial.Clean {
			t.Fatalf("buildChainLink(%s): expected clean trial, conflicts=%v", cand.Topic, trial.Conflicts)
		}
		links = append(links, link)
		cur = link.mergeOID
	}
	tip := links[len(links)-1].mergeOID

	want := map[string]string{"base.txt": "base\n", "a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"}
	for path, want := range want {
		got, err := repo.ReadFileFromTree(ctx, tip, path)
		if err != nil {
			t.Fatalf("ReadFileFromTree(%s) at tip: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("%s at tip = %q, want %q", path, got, want)
		}
	}

	commits := firstParentChain(t, dir, tip, len(cands))
	if len(commits) != len(cands) {
		t.Fatalf("first-parent chain has %d commits, want %d", len(commits), len(cands))
	}
	wantOrder := []core.Candidate{cands[2], cands[1], cands[0]} // tip first: c, b, a
	for i, c := range commits {
		if len(c.parents) != 2 {
			t.Fatalf("commit %d (%s): want 2 parents, got %v", i, c.sha, c.parents)
		}
		if c.parents[1] != wantOrder[i].SHA {
			t.Fatalf("commit %d parent[1] = %s, want candidate %s SHA %s verbatim", i, c.parents[1], wantOrder[i].Topic, wantOrder[i].SHA)
		}
	}
	// The innermost link's parent[0] is the pre-chain target tip.
	if got := commits[len(commits)-1].parents[0]; got != base {
		t.Fatalf("innermost link parent[0] = %s, want target tip %s", got, base)
	}
	// mergeOID must equal the SHA git itself reports at each first-parent
	// step, tip down to the first link.
	for i, link := range []chainLink{links[2], links[1], links[0]} {
		if commits[i].sha != link.mergeOID {
			t.Fatalf("commit %d sha = %s, want link.mergeOID %s", i, commits[i].sha, link.mergeOID)
		}
	}
}

// TestChainConflictAborts builds a first link cleanly, then a second member
// whose change to the same file conflicts with the first member's — the
// UNPUSHED first link's tree, not the real target tip. MergeTree must
// detect this identically against the chained base as it would against a
// real ref: buildChainLink returns a zero chainLink and
// trial.Clean == false (a conflict is data, not an error — nil err), and
// nothing lands (buildChainLink itself never mutates any ref; the CAS land
// is always a separate, later step, which never gets a chance to run here).
func TestChainConflictAborts(t *testing.T) {
	ctx := context.Background()
	d, repo, _, remote := newChainHarness(t, nil, nil)

	remote.Seed("main", map[string]string{"shared.txt": "orig\n"})
	refA := remote.PushCandidate("main", "alice", "a", map[string]string{"shared.txt": "alice's change\n"})
	refB := remote.PushCandidate("main", "bob", "b", map[string]string{"shared.txt": "bob's conflicting change\n"})

	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base := refs["refs/heads/main"]
	a := core.Candidate{Ref: refA, Target: "main", User: "alice", Topic: "a", SHA: refs[refA]}
	b := core.Candidate{Ref: refB, Target: "main", User: "bob", Topic: "b", SHA: refs[refB]}

	link1, trial1, err := d.buildChainLink(ctx, ctx, "main", base, a, chainOnClean)
	if err != nil {
		t.Fatalf("link1: %v", err)
	}
	if !trial1.Clean {
		t.Fatalf("link1: expected clean trial, conflicts=%v", trial1.Conflicts)
	}

	link2, trial2, err := d.buildChainLink(ctx, ctx, "main", link1.mergeOID, b, chainOnClean)
	if err != nil {
		t.Fatalf("link2: unexpected error (a conflict must be data, not an error): %v", err)
	}
	if trial2.Clean {
		t.Fatalf("link2: expected a conflict against the unpushed chained base, got clean tree %s", trial2.TreeOID)
	}
	if link2 != (chainLink{}) {
		t.Fatalf("link2: expected a zero chainLink on conflict, got %+v", link2)
	}
	found := false
	for _, p := range trial2.Conflicts {
		if p == "shared.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected shared.txt among reported conflicts, got %v", trial2.Conflicts)
	}

	if got := remote.Ref("refs/heads/main"); got != base {
		t.Fatalf("target ref moved to %s; nothing should have landed (base was %s)", got, base)
	}
}

// TestSpecChanged proves specChanged's content-compare contract directly:
// a member that leaves cfg.CheckSpec untouched is not flagged; one that
// modifies it is.
func TestSpecChanged(t *testing.T) {
	ctx := context.Background()
	d, repo, _, remote := newChainHarness(t, nil, nil)

	remote.Seed("main", map[string]string{
		testCheckSpecPath: "check \"test\" {}\n",
		"f.txt":           "1\n",
	})
	refSame := remote.PushCandidate("main", "alice", "same", map[string]string{"f.txt": "2\n"})
	refChanged := remote.PushCandidate("main", "bob", "changed", map[string]string{testCheckSpecPath: "check \"test2\" {}\n"})

	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base := refs["refs/heads/main"]

	if changed := d.specChanged(ctx, base, refs[refSame]); changed {
		t.Fatalf("spec untouched by member: specChanged = true, want false")
	}
	if changed := d.specChanged(ctx, base, refs[refChanged]); !changed {
		t.Fatalf("spec modified by member: specChanged = false, want true")
	}
}

// TestChainBuild_MergeBodyCalledPerLinkWithChainedBase is the chained-base
// MergeBody sanity case: buildChainLink invokes Config.MergeBody once per
// call (see docs/design/queue-modes.md, "Merge-body cost"), so building a
// chain via N buildChainLink calls must invoke it exactly N times, each
// with that link's own candidate and its own (possibly chained, unpushed)
// base —
// proving the second call's base is genuinely link1.mergeOID, not the
// real target tip.
func TestChainBuild_MergeBodyCalledPerLinkWithChainedBase(t *testing.T) {
	ctx := context.Background()
	type call struct {
		cand core.Candidate
		base string
	}
	var calls []call
	mergeBody := func(_ context.Context, cand core.Candidate, base string) string {
		calls = append(calls, call{cand: cand, base: base})
		return ""
	}
	d, repo, _, remote := newChainHarness(t, nil, mergeBody)

	remote.Seed("main", map[string]string{"base.txt": "base\n"})
	refA := remote.PushCandidate("main", "alice", "a", map[string]string{"a.txt": "a\n"})
	refB := remote.PushCandidate("main", "bob", "b", map[string]string{"b.txt": "b\n"})

	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	base := refs["refs/heads/main"]
	a := core.Candidate{Ref: refA, Target: "main", User: "alice", Topic: "a", SHA: refs[refA]}
	b := core.Candidate{Ref: refB, Target: "main", User: "bob", Topic: "b", SHA: refs[refB]}

	link1, _, err := d.buildChainLink(ctx, ctx, "main", base, a, chainOnClean)
	if err != nil {
		t.Fatalf("link1: %v", err)
	}
	if _, _, err := d.buildChainLink(ctx, ctx, "main", link1.mergeOID, b, chainOnClean); err != nil {
		t.Fatalf("link2: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("MergeBody calls = %d, want 2 (once per link)", len(calls))
	}
	if calls[0].cand.SHA != a.SHA || calls[0].base != base {
		t.Fatalf("call 0 = %+v, want cand.SHA=%s base=%s", calls[0], a.SHA, base)
	}
	if calls[1].cand.SHA != b.SHA || calls[1].base != link1.mergeOID {
		t.Fatalf("call 1 = %+v, want cand.SHA=%s base=%s (the chained, unpushed link)", calls[1], b.SHA, link1.mergeOID)
	}
}
