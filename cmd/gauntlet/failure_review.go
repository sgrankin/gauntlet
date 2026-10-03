package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/flaky"
)

func buildFailureReview(cfg *config.Daemon) (*flaky.Retrier, error) {
	f := cfg.FailureReview
	if f == nil {
		return nil, nil
	}
	token := os.Getenv(f.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("failure-review: %s is empty or unset", f.TokenEnv)
	}
	executable, err := exec.LookPath(f.Codex)
	if err != nil {
		return nil, fmt.Errorf("failure-review: Codex executable unavailable")
	}
	classifier := flaky.Codex{Auth: f.Auth, APIURL: f.APIURL, Model: f.Model, Token: token, Executable: executable, MaxOutputBytes: f.MaxOutputBytes}
	return &flaky.Retrier{Classifier: classifier, Model: f.Model, Checks: f.Checks, MaxRetries: f.MaxRetries, MinConfidence: f.MinConfidence, Timeout: f.Timeout}, nil
}
