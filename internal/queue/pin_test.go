// GC-pin lifecycle suite: every active run pins its chain tip
// (startRun/finishBatchStart), every terminal path releases the pin, and a
// landing's pin survives until the next successful Fetch anchors the chain
// through the remote-tracking target ref. Built on the fake harness — the
// fake's pins map mirrors gitx's refs/gauntlet/pin/* namespace, whose real
// gc-survival semantics gitx's own tests prove; the queue-level property
// under test here is purely *when* Pin and Unpin happen.
package queue

import (
	"errors"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestPin_ActiveRunPinsChainTip(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial clean; "test" started; pin must already exist
	r := h.d.headRun("main")
	if r == nil {
		t.Fatal("no in-flight run after reconcile")
	}
	if !h.git.pinned(r.chainTip) {
		t.Fatalf("chain tip %s not pinned while its check is in flight", r.chainTip)
	}
	if n := h.git.pinCount(); n != 1 {
		t.Fatalf("pinCount = %d while one run is in flight, want 1", n)
	}
}

func TestPin_LandedRunReleasesPinOnNextFetch(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	tip := h.d.headRun("main").chainTip
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // green: lands this tick

	// The landing tick must NOT release the pin: the chain only becomes
	// locally reachable through the remote-tracking target ref at the next
	// successful Fetch, and a queued post-land hook may export the merge in
	// between.
	if !h.git.pinned(tip) {
		t.Fatalf("landed chain tip %s unpinned before the next fetch", tip)
	}

	h.reconcile() // next tick's Fetch succeeded: the pin has done its job
	if h.git.pinned(tip) {
		t.Fatalf("landed chain tip %s still pinned after the post-land fetch", tip)
	}
	if n := h.git.pinCount(); n != 0 {
		t.Fatalf("pinCount = %d after land + fetch, want 0", n)
	}
}

func TestPin_LandedPinWaitsForAnchoredFetch(t *testing.T) {
	h := newHarness(t)
	base := h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	tip := h.d.headRun("main").chainTip
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed}) // lands; target now at tip

	// Simulate a lagging fetch: the next tick's view of the target still
	// shows the pre-push tip (a read replica serving stale refs). "A fetch
	// succeeded" alone must NOT release the pin — the fetched ref doesn't
	// anchor the landing yet.
	h.git.setRef("refs/heads/main", base)
	h.reconcile()
	if !h.git.pinned(tip) {
		t.Fatalf("landed chain tip %s unpinned on a fetch that did not anchor it", tip)
	}

	// The replica catches up; the fetched target ref reaches the tip and
	// the pin is released.
	h.git.setRef("refs/heads/main", tip)
	h.reconcile()
	if h.git.pinned(tip) {
		t.Fatalf("landed chain tip %s still pinned after an anchoring fetch", tip)
	}
}

func TestPin_AmbiguousPushFailureRetainsPin(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	tip := h.d.headRun("main").chainTip
	runID := h.currentRunID()

	// The land push fails ambiguously (a client-visible error that is NOT a
	// stale lease — the push may or may not have applied server-side). The
	// run Skips, but the pin must be retained: if the push DID apply, this
	// chain is the real target tip, and nothing local anchors it until a
	// fetch reflects that.
	h.git.casErr = errors.New("remote hung up unexpectedly")
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if !h.git.pinned(tip) {
		t.Fatalf("chain tip %s unpinned after an ambiguous land-push failure", tip)
	}

	// Here the push genuinely did not apply: the ref never anchors the tip,
	// so the pin deliberately rides (startup's sweep is its bound) while
	// the re-trial proceeds with its own fresh pin.
	h.git.casErr = nil
	h.reconcile()
	if !h.git.pinned(tip) {
		t.Fatalf("unanchored ambiguous-push pin %s was released; it must ride until startup's sweep", tip)
	}
	if r := h.d.headRun("main"); r == nil || r.chainTip == tip {
		t.Fatalf("expected a fresh re-trial run with its own chain tip, got %+v", r)
	}
}

func TestPin_RedRunReleasesPinAtTerminal(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	tip := h.d.headRun("main").chainTip
	if !h.git.pinned(tip) {
		t.Fatalf("chain tip %s not pinned while in flight", tip)
	}
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // red: parks this tick

	if n := h.git.pinCount(); n != 0 {
		t.Fatalf("pinCount = %d after a red terminal, want 0 (finalizeRun must unpin)", n)
	}
}

func TestPin_SpecRejectReleasesPin(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	// A tree with no check spec at all: startRun pins the merge, then
	// ReadFileFromTree fails and rejectRun must release that pin.
	h.git.pushCandidate(ref, "", map[string]string{"f.txt": "1\n"})

	h.reconcile()
	if n := h.git.pinCount(); n != 0 {
		t.Fatalf("pinCount = %d after a missing-spec reject, want 0 (rejectRun must unpin)", n)
	}
	recs := h.ch.Records()
	if len(recs) == 0 || recs[len(recs)-1].Outcome != core.OutcomeRejected {
		t.Fatalf("expected an OutcomeRejected record for the missing spec, got %+v", recs)
	}
}

func TestPin_PinFailureIsInfraError(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.git.pinErr = errors.New("update-ref exploded")

	h.reconcile()
	recs := h.ch.Records()
	if len(recs) == 0 {
		t.Fatal("expected a terminal record for the pin failure")
	}
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error (a pin failure is daemon-side infrastructure, never a red verdict)", last.Outcome)
	}
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventCheckStarted {
			t.Fatal("a check started despite the pin failing; nothing may read through an unpinned merge")
		}
	}
}

func TestPin_BatchPinsOnlyChainTip(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	h.git.seed("main", checkSpecFile("test"))
	h.git.pushCandidate(candidateRef("main", "alice", "a"), "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(candidateRef("main", "bob", "b"), "", map[string]string{"b.txt": "b\n"})
	h.git.pushCandidate(candidateRef("main", "carol", "c"), "", map[string]string{"c.txt": "c\n"})

	h.reconcile() // all three chain into one batch run
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 3 {
		t.Fatalf("headRun members = %+v, want 3 chained members", r)
	}
	// One pin covers the whole chain — the tip reaches every link through
	// commit parenthood — so intermediate links must not each hold one.
	if !h.git.pinned(r.chainTip) {
		t.Fatalf("batch chain tip %s not pinned", r.chainTip)
	}
	if n := h.git.pinCount(); n != 1 {
		t.Fatalf("pinCount = %d for one 3-member batch, want exactly 1 (the tip)", n)
	}

	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed}) // batch red: members skip, serial fallback

	if n := h.git.pinCount(); n != 0 {
		t.Fatalf("pinCount = %d after batch red, want 0 (finishBatchRed's finalizeRun must unpin)", n)
	}
}

func TestPin_InvalidatedRunReleasesPin(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	tip := h.d.headRun("main").chainTip
	runID := h.currentRunID()
	h.awaitStarted(runID, "test")

	// A human push moves the target mid-test: the validity sweep aborts the
	// run (Invariant 5) — Skip, re-queue — and the aborted chain's pin must
	// not outlive it (the re-trial pins its own new chain tip).
	h.git.directPush("main", map[string]string{"raced.txt": "x\n"})
	h.reconcile() // sweep detects the move, cancels + skips the run, refills against the new tip

	if h.git.pinned(tip) {
		t.Fatalf("invalidated chain tip %s still pinned after the target moved", tip)
	}
}
