package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/flaky"
)

func TestBuildFailureReview(t *testing.T) {
	if r, err := buildFailureReview(&config.Daemon{}); r != nil || err != nil {
		t.Fatal("review enabled by default")
	}
	f := &config.FailureReview{Auth: "api-key", Model: "chosen-model", TokenEnv: "GAUNTLET_TEST_MODEL_TOKEN", APIURL: "https://api.openai.com/v1", Checks: []string{"test"}, MaxRetries: 1, MinConfidence: .8, Timeout: time.Second, MaxOutputBytes: 256}
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.Codex = binary
	cfg := &config.Daemon{FailureReview: f}
	t.Setenv(f.TokenEnv, "")
	if _, err := buildFailureReview(cfg); err == nil {
		t.Fatal("missing token accepted")
	}
	t.Setenv(f.TokenEnv, "test-secret")
	r, err := buildFailureReview(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Classifier.(flaky.Codex); !ok {
		t.Fatal("wrong Codex backend")
	}
	f.Auth = "chatgpt"
	f.Codex = "/definitely/missing/codex"
	if _, err := buildFailureReview(cfg); err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatal("bad executable accepted or token leaked")
	}
}
