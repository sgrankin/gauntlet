package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/ghauth"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/review"
)

func githubReviewParams(cfg *config.Daemon, app *ghauth.App, repo *gitx.Repo) review.GitHubParams {
	p := cfg.GitHub.PullRequests
	params := review.GitHubParams{Repo: cfg.GitHub.Repo, APIURL: cfg.GitHub.APIURL, Git: repo, Targets: map[string]string{}, Bot: p.Bot, Approvals: *p.Approvals, RequiredChecks: p.RequiredChecks, PollInterval: p.PollInterval, EmergencyEnabled: cfg.EmergencyMerges}
	params.RequireResolvedConversations = p.RequireResolvedConversations
	if app != nil {
		params.Tokens = app
	} else {
		params.Tokens = review.StaticToken(os.Getenv(cfg.GitHub.TokenEnv))
	}
	for _, t := range cfg.Targets {
		params.Targets[t.Branch] = t.Name
	}
	if cfg.Policy != nil {
		params.PolicyTeams = cfg.Policy.Teams
	}
	return params
}

func buildReviewSource(cfg *config.Daemon, app *ghauth.App, repo *gitx.Repo, stateDir ...string) (core.ReviewSource, error) {
	if cfg.GitHub.PullRequests != nil {
		p := githubReviewParams(cfg, app, repo)
		if len(stateDir) > 0 {
			p.IntentPath = filepath.Join(stateDir[0], "github-emergency-intents.json")
		}
		if app == nil && os.Getenv(cfg.GitHub.TokenEnv) == "" {
			return nil, fmt.Errorf("github review token is unset")
		}
		return review.NewGitHub(p), nil
	}
	if g := cfg.Gerrit; g != nil {
		p := review.GerritParams{APIURL: g.APIURL, Project: g.Project, Username: os.Getenv(g.UsernameEnv), Token: os.Getenv(g.TokenEnv), VerificationRequirement: g.VerificationRequirement, Git: repo, Targets: map[string]string{}}
		if p.Username == "" || p.Token == "" {
			return nil, fmt.Errorf("Gerrit credentials are unset")
		}
		for _, t := range cfg.Targets {
			p.Targets[t.Branch] = t.Name
		}
		return review.NewGerrit(p), nil
	}
	return nil, nil
}

// land-pr posts the same durable, permission-checked command as a human PR
// comment. It neither pushes master nor rewrites any contributor branch.
func runLandPR(args []string) error {
	f := flag.NewFlagSet("land-pr", flag.ContinueOnError)
	path := f.String("config", "", "daemon config with github pull-requests enabled")
	number := f.Int("pr", 0, "PR number; default requests only this PR")
	stack := f.Bool("stack", false, "request the stack through this PR")
	wholeStack := f.Bool("whole-stack", false, "request the entire stack, including successors")
	ready := f.Bool("ready", false, "request the ready prefix from the bottom")
	count := f.Int("prefix", 0, "request the bottom N unlanded PRs")
	cancel := f.Bool("cancel", false, "withdraw this PR's queue request")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *path == "" || *number <= 0 || len(f.Args()) != 0 {
		return fmt.Errorf("-config and positive -pr required")
	}
	choices := 0
	for _, v := range []bool{*stack, *wholeStack, *ready, *count > 0, *cancel} {
		if v {
			choices++
		}
	}
	if choices > 1 || *count < 0 {
		return fmt.Errorf("choose at most one of -stack, -whole-stack, -ready, -prefix, -cancel")
	}
	cfg, err := config.LoadDaemon(*path)
	if err != nil {
		return err
	}
	if cfg.GitHub.PullRequests == nil {
		return fmt.Errorf("github pull-requests is not configured")
	}
	app, err := buildAppTokens(cfg)
	if err != nil {
		return err
	}
	action := "merge"
	if *stack {
		action = "merge-through"
	}
	if *wholeStack {
		action = "merge-stack"
	}
	if *ready {
		action = "merge-ready"
	}
	if *count > 0 {
		action = "merge-prefix"
	}
	if *cancel {
		action = "cancel"
	}
	g := review.NewGitHub(githubReviewParams(cfg, app, nil))
	if err := g.Request(context.Background(), *number, action, *count); err != nil {
		return err
	}
	fmt.Printf("Requested %s for PR #%d; the daemon will check permissions and readiness.\n", action, *number)
	return nil
}

func buildGitHubWebhook(cfg *config.Daemon, source core.ReviewSource, ticks chan<- time.Time) (http.Handler, error) {
	p := cfg.GitHub.PullRequests
	if p == nil || p.WebhookSecretEnv == "" {
		return nil, nil
	}
	secret := os.Getenv(p.WebhookSecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("GitHub webhook secret environment variable is unset")
	}
	g, ok := source.(*review.GitHub)
	if !ok {
		return nil, fmt.Errorf("GitHub webhook requires GitHub review admission")
	}
	return g.Webhook(secret, func() {
		select {
		case ticks <- time.Now():
		default:
		}
	}), nil
}

// reviewTicks coalesces webhook hints with the normal recovery heartbeat.
func reviewTicks(ctx context.Context, periodic <-chan time.Time, hints <-chan time.Time) <-chan time.Time {
	ticks := make(chan time.Time)
	go func() {
		defer close(ticks)
		for {
			var at time.Time
			select {
			case <-ctx.Done():
				return
			case value, ok := <-hints:
				if !ok {
					hints = nil
					continue
				}
				at = value
			case value, ok := <-periodic:
				if !ok {
					return
				}
				at = value
			}
			select {
			case ticks <- at:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ticks
}
