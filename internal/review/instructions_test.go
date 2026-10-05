package review

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
)

func TestInstructionsAreIndependentOfReadinessAndDurable(t *testing.T) {
	for _, appID := range []int64{0, 42} {
		t.Run(fmt.Sprint(appID), func(t *testing.T) {
			f := newGitHubFixture(t)
			f.g.p.AppID = appID
			f.pulls[0].Draft = true
			f.approvals[1] = false
			for range 2 {
				candidates, err := f.g.Candidates(context.Background())
				if err != nil || len(candidates) != 0 || len(f.git.fetched) != 0 {
					t.Fatalf("welcome enqueued a PR: %v %v", candidates, err)
				}
				f.g = NewGitHub(f.g.p)
			}
			if len(f.instructions[1]) != 1 {
				t.Fatalf("duplicate instructions: %+v", f.instructions[1])
			}
			body := f.instructions[1][0].Body
			for _, command := range []string{"@gauntlet check", "@gauntlet merge", "@gauntlet merge stack", "@gauntlet merge-prefix N"} {
				if !strings.Contains(body, command) {
					t.Fatalf("missing command %s: %s", command, body)
				}
			}
			f.pulls[0].Head.SHA = "updated"
			f.g.p.Bot = "land-bot"
			f.g.Invalidate()
			if _, err := f.g.Candidates(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.instructions[1]) != 1 || !strings.Contains(f.instructions[1][0].Body, "@land-bot merge") {
				t.Fatal("edited config duplicated or failed to update instructions")
			}
		})
	}
}

func TestInstructionsIgnoreForgedMarkersAndKeepFailureFeedbackSeparate(t *testing.T) {
	f := newGitHubFixture(t)
	forged := comment{ID: 5, Body: instructionsMarker}
	forged.User.Login = "contributor"
	f.commands[1] = []comment{forged}
	if _, err := f.g.Candidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.instructions[1]) != 1 || f.commands[1][0].Body != instructionsMarker {
		t.Fatal("forged marker suppressed instructions or was edited")
	}
	c := core.Candidate{Ref: slot("main", 1), Source: "github", SHA: f.pulls[0].Head.SHA}
	if err := f.g.Feedback(context.Background(), core.Event{Candidate: c, Record: &core.RunRecord{Outcome: core.OutcomeRejected, Detail: "test failed"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.instructions[1]) != 1 || !strings.Contains(f.instructions[1][0].Body, "@gauntlet check") || len(f.commands[1]) != 2 {
		t.Fatal("failure replaced instructions")
	}
}

func TestUnconfiguredPullDoesNotBlockManagedPulls(t *testing.T) {
	f := newGitHubFixture(t)
	for i := range f.pulls {
		f.pulls[i].Stack = nil
	}
	f.pulls[2].Base.Ref = "other-branch"
	f.pulls[2].State = "open"
	f.request(1, "@gauntlet merge")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("unmanaged PR blocked queue: %+v %v", candidates, err)
	}
	if len(f.instructions[1]) != 1 || len(f.instructions[3]) != 0 {
		t.Fatalf("incorrect scope: %+v", f.instructions)
	}
}

func TestScopePolicyControlsInstructionsAdmissionAndFreshLanding(t *testing.T) {
	f := newGitHubFixture(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
import rego.v1
excluded if "external-merge" in input.forge.labels
owned := count([true | excluded]) == 0
scope := {"allow":true,"requirements":[{"name":"queue-owned","satisfied":owned,"reason":"external-merge PRs use another queue"}]}`, time.Second, policy.Options{Extend: []string{"scope"}})
	if err != nil {
		t.Fatal(err)
	}
	f.g.p.Policy = engine
	for _, p := range f.pulls {
		f.responses[fmt.Sprintf("/pulls/%d/files?per_page=100&page=1", p.Number)] = []any{}
	}
	f.pulls[2].Labels = []struct{ Name string }{{"external-merge"}}
	f.request(1, "@gauntlet merge")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("scope intake: %+v %v", candidates, err)
	}
	if len(f.instructions[1]) != 1 || len(f.instructions[3]) != 0 {
		t.Fatal("custom scope did not gate welcome")
	}
	f.pulls[0].Labels = []struct{ Name string }{{"external-merge"}}
	if _, err := f.g.ValidatePolicyInputs(context.Background(), candidates, false); err == nil {
		t.Fatal("PR removed from scope could still land")
	}
	f.request(2, "@gauntlet merge stack")
	f.g.Invalidate()
	candidates, err = f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 0 {
		t.Fatalf("stack bypassed excluded prerequisite: %+v %v", candidates, err)
	}
}

func TestScopeErrorsFailClosedWithoutWelcome(t *testing.T) {
	f := newGitHubFixture(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
scope := {"allow":true}`, time.Second, policy.Options{Replace: []string{"scope"}})
	if err != nil {
		t.Fatal(err)
	}
	f.g.p.Policy = engine
	f.responses["/pulls/1/files?per_page=100&page=1"] = []any{}
	f.request(1, "@gauntlet merge")
	if _, err := f.g.Candidates(context.Background()); err == nil {
		t.Fatal("malformed scope allowed discovery")
	}
	if len(f.git.fetched) != 0 || len(f.instructions) != 0 {
		t.Fatal("scope error published welcome or imported code")
	}
}

func TestReadyPrefixStopsBeforeAnExcludedSuccessor(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    int
	}{{"merge-ready", 2}, {"merge-stack", 0}} {
		t.Run(tc.command, func(t *testing.T) {
			f := newGitHubFixture(t)
			engine, err := policy.Compile(context.Background(), `package gauntlet
scope := {"allow":input.forge.pr != 3,"requirements":[]}`, time.Second, policy.Options{Extend: []string{"scope"}})
			if err != nil {
				t.Fatal(err)
			}
			f.g.p.Policy = engine
			for _, p := range f.pulls {
				f.responses[fmt.Sprintf("/pulls/%d/files?per_page=100&page=1", p.Number)] = []any{}
			}
			f.request(1, "@gauntlet "+tc.command)
			candidates, err := f.g.Candidates(context.Background())
			if err != nil || len(candidates) != tc.want {
				t.Fatalf("incorrect scoped prefix: %+v %v", candidates, err)
			}
			if tc.want == 0 && !strings.Contains(f.commands[1][len(f.commands[1])-1].Body, "outside Gauntlet's scope") {
				t.Fatal("excluded stack member was not explained to the requester")
			}
		})
	}
}

func TestWelcomePublicationFailureDoesNotBlockAdmissionAndRetries(t *testing.T) {
	f := newGitHubFixture(t)
	f.welcomeFailed = true
	f.request(1, "@gauntlet merge")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 1 || len(f.instructions) != 0 {
		t.Fatalf("advisory failure blocked admission: %+v %v", candidates, err)
	}
	f.welcomeFailed = false
	f.g.Invalidate()
	if _, err := f.g.Candidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.instructions[1]) != 1 {
		t.Fatal("welcome was not retried")
	}
}

func TestScopePolicyReceivesMetadataAndRenamedPaths(t *testing.T) {
	f := newGitHubFixture(t)
	engine, err := policy.Compile(context.Background(), `package gauntlet
import rego.v1
owned := count([true |
 input.forge.author == "author"
 input.forge.repository == "acme/repo"
 input.forge.title == "Change 1"
 input.branch == "main"
 input.target == "main"
 input.paths_available
 "previous.go" in input.paths
]) > 0
scope := {"allow":owned,"requirements":[]}`, time.Second, policy.Options{Extend: []string{"scope"}})
	if err != nil {
		t.Fatal(err)
	}
	f.g.p.Policy = engine
	for _, p := range f.pulls {
		f.responses[fmt.Sprintf("/pulls/%d/files?per_page=100&page=1", p.Number)] = []any{}
	}
	f.responses["/pulls/1/files?per_page=100&page=1"] = []any{map[string]string{"filename": "current.go", "previous_filename": "previous.go"}}
	if _, err := f.g.Candidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.instructions[1]) != 1 || len(f.instructions[2]) != 0 {
		t.Fatal("metadata or rename facts did not select the managed PR")
	}
}
