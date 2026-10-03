package review

import (
	"context"
	"fmt"
	"github.com/sgrankin/gauntlet/internal/core"
	"net/url"
	"strings"
)

// PolicyFacts never reuses admission's cached facts at the publication boundary.
func (g *GitHub) PolicyFacts(ctx context.Context, c core.Candidate) (map[string]any, error) {
	n, err := pullNumber(c.Ref)
	if err != nil {
		return nil, err
	}
	p, err := g.getPull(ctx, n)
	if err != nil {
		return nil, err
	}
	if p.Head.SHA != c.SHA {
		return nil, fmt.Errorf("policy revision changed")
	}
	request, err := g.latestRequest(ctx, p, map[string]bool{})
	if err != nil {
		return nil, err
	}
	comments, err := pages[comment](ctx, g, fmt.Sprintf("/issues/%d/comments", n))
	if err != nil {
		return nil, err
	}
	requester := ""
	for _, comment := range comments {
		if comment.ID == request.ID {
			requester = comment.User.Login
		}
	}
	if requester == "" {
		return nil, fmt.Errorf("merge requester unavailable")
	}
	var permission struct {
		Permission string `json:"permission"`
	}
	if err := g.call(ctx, "GET", g.endpoint("/collaborators/"+url.PathEscape(requester)+"/permission"), nil, &permission); err != nil {
		return nil, err
	}
	type vote struct {
		State  string `json:"state"`
		Commit string `json:"commit_id"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	votes, err := pages[vote](ctx, g, fmt.Sprintf("/pulls/%d/reviews", n))
	if err != nil {
		return nil, err
	}
	reviews := map[string]any{}
	for _, vote := range votes {
		if vote.State != "COMMENTED" && vote.State != "PENDING" {
			reviews[vote.User.Login] = map[string]any{"state": vote.State, "commit": vote.Commit, "current": vote.Commit == c.SHA}
		}
	}
	resolved, err := g.conversationsResolved(ctx, n)
	if err != nil {
		return nil, err
	}
	type file struct {
		Previous string `json:"previous_filename"`
		Filename string `json:"filename"`
	}
	files, err := pages[file](ctx, g, fmt.Sprintf("/pulls/%d/files", n))
	if err != nil {
		return nil, err
	}
	// GitHub truncates large file lists. An incomplete policy input cannot
	// satisfy path rules merely because a sensitive path was omitted.
	if len(files) >= 3000 {
		return nil, fmt.Errorf("GitHub changed-path facts may be truncated")
	}
	paths := []string{}
	for _, file := range files {
		paths = append(paths, file.Filename)
		if file.Previous != "" {
			paths = append(paths, file.Previous)
		}
	}
	type status struct {
		Context string `json:"context"`
		State   string `json:"state"`
		Creator struct {
			Login string `json:"login"`
		} `json:"creator"`
	}
	statuses, err := pages[status](ctx, g, "/commits/"+c.SHA+"/statuses")
	if err != nil {
		return nil, err
	}
	checks := []map[string]any{}
	seen := map[string]bool{}
	for _, status := range statuses {
		key := status.Context + "/" + status.Creator.Login
		if !seen[key] {
			seen[key] = true
			checks = append(checks, map[string]any{"name": status.Context, "state": status.State, "producer": status.Creator.Login, "kind": "status"})
		}
	}
	for page := 1; ; page++ {
		var envelope struct {
			Runs []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				App        struct {
					Slug string `json:"slug"`
					ID   int64  `json:"id"`
				} `json:"app"`
			} `json:"check_runs"`
		}
		if err := g.call(ctx, "GET", g.endpoint(fmt.Sprintf("/commits/%s/check-runs?per_page=100&page=%d", c.SHA, page)), nil, &envelope); err != nil {
			return nil, err
		}
		for _, run := range envelope.Runs {
			checks = append(checks, map[string]any{"name": run.Name, "state": run.Status, "conclusion": run.Conclusion, "producer": run.App.Slug, "producer_id": run.App.ID, "kind": "check_run"})
		}
		if len(envelope.Runs) < 100 {
			break
		}
		if page >= 1000 {
			return nil, fmt.Errorf("policy check facts incomplete")
		}
	}
	teams := map[string]map[string]bool{}
	users := []string{requester}
	for reviewer := range reviews {
		if reviewer != requester {
			users = append(users, reviewer)
		}
	}
	if len(users) > 100 {
		return nil, fmt.Errorf("policy reviewer facts exceed limit")
	}
	for _, team := range g.p.PolicyTeams {
		org, slug, ok := strings.Cut(team, "/")
		if !ok || org == "" || slug == "" {
			return nil, fmt.Errorf("policy team must be org/slug")
		}
		members := map[string]bool{}
		for _, user := range users {
			var member struct {
				State string `json:"state"`
			}
			endpoint := strings.TrimSuffix(g.p.APIURL, "/") + "/orgs/" + url.PathEscape(org) + "/teams/" + url.PathEscape(slug) + "/memberships/" + url.PathEscape(user)
			err := g.call(ctx, "GET", endpoint, nil, &member)
			if e, ok := err.(*apiError); ok && e.status == 404 {
				members[user] = false
				continue
			}
			if err != nil {
				return nil, err
			}
			members[user] = member.State == "active"
		}
		teams[team] = members
	}
	return map[string]any{"kind": "github", "pr": n, "head_sha": p.Head.SHA, "draft": p.Draft, "state": p.State, "requester": requester, "request_id": request.ID, "permission": permission.Permission, "reviews": reviews, "conversations_resolved": resolved, "checks": checks, "paths": paths, "teams": teams}, nil
}
