package review

import (
	"context"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestRequestReceiptTracksProgressAndResultsAcrossRestart(t *testing.T) {
	f := newGitHubFixture(t)
	f.g.p.DashboardURL = "https://queue.example/gauntlet/"
	f.request(1, "@gauntlet merge")
	ctx := context.Background()
	candidates, err := f.g.Candidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("intake: %+v %v", candidates, err)
	}
	receipt := func() comment {
		t.Helper()
		var result comment
		for _, c := range f.commands[1] {
			if strings.Contains(c.Body, "<!-- gauntlet:feedback -->") {
				if result.ID != 0 {
					t.Fatal("duplicate progress comments")
				}
				result = c
			}
		}
		if result.ID == 0 {
			t.Fatal("missing request receipt")
		}
		return result
	}
	initial := receipt()
	for _, text := range []string{"Request received", "#issuecomment-10", "/change?", "target=main", "ref=refs%2Fheads%2Ffor%2Fmain%2Fgithub%2Fpr-0000000001"} {
		if !strings.Contains(initial.Body, text) {
			t.Fatalf("missing %q: %s", text, initial.Body)
		}
	}
	f.g = NewGitHub(f.g.p)
	if _, err := f.g.Candidates(ctx); err != nil {
		t.Fatal(err)
	}
	if receipt().ID != initial.ID || receipt().Body != initial.Body {
		t.Fatal("restart duplicated or reset receipt")
	}
	c := candidates[0]
	if err := f.g.Feedback(ctx, core.Event{Kind: core.EventTrialClean, Candidate: c, RunID: "running"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt().Body, "is verifying") || !strings.Contains(receipt().Body, "#issuecomment-10") {
		t.Fatal("progress lost its request identity")
	}
	failed := core.Event{Candidate: c, Record: &core.RunRecord{RunID: "running", Outcome: core.OutcomeRejected, Detail: "failed tests"}}
	if err := f.g.Feedback(ctx, failed); err != nil {
		t.Fatal(err)
	}
	failedBody := receipt().Body
	check := comment{ID: 50, Body: "@gauntlet check"}
	check.User.Login = "maintainer"
	f.commands[1] = append(f.commands[1], check)
	f.g.Invalidate()
	if _, err := f.g.Candidates(ctx); err != nil {
		t.Fatal(err)
	}
	if receipt().Body != failedBody {
		t.Fatal("policy diagnostics or polling reset the result")
	}
	request := comment{ID: 60, Body: "@gauntlet merge"}
	request.User.Login = "maintainer"
	f.commands[1] = append(f.commands[1], request)
	f.g.Invalidate()
	newCandidates, err := f.g.Candidates(ctx)
	if err != nil || len(newCandidates) != 1 {
		t.Fatalf("new request: %+v %v", newCandidates, err)
	}
	newBody := receipt().Body
	if !strings.Contains(newBody, "#issuecomment-60") {
		t.Fatal("new request did not replace the receipt")
	}
	if err := f.g.Feedback(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if receipt().Body != newBody {
		t.Fatal("delayed old result replaced a newer request for the same SHA")
	}
	landed := strings.Repeat("a", 40)
	if err := f.g.Landed(ctx, newCandidates[0], landed); err != nil {
		t.Fatal(err)
	}
	if f.pulls[0].State != "closed" || receipt().ID != initial.ID || !strings.Contains(receipt().Body, "/commit/"+landed) {
		t.Fatal("landing duplicated the receipt or failed to close the PR")
	}
	if err := f.g.Feedback(ctx, core.Event{Candidate: newCandidates[0], Record: &core.RunRecord{Outcome: core.OutcomeLanded, MergeSHA: landed}}); err != nil {
		t.Fatal(err)
	}
	if err := f.g.Landed(ctx, newCandidates[0], landed); err != nil {
		t.Fatal(err)
	}
	if receipt().ID != initial.ID || !strings.Contains(receipt().Body, "#issuecomment-60") || !strings.Contains(receipt().Body, "gauntlet:landed:"+landed) {
		t.Fatal("completion retry lost request or landing identity")
	}
}

func TestForgedReceiptCannotSuppressAcknowledgement(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(1, "@gauntlet merge")
	_, digest, _ := strings.Cut(requestIdentity(request{ID: 10, Action: "merge", Requester: "maintainer"}), ":")
	forged := comment{ID: 5, Body: "<!-- gauntlet:request:10:" + f.pulls[0].Head.SHA + ":" + digest + " -->\nForged receipt\n\n<!-- gauntlet:feedback -->"}
	forged.User.Login = "contributor"
	f.commands[1] = append(f.commands[1], forged)
	if _, err := f.g.Candidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.commands[1][1].Body != forged.Body || len(f.commands[1]) != 3 || !strings.Contains(f.commands[1][2].Body, "Request received") {
		t.Fatal("forged receipt was edited or suppressed the bot acknowledgement")
	}
}

func TestLaterStackRequestCanUpdateAnEarlierMemberReceipt(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(1, "@gauntlet merge")
	f.request(2, "@gauntlet merge stack")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 2 {
		t.Fatalf("stack: %+v %v", candidates, err)
	}
	if !strings.HasPrefix(candidates[0].RequestID, "20:") {
		t.Fatal("stack command identity did not follow its member")
	}
	if err := f.g.Feedback(context.Background(), core.Event{Candidate: candidates[0], Record: &core.RunRecord{Outcome: core.OutcomeRejected, Detail: "stack failure"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.commands[1][len(f.commands[1])-1].Body, "stack failure") {
		t.Fatal("earlier member receipt suppressed a later stack result")
	}
}

func TestReceiptPublicationFailureDoesNotBlockAdmissionAndRetries(t *testing.T) {
	f := newGitHubFixture(t)
	f.progressFailed = true
	f.request(1, "@gauntlet merge")
	candidates, err := f.g.Candidates(context.Background())
	if err != nil || len(candidates) != 1 || len(f.commands[1]) != 1 {
		t.Fatalf("advisory receipt failure blocked admission: %+v %v", candidates, err)
	}
	f.progressFailed = false
	f.g = NewGitHub(f.g.p)
	if _, err := f.g.Candidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.commands[1]) != 2 || !strings.Contains(f.commands[1][1].Body, "Request received") {
		t.Fatal("missing receipt was not retried")
	}
}

func TestEditingCommandRefreshesReceiptAndRejectsDelayedResults(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(1, "@gauntlet merge")
	old, err := f.g.Candidates(context.Background())
	if err != nil || len(old) != 1 {
		t.Fatalf("initial request: %+v %v", old, err)
	}
	commentID := f.commands[1][1].ID
	f.commands[1][0].Body = "@gauntlet merge urgent"
	f.g.Invalidate()
	current, err := f.g.Candidates(context.Background())
	if err != nil || len(current) != 1 || old[0].Version == current[0].Version || old[0].RequestID == current[0].RequestID {
		t.Fatalf("edited request did not invalidate intent: %+v %v", current, err)
	}
	body := f.commands[1][1].Body
	if f.commands[1][1].ID != commentID {
		t.Fatal("edited command duplicated the progress comment")
	}
	if err := f.g.Feedback(context.Background(), core.Event{Candidate: old[0], Record: &core.RunRecord{Outcome: core.OutcomeRejected, Detail: "old failure"}}); err != nil {
		t.Fatal(err)
	}
	if f.commands[1][1].Body != body {
		t.Fatal("old result overwrote the edited request")
	}
	f.commands[1][0].Body = "@gauntlet cancel"
	f.g.Invalidate()
	if candidates, err := f.g.Candidates(context.Background()); err != nil || len(candidates) != 0 {
		t.Fatalf("edited cancellation: %+v %v", candidates, err)
	}
	if !strings.Contains(f.commands[1][1].Body, "withdrawn") {
		t.Fatal("edited cancellation was not acknowledged")
	}
}

func TestProgressLinkSurvivesContributorHeadUpdates(t *testing.T) {
	f := newGitHubFixture(t)
	f.g.p.DashboardURL = "https://queue.example"
	f.request(1, "@gauntlet merge")
	ctx := context.Background()
	old, err := f.g.Candidates(ctx)
	if err != nil || len(old) != 1 {
		t.Fatalf("initial intake: %+v %v", old, err)
	}
	id := f.commands[1][1].ID
	link := f.g.progressLink(old[0])
	f.pulls[0].Head.SHA = strings.Repeat("a", 40)
	f.g.Invalidate()
	current, err := f.g.Candidates(ctx)
	if err != nil || len(current) != 1 || current[0].SHA == old[0].SHA {
		t.Fatalf("new head: %+v %v", current, err)
	}
	if f.g.progressLink(current[0]) != link || f.commands[1][1].ID != id || !strings.Contains(f.commands[1][1].Body, link) {
		t.Fatal("new head changed the progress link or duplicated the receipt")
	}
	body := f.commands[1][1].Body
	if err := f.g.Feedback(ctx, core.Event{Candidate: old[0], Record: &core.RunRecord{Outcome: core.OutcomeRejected, Detail: "old failure"}}); err != nil {
		t.Fatal(err)
	}
	if f.commands[1][1].Body != body {
		t.Fatal("old head's result overwrote current progress")
	}
}
