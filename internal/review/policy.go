package review

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
)

func (g *GitHub) settings() policy.Settings {
	return policy.Settings{Approvals: g.p.Approvals, RequiredChecks: g.p.RequiredChecks, ResolvedConversations: g.p.RequireResolvedConversations, EmergencyEnabled: g.p.EmergencyEnabled}
}
func (g *GitHub) permission(ctx context.Context, login string) (string, error) {
	cache, _ := ctx.Value(permissionCacheKey{}).(map[string]string)
	if value, ok := cache[login]; ok {
		return value, nil
	}
	var result struct{ Permission string }
	err := g.call(ctx, "GET", g.endpoint("/collaborators/"+url.PathEscape(login)+"/permission"), nil, &result)
	if e, ok := errors.AsType[*apiError](err); ok && e.status == 404 {
		result.Permission, err = "none", nil
	}
	if err == nil && cache != nil {
		cache[login] = result.Permission
	}
	return result.Permission, err
}
func (g *GitHub) principal(ctx context.Context, login string) (policy.Principal, error) {
	permission, err := g.permission(ctx, login)
	if err != nil {
		return policy.Principal{}, err
	}
	principal := policy.Principal{Source: "github", ID: login, Authenticated: login != "", Permission: permission, Teams: map[string]bool{}}
	for _, team := range g.p.PolicyTeams {
		member, err := g.teamMember(ctx, team, login)
		if err != nil {
			return policy.Principal{}, err
		}
		principal.Teams[team] = member
	}
	return principal, nil
}
func (g *GitHub) teamMember(ctx context.Context, team, user string) (bool, error) {
	org, slug, ok := strings.Cut(team, "/")
	if !ok || org == "" || slug == "" {
		return false, fmt.Errorf("policy team must be org/slug")
	}
	var member struct{ State string }
	endpoint := strings.TrimSuffix(g.p.APIURL, "/") + "/orgs/" + url.PathEscape(org) + "/teams/" + url.PathEscape(slug) + "/memberships/" + url.PathEscape(user)
	err := g.call(ctx, "GET", endpoint, nil, &member)
	if e, ok := errors.AsType[*apiError](err); ok && e.status == 404 {
		return false, nil
	}
	return member.State == "active", err
}
func (g *GitHub) commandDecision(ctx context.Context, p pull, c comment, parsed parsedCommand, principal policy.Principal, open []pull) (policy.Decision, error) {
	input := policy.Input{SchemaVersion: 1, Phase: "command", Principal: principal, Command: policy.Command{Kind: parsed.Action, ID: fmt.Sprint(c.ID), Count: parsed.Count, Urgent: parsed.Urgent, Reason: parsed.Reason}, Settings: g.settings(), Emergency: policy.Emergency{SkipChecks: parsed.SkipChecks, OverridePause: parsed.OverridePause}, Candidate: map[string]any{"source": "github", "sha": p.Head.SHA}, Forge: map[string]any{"kind": "github", "pr": p.Number, "draft": p.Draft, "state": p.State}}
	input.Branch = p.Base.Ref
	if p.Stack != nil {
		input.Branch = p.Stack.Base.Ref
	}
	input.Target = g.p.Targets[input.Branch]
	if g.p.Policy.HasCustom("command") {
		facts, err := g.collectFacts(ctx, p, true)
		if err != nil {
			return policy.Decision{}, err
		}
		input.Forge = facts
		input.Paths, _ = facts["paths"].([]string)
		input.PathsAvailable = true
		members, branch, err := g.stack(ctx, p, open, parsed.Action == "merge-stack" || parsed.Action == "merge-ready" || parsed.Action == "merge-prefix")
		if err != nil {
			return policy.Decision{}, err
		}
		if parsed.Action == "merge" || parsed.Action == "merge-through" || parsed.Action == "check" || parsed.Action == "cancel" {
			for i, m := range members {
				if m.Number == p.Number {
					members = members[:i+1]
					break
				}
			}
		}
		if parsed.Action == "merge-prefix" {
			remaining := parsed.Count
			for i, m := range members {
				if m.State == "open" {
					remaining--
					if remaining == 0 {
						members = members[:i+1]
						break
					}
				}
			}
		}
		input.Target, input.Branch = g.p.Targets[branch], branch
		for _, m := range members {
			input.Stack = append(input.Stack, map[string]any{"source": "github", "ref": slot(input.Target, m.Number), "sha": m.Head.SHA, "pr": m.Number, "state": m.State, "draft": m.Draft})
		}
	}
	return g.p.Policy.Decide(ctx, "command", input)

}

// collectFacts supplies readiness and policy from the same immutable snapshot.
func (g *GitHub) collectFacts(ctx context.Context, p pull, details bool) (map[string]any, error) {
	cache, _ := ctx.Value(factCacheKey{}).(map[string]map[string]any)
	key := fmt.Sprintf("%d:%s:%t", p.Number, p.Head.SHA, details)
	if facts, ok := cache[key]; ok {
		return cloneFacts(facts), nil
	}
	if !details {
		if facts, ok := cache[fmt.Sprintf("%d:%s:true", p.Number, p.Head.SHA)]; ok {
			return cloneFacts(facts), nil
		}
	}
	n := p.Number
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
			permission, err := g.permission(ctx, vote.User.Login)
			if err != nil {
				return nil, err
			}
			reviews[vote.User.Login] = map[string]any{"state": vote.State, "commit": vote.Commit, "current": vote.Commit == p.Head.SHA, "permission": permission}
		}
	}
	var resolved any
	if g.p.RequireResolvedConversations || details {
		value, err := g.conversationsResolved(ctx, n)
		if err != nil {
			return nil, err
		}
		resolved = value
	}
	paths := []string{}
	if details {
		type file struct {
			Previous string `json:"previous_filename"`
			Filename string `json:"filename"`
		}
		files, err := pages[file](ctx, g, fmt.Sprintf("/pulls/%d/files", n))
		if err != nil {
			return nil, err
		}
		if len(files) >= 3000 {
			return nil, fmt.Errorf("GitHub changed-path facts may be truncated")
		}
		for _, file := range files {
			paths = append(paths, file.Filename)
			if file.Previous != "" {
				paths = append(paths, file.Previous)
			}
		}
	}
	checks := []map[string]any{}
	if len(g.p.RequiredChecks) > 0 || details {
		type status struct {
			Context string `json:"context"`
			State   string `json:"state"`
			Creator struct {
				Login string `json:"login"`
			} `json:"creator"`
		}
		statuses, err := pages[status](ctx, g, "/commits/"+p.Head.SHA+"/statuses")
		if err != nil {
			return nil, err
		}
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
			if err := g.call(ctx, "GET", g.endpoint(fmt.Sprintf("/commits/%s/check-runs?per_page=100&page=%d", p.Head.SHA, page)), nil, &envelope); err != nil {
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
	}
	teams := map[string]map[string]bool{}
	for _, team := range g.p.PolicyTeams {
		members := map[string]bool{}
		if len(reviews) > 100 {
			return nil, fmt.Errorf("policy reviewer facts exceed limit")
		}
		for user := range reviews {
			value, err := g.teamMember(ctx, team, user)
			if err != nil {
				return nil, err
			}
			members[user] = value
		}
		teams[team] = members
	}
	facts := map[string]any{"kind": "github", "pr": n, "head_sha": p.Head.SHA, "draft": p.Draft, "state": p.State, "reviews": reviews, "conversations_resolved": resolved, "checks": checks, "paths": paths, "paths_available": details, "teams": teams}
	if cache != nil {
		cache[key] = facts
	}
	return cloneFacts(facts), nil
}

func (g *GitHub) readiness(ctx context.Context, p pull, c core.Candidate, r request, branch string, skip bool, stack ...pull) (policy.Decision, policy.Input, error) {
	facts, err := g.collectFacts(ctx, p, g.p.Policy.HasCustom("submission"))
	if err != nil {
		return policy.Decision{}, policy.Input{}, err
	}
	principal, err := g.principal(ctx, r.Requester)
	if err != nil {
		return policy.Decision{}, policy.Input{}, err
	}
	facts["requester"], facts["request_id"], facts["permission"] = r.Requester, r.ID, principal.Permission
	for team, member := range principal.Teams {
		facts["teams"].(map[string]map[string]bool)[team][r.Requester] = member
	}
	paths, _ := facts["paths"].([]string)
	input := policy.Input{SchemaVersion: 1, Phase: "admission", Principal: principal, Command: policy.Command{Kind: r.Action, ID: fmt.Sprint(r.ID), Count: r.Count, Urgent: r.Urgent, Reason: r.Reason}, Candidate: map[string]any{"ref": c.Ref, "source": c.Source, "sha": c.SHA, "version": c.Version, "source_base": c.SourceBase, "depends_on": c.DependsOn, "requester": c.Requester, "urgent": c.Urgent}, Target: c.Target, Branch: branch, Forge: facts, Settings: g.settings(), Emergency: policy.Emergency{SkipChecks: skip, OverridePause: r.OverridePause}, Paths: paths, PathsAvailable: g.p.Policy.HasCustom("submission")}
	for _, m := range stack {
		input.Stack = append(input.Stack, map[string]any{"source": "github", "ref": slot(c.Target, m.Number), "sha": m.Head.SHA, "pr": m.Number, "state": m.State, "draft": m.Draft})
	}
	decision, err := g.p.Policy.Decide(ctx, "submission", input)
	return decision, input, err
}

// PolicyInput returns the same intake snapshot; landing uses ValidatePolicyInputs.
func (g *GitHub) PolicyInput(ctx context.Context, c core.Candidate) (policy.Input, error) {
	g.pollMu.Lock()
	input, ok := g.cachedInputs[c.Ref]
	g.pollMu.Unlock()
	if !ok || input.Candidate["version"] != c.Version {
		return policy.Input{}, fmt.Errorf("policy intake snapshot unavailable")
	}
	return input, nil
}
func (g *GitHub) ValidatePolicyInputs(ctx context.Context, members []core.Candidate, skip bool) (map[string]policy.Input, error) {
	defer g.Invalidate()
	inputs := map[string]policy.Input{}
	current, err := g.scan(ctx, false, skip, inputs)
	if err != nil {
		return nil, err
	}
	for _, c := range members {
		found := false
		for _, n := range current {
			if n.AdmissionBlocked == "" && n.Ref == c.Ref && n.SHA == c.SHA && n.Version == c.Version {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("review revision, request, dependency, or policy changed")
		}
	}
	return inputs, nil
}

func onlyCheckFailures(decision policy.Decision) bool {
	failed := false
	for _, r := range decision.Requirements {
		if !r.Satisfied {
			if r.Name != "required-check" {
				return false
			}
			failed = true
		}
	}
	return failed
}

func (g *GitHub) commandFeedback(ctx context.Context, p pull, c comment, parsed parsedCommand, command policy.Decision) error {
	var body strings.Builder
	fmt.Fprintf(&body, "Gauntlet policy for request %d (%s), revision `%s`:\n", c.ID, parsed.Action, p.Head.SHA)
	render := func(decision policy.Decision) {
		for _, requirement := range decision.Requirements {
			state := "satisfied"
			if !requirement.Satisfied {
				state = "blocked"
			}
			fmt.Fprintf(&body, "\n- %s: **%s**", safeMarkdown(requirement.Name), state)
			if !requirement.Satisfied {
				fmt.Fprintf(&body, " — %s", safeMarkdown(requirement.Reason))
			}
		}
	}
	if !command.Allow {
		body.WriteString("\nCommand denied.")
	}
	render(command)
	if command.Allow && parsed.Action == "check" {
		branch := p.Base.Ref
		if p.Stack != nil {
			branch = p.Stack.Base.Ref
		}
		candidate := core.Candidate{Ref: slot(g.p.Targets[branch], p.Number), Target: g.p.Targets[branch], Source: "github", SHA: p.Head.SHA}
		request := request{PR: p, ID: c.ID, Action: "check", Requester: c.User.Login}
		decision, _, err := g.readiness(ctx, p, candidate, request, branch, false)
		if err != nil {
			fmt.Fprintf(&body, "\n\nReadiness unavailable: %s", safeMarkdown(err.Error()))
		} else {
			render(decision)
			if decision.Allow {
				body.WriteString("\n\nThis PR is authorized and ready. Stack prerequisites are checked when a merge is requested. This check does not enqueue a merge.")
			} else {
				body.WriteString("\n\nAuthorized; waiting for readiness requirements.")
			}
		}
	}
	return g.feedback(ctx, core.Candidate{Ref: slot("policy", p.Number), SHA: p.Head.SHA}, body.String())
}

type factCacheKey struct{}

func cloneFacts(facts map[string]any) map[string]any {
	out := maps.Clone(facts)
	teams := maps.Clone(facts["teams"].(map[string]map[string]bool))
	for team, members := range teams {
		teams[team] = maps.Clone(members)
	}
	out["teams"] = teams
	return out
}

type permissionCacheKey struct{}
