package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/flaky"
)

func buildFailureReview(cfg *config.Daemon, stateDir ...string) (*flaky.Retrier, error) {
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
	var history *flaky.History
	if len(stateDir) > 0 {
		history, err = flaky.OpenHistory(filepath.Join(stateDir[0], "failure-review.db"))
		if err != nil {
			return nil, fmt.Errorf("failure-review history: %w", err)
		}
	}
	helper, err := os.Executable()
	if err != nil {
		return nil, err
	}
	classifier := flaky.Codex{Tools: f.Tools, ToolExecutable: helper, History: history, Auth: f.Auth, APIURL: f.APIURL, Model: f.Model, Token: token, Executable: executable, MaxOutputBytes: f.MaxOutputBytes}
	return &flaky.Retrier{History: history, MaxRunRetries: func() int {
		if history != nil {
			return f.MaxRunRetries
		}
		return 0
	}(), Classifier: classifier, Model: f.Model, Checks: f.Checks, MaxRetries: f.MaxRetries, MinConfidence: f.MinConfidence, Timeout: f.Timeout}, nil
}
