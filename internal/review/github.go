// Package review adapts forge review identities to immutable queue inputs.
// Polling reconstructs admission; optional webhooks request an early refresh.
package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

type Tokens interface {
	Token(context.Context) (string, error)
	Invalidate(string)
}

type StaticToken string

func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }
func (StaticToken) Invalidate(string)                       {}

type Git interface {
	FetchReview(context.Context, string, string, string) error
	ReviewLanded(context.Context, string, string, string) (bool, error)
}

type GitHubParams struct {
	EmergencyEnabled  bool
	IntentPath        string
	PolicyTeams       []string
	Repo, APIURL, Bot string
	Tokens            Tokens
	Git               Git
	// Targets maps real branch names to queue target names.
	Targets                      map[string]string
	Approvals                    int
	RequiredChecks               []string
	RequireResolvedConversations bool
	PollInterval                 time.Duration
}

type GitHub struct {
	feedbackQueue  chan core.Event
	intentMu       sync.Mutex
	feedbackMu     sync.Mutex
	feedbackBodies map[int]string
	p              GitHubParams
	client         *http.Client
	pollMu         sync.Mutex
	pollGeneration uint64
	cached         []core.Candidate
	nextPoll       time.Time
	now            func() time.Time
}

func NewGitHub(p GitHubParams) *GitHub {
	if p.APIURL == "" {
		p.APIURL = "https://api.github.com"
	}
	if p.Bot == "" {
		p.Bot = "gauntlet"
	}
	return &GitHub{p: p, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

type ref struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	}
}

type apiError struct {
	status int
	method string
}

func (e *apiError) Error() string { return fmt.Sprintf("github %s: HTTP %d", e.method, e.status) }

type pull struct {
	Number             int
	Title, Body, State string
	Draft              bool
	HTMLURL            string `json:"html_url"`
	Head, Base         ref
	User               struct{ Login string }
	Stack              *struct {
		Number, Position, Size int
		Base                   ref
	}
}

type comment struct {
	ID   int64
	Body string
	User struct{ Login string }
}

type request struct {
	SkipChecks, OverridePause bool
	Requester, Reason         string
	Urgent                    bool
	PR                        pull
	ID                        int64
	Action                    string
	Count                     int
	Notified                  bool
}

// Command accepts one whole line, not quoted prose or a substring. A count
// always means the bottom N unlanded PRs of the stack.
func Command(body, bot string) (string, int) {
	parsed, ok := parseCommand(body, bot)
	if !ok {
		return "", 0
	}
	action := parsed.Action
	if parsed.Urgent {
		action += "-urgent"
	}
	return action, parsed.Count
}

func command(body, bot string) (string, int) {
	fields := strings.Fields(strings.TrimSpace(body))
	if len(fields) < 2 || fields[0] != "@"+bot {
		return "", 0
	}
	switch fields[1] {
	case "merge":
		if len(fields) == 2 {
			return "merge", 0
		}
		if len(fields) == 3 && fields[2] == "stack" {
			return "merge-through", 0
		}
	case "merge-stack", "merge-ready", "cancel":
		if len(fields) == 2 {
			return fields[1], 0
		}
	case "merge-prefix":
		if len(fields) == 3 {
			n, err := strconv.Atoi(fields[2])
			if err == nil && n > 0 {
				return fields[1], n
			}
		}
	}
	return "", 0
}

func (g *GitHub) endpoint(path string) string {
	return strings.TrimRight(g.p.APIURL, "/") + "/repos/" + g.p.Repo + path
}

func (g *GitHub) call(ctx context.Context, method, endpoint string, input, output any) error {
	var data []byte
	var err error
	if input != nil {
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := g.p.Tokens.Token(ctx)
		if err != nil {
			return err
		}
		r, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Accept", "application/vnd.github+json")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-GitHub-Api-Version", "2026-03-10")
		resp, err := g.client.Do(r)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			g.p.Tokens.Invalidate(token)
			continue
		}
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			return &apiError{status: resp.StatusCode, method: method}
		}
		if output != nil {
			err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output)
		}
		resp.Body.Close()
		return err
	}
	return fmt.Errorf("github: authentication failed")
}

// pages traverses all pages; silently treating page 1 as the whole review
// would lose late cancellations, approvals, or stack members.
func pages[T any](ctx context.Context, g *GitHub, path string) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		var rows []T
		if err := g.call(ctx, "GET", g.endpoint(path+sep+"per_page=100&page="+strconv.Itoa(page)), nil, &rows); err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < 100 {
			return all, nil
		}
		if page >= 1000 {
			return nil, fmt.Errorf("github pagination exceeds limit")
		}
	}
}

func (g *GitHub) getPull(ctx context.Context, number int) (pull, error) {
	var p pull
	err := g.call(ctx, "GET", g.endpoint("/pulls/"+strconv.Itoa(number)), nil, &p)
	return p, err
}

func (g *GitHub) latestRequest(ctx context.Context, p pull, permissions map[string]bool) (request, error) {
	cs, err := pages[comment](ctx, g, "/issues/"+strconv.Itoa(p.Number)+"/comments")
	if err != nil {
		return request{}, err
	}
	var result request
	for _, c := range cs {
		parsed, valid := parseCommand(c.Body, g.p.Bot)
		action, count := parsed.Action, parsed.Count
		if !valid {
			continue
		}
		if action == "" {
			continue
		}
		allowed, known := permissions[c.User.Login]
		if !known {
			var err error
			allowed, err = g.writer(ctx, c.User.Login)
			if err != nil {
				return request{}, err
			}
			permissions[c.User.Login] = allowed
		}
		if allowed && c.ID > result.ID {
			result = request{PR: p, ID: c.ID, Action: action, Count: count, Urgent: parsed.Urgent, SkipChecks: parsed.SkipChecks, OverridePause: parsed.OverridePause, Requester: c.User.Login, Reason: parsed.Reason}
		}
	}
	for _, c := range cs {
		if strings.Contains(c.Body, stackRejectionMarker(result.ID)) {
			result.Notified = true
		}
	}
	return result, nil
}

func stackRejectionMarker(id int64) string {
	return fmt.Sprintf("<!-- gauntlet:stack-required:%d -->", id)
}

func (g *GitHub) writer(ctx context.Context, login string) (bool, error) {
	var p struct{ Permission string }
	if err := g.call(ctx, "GET", g.endpoint("/collaborators/"+url.PathEscape(login)+"/permission"), nil, &p); err != nil {
		if e, ok := errors.AsType[*apiError](err); ok && e.status == 404 {
			return false, nil
		}
		return false, err
	}
	return p.Permission == "admin" || p.Permission == "maintain" || p.Permission == "write", nil
}

func (g *GitHub) ready(ctx context.Context, p pull) (bool, error) {
	ready, _, err := g.readyReason(ctx, p)
	return ready, err
}

func (g *GitHub) readyReason(ctx context.Context, p pull, skipChecks ...bool) (bool, string, error) {
	if p.State != "open" || p.Draft {
		return false, "PR is closed or a draft", nil
	}
	type vote struct {
		State    string
		CommitID string `json:"commit_id"`
		User     struct{ Login string }
	}
	reviews, err := pages[vote](ctx, g, "/pulls/"+strconv.Itoa(p.Number)+"/reviews")
	if err != nil {
		return false, "readiness facts unavailable", err
	}
	latest := map[string]vote{}
	for _, v := range reviews {
		if v.State != "COMMENTED" && v.State != "PENDING" {
			latest[v.User.Login] = v
		}
	}
	approvals := 0
	for _, v := range latest {
		trusted, err := g.writer(ctx, v.User.Login)
		if err != nil {
			return false, "readiness facts unavailable", err
		}
		if !trusted {
			continue
		}
		if v.State == "CHANGES_REQUESTED" {
			return false, "changes requested by " + v.User.Login, nil
		}
		if v.State == "APPROVED" && v.CommitID == p.Head.SHA {
			approvals++
		}
	}
	if approvals < g.p.Approvals {
		return false, fmt.Sprintf("%d current approvals; %d required", approvals, g.p.Approvals), nil
	}
	if g.p.RequireResolvedConversations {
		resolved, err := g.conversationsResolved(ctx, p.Number)
		if err != nil || !resolved {
			return false, "unresolved or inaccessible review conversations", err
		}
	}
	if len(g.p.RequiredChecks) == 0 || (len(skipChecks) > 0 && skipChecks[0]) {
		return true, "", nil
	}
	// Status history is newest-first. Keep the latest result for each context,
	// including contexts that appear beyond the first page.
	type status struct{ Context, State string }
	statuses, err := pages[status](ctx, g, "/commits/"+p.Head.SHA+"/statuses")
	if err != nil {
		return false, "readiness facts unavailable", err
	}
	green := map[string]bool{}
	for _, s := range statuses {
		if _, seen := green[s.Context]; !seen {
			green[s.Context] = s.State == "success"
		}
	}
	// Check runs use their own paginated envelope, not the statuses API.
	for page := 1; ; page++ {
		var runs struct {
			CheckRuns []struct{ Name, Status, Conclusion string } `json:"check_runs"`
		}
		if err := g.call(ctx, "GET", g.endpoint(fmt.Sprintf("/commits/%s/check-runs?per_page=100&page=%d", p.Head.SHA, page)), nil, &runs); err != nil {
			return false, "readiness facts unavailable", err
		}
		for _, r := range runs.CheckRuns {
			passed := r.Status == "completed" && (r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped")
			// Different apps (and the statuses API) may publish the same name.
			// One passing result must not hide another producer's failure.
			previous, seen := green[r.Name]
			green[r.Name] = passed && (!seen || previous)
		}
		if len(runs.CheckRuns) < 100 {
			break
		}
		if page >= 1000 {
			return false, "check facts incomplete", fmt.Errorf("check-run pagination exceeds limit")
		}
	}
	for _, name := range g.p.RequiredChecks {
		if !green[name] {
			return false, "required check is missing or not passing: " + name, nil
		}
	}
	return true, "", nil
}

func slot(target string, number int) string {
	return fmt.Sprintf("refs/heads/for/%s/github/pr-%010d", target, number)
}

// stack returns native stack members bottom-first, including closed members
// whose original heads still bound a successor's delta. Legacy branch stacks
// are supported too, with ambiguity rejected rather than guessed.
func (g *GitHub) stack(ctx context.Context, p pull, open []pull, whole bool) ([]pull, string, error) {
	if p.Stack != nil {
		var s struct {
			Base         ref
			PullRequests []struct{ Number int } `json:"pull_requests"`
		}
		if err := g.call(ctx, "GET", g.endpoint("/stacks/"+strconv.Itoa(p.Stack.Number)), nil, &s); err != nil {
			return nil, "", err
		}
		var members []pull
		for _, entry := range s.PullRequests {
			member, err := g.getPull(ctx, entry.Number)
			if err != nil {
				return nil, "", err
			}
			members = append(members, member)
		}
		return members, s.Base.Ref, nil
	}
	chain := []pull{p}
	seen := map[int]bool{p.Number: true}
	for {
		bottom := chain[0]
		if _, ok := g.p.Targets[bottom.Base.Ref]; ok {
			break
		}
		var parents []pull
		for _, other := range open {
			if other.Head.Ref == bottom.Base.Ref && (other.Head.Repo == nil || strings.EqualFold(other.Head.Repo.FullName, g.p.Repo)) {
				parents = append(parents, other)
			}
		}
		if len(parents) != 1 || seen[parents[0].Number] {
			return nil, "", fmt.Errorf("PR #%d has an ambiguous or cyclic base dependency", bottom.Number)
		}
		seen[parents[0].Number] = true
		chain = append([]pull{parents[0]}, chain...)
	}
	for whole {
		top := chain[len(chain)-1]
		var children []pull
		for _, other := range open {
			if other.Base.Ref == top.Head.Ref && !seen[other.Number] {
				children = append(children, other)
			}
		}
		if len(children) == 0 {
			break
		}
		if len(children) > 1 {
			return nil, "", fmt.Errorf("PR #%d has multiple stack successors", top.Number)
		}
		seen[children[0].Number] = true
		chain = append(chain, children[0])
	}
	return chain, chain[0].Base.Ref, nil
}

func version(c core.Candidate, requestID int64) string {
	// A predecessor landing changes DependsOn from a slot to "" without
	// changing the tested delta. SourceBase still pins its original revision.
	data, _ := json.Marshal([]any{c.SHA, c.SourceBase, c.Message, c.ReviewURL, requestID})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (g *GitHub) candidates(ctx context.Context, fetch bool, bypassChecks ...bool) ([]core.Candidate, error) {
	// Closed roots retain stack-wide requests while later members land.
	// This also distinguishes a genuinely landed ancestor from an abandoned PR.
	open, err := pages[pull](ctx, g, "/pulls?state=all")
	if err != nil {
		return nil, err
	}
	// Only closed PRs that still bound an open stack can affect admission.
	// Reconstruct native membership from the stack API: GitHub may omit a
	// closed member's stack field while its root request is still operative.
	active := map[int]bool{}
	type nativeStack struct {
		members []pull
		branch  string
	}
	nativeByMember := map[int]nativeStack{}
	seenStacks := map[int]bool{}
	for _, p := range open {
		if p.State != "open" {
			continue
		}
		active[p.Number] = true
		if p.Stack == nil || seenStacks[p.Stack.Number] {
			continue
		}
		seenStacks[p.Stack.Number] = true
		members, branch, err := g.stack(ctx, p, open, true)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			active[member.Number] = true
			nativeByMember[member.Number] = nativeStack{members, branch}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range open {
			if !active[p.Number] {
				continue
			}
			if _, configured := g.p.Targets[p.Base.Ref]; configured {
				continue
			}
			for _, parent := range open {
				if !active[parent.Number] && parent.Head.Ref == p.Base.Ref && (parent.Head.Repo == nil || strings.EqualFold(parent.Head.Repo.FullName, g.p.Repo)) {
					active[parent.Number] = true
					changed = true
				}
			}
		}
	}
	var requests []request
	permissions := map[string]bool{}
	for _, p := range open {
		if !active[p.Number] {
			continue
		}
		r, err := g.latestRequest(ctx, p, permissions)
		if err != nil {
			return nil, err
		}
		if r.ID != 0 {
			requests = append(requests, r)
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	selected := map[string]core.Candidate{}
	for _, r := range requests {
		if (r.SkipChecks || r.OverridePause) && (!g.p.EmergencyEnabled || r.Reason == "" || g.p.IntentPath == "") {
			continue
		}
		var members []pull
		var branch string
		if native, ok := nativeByMember[r.PR.Number]; ok {
			members, branch = native.members, native.branch
		} else {
			var err error
			members, branch, err = g.stack(ctx, r.PR, open, r.Action != "merge" && r.Action != "merge-through" && r.Action != "cancel")
			if err != nil {
				return nil, err
			}
		}
		target, configured := g.p.Targets[branch]
		if !configured {
			continue
		}
		if r.Action == "cancel" {
			delete(selected, slot(target, r.PR.Number))
			continue
		}
		if r.Action == "merge" || r.Action == "merge-through" {
			end := slices.IndexFunc(members, func(p pull) bool { return p.Number == r.PR.Number })
			if end < 0 {
				return nil, fmt.Errorf("PR #%d is absent from its stack", r.PR.Number)
			}
			members = members[:end+1]
			if r.Action == "merge" && slices.ContainsFunc(members[:end], func(p pull) bool { return p.State == "open" }) {
				// A rejected request admits no prerequisites, even if another
				// request independently admits them. Feedback survives restarts.
				if fetch && !r.Notified {
					body := fmt.Sprintf("Cannot merge PR #%d alone: it has unlanded prerequisite PRs. Use `@%s merge stack` to request the stack through this PR; all members must be ready.\n\n%s", r.PR.Number, g.p.Bot, stackRejectionMarker(r.ID))
					if err := g.call(ctx, "POST", g.endpoint(fmt.Sprintf("/issues/%d/comments", r.PR.Number)), map[string]string{"body": body}, nil); err != nil {
						return nil, err
					}
				}
				continue
			}
		}
		var prefix []core.Candidate
		var previous, sourceBase string
		remaining := r.Count
		for _, p := range members {
			if p.State != "open" {
				// A manually closed PR is not evidence of a landing. Only a
				// predecessor whose original head is on the target, or whose
				// gauntlet landing marker exists, may satisfy this dependency.
				landed, err := g.p.Git.ReviewLanded(ctx, branch, slot(target, p.Number), p.Head.SHA)
				if err != nil {
					return nil, err
				}
				if !landed {
					prefix = nil
					break
				}
				sourceBase = p.Head.SHA
				continue
			}
			ready, reason, err := g.readyReason(ctx, p, r.SkipChecks || (len(bypassChecks) > 0 && bypassChecks[0]))
			if err != nil {
				return nil, err
			}
			blocked := ""
			if !ready && g.p.EmergencyEnabled && strings.HasPrefix(reason, "required check") {
				blocked = reason
				ready = true
			}
			if !ready {
				if fetch {
					g.admissionFeedback(ctx, p, target, reason)
				}
				if r.Action != "merge-ready" {
					prefix = nil
				}
				break
			}
			c := core.Candidate{Ref: slot(target, p.Number), Target: target, User: p.User.Login,
				Topic: fmt.Sprintf("pr-%d", p.Number), SHA: p.Head.SHA, Source: "github",
				SourceBase: sourceBase, DependsOn: previous, ReviewURL: p.HTMLURL, Urgent: r.Urgent, AdmissionBlocked: blocked, SkipChecks: r.SkipChecks, OverridePause: r.OverridePause, Requester: r.Requester, RequestReason: r.Reason,
				Message: fmt.Sprintf("%s (#%d)\n\n%s", p.Title, p.Number, p.Body)}
			c.Version = version(c, r.ID)
			if r.Urgent {
				c.Version += ":urgent"
			}
			if r.SkipChecks || r.OverridePause {
				flags := fmt.Sprintf("%t:%t:%s", r.SkipChecks, r.OverridePause, r.Reason)
				c.Version += ":" + fmt.Sprintf("%x", sha256.Sum256([]byte(flags)))
				c.EmergencyID = fmt.Sprint(r.ID)
			}
			prefix = append(prefix, c)
			previous, sourceBase = c.Ref, c.SHA
			if r.Action == "merge" && p.Number == r.PR.Number {
				break
			}
			if r.Action == "merge-prefix" {
				remaining--
				if remaining == 0 {
					break
				}
			}
		}
		if (r.SkipChecks || r.OverridePause) && len(prefix) > 0 {
			if err := g.freezeIntent(ctx, r, prefix); err != nil {
				if fetch {
					g.admissionFeedback(ctx, r.PR, target, err.Error())
				}
				continue
			}
			for i := range prefix {
				prefix[i].RequestedCount = len(prefix)
			}
		}
		for _, c := range prefix {
			selected[c.Ref] = c
		}
	}
	// Cancelled parents also block descendants, regardless of request order.
	var out []core.Candidate
	for _, c := range selected {
		if c.DependsOn != "" {
			if _, ok := selected[c.DependsOn]; !ok {
				continue
			}
		}
		if fetch {
			number, err := pullNumber(c.Ref)
			if err != nil {
				return nil, err
			}
			if err := g.p.Git.FetchReview(ctx, fmt.Sprintf("refs/pull/%d/head", number), fmt.Sprintf("refs/gauntlet/reviews/github/%d", number), c.SHA); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// Invalidate requests a fresh admission snapshot on the next queue tick.
// Local request/completion writes invalidate immediately; webhook intake can
// use the same operation without treating a delivery as landing authority.
func (g *GitHub) Invalidate() {
	g.pollMu.Lock()
	defer g.pollMu.Unlock()
	g.nextPoll = time.Time{}
	g.cached = nil
	g.pollGeneration++
}

func (g *GitHub) Candidates(ctx context.Context) ([]core.Candidate, error) {
	g.pollMu.Lock()
	if g.now().Before(g.nextPoll) {
		current := slices.Clone(g.cached)
		g.pollMu.Unlock()
		return current, nil
	}
	generation := g.pollGeneration
	g.pollMu.Unlock()
	current, err := g.candidates(ctx, true)
	g.pollMu.Lock()
	defer g.pollMu.Unlock()
	if err != nil {
		g.nextPoll = time.Time{}
		g.cached = nil
		return nil, err
	}
	// A delivery arriving during the poll requests another refresh. Do not
	// overwrite it with a snapshot assembled before the delivery.
	if generation == g.pollGeneration {
		g.cached = slices.Clone(current)
		g.nextPoll = g.now().Add(g.p.PollInterval)
	}
	return current, nil
}

func (g *GitHub) Validate(ctx context.Context, c core.Candidate) error {
	defer g.Invalidate()
	current, err := g.candidates(ctx, false)
	if err != nil {
		return err
	}
	for _, n := range current {
		if n.AdmissionBlocked == "" && n.Ref == c.Ref && n.SHA == c.SHA && n.Version == c.Version {
			return nil
		}
	}
	return fmt.Errorf("review revision, message, dependency, approval, or request changed")
}

func pullNumber(ref string) (int, error) {
	_, tail, _ := strings.Cut(ref, "/github/pr-")
	n, err := strconv.Atoi(tail)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid GitHub review slot")
	}
	return n, nil
}

func (g *GitHub) Landed(ctx context.Context, c core.Candidate, commit string) error {
	defer g.Invalidate()
	n, err := pullNumber(c.Ref)
	if err != nil {
		return err
	}
	p, err := g.getPull(ctx, n)
	if err != nil {
		return err
	}
	marker := "<!-- gauntlet:landed:" + commit + " -->"
	cs, err := pages[comment](ctx, g, fmt.Sprintf("/issues/%d/comments", n))
	if err != nil {
		return err
	}
	found := false
	for _, v := range cs {
		if strings.Contains(v.Body, marker) {
			found = true
		}
	}
	if !found {
		commitURL := strings.TrimSuffix(strings.Split(c.ReviewURL, "/pull/")[0], "/") + "/commit/" + commit
		short := commit
		if len(short) > 12 {
			short = short[:12]
		}
		body := fmt.Sprintf("Landed by Gauntlet as [%s](%s), from revision `%s`.\n\nThe exact constructed commit was pushed to the target. Gauntlet preserves contributor branches; GitHub’s merge API would create another commit, so this PR is closed with a landing link.\n\n%s", short, commitURL, c.SHA, marker)
		if err := g.call(ctx, "POST", g.endpoint(fmt.Sprintf("/issues/%d/comments", n)), map[string]string{"body": body}, nil); err != nil {
			return err
		}
	}
	if p.State == "open" && p.Head.SHA == c.SHA {
		return g.call(ctx, "PATCH", g.endpoint(fmt.Sprintf("/pulls/%d", n)), map[string]string{"state": "closed"}, nil)
	}
	return nil
}

// Request is also used by the CLI; no shell or installed gh binary needed.
func (g *GitHub) Request(ctx context.Context, number int, action string, count int) error {
	defer g.Invalidate()
	body := "@" + g.p.Bot + " " + action
	if action == "merge-through" {
		body = "@" + g.p.Bot + " merge stack"
	}
	if action == "merge-prefix" {
		body += " " + strconv.Itoa(count)
	}
	if parsed, _ := Command(body, g.p.Bot); parsed == "" {
		return fmt.Errorf("invalid queue command")
	}
	return g.call(ctx, "POST", g.endpoint(fmt.Sprintf("/issues/%d/comments", number)), map[string]string{"body": body}, nil)
}

func (g *GitHub) ValidateEmergency(ctx context.Context, c core.Candidate) error {
	defer g.Invalidate()
	current, err := g.candidates(ctx, false, true)
	if err != nil {
		return err
	}
	for _, n := range current {
		if n.Ref == c.Ref && n.SHA == c.SHA && n.Version == c.Version {
			return nil
		}
	}
	return fmt.Errorf("review revision, authorization, conversations, or request changed")
}
