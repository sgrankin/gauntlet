package queue

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// isolatedSpecFile renders a .gauntlet.kdl with workspace "isolated",
// max-parallel, and the given checks (each "name" or "name:dep1+dep2").
func isolatedSpecFile(maxParallel int, entries ...string) map[string]string {
	var b strings.Builder
	b.WriteString("workspace \"isolated\"\n")
	fmt.Fprintf(&b, "max-parallel %d\n", maxParallel)
	for _, e := range entries {
		name, deps, _ := strings.Cut(e, ":")
		fmt.Fprintf(&b, "check %q {\n    command \"true\"\n", name)
		if deps != "" {
			fmt.Fprintf(&b, "    after")
			for _, d := range strings.Split(deps, "+") {
				fmt.Fprintf(&b, " %q", d)
			}
			fmt.Fprintf(&b, "\n")
		}
		fmt.Fprintf(&b, "}\n")
	}
	return map[string]string{testCheckSpecPath: b.String()}
}

// TestIsolated_MaterializesPerNodeNotRunLevel: isolated mode does NOT
// export a shared run-level tree; instead each node exports its own once
// it starts. Two parallel checks therefore drive exactly two exports.
func TestIsolated_MaterializesPerNodeNotRunLevel(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", isolatedSpecFile(2, "a", "b"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "a")
	h.awaitStarted(runID, "b")

	// No run-level export (shared mode would have made exactly 1 before any
	// check started); two node materializations, one per started check.
	if got := h.git.exportCalls; got != 2 {
		t.Fatalf("exportCalls = %d, want 2 (one private workspace per node, no shared run export)", got)
	}

	h.release(runID, "a", core.CheckResult{Name: "a", Status: core.CheckPassed})
	h.release(runID, "b", core.CheckResult{Name: "b", Status: core.CheckPassed})
	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
}

// TestShared_ExportsOncePerRun pins the compatibility default: shared mode
// still makes exactly one run-level export handed to every check, even at
// max-parallel 1.
func TestShared_ExportsOncePerRun(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	// parallelSpecFile has no workspace node → shared (the default).
	h.git.pushCandidate(ref, "", parallelSpecFile(2, "a", "b"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "a")
	h.awaitStarted(runID, "b")

	if got := h.git.exportCalls; got != 1 {
		t.Fatalf("exportCalls = %d, want 1 (shared mode: one export per run)", got)
	}
	h.release(runID, "a", core.CheckResult{Name: "a", Status: core.CheckPassed})
	h.release(runID, "b", core.CheckResult{Name: "b", Status: core.CheckPassed})
}

// TestIsolated_MaterializeFailureIsError: a per-node export failure is an
// infrastructure failure (OutcomeError, park-as-error), never a source
// rejection and never a fallback to a shared dir. The run itself starts
// fine (isolated mode does no run-level export), then the node's
// materialization fails inside its goroutine.
func TestIsolated_MaterializeFailureIsError(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", isolatedSpecFile(1, "a"))
	h.git.exportErr = errors.New("archive: no space left on device")

	h.reconcile() // run starts; the node goroutine's materialize fails

	// The errored result lands on the run's result channel; drain it.
	var outcome core.Outcome
	for i := 0; i < 100000; i++ {
		h.reconcile()
		if recs := h.ch.Records(); len(recs) > 0 {
			outcome = recs[len(recs)-1].Outcome
			if outcome == core.OutcomeError {
				break
			}
		}
		runtime.Gosched()
	}
	if outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error on a materialization failure", outcome)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot removed on an infra error")
	}
}
