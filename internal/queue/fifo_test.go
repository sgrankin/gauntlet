package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestReconcile_FIFOArrivalOrder proves arrival order beats a candidate's
// ref name: a lexically-later ref that arrives first still holds the lane
// against a lexically-earlier one that arrives while the first is in
// flight.
func TestReconcile_FIFOArrivalOrder(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)

	refFirstArrival := candidateRef("main", "zeta", "second") // lexically later
	h.git.pushCandidate(refFirstArrival, "", checkSpecFile("test"))
	h.reconcile() // the only candidate: its trial starts
	runIDFirst := h.currentRunID()

	refSecondArrival := candidateRef("main", "alpha", "first") // lexically earlier, arrives second
	h.git.pushCandidate(refSecondArrival, "", checkSpecFile("test"))
	h.reconcile() // one lane: the in-flight run still owns it; the new arrival just queues

	h.awaitStarted(runIDFirst, "test") // the first arrival's check is already running

	h.release(runIDFirst, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // rejects + parks; frees the lane
	h.reconcile()                                                                           // next tick: the second arrival's trial starts

	runIDSecond := h.currentRunID()
	if runIDSecond == runIDFirst {
		t.Fatal("no new run started for the second arrival after the first vacated")
	}
	// FIFO arrival order wins despite the second arrival's lexically
	// earlier ref.
	h.awaitStarted(runIDSecond, "test")
}

// TestPickHead_LexicalTieBreak exercises the lexical tie-break directly.
// Two refs at equal order can't actually arise through ReconcileOnce itself
// — syncBookkeeping assigns strictly increasing sequence numbers to new
// refs within a batch, in lexical order, so ties never occur in practice —
// but §3's documented contract for pickHead is to tie-break lexically, and
// that must hold regardless of how order got assigned. Constructing the
// tie directly (poking d.order, a package-internal field) is the only way
// to exercise it.
func TestPickHead_LexicalTieBreak(t *testing.T) {
	h := newHarness(t)
	d := h.d
	d.order["main"] = map[string]int64{
		"refs/heads/for/main/zeta/z":  5,
		"refs/heads/for/main/alpha/a": 5,
	}
	d.done["main"] = map[string]string{}
	cands := map[string]core.Candidate{
		"refs/heads/for/main/zeta/z":  {Ref: "refs/heads/for/main/zeta/z", SHA: "sha-z"},
		"refs/heads/for/main/alpha/a": {Ref: "refs/heads/for/main/alpha/a", SHA: "sha-a"},
	}

	got, ok := d.pickHead("main", cands)
	if !ok {
		t.Fatal("pickHead returned ok=false")
	}
	if got.Ref != "refs/heads/for/main/alpha/a" {
		t.Fatalf("pickHead = %q, want the lexically smaller ref at equal order", got.Ref)
	}
}
