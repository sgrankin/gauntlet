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
	var classifier flaky.Classifier
	if f.Auth == "chatgpt" {
		executable, err := exec.LookPath(f.Codex)
		if err != nil {
			return nil, fmt.Errorf("failure-review: Codex executable unavailable")
		}
		classifier = flaky.Codex{Model: f.Model, Token: token, Executable: executable, MaxOutputBytes: f.MaxOutputBytes}
	} else {
		classifier = flaky.OpenAI{Model: f.Model, Token: token, BaseURL: f.APIURL, MaxOutputBytes: f.MaxOutputBytes}
	}
	return &flaky.Retrier{Classifier: classifier, Model: f.Model, Checks: f.Checks, MaxRetries: f.MaxRetries, MinConfidence: f.MinConfidence, Timeout: f.Timeout}, nil
}
