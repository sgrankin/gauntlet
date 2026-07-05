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
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/ghstatus"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
	"github.com/sgrankin/gauntlet/internal/slack"
	"github.com/sgrankin/gauntlet/internal/summarize"
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

// buildSeedParks returns the queue.Config.SeedParks closure (Feature 2,
// "park persistence across restarts"): for a target, it asks store's
// LatestTerminalPerRef for every candidate ref's most recent verdict and
// maps each back to a queue.ParkSeed, string outcome parsed back to
// core.Outcome via parseOutcome. queue.Daemon itself is what actually
// filters to red-family outcomes (Rejected/Conflict/Error) before seeding
// anything — this closure hands over every verdict, landed included, and
// lets that filtering live in one place (internal/queue/reconcile.go's
// seedParksOnce).
//
// A query failure degrades to "no seeds" (logged, not fatal) rather than
// failing daemon startup: this is efficiency state, never correctness state
// (DESIGN.md's decision ledger) — worst case, a broken history db just costs
// some avoidable re-tests on this restart, exactly as if history had never
// been configured at all.
func buildSeedParks(store *history.Store) func(target string) []queue.ParkSeed {
	return func(target string) []queue.ParkSeed {
		verdicts, err := store.LatestTerminalPerRef(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gauntlet: history: seed parks %s: %v\n", target, err)
			return nil
		}
		out := make([]queue.ParkSeed, 0, len(verdicts))
		for _, v := range verdicts {
			outcome, ok := parseOutcome(v.Outcome)
			if !ok {
				continue // an outcome string history never actually writes; defensive only
			}
			out = append(out, queue.ParkSeed{
				Ref: v.Ref, SHA: v.SHA, Outcome: outcome, Reason: v.Detail, At: v.EndedAt,
			})
		}
		return out
	}
}

// parseOutcome maps history's stored outcome string (history.outcomeString's
// vocabulary; unexported there, since internal/history never imports
// internal/core) back to a core.Outcome. ok is false for anything else —
// defensive only, since history never writes any other value.
func parseOutcome(s string) (core.Outcome, bool) {
	switch s {
	case "landed":
		return core.OutcomeLanded, true
	case "rejected":
		return core.OutcomeRejected, true
	case "conflict":
		return core.OutcomeConflict, true
	case "skipped":
		return core.OutcomeSkipped, true
	case "error":
		return core.OutcomeError, true
	default:
		return 0, false
	}
}

// buildSummarizer constructs the optional Claude-written merge-commit-body
// enricher per cfg.Summarize. A nil section (disabled) returns a nil
// *summarize.Summarizer and no error. Since the section was explicitly
// configured, an empty API key is a loud config error, same rationale as
// buildGHStatusChannel/buildSlackChannel: the admin turned the feature on
// and gave it no way to authenticate.
//
// git is the minimal summarize.Git surface (Log/DiffStat); cmd passes its
// already-constructed *gitx.Repo, which satisfies it structurally.
func buildSummarizer(cfg *config.Daemon, git summarize.Git) (*summarize.Summarizer, error) {
	if cfg.Summarize == nil {
		return nil, nil
	}
	key := os.Getenv(cfg.Summarize.APIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("summarize: %s is empty or unset, but summarize is configured with model %s", cfg.Summarize.APIKeyEnv, cfg.Summarize.Model)
	}
	effort := cfg.Summarize.Effort
	if effort == "none" {
		effort = "" // omit output_config entirely (validated sentinel)
	}
	return summarize.New(summarize.Params{
		Git:     git,
		Model:   cfg.Summarize.Model,
		Effort:  effort,
		APIKey:  key,
		Timeout: cfg.Summarize.Timeout,
		Log:     os.Stderr,
	}), nil
}
