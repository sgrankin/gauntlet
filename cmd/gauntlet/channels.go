// Config -> package-local Params mapping for the phase-2/3 channels lives
// here, per docs/plans/phase23.md §9.5: history, ghstatus, and slack all
// take package-local params structs so those packages never import
// internal/config; cmd is the sole place that bridges the two.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/ghstatus"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/slack"
)

// buildHistoryStore opens the SQLite history store per cfg.History. A
// Path=="" section (disabled, config's documented convention) returns a nil
// store and no error — callers must treat a nil *history.Store as "history
// disabled" throughout (dashboard.New already does).
func buildHistoryStore(cfg *config.Daemon) (*history.Store, error) {
	if cfg.History.Path == "" {
		return nil, nil
	}
	store, err := history.Open(cfg.History.Path)
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", cfg.History.Path, err)
	}
	return store, nil
}

// buildGHStatusChannel constructs the GitHub commit-status channel per
// cfg.GitHub. A Repo=="" section (disabled) returns a nil channel and no
// error. Since the section was explicitly configured (Repo != ""), an empty
// token is a loud config error, not a silent no-op: the admin turned the
// feature on and gave it no way to authenticate.
func buildGHStatusChannel(cfg *config.Daemon) (*ghstatus.Channel, error) {
	if cfg.GitHub.Repo == "" {
		return nil, nil
	}
	owner, repo, ok := strings.Cut(cfg.GitHub.Repo, "/")
	if !ok {
		// config.Daemon.validate already rejects a repo without '/', but
		// queue.Config-style callers can build a Daemon by hand, so this
		// stays a real error rather than an unreachable panic.
		return nil, fmt.Errorf("github: repo must be \"owner/name\", got %q", cfg.GitHub.Repo)
	}
	token := os.Getenv(cfg.GitHub.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("github: %s is empty or unset, but github is configured for %s", cfg.GitHub.TokenEnv, cfg.GitHub.Repo)
	}
	return ghstatus.New(ghstatus.Params{
		Owner:        owner,
		Repo:         repo,
		Token:        token,
		APIURL:       cfg.GitHub.APIURL,
		DashboardURL: cfg.Dashboard.URL,
	}), nil
}

// buildSlackChannel constructs the Slack channel per cfg.Slack. A
// Channel=="" section (disabled) returns a nil channel and no error. Since
// the section was explicitly configured, either token missing is a loud
// config error, same rationale as buildGHStatusChannel.
func buildSlackChannel(cfg *config.Daemon) (*slack.Slack, error) {
	if cfg.Slack.Channel == "" {
		return nil, nil
	}
	appToken := os.Getenv(cfg.Slack.AppTokenEnv)
	if appToken == "" {
		return nil, fmt.Errorf("slack: %s is empty or unset, but slack is configured for channel %s", cfg.Slack.AppTokenEnv, cfg.Slack.Channel)
	}
	botToken := os.Getenv(cfg.Slack.BotTokenEnv)
	if botToken == "" {
		return nil, fmt.Errorf("slack: %s is empty or unset, but slack is configured for channel %s", cfg.Slack.BotTokenEnv, cfg.Slack.Channel)
	}
	return slack.New(slack.Params{
		Channel:  cfg.Slack.Channel,
		AppToken: appToken,
		BotToken: botToken,
	}), nil
}
