package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/ghauth"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/review"
)

func githubReviewParams(cfg *config.Daemon, app *ghauth.App, repo *gitx.Repo) review.GitHubParams {
	p := cfg.GitHub.PullRequests
	params := review.GitHubParams{Repo: cfg.GitHub.Repo, APIURL: cfg.GitHub.APIURL, Git: repo, Targets: map[string]string{}, Bot: p.Bot, Approvals: *p.Approvals, RequiredChecks: p.RequiredChecks}
	if app != nil {
		params.Tokens = app
	} else {
		params.Tokens = review.StaticToken(os.Getenv(cfg.GitHub.TokenEnv))
	}
	for _, t := range cfg.Targets {
		params.Targets[t.Branch] = t.Name
	}
	return params
}

func buildReviewSource(cfg *config.Daemon, app *ghauth.App, repo *gitx.Repo) (core.ReviewSource, error) {
	if cfg.GitHub.PullRequests != nil {
		p := githubReviewParams(cfg, app, repo)
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
	number := f.Int("pr", 0, "PR number; merge includes its unlanded ancestors")
	stack := f.Bool("stack", false, "request the whole stack")
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
	for _, v := range []bool{*stack, *ready, *count > 0, *cancel} {
		if v {
			choices++
		}
	}
	if choices > 1 || *count < 0 {
		return fmt.Errorf("choose at most one of -stack, -ready, -prefix, -cancel")
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
