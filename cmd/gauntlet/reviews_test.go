package main

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
)

func TestGitHubReviewLinksRequireDashboardAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name, bind, history, want string
	}{
		{"enabled", "127.0.0.1:8080", "history.db", "https://queue.example"},
		{"no dashboard", "", "history.db", ""},
		{"no history", "127.0.0.1:8080", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approvals := 0
			cfg := &config.Daemon{
				GitHub:    config.GitHub{Auth: &config.GitHubAuth{AppID: 42}, PullRequests: &config.GitHubPullRequests{Bot: "queue", Approvals: &approvals}},
				Dashboard: config.Dashboard{Bind: tc.bind, URL: "https://queue.example"},
				History:   config.History{Path: tc.history},
			}
			params := githubReviewParams(cfg, nil, nil)
			if params.DashboardURL != tc.want || params.AppID != 42 || params.Bot != "queue" {
				t.Fatalf("incorrect review wiring: %+v", params)
			}
		})
	}
}
