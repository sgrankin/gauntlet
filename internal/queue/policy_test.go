package queue

import (
	"context"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
)

func TestLandingPolicyRechecksAndRetainsNamedDenial(t *testing.T) {
	h := newHarness(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
import rego.v1
allow := input.phase == "admission"
submission := {"allow":allow,"requirements":[{"name":"publication-freeze","satisfied":allow,"reason":"publication is frozen"}]}
`, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h.d.cfg.Policy = engine
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "a")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if !h.git.hasRef(ref) {
		t.Fatal("landing policy did not stop publication")
	}
	audit := h.d.controls.PolicyAudit
	if len(audit) < 2 || audit[len(audit)-1].Phase != "landing" || audit[len(audit)-1].Decision.Allow || audit[len(audit)-1].Version != engine.Version {
		t.Fatalf("missing fresh policy audit: %+v", audit)
	}
}

func TestExecutionPolicyRejectsBeforeCommandsRun(t *testing.T) {
	h := newHarness(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
import rego.v1
execution := {"allow":false,"requirements":[{"name":"execution-freeze","satisfied":false,"reason":"Execution is frozen"}]}`, time.Second, policy.Options{Extend: []string{"execution"}})
	if err != nil {
		t.Fatal(err)
	}
	h.d.cfg.Policy = engine
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "a")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()
	snap := h.d.Snapshot()
	if snap.Targets[0].InFlight != nil || len(snap.Targets[0].Parked) != 1 {
		t.Fatalf("execution started: %+v", snap.Targets[0])
	}
	if len(snap.Policy) == 0 || snap.Policy[len(snap.Policy)-1].Phase != "execution" {
		t.Fatalf("missing execution audit: %+v", snap.Policy)
	}
}

func TestPolicyCanAuthorizePriorityWithoutPreemption(t *testing.T) {
	h := newHarness(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
import rego.v1
command := {"allow":not_preempting,"requirements":[]}
not_preempting := input.command.preempt == false
`, time.Second, policy.Options{Extend: []string{"command"}})
	if err != nil {
		t.Fatal(err)
	}
	h.d.cfg.Policy = engine
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "a")
	sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))
	cmd := core.Command{Kind: core.CommandPriority, Target: "main", Actor: "operator", Reason: "CI fix", Priority: "urgent", Preempt: true, Revisions: []core.Revision{{Ref: ref, SHA: sha}}}
	h.ch.SendCommand(cmd)
	h.reconcile()
	if _, ok := h.d.controls.Priorities[ref]; ok {
		t.Fatal("policy denial did not block preemption")
	}
	cmd.Preempt = false
	h.ch.SendCommand(cmd)
	h.reconcile()
	if h.d.controls.Priorities[ref].Class != "urgent" {
		t.Fatalf("authorized priority rejected: %s", h.d.controls.LastError)
	}
}
