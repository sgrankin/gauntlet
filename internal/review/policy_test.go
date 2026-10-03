package review

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/policy"
)

func TestCommandPolicyCanReplaceAuthorityAndReadiness(t *testing.T) {
	f := newGitHubFixture(t)
	f.permission = "read"
	f.approvals[1] = false
	f.request(1, "@gauntlet merge")
	ctx := context.Background()
	if candidates, err := f.g.Candidates(ctx); err != nil || len(candidates) != 0 {
		t.Fatalf("default admitted reader: %+v %v", candidates, err)
	}
	engine, err := policy.Compile(ctx, `package gauntlet
command := {"allow":true,"requirements":[]}
submission := {"allow":true,"requirements":[]}`, time.Second, policy.Options{Replace: []string{"command", "submission"}})
	if err != nil {
		t.Fatal(err)
	}
	f.g.p.Policy = engine
	f.g.Invalidate()
	// Custom command facts include conversations and paths, with no hidden Go readiness gate.
	f.responses["/pulls/1/files?per_page=100&page=1"] = []any{}
	f.responses["/commits/"+f.pulls[0].Head.SHA+"/statuses?per_page=100&page=1"] = []any{}
	f.responses["/commits/"+f.pulls[0].Head.SHA+"/check-runs?per_page=100&page=1"] = map[string]any{"total_count": 0, "check_runs": []any{}}
	f.threadPages = []any{threadPage(false)}
	candidates, err := f.g.Candidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("replacement did not admit: %+v %v", candidates, err)
	}
}

func TestPermissionRevocationStopsLanding(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(1, "@gauntlet merge")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("intake: %+v %v", candidates, err)
	}
	f.permission = "read"
	if _, err := f.g.ValidatePolicyInputs(context.Background(), candidates, false); err == nil {
		t.Fatal("revoked requester still allowed to land")
	}
}

func TestCheckReportsRequirementsWithoutEnqueue(t *testing.T) {
	f := newGitHubFixture(t)
	f.permission = "read"
	f.request(1, "@gauntlet check")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 0 || len(f.git.fetched) != 0 {
		t.Fatalf("check enqueued: %+v %v", candidates, err)
	}
	feedback := f.commands[1][len(f.commands[1])-1].Body
	if !strings.Contains(feedback, "command-authority") || !strings.Contains(feedback, "approvals") {
		t.Fatalf("missing shared diagnostics: %s", feedback)
	}
}

func TestDeniedCancelDoesNotSupersedeAuthorizedMerge(t *testing.T) {
	f := newGitHubFixture(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
import rego.v1
command := {"allow":allowed,"requirements":[]}
allowed := input.command.kind != "cancel"`, time.Second, policy.Options{Extend: []string{"command"}})
	if err != nil {
		t.Fatal(err)
	}
	f.g.p.Policy = engine
	f.request(1, "@gauntlet merge")
	cancel := comment{ID: 11, Body: "@gauntlet cancel"}
	cancel.User.Login = "maintainer"
	f.commands[1] = append(f.commands[1], cancel)
	f.responses["/pulls/1/files?per_page=100&page=1"] = []any{}
	f.responses["/commits/"+f.pulls[0].Head.SHA+"/statuses?per_page=100&page=1"] = []any{}
	f.responses["/commits/"+f.pulls[0].Head.SHA+"/check-runs?per_page=100&page=1"] = map[string]any{"total_count": 0, "check_runs": []any{}}
	f.threadPages = []any{threadPage(false)}
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("denied cancel displaced merge: %+v %v", candidates, err)
	}
}

func TestFreshBatchPolicyInputsRetainRequestedStack(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(2, "@gauntlet merge stack")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 2 {
		t.Fatalf("intake: %+v %v", candidates, err)
	}
	facts, err := f.g.ValidatePolicyInputs(context.Background(), candidates, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		input := facts[candidate.Ref]
		if len(input.Stack) != 2 || input.Candidate["version"] != candidate.Version || input.Principal.ID != "maintainer" {
			t.Fatalf("fresh facts lost stack or identity: %+v", input)
		}
	}
}
