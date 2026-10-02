package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFailureReviewConfig(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		wantErr     bool
	}{
		{"absent", "", false},
		{"api", `failure-review { model "chosen-model"; checks "integration"; }`, false},
		{"service account", `failure-review { auth "chatgpt"; model "chosen-model"; checks "integration"; token-env "GAUNTLET_CODEX_TOKEN"; }`, false},
		{"unlimited retry", `failure-review { model "model"; checks "test"; max-retries 4; }`, true},
		{"no allowlist", `failure-review { model "model"; }`, true},
		{"no model", `failure-review { checks "test"; }`, true},
		{"bad auth", `failure-review { auth "session-cookie"; model "model"; checks "test"; }`, true},
		{"bad confidence", `failure-review { model "model"; checks "test"; min-confidence 1.1; }`, true},
		{"side effect", `failure-review { model "model"; checks "receipt:publish"; }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.kdl")
			data := `remote "https://github.com/acme/repo.git"
committer { name "Gauntlet"; email "gauntlet@example.com"; }
target "main" branch="main"
` + tc.block
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadDaemon(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("config err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if tc.name == "absent" {
				if cfg.FailureReview != nil {
					t.Fatal("enabled by default")
				}
				return
			}
			f := cfg.FailureReview
			if f.MaxRetries != 1 || f.MinConfidence != .8 || f.Timeout.String() != "30s" {
				t.Fatalf("defaults=%+v", f)
			}
			if !slices.Contains(cfg.SecretEnvNames(), f.TokenEnv) {
				t.Fatal("credential exposed to candidate commands")
			}
			if strings.Contains(tc.name, "service") && f.Auth != "chatgpt" {
				t.Fatal("wrong authentication")
			}
		})
	}
}
