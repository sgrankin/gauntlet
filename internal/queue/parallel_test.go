// Dependency-aware parallel scheduling suite (`after` edges, max-parallel,
// the daemon-wide execution cap): diamond overlap, fail-fast with blocked
// rows, slot starvation with Waited accounting, and cancellation with
// several nodes running. Built on the fake harness; the gated executor's
// (RunID, Name)-keyed gates make concurrent in-flight checks exactly as
// steppable as serial ones.
package queue

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// parallelSpecFile renders a .gauntlet.kdl with max-parallel and per-check
// after edges: each entry is "name" or "name:dep1+dep2".
func parallelSpecFile(maxParallel int, entries ...string) map[string]string {
	var b strings.Builder
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

// started reports whether (runID, name) has registered with the gated
// executor, without blocking — the negative-assertion form of
// awaitStarted (see its doc: a never-started check is a standing logical
// guarantee, not a race).
func (h *testHarness) started(runID, name string) bool {
	select {
	case <-h.exec.Started(runID, name):
		return true
	default:
		return false
	}
}

func TestParallel_DiamondOverlapAndJoin(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", parallelSpecFile(4, "unit", "lint", "package:unit+lint"))

	h.reconcile() // both roots start together; package waits on its edges
	runID := h.currentRunID()
	h.awaitStarted(runID, "unit")
	h.awaitStarted(runID, "lint")
	if h.started(runID, "package") {
		t.Fatal("package started before its prerequisites finished")
	}

	h.release(runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckPassed})
	if h.started(runID, "package") {
		t.Fatal("package started with unit still running (after \"unit\" \"lint\" demands both)")
	}

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.awaitStarted(runID, "package") // join satisfied
	h.release(runID, "package", core.CheckResult{Name: "package", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	// Spec-declaration order is the durable row identity, regardless of
	// the order results arrived (lint finished before unit here).
	wantOrder := []string{"unit", "lint", "package"}
	if len(last.Checks) != 3 {
		t.Fatalf("Checks = %+v, want 3 rows", last.Checks)
	}
	for i, want := range wantOrder {
		if last.Checks[i].Name != want {
			t.Errorf("Checks[%d] = %q, want %q (spec order, not completion order)", i, last.Checks[i].Name, want)
		}
	}
}

func TestParallel_FailFastCancelsSiblingsAndBlocksDependents(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", parallelSpecFile(2, "a", "b", "c:b"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "a")
	h.awaitStarted(runID, "b")

	// a goes red while b is mid-flight: fail fast — b is cancelled, c never
	// becomes ready, and the run rejects with a (not whichever row appended
	// last) as the explicit culprit.
	h.release(runID, "a", core.CheckResult{Name: "a", Status: core.CheckFailed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if !strings.Contains(last.Detail, `check "a" failed`) {
		t.Fatalf("Detail = %q, want the culprit named explicitly", last.Detail)
	}
	if h.started(runID, "c") {
		t.Fatal("c started despite its prerequisite chain failing")
	}
	if len(last.Checks) != 3 {
		t.Fatalf("Checks = %+v, want one row per declared check", last.Checks)
	}
	if last.Checks[0].Name != "a" || last.Checks[0].Status != core.CheckFailed {
		t.Errorf("Checks[0] = %+v, want a's red verdict", last.Checks[0])
	}
	// b was in flight with no failed edge of its own: blocked by the run's
	// root failure. c's proximate cause is its own unfinished edge, b.
	if b := last.Checks[1]; b.Status != core.CheckBlocked || len(b.BlockedBy) != 1 || b.BlockedBy[0] != "a" {
		t.Errorf("Checks[1] (b) = %+v, want blocked by the root failure a", b)
	}
	if c := last.Checks[2]; c.Status != core.CheckBlocked || len(c.BlockedBy) != 1 || c.BlockedBy[0] != "b" {
		t.Errorf("Checks[2] (c) = %+v, want blocked by its own edge b", c)
	}
}

func TestParallel_ExecutionCapStarvesAndRecordsWaited(t *testing.T) {
	h := newHarness(t)
	// One slot for the whole daemon: with max-parallel 2, the run may WANT
	// both roots at once but only one can hold a slot. Installed before
	// the first reconcile, so no reconcile-goroutine race.
	h.d.cfg.Slots = core.NewSlots(1)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", parallelSpecFile(2, "a", "b"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "a")
	if h.started(runID, "b") {
		t.Fatal("b started with the single execution slot already held by a")
	}
	// More ticks don't help while a holds the slot.
	h.reconcile()
	if h.started(runID, "b") {
		t.Fatal("b started without a free slot")
	}

	h.release(runID, "a", core.CheckResult{Name: "a", Status: core.CheckPassed}) // frees the slot; b starts
	h.awaitStarted(runID, "b")
	h.release(runID, "b", core.CheckResult{Name: "b", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if last.Checks[0].Waited != 0 {
		t.Errorf("a.Waited = %v, want 0 (started immediately)", last.Checks[0].Waited)
	}
	if last.Checks[1].Waited <= 0 {
		t.Errorf("b.Waited = %v, want > 0 (sat ready while a held the slot)", last.Checks[1].Waited)
	}
}

func TestParallel_CancelWhileSeveralRunning(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", parallelSpecFile(3, "a", "b", "c"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "a")
	h.awaitStarted(runID, "b")
	h.awaitStarted(runID, "c")

	// Operator cancel with three nodes running: every node is cancelled,
	// the ref parks at its SHA, and the record carries no fabricated
	// verdicts — nothing finished, so no check rows.
	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: ref})
	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected (operator cancel parks)", last.Outcome)
	}
	if len(last.Checks) != 0 {
		t.Fatalf("Checks = %+v, want none (nothing finished; a cancel attributes no failure)", last.Checks)
	}
	if h.d.headRun("main") != nil {
		t.Fatal("lane still holds a run after cancel")
	}
}

func TestParallel_SkippedPrerequisiteCountsGreen(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", parallelSpecFile(2, "affected", "deploy:affected"))

	h.reconcile()
	runID := h.currentRunID()
	// A prerequisite reporting skipped (its own successful nothing-to-do
	// verdict) satisfies an after edge exactly like passed.
	h.release(runID, "affected", core.CheckResult{Name: "affected", Status: core.CheckSkipped})
	h.awaitStarted(runID, "deploy")
	h.release(runID, "deploy", core.CheckResult{Name: "deploy", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if last.Checks[0].Status != core.CheckSkipped {
		t.Errorf("Checks[0] = %+v, want the honest Skipped row", last.Checks[0])
	}
}

// TestParallel_WaitedSurvivesToHistoryShape sanity-checks the Waited value
// flows through the terminal record with the run's injected clock (one
// second per now() call), not wall time.
func TestParallel_WaitedIsClockDerived(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.Slots = core.NewSlots(1)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", parallelSpecFile(2, "a", "b"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "a")
	h.release(runID, "a", core.CheckResult{Name: "a", Status: core.CheckPassed})
	h.awaitStarted(runID, "b")
	h.release(runID, "b", core.CheckResult{Name: "b", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	waited := last.Checks[1].Waited
	if waited <= 0 || waited > time.Hour {
		t.Fatalf("b.Waited = %v, want a positive injected-clock duration", waited)
	}
}
