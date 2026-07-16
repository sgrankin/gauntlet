// Real-git GC-pin suite: the fake-harness tests (pin_test.go) prove WHEN
// the queue pins and unpins; these prove the pin actually does its job
// against genuine git maintenance — the trial merge stays resolvable
// through the most aggressive collection an operator can run
// (`git gc --prune=now`, no grace period) for exactly as long as a run
// needs it, and becomes collectable garbage afterwards.
package queue

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// gitDirQuery runs one git query against the daemon's bare repo exactly the
// way a check script would through GAUNTLET_GIT_DIR.
func (h *integrationHarness) gitDirQuery(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"--git-dir=" + h.dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (h *integrationHarness) pinRefs() []string {
	h.t.Helper()
	out, err := h.gitDirQuery("for-each-ref", "--format=%(refname)", "refs/gauntlet/pin/")
	if err != nil {
		h.t.Fatalf("for-each-ref pins: %v: %s", err, out)
	}
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestIntegration_PinnedTrialSurvivesGCPruneNow(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	h.remote.Seed("main", checkSpecFile("test"))
	h.remote.PushCandidate("main", "alice", "widget", map[string]string{"a.txt": "a\n"})

	h.reconcile() // trial merged, pinned, exported; "test" now blocked on the gate
	runID := h.currentRunID()
	h.awaitStarted(gated, runID, "test")
	r := h.d.headRun("main")
	if r == nil {
		t.Fatal("no in-flight run")
	}
	base, tip := r.baseOID, r.chainTip

	// The operator runs the most aggressive maintenance possible while the
	// check is mid-flight. Without the pin, the trial merge is an
	// unreferenced loose object and this collects it.
	testutil.GCPruneNow(h.t, h.dir)

	// The exact queries docs/checks.md tells check scripts to run against
	// $GAUNTLET_GIT_DIR must still work mid-check.
	if out, err := h.gitDirQuery("cat-file", "-e", tip); err != nil {
		t.Fatalf("merge commit %s lost to gc --prune=now mid-check: %v %s", tip, err, out)
	}
	if out, err := h.gitDirQuery("diff", "--name-only", base, tip); err != nil {
		t.Fatalf("git diff %s %s after gc: %v: %s", base, tip, err, out)
	} else if !strings.Contains(out, "a.txt") {
		t.Fatalf("git diff --name-only = %q, want it to name a.txt", out)
	}

	// Green: the run lands. The pin must survive the landing tick itself
	// (a queued post-land hook may still export the merge) and be released
	// once a later fetch shows the chain anchored by the remote-tracking
	// target ref instead.
	h.releaseGated(gated, runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if pins := h.pinRefs(); len(pins) != 1 {
		t.Fatalf("pins right after landing = %v, want exactly the landed tip's (released only at the next fetch)", pins)
	}
	h.reconcile()
	if pins := h.pinRefs(); len(pins) != 0 {
		t.Fatalf("pins after post-land fetch = %v, want none", pins)
	}

	// With the pin gone the landed merge must STILL survive aggressive gc —
	// reachability now flows from refs/remotes/origin/main, proving the
	// pin-release handoff happened only once ground truth itself anchored
	// the landing.
	testutil.GCPruneNow(h.t, h.dir)
	if out, err := h.gitDirQuery("cat-file", "-e", tip); err != nil {
		t.Fatalf("landed merge %s lost to gc after pin release: %v %s", tip, err, out)
	}
}

func TestIntegration_RedRunPinReleasedAndCollectable(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	h.remote.Seed("main", checkSpecFile("test"))
	h.remote.PushCandidate("main", "alice", "widget", map[string]string{"a.txt": "a\n"})

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(gated, runID, "test")
	tip := h.d.headRun("main").chainTip

	h.releaseGated(gated, runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed})
	if pins := h.pinRefs(); len(pins) != 0 {
		t.Fatalf("pins after red terminal = %v, want none", pins)
	}
	// A rejected trial's merge is garbage by design; nothing may anchor it.
	testutil.GCPruneNow(h.t, h.dir)
	if _, err := h.gitDirQuery("cat-file", "-e", tip); err == nil {
		t.Fatalf("rejected trial merge %s survived gc --prune=now; something still anchors it", tip)
	}
}
