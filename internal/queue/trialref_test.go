package queue

import (
	"time"

	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// enableTrialRefs turns on issue #7's publication for a harness-built
// daemon, mirroring how the infra tests flip HistoryMtimes post-construct.
func enableTrialRefs(h *testHarness, retention time.Duration) {
	h.d.cfg.TrialRefs = true
	h.d.cfg.TrialRefPrefix = "refs/gauntlet/trials"
	h.d.cfg.TrialRefRetention = retention
}

func eventsOfKind(h *testHarness, k core.EventKind) []core.Event {
	var out []core.Event
	for _, e := range h.ch.Events() {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// TestTrialRef_PublishVerifyLand is the whole happy path: a run publishes
// its merge under the trial ref and fires EventTrialMerged before checks;
// green fires EventVerified (before the land) at the merge SHA; landing
// CAS-deletes the now-redundant ref.
func TestTrialRef_PublishVerifyLand(t *testing.T) {
	h := newHarness(t)
	enableTrialRefs(h, time.Hour)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile() // trial starts: merge published, EventTrialMerged emitted
	runID := h.currentRunID()
	trialRef := "refs/gauntlet/trials/" + runID

	merged := eventsOfKind(h, core.EventTrialMerged)
	if len(merged) != 1 {
		t.Fatalf("EventTrialMerged count = %d, want 1", len(merged))
	}
	if merged[0].Record == nil || merged[0].Record.MergeSHA == "" {
		t.Fatalf("EventTrialMerged carries no MergeSHA: %+v", merged[0].Record)
	}
	publishedMerge := merged[0].Record.MergeSHA
	if got := h.git.ref(trialRef); got != publishedMerge {
		t.Fatalf("trial ref = %q, want the published merge %q", got, publishedMerge)
	}

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	verified := eventsOfKind(h, core.EventVerified)
	if len(verified) != 1 {
		t.Fatalf("EventVerified count = %d, want 1", len(verified))
	}
	if verified[0].Record == nil || verified[0].Record.MergeSHA != publishedMerge {
		t.Fatalf("EventVerified MergeSHA = %+v, want %q", verified[0].Record, publishedMerge)
	}

	// Landed, and the redundant trial ref is gone.
	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if h.git.hasRef(trialRef) {
		t.Fatalf("trial ref %s survived the landing, want deleted", trialRef)
	}
}

// TestTrialRef_VerifiedPrecedesLanded pins the ordering the "green before
// ref update" policy depends on: EventVerified is emitted strictly before
// the run's EventLanded.
func TestTrialRef_VerifiedPrecedesLanded(t *testing.T) {
	h := newHarness(t)
	enableTrialRefs(h, time.Hour)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	verifiedAt, landedAt := -1, -1
	for i, e := range h.ch.Events() {
		switch e.Kind {
		case core.EventVerified:
			verifiedAt = i
		case core.EventLanded:
			landedAt = i
		}
	}
	if verifiedAt < 0 || landedAt < 0 {
		t.Fatalf("missing events: verified@%d landed@%d", verifiedAt, landedAt)
	}
	if verifiedAt >= landedAt {
		t.Fatalf("EventVerified (@%d) must precede EventLanded (@%d)", verifiedAt, landedAt)
	}
}

// TestTrialRef_RejectRetainsThenReaps: a red run keeps its trial ref for
// the retention window (a failed synthetic merge stays inspectable), then
// the per-tick reaper CAS-deletes it once the window elapses.
func TestTrialRef_RejectRetainsThenReaps(t *testing.T) {
	h := newHarness(t)
	enableTrialRefs(h, 10*time.Second)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()
	runID := h.currentRunID()
	trialRef := "refs/gauntlet/trials/" + runID

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed})

	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	// Retained right after the terminal: still inspectable.
	if !h.git.hasRef(trialRef) {
		t.Fatalf("trial ref %s deleted immediately on a red verdict, want retained", trialRef)
	}

	// The reaper only acts once the retention window has elapsed. h.now()
	// advances one second per call; spin reconciles until it does.
	for i := 0; i < 40 && h.git.hasRef(trialRef); i++ {
		h.reconcile()
	}
	if h.git.hasRef(trialRef) {
		t.Fatalf("trial ref %s never reaped after its retention window", trialRef)
	}
}

// TestTrialRef_DisabledIsUnchanged: with the feature off (the default), no
// trial ref is published and neither new event fires — the disabled-mode
// event stream stays byte-identical to today's.
func TestTrialRef_DisabledIsUnchanged(t *testing.T) {
	h := newHarness(t) // TrialRefs off by default
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	if got := eventsOfKind(h, core.EventTrialMerged); len(got) != 0 {
		t.Fatalf("EventTrialMerged fired with the feature off: %d", len(got))
	}
	if got := eventsOfKind(h, core.EventVerified); len(got) != 0 {
		t.Fatalf("EventVerified fired with the feature off: %d", len(got))
	}
	if n := h.git.lsRemoteCalls; n != 0 {
		t.Fatalf("ListRemoteRefs called %d times with the feature off", n)
	}
	for r := range h.git.refs {
		if len(r) >= 14 && r[:14] == "refs/gauntlet/" {
			t.Fatalf("a gauntlet-namespace ref %q exists with the feature off", r)
		}
	}
}

// TestTrialRef_PublishCollisionIsInfraError: a trial ref already present at
// a DIFFERENT SHA (a second daemon, a run-id reuse) is an operational
// collision — the run rejects as OutcomeError (park + retry), never
// force-overwriting the ref.
func TestTrialRef_PublishCollisionIsInfraError(t *testing.T) {
	h := newHarness(t)
	enableTrialRefs(h, time.Hour)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))

	// Pre-occupy the ref the run is about to publish. Run IDs are
	// content-derived, so mint it the way the daemon will: newRunID over
	// the trial tree OID at the daemon's next clock tick. Simpler: run
	// once to discover the ID, tear down, and re-seed — instead, seize
	// via beforeCAS on the create.
	seized := false
	h.git.beforeCAS = func(remoteRef string) {
		if !seized && len(remoteRef) > 20 && remoteRef[:21] == "refs/gauntlet/trials/" {
			seized = true
			h.git.mu.Lock()
			h.git.refs[remoteRef] = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
			h.git.mu.Unlock()
		}
	}

	h.reconcile()

	if !seized {
		t.Fatal("no trial-ref create observed; test exercised nothing")
	}
	recs := h.ch.Records()
	if len(recs) == 0 || recs[len(recs)-1].Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %+v, want Error on a trial-ref collision", recs)
	}
	if !h.git.hasRef(ref) {
		t.Fatal("candidate slot removed on an infra error")
	}
}
