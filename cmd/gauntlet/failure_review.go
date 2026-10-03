package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/flaky"
	"github.com/sgrankin/gauntlet/internal/llm"
)

func buildFailureReview(cfg *config.Daemon, stateDir string) (*flaky.Retrier, error) {
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
	var helper string
	if f.Tools {
		helper, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	// Doctor checks credentials/executable without creating daemon state.
	var history *flaky.History
	if stateDir != "" {
		history, err = flaky.OpenHistory(filepath.Join(stateDir, "failure-review.db"))
		if err != nil {
			return nil, fmt.Errorf("failure-review history: %w", err)
		}
	}
	classifier := flaky.Codex{Tools: f.Tools, ToolExecutable: helper, History: history, Client: llm.Codex{Auth: f.Auth, APIURL: f.APIURL, Model: f.Model, Token: token, Executable: executable}, MaxOutputBytes: f.MaxOutputBytes}
	return &flaky.Retrier{History: history, MaxRunRetries: f.MaxRunRetries, Classifier: classifier, Model: f.Model, Checks: f.Checks, MaxRetries: f.MaxRetries, MinConfidence: f.MinConfidence, Timeout: f.Timeout}, nil
}
