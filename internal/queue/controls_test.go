package queue

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestIncidentPausePersistsAndCancelsVerification(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "a")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()
	h.awaitStarted(h.currentRunID(), "test")
	h.ch.SendCommand(core.Command{Kind: core.CommandPause, Target: "main", Actor: "operator", Reason: "incident"})
	h.reconcile()
	if h.d.headRun("main") != nil || h.d.Snapshot().Targets[0].Pause == nil {
		t.Fatal("pause did not cancel and suspend")
	}
	state, err := loadControls(h.d.cfg.ControlPath)
	if err != nil || state.Pauses["main"].Reason != "incident" {
		t.Fatalf("pause not durable: %+v %v", state, err)
	}
	h.reconcile()
	if h.d.headRun("main") != nil {
		t.Fatal("paused work refilled")
	}
	h.ch.SendCommand(core.Command{Kind: core.CommandResume, Target: "main", Actor: "operator", Reason: "recovered"})
	h.reconcile()
	if h.d.Snapshot().Targets[0].Pause != nil || h.d.headRun("main") == nil {
		t.Fatal("resume did not restart")
	}
}

func TestEmergencyMergeIsRevisionBoundAndRecordsWaiver(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "repushed"}[changed], func(t *testing.T) {
			h := newHarness(t)
			h.d.cfg.AllowEmergency = true
			h.git.seed("main", nil)
			ref := candidateRef("main", "alice", "a")
			sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))
			h.ch.SendCommand(core.Command{Kind: core.CommandMergeAnyway, Target: "main", Actor: "operator", Reason: "urgent fix", Revisions: []core.Revision{{Ref: ref, SHA: sha}}})
			if changed {
				h.git.pushCandidate(ref, "", checkSpecFile("test", "other"))
			}
			h.reconcile()
			h.reconcile()
			if changed {
				if !h.git.hasRef(ref) {
					t.Fatal("stale emergency merged new revision")
				}
				return
			}
			if h.git.hasRef(ref) {
				t.Fatal("emergency did not land")
			}
			var landed *core.RunRecord
			for _, rec := range h.ch.Records() {
				if rec.Outcome == core.OutcomeLanded {
					landed = rec
				}
			}
			if landed == nil || !strings.Contains(landed.Detail, "EMERGENCY") || len(landed.Checks) != 1 || landed.Checks[0].Status != core.CheckWaived {
				t.Fatalf("waiver not honestly recorded: %+v", landed)
			}
		})
	}
}

func TestEmergencyPauseOverrideDoesNotResumeQueue(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.AllowEmergency = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "hotfix")
	sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.ch.SendCommand(core.Command{Kind: core.CommandPause, Target: "main", Actor: "operator", Reason: "incident"})
	h.reconcile()
	cmd := core.Command{Kind: core.CommandMergeAnyway, Target: "main", Actor: "operator", Reason: "recovery", Revisions: []core.Revision{{Ref: ref, SHA: sha}}}
	h.ch.SendCommand(cmd)
	h.reconcile()
	if !h.git.hasRef(ref) {
		t.Fatal("merged without pause override")
	}
	cmd.OverridePause = true
	h.ch.SendCommand(cmd)
	h.reconcile()
	h.reconcile()
	if h.git.hasRef(ref) {
		t.Fatal("explicit emergency did not land")
	}
	if h.d.Snapshot().Targets[0].Pause == nil {
		t.Fatal("emergency cleared manual pause")
	}
}

func TestControlPersistenceFailureStopsPublication(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "a")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "missing", "controls.json")
	h.ch.SendCommand(core.Command{Kind: core.CommandPause, Target: "main", Actor: "operator", Reason: "incident"})
	h.reconcile()
	h.reconcile()
	if !h.d.controls.Uncertain || h.d.headRun("main") != nil || !h.git.hasRef(ref) {
		t.Fatal("persistence failure did not suspend queue")
	}
}

func TestPauseOnlyOverrideRunsChecksAndLeavesPause(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.AllowEmergency = true
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "fix")
	sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.ch.SendCommand(core.Command{Kind: core.CommandPause, Target: "main", Actor: "operator", Reason: "incident"})
	h.reconcile()
	h.ch.SendCommand(core.Command{Kind: core.CommandMergePaused, Target: "main", Actor: "operator", Reason: "verified recovery", OverridePause: true, Revisions: []core.Revision{{Ref: ref, SHA: sha}}})
	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "test")
	if !h.git.hasRef(ref) {
		t.Fatal("pause-only override waived verification")
	}
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if h.git.hasRef(ref) || h.d.Snapshot().Targets[0].Pause == nil {
		t.Fatal("verified recovery did not land while preserving pause")
	}
	for _, rec := range h.ch.Records() {
		if rec.Outcome == core.OutcomeLanded && (len(rec.Checks) != 1 || rec.Checks[0].Status != core.CheckPassed || !strings.Contains(rec.Detail, "PAUSE OVERRIDE")) {
			t.Fatalf("dishonest pause override: %+v", rec)
		}
	}
}
