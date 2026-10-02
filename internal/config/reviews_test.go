package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReviewConfiguration(t *testing.T) {
	base := `remote "https://github.com/acme/widgets.git"
committer { name "Gauntlet"; email "bot@example.com"; }
target "main" { branch "main"; }
`
	for _, tc := range []struct {
		name, extra string
		invalid     bool
	}{
		{"github", `github "acme/widgets" { pull-requests { bot "lander"; approvals 2; require-check "lint" "build"; }; }`, false},
		{"gerrit", `gerrit "https://review.example.com" { project "widgets"; username-env "BOT_USER"; token-env "BOT_TOKEN"; }`, false},
		{"both", `github "acme/widgets" { pull-requests {}; }
gerrit "https://review.example.com" { project "widgets"; }`, true},
		{"approvals", `github "acme/widgets" { pull-requests { approvals -1; }; }`, true},
		{"gerrit-project", `gerrit "https://review.example.com"`, true},
		{"bot", `github "acme/widgets" { pull-requests { bot "@invalid"; }; }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.kdl")
			if err := os.WriteFile(path, []byte(base+tc.extra), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadDaemon(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid review policy accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Targets[0].Landing != "squash" {
				t.Fatal("linear landing not default")
			}
			if tc.name == "github" && (cfg.GitHub.PullRequests.Bot != "lander" || *cfg.GitHub.PullRequests.Approvals != 2 || !slices.Equal(cfg.GitHub.PullRequests.RequiredChecks, []string{"lint", "build"})) {
				t.Fatalf("bad PR config: %+v", cfg.GitHub.PullRequests)
			}
			if tc.name == "gerrit" {
				names := cfg.SecretEnvNames()
				if !slices.Contains(names, "BOT_USER") || !slices.Contains(names, "BOT_TOKEN") {
					t.Fatal("Gerrit credentials not filtered")
				}
			}
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(base, `branch "main";`, `branch "main"; landing "merge";`)+tc.extra), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadDaemon(path); err == nil {
				t.Fatal("review admission accepted merge landing")
			}
		})
	}
}
