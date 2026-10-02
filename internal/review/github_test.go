package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type reviewGit struct {
	fetched []string
	landed  bool
}

func (g *reviewGit) FetchReview(_ context.Context, remote, local, sha string) error {
	g.fetched = append(g.fetched, sha)
	return nil
}
func (g *reviewGit) ReviewLanded(context.Context, string, string, string) (bool, error) {
	return g.landed, nil
}
func (g *reviewGit) ReviewBaseLanded(context.Context, string, string) (bool, error) {
	return g.landed, nil
}

type githubFixture struct {
	pulls      []pull
	commands   map[int][]comment
	approvals  map[int]bool
	permission string
	responses  map[string]any
	g          *GitHub
	git        *reviewGit
}

func newGitHubFixture(t *testing.T) *githubFixture {
	t.Helper()
	f := &githubFixture{commands: map[int][]comment{}, approvals: map[int]bool{1: true, 2: true}, permission: "write", responses: map[string]any{}, git: &reviewGit{}}
	for n := 1; n <= 3; n++ {
		p := pull{Number: n, Title: fmt.Sprintf("Change %d", n), Body: "Description", State: "open", HTMLURL: fmt.Sprintf("https://github.com/acme/repo/pull/%d", n), Head: ref{SHA: strings.Repeat(fmt.Sprint(n), 40), Ref: fmt.Sprintf("feature-%d", n)}, Base: ref{Ref: "main"}}
		p.User.Login = "author"
		p.Stack = &struct {
			Number, Position, Size int
			Base                   ref
		}{Number: 7, Position: n, Size: 3, Base: ref{Ref: "main"}}
		f.pulls = append(f.pulls, p)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing authentication")
		}
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo")
		write := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
		if v, ok := f.responses[strings.TrimPrefix(r.URL.RequestURI(), "/repos/acme/repo")]; ok {
			write(v)
			return
		}
		if path == "/pulls" {
			write(f.pulls)
			return
		}
		if path == "/stacks/7" {
			write(map[string]any{"base": ref{Ref: "main"}, "pull_requests": []map[string]int{{"number": 1}, {"number": 2}, {"number": 3}}})
			return
		}
		if strings.HasPrefix(path, "/collaborators/") {
			write(map[string]string{"permission": f.permission})
			return
		}
		for n := 1; n <= 3; n++ {
			if path == fmt.Sprintf("/pulls/%d", n) {
				if r.Method == "PATCH" {
					f.pulls[n-1].State = "closed"
				}
				write(f.pulls[n-1])
				return
			}
			if path == fmt.Sprintf("/pulls/%d/reviews", n) {
				if f.approvals[n] {
					write([]any{map[string]any{"state": "APPROVED", "commit_id": f.pulls[n-1].Head.SHA, "user": map[string]string{"login": "reviewer"}}})
				} else {
					write([]any{})
				}
				return
			}
			if path == fmt.Sprintf("/issues/%d/comments", n) {
				if r.Method == "POST" {
					var c comment
					json.NewDecoder(r.Body).Decode(&c)
					c.ID = 999
					f.commands[n] = append(f.commands[n], c)
				}
				cs := f.commands[n]
				if cs == nil {
					cs = []comment{}
				}
				write(cs)
				return
			}
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	f.g = NewGitHub(GitHubParams{Repo: "acme/repo", APIURL: srv.URL, Tokens: StaticToken("test-token"), Git: f.git, Targets: map[string]string{"main": "main"}, Approvals: 1})
	return f
}

func (f *githubFixture) request(n int, body string) {
	c := comment{ID: int64(n * 10), Body: body}
	c.User.Login = "maintainer"
	f.commands[n] = []comment{c}
}

func TestGitHubStackAdmission(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    int
	}{
		{"@gauntlet merge", 2}, {"@gauntlet merge-stack", 0}, {"@gauntlet merge-ready", 2}, {"@gauntlet merge-prefix 2", 2}, {"@gauntlet merge-prefix 3", 0}, {"@gauntlet cancel", 0},
	} {
		t.Run(tc.command, func(t *testing.T) {
			f := newGitHubFixture(t)
			f.request(2, tc.command)
			cs, err := f.g.Candidates(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(cs) != tc.want {
				t.Fatalf("candidates=%+v; want %d", cs, tc.want)
			}
			if len(cs) == 2 {
				if cs[1].DependsOn != cs[0].Ref || cs[1].SourceBase != cs[0].SHA {
					t.Fatalf("stack delta/dependency lost: %+v", cs)
				}
				if cs[1].Message != "Change 2 (#2)\n\nDescription" {
					t.Fatalf("message=%q", cs[1].Message)
				}
			}
		})
	}
}

func TestGitHubReadinessAndCurrency(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(2, "@gauntlet merge")
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.Validate(context.Background(), cs[1]); err != nil {
		t.Fatal(err)
	}
	f.pulls[1].Body = "Edited after verification"
	if err := f.g.Validate(context.Background(), cs[1]); err == nil {
		t.Fatal("edited description accepted")
	}
	f.permission = "read"
	cs, err = f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatal("unauthorized command admitted")
	}
}

func TestGitHubClosedParentRequiresTargetEvidence(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(2, "@gauntlet merge")
	f.pulls[0].State = "closed"
	f.commands[1] = []comment{{Body: "<!-- gauntlet:landed:forged -->"}}
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatal("editable comment trusted as landing evidence")
	}
	f.git.landed = true
	cs, err = f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].SourceBase != f.pulls[0].Head.SHA || cs[0].DependsOn != "" {
		t.Fatalf("landed parent not retained as source base: %+v", cs)
	}
}

func TestGitHubStackRequestSurvivesRootClosure(t *testing.T) {
	f := newGitHubFixture(t)
	f.approvals[3] = true
	f.request(1, "@gauntlet merge-stack")
	before, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.pulls[0].State = "closed"
	f.git.landed = true
	after, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].Version != before[1].Version {
		t.Fatalf("root closure lost request or invalidated successor: %+v", after)
	}
}

func TestGitHubLandingClosureIdempotentAndRepushSafe(t *testing.T) {
	for _, repush := range []bool{false, true} {
		t.Run(fmt.Sprint(repush), func(t *testing.T) {
			f := newGitHubFixture(t)
			f.request(2, "@gauntlet merge")
			cs, err := f.g.Candidates(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if repush {
				f.pulls[1].Head.SHA = strings.Repeat("f", 40)
			}
			for range 2 {
				if err := f.g.Landed(context.Background(), cs[1], strings.Repeat("a", 40)); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.commands[2]) != 2 {
				t.Fatalf("duplicate landing comments: %+v", f.commands[2])
			}
			if closed := f.pulls[1].State == "closed"; closed == repush {
				t.Fatal("repushed revision was closed or unchanged revision left open")
			}
		})
	}
}

func TestCommandDoesNotRecognizeQuotedOrPartialProse(t *testing.T) {
	for _, body := range []string{"> @gauntlet merge", "please @gauntlet merge", "@gauntlet merge-prefix 0", "@gauntlet merge and do more", "@other merge"} {
		if action, _ := Command(body, "gauntlet"); action != "" {
			t.Fatalf("recognized %q", body)
		}
	}
}

func TestGitHubRequiredChecks(t *testing.T) {
	type status struct{ Context, State string }
	type check struct{ Name, Status, Conclusion string }
	passed := check{"build", "completed", "success"}
	failed := check{"build", "completed", "failure"}
	fillerStatuses := make([]status, 100)
	fillerChecks := make([]check, 100)
	for i := range 100 {
		fillerStatuses[i] = status{fmt.Sprintf("other-%d", i), "success"}
		fillerChecks[i] = check{fmt.Sprintf("other-%d", i), "completed", "success"}
	}
	for _, tc := range []struct {
		name                   string
		statuses, moreStatuses []status
		checks, moreChecks     []check
		want                   bool
	}{
		{name: "missing"},
		{name: "status success", statuses: []status{{"build", "success"}}, want: true},
		{name: "latest status fails", statuses: []status{{"build", "failure"}, {"build", "success"}}},
		{name: "latest status passes", statuses: []status{{"build", "success"}, {"build", "failure"}}, want: true},
		{name: "status on second page", statuses: fillerStatuses, moreStatuses: []status{{"build", "success"}}, want: true},
		{name: "check on second page", checks: fillerChecks, moreChecks: []check{passed}, want: true},
		{name: "successful check", checks: []check{passed}, want: true},
		{name: "neutral check", checks: []check{{"build", "completed", "neutral"}}, want: true},
		{name: "skipped check", checks: []check{{"build", "completed", "skipped"}}, want: true},
		{name: "unfinished check", checks: []check{{"build", "in_progress", "success"}}},
		{name: "failed producer first", checks: []check{failed, passed}},
		{name: "failed producer last", checks: []check{passed, failed}},
		{name: "failed producer on second page", checks: append(append([]check{}, fillerChecks[:99]...), passed), moreChecks: []check{failed}},
		{name: "failed status and passing check", statuses: []status{{"build", "failure"}}, checks: []check{passed}},
		{name: "passing status and failed check", statuses: []status{{"build", "success"}}, checks: []check{failed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitHubFixture(t)
			f.pulls[0].Stack = nil
			f.request(1, "@gauntlet merge")
			f.g.p.RequiredChecks = []string{"build"}
			base := "/commits/" + f.pulls[0].Head.SHA
			f.responses[base+"/statuses?per_page=100&page=1"] = tc.statuses
			f.responses[base+"/statuses?per_page=100&page=2"] = tc.moreStatuses
			f.responses[base+"/check-runs?per_page=100&page=1"] = map[string]any{"check_runs": tc.checks}
			f.responses[base+"/check-runs?per_page=100&page=2"] = map[string]any{"check_runs": tc.moreChecks}
			cs, err := f.g.Candidates(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := len(cs) == 1; got != tc.want {
				t.Fatalf("admitted=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestGitHubApprovalCurrency(t *testing.T) {
	for _, tc := range []struct {
		name              string
		states, revisions []string
		want              bool
	}{
		{"current approval", []string{"APPROVED"}, []string{"current"}, true},
		{"stale approval", []string{"APPROVED"}, []string{"old"}, false},
		{"dismissed approval", []string{"APPROVED", "DISMISSED"}, []string{"current", "current"}, false},
		{"comment preserves approval", []string{"APPROVED", "COMMENTED"}, []string{"current", "current"}, true},
		{"change request blocks", []string{"APPROVED", "CHANGES_REQUESTED"}, []string{"current", "current"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitHubFixture(t)
			f.pulls[0].Stack = nil
			f.request(1, "@gauntlet merge")
			var reviews []map[string]any
			for i, state := range tc.states {
				sha := "old"
				if tc.revisions[i] == "current" {
					sha = f.pulls[0].Head.SHA
				}
				reviews = append(reviews, map[string]any{"state": state, "commit_id": sha, "user": map[string]string{"login": "reviewer"}})
			}
			f.responses["/pulls/1/reviews?per_page=100&page=1"] = reviews
			cs, err := f.g.Candidates(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := len(cs) == 1; got != tc.want {
				t.Fatalf("admitted=%v, want %v", got, tc.want)
			}
		})
	}
}
