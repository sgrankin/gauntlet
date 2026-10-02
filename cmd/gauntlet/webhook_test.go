package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/queue"
	"github.com/sgrankin/gauntlet/internal/review"
)

func TestGitHubWebhookServerWakesReconciliation(t *testing.T) {
	cfg := &config.Daemon{Dashboard: config.Dashboard{Bind: reserveAddr(t)}, GitHub: config.GitHub{Repo: "acme/repo", PullRequests: &config.GitHubPullRequests{WebhookSecretEnv: "TEST_WEBHOOK_SECRET"}}}
	source := review.NewGitHub(review.GitHubParams{Repo: "acme/repo"})
	hints := make(chan time.Time, 1)
	t.Setenv("TEST_WEBHOOK_SECRET", "")
	if _, err := buildGitHubWebhook(cfg, source, hints); err == nil {
		t.Fatal("missing webhook secret accepted")
	}
	t.Setenv("TEST_WEBHOOK_SECRET", "secret")
	handler, err := buildGitHubWebhook(cfg, source, hints)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	startDashboard(ctx, cfg, func() *queue.Snapshot { return nil }, nil, nil, t.TempDir(), nil, nil, nil, deployWiring{}, nil, &wg, handler)
	base := "http://" + cfg.Dashboard.Bind
	waitForServer(t, base+"/deploys")
	body := `{"repository":{"full_name":"acme/repo"}}`
	request, err := http.NewRequest("POST", base+"/hooks/github", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-GitHub-Event", "issue_comment")
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal(response.StatusCode)
	}
	ticks := reviewTicks(ctx, make(chan time.Time), hints)
	select {
	case <-ticks:
	case <-time.After(time.Second):
		t.Fatal("webhook did not wake reconciliation")
	}
}
