package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type reviewGit struct {
	fetched    []string
	remoteRefs []string
	landed     bool
}

func (g *reviewGit) FetchReview(_ context.Context, remote, local, sha string) error {
	g.fetched = append(g.fetched, sha)
	g.remoteRefs = append(g.remoteRefs, remote)
	return nil
}
func (g *reviewGit) ReviewLanded(context.Context, string, string, string) (bool, error) {
	return g.landed, nil
}
func (g *reviewGit) ReviewBaseLanded(context.Context, string, string) (bool, error) {
	return g.landed, nil
}

type githubFixture struct {
	failed        bool
	pulls         []pull
	commands      map[int][]comment
	approvals     map[int]bool
	permission    string
	responses     map[string]any
	threadPages   []any
	threadCalls   int
	threadStatus  int
	threadCursors []string
	g             *GitHub
	git           *reviewGit
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
		if r.URL.Path == "/graphql" || r.URL.Path == "/api/graphql" {
			var input struct {
				Query     string
				Variables struct {
					Owner, Repo string
					Number      int
					Cursor      *string
				}
			}
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&input) != nil || !strings.Contains(input.Query, "reviewThreads") || input.Variables.Owner != "acme" || input.Variables.Repo != "repo" || input.Variables.Number != 1 {
				t.Error("incorrect review-thread query")
			}
			if f.threadStatus != 0 {
				http.Error(w, "unavailable", f.threadStatus)
				return
			}
			if f.threadCalls >= len(f.threadPages) {
				t.Error("unexpected review-thread page")
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			cursor := ""
			if input.Variables.Cursor != nil {
				cursor = *input.Variables.Cursor
			}
			f.threadCursors = append(f.threadCursors, cursor)
			page := f.threadPages[f.threadCalls]
			f.threadCalls++
			write(page)
			return
		}
		if v, ok := f.responses[strings.TrimPrefix(r.URL.RequestURI(), "/repos/acme/repo")]; ok {
			write(v)
			return
		}
		if path == "/pulls" {
			if f.failed {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			write(f.pulls)
			return
		}
		if path == "/stacks/7" {
			write(map[string]any{"base": ref{Ref: "main"}, "pull_requests": []map[string]int{{"number": 1}, {"number": 2}, {"number": 3}}})
			return
		}
		if strings.HasPrefix(path, "/issues/comments/") && r.Method == "PATCH" {
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			for n, cs := range f.commands {
				for i, c := range cs {
					if fmt.Sprint(c.ID) == strings.TrimPrefix(path, "/issues/comments/") {
						f.commands[n][i].Body = body.Body
						write(map[string]any{})
						return
					}
				}
			}
			http.NotFound(w, r)
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
		{"@gauntlet merge", 0}, {"@gauntlet merge stack", 2}, {"@gauntlet merge-stack", 0}, {"@gauntlet merge-ready", 2}, {"@gauntlet merge-prefix 2", 2}, {"@gauntlet merge-prefix 3", 0}, {"@gauntlet cancel", 0},
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

func TestGitHubSingleMergeRejectsPrerequisites(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			f := newGitHubFixture(t)
			if !native {
				for i := range f.pulls {
					f.pulls[i].Stack = nil
					if i > 0 {
						f.pulls[i].Base.Ref = f.pulls[i-1].Head.Ref
					}
				}
			}
			f.approvals[3] = true
			f.request(1, "@gauntlet merge")
			f.request(2, "@gauntlet merge")
			for range 2 {
				cs, err := f.g.Candidates(context.Background())
				if err != nil || len(cs) != 1 || cs[0].SHA != f.pulls[0].Head.SHA {
					t.Fatalf("unsafe request admitted, or unrelated request blocked: %+v %v", cs, err)
				}
				// Reconstruct from GitHub comments, including rejection feedback.
				f.g = NewGitHub(f.g.p)
			}
			if len(f.commands[2]) != 2 || !strings.Contains(f.commands[2][1].Body, "@gauntlet merge stack") {
				t.Fatalf("missing or duplicated rejection feedback: %+v", f.commands[2])
			}
			f.request(2, "@gauntlet merge stack")
			cs, err := f.g.Candidates(context.Background())
			if err != nil || len(cs) != 2 || cs[1].SHA != f.pulls[1].Head.SHA {
				t.Fatalf("explicit request should include A+B, never C: %+v %v", cs, err)
			}
			f.request(2, "@gauntlet merge")
			if err := f.g.Validate(context.Background(), cs[1]); err == nil {
				t.Fatal("unsafe replacement command passed pre-landing validation")
			}
		})
	}
}

func TestGitHubStackThroughRequest(t *testing.T) {
	f := newGitHubFixture(t)
	if err := f.g.Request(context.Background(), 2, "merge-through", 0); err != nil {
		t.Fatal(err)
	}
	if len(f.commands[2]) != 1 || f.commands[2][0].Body != "@gauntlet merge stack" {
		t.Fatalf("incorrect CLI command: %+v", f.commands[2])
	}
	f.commands[2][0].User.Login = "maintainer"
	f.approvals[1] = false
	cs, err := f.g.Candidates(context.Background())
	if err != nil || len(cs) != 0 {
		t.Fatalf("unready prerequisite admitted: %+v %v", cs, err)
	}
	f.approvals[1] = true
	f.approvals[2] = false
	cs, err = f.g.Candidates(context.Background())
	if err != nil || len(cs) != 0 {
		t.Fatalf("part of an unready requested stack admitted: %+v %v", cs, err)
	}
}

func TestGitHubReadinessAndCurrency(t *testing.T) {
	f := newGitHubFixture(t)
	f.request(2, "@gauntlet merge stack")
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
			f.request(2, "@gauntlet merge stack")
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

func TestGitHubPollingStillValidatesFreshState(t *testing.T) {
	f := newGitHubFixture(t)
	f.pulls[0].Stack = nil
	f.request(1, "@gauntlet merge")
	now := time.Now()
	f.g.now = func() time.Time { return now }
	f.g.p.PollInterval = time.Minute
	first, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first candidates: %+v", first)
	}
	f.pulls[0].Body = "edited while cached"
	cached, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 || cached[0].Version != first[0].Version {
		t.Fatal("poll interval did not retain snapshot")
	}
	cached[0].Message = "caller mutation"
	cached, err = f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cached[0].Message != first[0].Message {
		t.Fatal("caller mutated cached admission")
	}
	if err := f.g.Validate(context.Background(), first[0]); err == nil {
		t.Fatal("landing trusted cached metadata")
	}
	fresh, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].Version == first[0].Version {
		t.Fatal("validation failure did not refresh admission")
	}
	now = now.Add(time.Minute)
	f.failed = true
	if _, err := f.g.Candidates(context.Background()); err == nil {
		t.Fatal("expired snapshot hid API failure")
	}
	f.failed = false
	f.request(1, "@gauntlet cancel")
	empty, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatal("refresh did not recover with current cancellation")
	}
	f.request(1, "@gauntlet merge")
	f.g.Invalidate()
	fresh, err = f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatal("invalidation did not bypass interval")
	}
}

type refreshingTokens struct{ token, invalidated string }

func (t *refreshingTokens) Token(context.Context) (string, error) { return t.token, nil }
func (t *refreshingTokens) Invalidate(token string)               { t.invalidated = token; t.token = "fresh" }

func TestGitHubUnauthorizedRefreshAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			tokens := &refreshingTokens{token: "expired"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail || r.Header.Get("Authorization") == "Bearer expired" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer fresh" {
					t.Error("refresh token not used")
				}
				json.NewEncoder(w).Encode([]pull{})
			}))
			defer srv.Close()
			g := NewGitHub(GitHubParams{Repo: "acme/repo", APIURL: srv.URL, Tokens: tokens})
			_, err := g.Candidates(context.Background())
			if (err != nil) != fail {
				t.Fatalf("err=%v, fail=%v", err, fail)
			}
			if tokens.invalidated != "expired" {
				t.Fatal("expired token not invalidated")
			}
		})
	}
}

func TestGitHubSkipsUnrelatedClosedHistory(t *testing.T) {
	f := newGitHubFixture(t)
	f.pulls = append(f.pulls, pull{Number: 4, State: "closed", Head: ref{Ref: "old-feature"}, Base: ref{Ref: "main"}})
	f.request(2, "@gauntlet merge stack")
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatal(cs)
	}
	// The fixture has no comments endpoint for #4: consulting it fails the test.
}

func TestGitHubClosedRootRequestWithoutStackField(t *testing.T) {
	f := newGitHubFixture(t)
	f.approvals[3] = true
	f.request(1, "@gauntlet merge-stack")
	f.pulls[0].State = "closed"
	f.pulls[0].Stack = nil
	f.git.landed = true
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].SourceBase != f.pulls[0].Head.SHA {
		t.Fatalf("closed native root request lost: %+v", cs)
	}
}

func TestGitHubBranchStackRetainsClosedRoot(t *testing.T) {
	f := newGitHubFixture(t)
	for i := range f.pulls {
		f.pulls[i].Stack = nil
		if err := json.Unmarshal([]byte(`{"full_name":"ACME/repo"}`), &f.pulls[i].Head.Repo); err != nil {
			t.Fatal(err)
		}
	}
	f.pulls[1].Base.Ref = f.pulls[0].Head.Ref
	f.pulls[2].Base.Ref = f.pulls[1].Head.Ref
	f.pulls[0].State = "closed"
	f.git.landed = true
	f.approvals[3] = true
	f.request(1, "@gauntlet merge-stack")
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].SourceBase != f.pulls[0].Head.SHA || cs[1].DependsOn != cs[0].Ref {
		t.Fatalf("branch stack lost: %+v", cs)
	}
}

func TestGitHubForkHeadUsesBaseRepositoryPullRef(t *testing.T) {
	f := newGitHubFixture(t)
	for i := range f.pulls {
		f.pulls[i].Stack = nil
	}
	if err := json.Unmarshal([]byte(`{"full_name":"contributor/fork"}`), &f.pulls[0].Head.Repo); err != nil {
		t.Fatal(err)
	}
	f.request(1, "@gauntlet merge")
	cs, err := f.g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || len(f.git.remoteRefs) != 1 || f.git.remoteRefs[0] != "refs/pull/1/head" {
		t.Fatalf("fork not fetched via base repo: %+v %v", cs, f.git.remoteRefs)
	}
	f.pulls[1].Base.Ref = f.pulls[0].Head.Ref
	f.request(2, "@gauntlet merge stack")
	if _, err := f.g.Candidates(context.Background()); err == nil {
		t.Fatal("fork branch mistaken for a base-repository prerequisite")
	}
}
