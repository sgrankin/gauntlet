package queue

import (
	"errors"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestDrain_IdleReachesDrainedImmediately: draining a daemon with nothing
// in flight completes on the next tick — lifecycle goes straight to
// drained and Run would exit.
func TestDrain_IdleReachesDrainedImmediately(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)

	h.d.Drain(time.Time{})
	h.reconcile()

	if !h.d.drainComplete() {
		t.Fatal("idle drain did not complete on the first tick")
	}
	if got := h.d.Snapshot().Lifecycle; got != LifecycleDrained {
		t.Fatalf("lifecycle = %q, want %q", got, LifecycleDrained)
	}
}

// TestDrain_AdmittedCandidateFinishesAndLands: a candidate already in
// flight at drain entry runs its remaining graph and gets its one landing
// CAS — the drain set finishes, it isn't abandoned.
func TestDrain_AdmittedCandidateFinishesAndLands(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // admit + start the check
	runID := h.currentRunID()

	// Drain now, mid-check. The admitted candidate must still finish.
	// The live state (and Snapshot) reflects the request on the next tick.
	h.d.Drain(time.Time{})
	h.reconcile()
	if got := h.d.Snapshot().Lifecycle; got != LifecycleDraining {
		t.Fatalf("lifecycle = %q with a run still in flight, want draining", got)
	}

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeLanded {
		t.Fatalf("admitted candidate outcome = %v, want Landed", last.Outcome)
	}
	if got := h.git.ref("refs/heads/main"); got == "" {
		t.Fatal("target did not advance; the admitted landing CAS didn't happen")
	}
	h.reconcile()
	if !h.d.drainComplete() {
		t.Fatal("drain did not complete after the admitted set landed")
	}
}

// TestDrain_NoNewCandidateAdmitted: a candidate waiting behind the admitted
// one (serial mode: one in flight) must NOT start once draining — the
// admission boundary is new candidates.
func TestDrain_NoNewCandidateAdmitted(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	first := candidateRef("main", "alice", "one")
	h.git.pushCandidate(first, "", checkSpecFile("test"))
	h.reconcile()
	runID := h.currentRunID()

	// A second candidate is pushed, then we drain before it can be admitted
	// (serial: the first still holds the lane).
	second := candidateRef("main", "bob", "two")
	h.git.pushCandidate(second, "", checkSpecFile("test"))
	h.d.Drain(time.Time{})

	// Land the first. In a non-draining daemon the second would now be
	// admitted; draining, it must not be.
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	h.reconcile()
	h.reconcile()

	if !h.d.drainComplete() {
		t.Fatal("drain did not complete; a waiting candidate was admitted")
	}
	// The second ref is untouched and still discoverable for the next start.
	if !h.git.hasRef(second) {
		t.Fatal("unadmitted candidate ref was consumed during drain")
	}
	for _, r := range h.ch.Records() {
		if r.Candidate.Ref == second && r.Outcome == core.OutcomeLanded {
			t.Fatal("a candidate not admitted before drain landed anyway")
		}
	}
}

// TestDrain_Idempotent: repeated Drain calls never resume admission and
// keep the lifecycle monotonic.
func TestDrain_Idempotent(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.d.Drain(time.Time{})
	h.d.Drain(time.Time{})
	h.reconcile()
	h.d.Drain(time.Time{}) // a repeat after draining must not re-admit

	// The candidate pushed before the drain was never admitted (drain was
	// requested before the first reconcile), so the lane stays empty.
	h.reconcile()
	if !h.d.drainComplete() {
		t.Fatal("repeated drain resumed admission or never completed")
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate admitted despite drain-before-first-tick")
	}
}

// TestDrain_AutoRetrySuppressed: an infra error during drain must not
// auto-retry (that would put fresh work in the finite set); the park
// stands for the next daemon start.
func TestDrain_AutoRetrySuppressed(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.AutoRetryErrors = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // admit + start
	runID := h.currentRunID()
	h.d.Drain(time.Time{})

	// A check that reports an infra error (Err set) → OutcomeError park.
	h.release(runID, "test", core.CheckResult{Name: "test", Err: errors.New("executor unreachable")})

	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeError {
		t.Fatalf("outcome = %v, want Error", last.Outcome)
	}
	// Auto-retry would re-admit and re-run; draining suppresses it, so the
	// drain completes with the candidate parked.
	before := h.git.mergeTreeCalls
	h.reconcile()
	h.reconcile()
	if h.git.mergeTreeCalls != before {
		t.Fatal("candidate was auto-retried (re-admitted) during drain")
	}
	if !h.d.drainComplete() {
		t.Fatal("drain did not complete after the infra-error park")
	}
}
