package main

import (
	"context"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

// fakeSummarizeGit is a no-op summarize.Git, just enough for buildSummarizer
// to type-check and construct a *summarize.Summarizer without touching real
// git — this file only tests cmd's Config -> Params wiring, not the
// summarizer's own behavior (covered by internal/summarize's own tests).
type fakeSummarizeGit struct{}

func (fakeSummarizeGit) Log(ctx context.Context, base, tip string) ([]gitx.CommitInfo, error) {
	return nil, nil
}

func (fakeSummarizeGit) DiffStat(ctx context.Context, base, tip string) (string, error) {
	return "", nil
}

func TestBuildSummarizer_DisabledWhenSectionAbsent(t *testing.T) {
	cfg := &config.Daemon{}
	s, err := buildSummarizer(cfg, fakeSummarizeGit{})
	if err != nil {
		t.Fatalf("buildSummarizer: %v", err)
	}
	if s != nil {
		t.Fatalf("buildSummarizer = %v, want nil when Summarize is unset", s)
	}
}

func TestBuildSummarizer_MissingAPIKeyIsLoudError(t *testing.T) {
	t.Setenv("GAUNTLET_TEST_SUMMARIZE_KEY", "")
	cfg := &config.Daemon{
		Summarize: &config.Summarize{
			Model:     "claude-haiku-4-5",
			APIKeyEnv: "GAUNTLET_TEST_SUMMARIZE_KEY",
		},
	}
	_, err := buildSummarizer(cfg, fakeSummarizeGit{})
	if err == nil {
		t.Fatal("buildSummarizer: want an error when the API key env var is unset")
	}
	if !strings.Contains(err.Error(), "GAUNTLET_TEST_SUMMARIZE_KEY") {
		t.Errorf("error = %q, want it to name the missing env var", err.Error())
	}
}

func TestBuildSummarizer_ConstructsWhenConfigured(t *testing.T) {
	t.Setenv("GAUNTLET_TEST_SUMMARIZE_KEY", "sk-test-key")
	cfg := &config.Daemon{
		Summarize: &config.Summarize{
			Model:     "claude-haiku-4-5",
			APIKeyEnv: "GAUNTLET_TEST_SUMMARIZE_KEY",
		},
	}
	s, err := buildSummarizer(cfg, fakeSummarizeGit{})
	if err != nil {
		t.Fatalf("buildSummarizer: %v", err)
	}
	if s == nil {
		t.Fatal("buildSummarizer = nil, want a constructed Summarizer")
	}
}
