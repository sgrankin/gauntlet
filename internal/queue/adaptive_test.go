package queue

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

func TestAdaptiveSpeculationGrowsOnUsefulLandingsAndRecoversState(t *testing.T) {
	target := speculateTarget(4)
	target.Adaptive = &config.AdaptiveScheduling{Start: 1, Successes: 1}
	h := newHarness(t, target)
	h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
	pushThreeSpeculateCandidates(h)
	h.reconcile()
	l := h.d.lanes["main"]
	if len(l.runs) != 1 {
		t.Fatalf("initial window: %d", len(l.runs))
	}
	first := l.runs[0]
	h.release(first.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: 10 * time.Second})
	h.reconcile()
	if g := h.d.controls.Governors["main"]; g.Limit != 2 || g.Landed != 1 || g.UsefulSeconds != 10 {
		t.Fatalf("green feedback: %+v", g)
	}
	l = h.d.lanes["main"]
	if len(l.runs) != 2 {
		t.Fatalf("window did not expand: %d", len(l.runs))
	}
	second, third := l.runs[0], l.runs[1]
	h.awaitStarted(third.runID, "test")
	h.release(second.runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed, Duration: 2 * time.Second})
	h.reconcile()
	g := h.d.controls.Governors["main"]
	if g.Limit != 1 || g.Failed != 1 {
		t.Fatalf("red feedback: %+v", g)
	}
	stored, err := loadControls(h.d.cfg.ControlPath)
	if err != nil || stored.Governors["main"].Limit != 1 {
		t.Fatalf("recovery: %+v %v", stored.Governors, err)
	}
	snap := h.d.Snapshot().Targets[0].Scheduling
	if snap == nil || snap.Limit != 1 || snap.Ceiling != 4 {
		t.Fatalf("snapshot: %+v", snap)
	}
}
func TestAdaptiveBatchSizeGrowsThenBisectsRealFailures(t *testing.T) {
	target := batchTarget(8)
	target.OnBatchRed = "bisect"
	target.Adaptive = &config.AdaptiveScheduling{Start: 2, Successes: 1}
	h := newHarness(t, target)
	h.git.seed("main", checkSpecFile("test"))
	for i := 0; i < 8; i++ {
		h.git.pushCandidate(candidateRef("main", "user", fmt.Sprint(i)), "", map[string]string{fmt.Sprint(i): "change"})
	}
	h.reconcile()
	r := h.d.headRun("main")
	if len(r.members) != 2 {
		t.Fatalf("start size: %d", len(r.members))
	}
	h.release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	h.reconcile()
	r = h.d.headRun("main")
	if r == nil || len(r.members) != 3 {
		t.Fatalf("grown batch: %+v", r)
	}
	h.release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed})
	h.reconcile()
	if g := h.d.controls.Governors["main"]; g.Limit != 1 || g.Failed != 1 {
		t.Fatalf("batch failure feedback: %+v", g)
	}
	if r = h.d.headRun("main"); r != nil && len(r.members) != 1 {
		t.Fatalf("recovery size: %d", len(r.members))
	}
}
func TestAdaptiveEvidenceRetriesWaiversAndCapacity(t *testing.T) {
	h := newHarness(t)
	target := speculateTarget(8)
	target.Adaptive = &config.AdaptiveScheduling{Start: 1, Successes: 1}
	h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
	makeRun := func(id string, result core.CheckResult) *run {
		return &run{runID: id, members: []runMember{{}}, results: map[string]core.CheckResult{"test": result}}
	}
	h.d.governorFeedback(target, makeRun("cached", core.CheckResult{Status: core.CheckPassed, Reused: true, Duration: time.Hour}), core.OutcomeLanded)
	if h.d.governor(target).Limit != 1 || h.d.governor(target).UsefulSeconds != 0 {
		t.Fatal("cached evidence inflated useful work")
	}
	h.d.governorFeedback(target, makeRun("retry", core.CheckResult{Status: core.CheckPassed, TestAttempts: []core.TestAttempt{{Number: 1}, {Number: 2}}}), core.OutcomeLanded)
	g := h.d.governor(target)
	if g.Limit != 1 || g.Retried != 1 {
		t.Fatalf("retry feedback: %+v", g)
	}
	r := makeRun("new", core.CheckResult{Status: core.CheckPassed, Duration: time.Second})
	h.d.governorFeedback(target, r, core.OutcomeLanded)
	h.d.governorFeedback(target, r, core.OutcomeLanded)
	if g = h.d.governor(target); g.Limit != 2 || g.UsefulSeconds != 1 {
		t.Fatalf("duplicate feedback: %+v", g)
	}
	h.d.cfg.ExecutionPressure = func(_ context.Context) bool { return true }
	if n := h.d.adaptiveLimit(t.Context(), target); n != 1 {
		t.Fatalf("pressure admission: %d", n)
	}
	if h.d.governor(target).Limit != 2 {
		t.Fatal("temporary pressure changed learned limit")
	}
	waived := makeRun("waived", core.CheckResult{Status: core.CheckPassed})
	waived.emergencyReason = "incident"
	h.d.governorFeedback(target, waived, core.OutcomeLanded)
	if h.d.governor(target).Limit != 2 {
		t.Fatal("waiver trained governor")
	}
	reusable := makeRun("reusable", core.CheckResult{Status: core.CheckPassed, Duration: time.Hour, VerificationID: "kept"})
	h.d.governorFeedback(target, reusable, core.OutcomeSkipped)
	if g = h.d.governor(target); g.Limit != 2 || g.WastedSeconds != 0 {
		t.Fatalf("retained verification marked waste: %+v", g)
	}
	cloned := h.d.controls.clone()
	g = cloned.Governors["main"]
	g.Recent[0] = "changed"
	if h.d.controls.Governors["main"].Recent[0] == "changed" {
		t.Fatal("clone aliases deduplication history")
	}
}

func TestAdaptiveRecoveredTrialCountsLandingAfterEarlierInvalidation(t *testing.T) {
	h := newHarness(t)
	target := speculateTarget(4)
	target.Adaptive = &config.AdaptiveScheduling{Start: 4, Successes: 1}
	r := &run{runID: "recovered-exact-trial", members: []runMember{{}}, results: map[string]core.CheckResult{"test": {Status: core.CheckPassed, Duration: time.Second}}}
	h.d.governorFeedback(target, r, core.OutcomeSkipped)
	if g := h.d.governor(target); g.Limit != 2 || g.Invalidated != 1 {
		t.Fatalf("invalidation: %+v", g)
	}
	h.d.governorFeedback(target, r, core.OutcomeLanded)
	if g := h.d.governor(target); g.Limit != 3 || g.Landed != 1 {
		t.Fatalf("recovered landing suppressed: %+v", g)
	}
}
