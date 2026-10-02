package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSigningConfig(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		invalid     bool
	}{
		{"absent", "", false},
		{"ssh", `signing { ssh-key "/etc/gauntlet/signing.pub"; }`, false},
		{"empty", `signing {}`, true},
		{"relative", `signing { ssh-key "key"; }`, true},
		{"timeout", `signing { ssh-key "/key"; timeout "-1s"; }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.kdl")
			data := `remote "https://github.com/acme/repo.git"
committer { name "Gauntlet"; email "bot@example.com"; }
target "main" branch="main"
` + tc.block
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadDaemon(path)
			if (err != nil) != tc.invalid {
				t.Fatalf("err=%v", err)
			}
			if err != nil {
				return
			}
			if tc.name == "absent" {
				if cfg.Signing != nil {
					t.Fatal("signing enabled by default")
				}
				return
			}
			if cfg.Signing.Timeout != 10*time.Second || !slices.Contains(cfg.SecretEnvNames(), "SSH_AUTH_SOCK") {
				t.Fatal("missing timeout or agent isolation")
			}
		})
	}
}
