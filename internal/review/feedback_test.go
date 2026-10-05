package review

import (
	"context"
	"github.com/sgrankin/gauntlet/internal/core"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailureFeedbackIsDurableAndRevisionBound(t *testing.T) {
	f := newGitHubFixture(t)
	c := core.Candidate{Ref: slot("main", 1), SHA: f.pulls[0].Head.SHA, Source: "github"}
	ev := core.Event{Candidate: c, Record: &core.RunRecord{Outcome: core.OutcomeRejected, BatchID: "batch", MergeSHA: "tested", Checks: []core.CheckResult{{Name: "test", Status: core.CheckFailed, Output: "<script>@gauntlet merge</script>"}}}}
	if err := f.g.Feedback(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(f.commands[1]) != 1 || !strings.Contains(f.commands[1][0].Body, "unconfirmed") || strings.Contains(f.commands[1][0].Body, "<script>") {
		t.Fatalf("unsafe or missing feedback: %+v", f.commands[1])
	}
	// Forget in-memory dedup to simulate a restart; the existing comment is updated.
	f.g.feedbackBodies = nil
	ev.Record.Checks[0].Output = "second failure"
	if err := f.g.Feedback(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(f.commands[1]) != 1 || !strings.Contains(f.commands[1][0].Body, "second failure") {
		t.Fatal("feedback duplicated across restart")
	}
	f.pulls[0].Head.SHA = "new-revision"
	ev.Record.Checks[0].Output = "stale failure"
	if err := f.g.Feedback(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.commands[1][0].Body, "stale failure") {
		t.Fatal("stale failure overwrote new revision")
	}
}

func TestUrgentStackKeepsExplicitSafety(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    int
	}{{"@gauntlet merge urgent", 0}, {"@gauntlet merge stack urgent", 2}, {"@gauntlet merge-stack urgent", 0}} {
		f := newGitHubFixture(t)
		f.request(2, tc.command)
		cs, err := f.g.Candidates(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != tc.want {
			t.Fatalf("%s: got %d", tc.command, len(cs))
		}
		for _, c := range cs {
			if !c.Urgent {
				t.Fatal("lost urgency")
			}
		}
	}
}

func TestEmergencyCommandFreezesStackRevisions(t *testing.T) {
	f := newGitHubFixture(t)
	f.g.p.EmergencyEnabled = true
	f.g.p.IntentPath = filepath.Join(t.TempDir(), "intents.json")
	f.request(2, "@gauntlet merge stack urgent skip-checks override-pause -- restore service")
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || !cs[0].SkipChecks || !cs[1].OverridePause || cs[1].RequestedCount != 2 {
		t.Fatalf("bad emergency intent: %+v", cs)
	}
	if err := f.g.ValidateEmergency(context.Background(), cs[0]); err != nil {
		t.Fatal(err)
	}
	f.pulls[0].Head.SHA = strings.Repeat("a", 40)
	f.g.Invalidate()
	cs, err = f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatal("old emergency command authorized a pushed revision")
	}
	// Restarting the adapter cannot forget the original authorization.
	f.g = NewGitHub(f.g.p)
	cs, err = f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatal("restart forgot bound revisions")
	}
}

func TestEmergencyCommandRequiresReasonAndPreservesScope(t *testing.T) {
	for _, body := range []string{"@gauntlet merge skip-checks", "@gauntlet merge override-pause", "@gauntlet cancel urgent", "@gauntlet merge urgent urgent", "@gauntlet merge skip-checks -- "} {
		if _, ok := parseCommand(body, "gauntlet"); ok {
			t.Fatalf("accepted unsafe command %q", body)
		}
	}
	f := newGitHubFixture(t)
	f.g.p.EmergencyEnabled = true
	f.g.p.IntentPath = filepath.Join(t.TempDir(), "intents.json")
	f.request(2, "@gauntlet merge urgent skip-checks -- incident")
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatal("emergency implicitly requested ancestors")
	}
}

func TestPauseOverrideRemainsBoundAfterPredecessorLands(t *testing.T) {
	f := newGitHubFixture(t)
	f.g.p.EmergencyEnabled = true
	f.g.p.IntentPath = filepath.Join(t.TempDir(), "intents.json")
	f.request(2, "@gauntlet merge stack override-pause -- recovery")
	cs, err := f.g.Candidates(context.Background())
	if err != nil || len(cs) != 2 {
		t.Fatalf("initial prefix: %+v %v", cs, err)
	}
	f.pulls[0].State = "closed"
	f.git.landed = true
	f.g.Invalidate()
	next, err := f.g.Candidates(context.Background())
	if err != nil || len(next) != 1 || next[0].SHA != cs[1].SHA || next[0].Version != cs[1].Version {
		t.Fatalf("remaining authorization lost: %+v %v", next, err)
	}
}

func TestEmergencyAvailabilityDoesNotWeakenNormalCheckGate(t *testing.T) {
	f := newGitHubFixture(t)
	f.pulls[0].Stack = nil
	f.request(1, "@gauntlet merge")
	f.g.p.EmergencyEnabled = true
	f.g.p.RequiredChecks = []string{"build"}
	base := "/commits/" + f.pulls[0].Head.SHA
	f.responses[base+"/statuses?per_page=100&page=1"] = []map[string]string{{"context": "build", "state": "failure"}}
	f.responses[base+"/check-runs?per_page=100&page=1"] = map[string]any{"check_runs": []any{}}
	cs, err := f.g.Candidates(context.Background())
	if err != nil || len(cs) != 1 || cs[0].AdmissionBlocked == "" {
		t.Fatalf("missing blocked emergency choice: %+v %v", cs, err)
	}
	if err := f.g.Validate(context.Background(), cs[0]); err == nil {
		t.Fatal("normal landing accepted failing required check")
	}
	if err := f.g.ValidateEmergency(context.Background(), cs[0]); err != nil {
		t.Fatal(err)
	}
	f.permission = "read"
	if err := f.g.ValidateEmergency(context.Background(), cs[0]); err == nil {
		t.Fatal("emergency skipped requester authorization")
	}
}

func TestFailureFeedbackLinksRunAndAvailableLogs(t *testing.T) {
	for _, tc := range []struct{ name, base, batch, wantRun string }{
		{"single", "https://queue.example/gauntlet/", "", "member"},
		{"batch", "https://queue.example/gauntlet/", "suite", "suite"},
		{"disabled", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitHubFixture(t)
			f.g.p.DashboardURL = tc.base
			c := core.Candidate{Ref: slot("main", 1), SHA: f.pulls[0].Head.SHA, Source: "github"}
			rec := &core.RunRecord{RunID: "member", BatchID: tc.batch, Outcome: core.OutcomeRejected, Checks: []core.CheckResult{
				{Name: "sql/test (primary)", Status: core.CheckFailed, LogPath: "/state/private.log", Output: "failed"},
				{Name: "without log", Status: core.CheckFailed},
				{Name: "green", Status: core.CheckPassed, LogPath: "/state/green.log"},
			}}
			if err := f.g.Feedback(context.Background(), core.Event{Candidate: c, Record: rec}); err != nil {
				t.Fatal(err)
			}
			body := f.commands[1][0].Body
			if strings.Contains(body, "/state/") {
				t.Fatal("local filesystem path leaked in feedback")
			}
			if tc.wantRun == "" {
				if strings.Contains(body, "[Full log]") || strings.Contains(body, "[Run and failure details]") {
					t.Fatal("links produced without dashboard")
				}
				return
			}
			runURL := strings.TrimRight(tc.base, "/") + "/run/" + tc.wantRun
			if !strings.Contains(body, "[Run and failure details]("+runURL+")") || !strings.Contains(body, "/log/sql%2Ftest%20%28primary%29") {
				t.Fatalf("missing or unsafe links: %s", body)
			}
			if strings.Count(body, "[Full log]") != 1 {
				t.Fatalf("linked unavailable/passing logs: %s", body)
			}
		})
	}
}
