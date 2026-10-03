package queue

import (
	"context"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
	"testing"
	"time"
)

func TestLandingPolicyRechecksAndRetainsNamedDenial(t *testing.T) {
	h := newHarness(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
import rego.v1
allow := input.phase == "admission"
decision := {"allow":allow,"requirements":[{"name":"publication-freeze","satisfied":allow,"reason":"publication is frozen"}]}
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
