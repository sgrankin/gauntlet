package queue

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

func TestSchedulePreviewSuffixAndDependencies(t *testing.T) {
	a := core.Candidate{Ref: "a", SHA: "a"}
	b := core.Candidate{Ref: "b", SHA: "b"}
	c := core.Candidate{Ref: "c", SHA: "c"}
	target := TargetSnapshot{Pipeline: []RunSnapshot{{RunID: "A", Members: []core.Candidate{a}}, {RunID: "B", Members: []core.Candidate{b}, Done: []core.CheckResult{{Duration: time.Second}}, Running: []CurrentCheck{{}}}}, Waiting: []WaitingEntry{{Candidate: c}}}
	cmd := core.Command{Kind: core.CommandMove, Revisions: []core.Revision{{Ref: "c", SHA: "c"}}, After: "a"}
	impact, err := PlanSchedule(target, cmd)
	if err != nil || !reflect.DeepEqual(impact.Order, []string{"a", "c", "b"}) || !reflect.DeepEqual(impact.AffectedRuns, []string{"B"}) || impact.CompletedChecks != 1 || impact.RunningChecks != 1 || impact.SpentSeconds != 1 {
		t.Fatalf("wrong preview: %+v %v", impact, err)
	}
	target.Pipeline[1].Running = nil
	second, _ := PlanSchedule(target, cmd)
	if impact.Token != second.Token {
		t.Fatal("timing changed preview token")
	}
	target.Waiting[0].Candidate.DependsOn = "b"
	if _, err := PlanSchedule(target, cmd); err == nil {
		t.Fatal("moved child before parent")
	}
	cmd.Kind = core.CommandPriority
	cmd.Priority = "urgent"
	cmd.Preempt = true
	cmd.After = ""
	impact, err = PlanSchedule(target, cmd)
	if err != nil || !reflect.DeepEqual(impact.Order, []string{"b", "c", "a"}) {
		t.Fatalf("preemption did not carry parent: %+v %v", impact, err)
	}
}

func TestPriorityPreservesActiveWorkAndPreemptionIsExplicit(t *testing.T) {
	for _, preempt := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve", true: "preempt"}[preempt], func(t *testing.T) {
			h := newHarness(t, config.Target{Name: "main", Branch: "main", Mode: "speculate", Window: 2})
			h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
			h.git.seed("main", nil)
			refs := []string{candidateRef("main", "alice", "a"), candidateRef("main", "alice", "b"), candidateRef("main", "alice", "c")}
			var revision core.Revision
			for _, ref := range refs {
				sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))
				revision = core.Revision{Ref: ref, SHA: sha}
			}
			h.reconcile()
			original := h.d.headRun("main").runID
			h.ch.SendCommand(core.Command{Kind: core.CommandPriority, Target: "main", Actor: "operator", Reason: "CI fix", Priority: "urgent", Preempt: preempt, Revisions: []core.Revision{revision}})
			h.reconcile()
			head := h.d.headRun("main")
			if preempt {
				if head.members[0].cand.Ref != refs[2] || head.runID == original {
					t.Fatal("explicit preemption did not move urgent change first")
				}
			} else if head.runID != original {
				t.Fatal("priority interrupted active verification")
			}
			state, err := loadControls(h.d.cfg.ControlPath)
			if err != nil || state.Priorities[revision.Ref].Class != "urgent" {
				t.Fatalf("priority not durable: %+v %v", state, err)
			}
		})
	}
}

func TestPendingMoveBeforeFirstAdmissionAndStalePreview(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	a := candidateRef("main", "alice", "a")
	b := candidateRef("main", "alice", "b")
	h.git.pushCandidate(a, "", checkSpecFile("test"))
	sha := h.git.pushCandidate(b, "", checkSpecFile("test"))
	cmd := core.Command{Kind: core.CommandMove, Target: "main", Actor: "operator", Reason: "fix first", Before: a, Revisions: []core.Revision{{Ref: b, SHA: sha}}, PlanToken: "stale"}
	h.ch.SendCommand(cmd)
	h.reconcile()
	if h.d.headRun("main").members[0].cand.Ref != a {
		t.Fatal("applied stale preview")
	}
	// Moving across active work requires the separate explicit preemption flag.
	cmd.PlanToken = ""
	h.ch.SendCommand(cmd)
	h.reconcile()
	if h.d.headRun("main").members[0].cand.Ref != a {
		t.Fatal("move implicitly preempted")
	}
	cmd.Preempt = true
	h.ch.SendCommand(cmd)
	h.reconcile()
	if h.d.headRun("main").members[0].cand.Ref != b {
		t.Fatalf("move failed: %s", h.d.controls.LastError)
	}
}

func TestPriorityAgingAndParentInheritance(t *testing.T) {
	h := newHarness(t)
	now := h.d.now()
	cands := map[string]core.Candidate{"old": {Ref: "old", SHA: "o", Priority: "low"}, "parent": {Ref: "parent", SHA: "p"}, "child": {Ref: "child", SHA: "c", Priority: "urgent", DependsOn: "parent"}}
	h.d.controls.Enqueued["old"] = now.Add(-time.Hour)
	if h.d.priorityAt("main", "old", cands, now) != 2 || h.d.priorityAt("main", "parent", cands, now) != 3 {
		t.Fatal("aging or dependency inheritance failed")
	}
}
