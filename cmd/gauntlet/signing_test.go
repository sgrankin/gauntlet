package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
)

func TestGitSigningOptions(t *testing.T) {
	if opts, err := gitSigningOptions(&config.Daemon{}); err != nil || len(opts) != 0 {
		t.Fatalf("signing should default off: %v %v", opts, err)
	}
	key := filepath.Join(t.TempDir(), "key.pub")
	cfg := &config.Daemon{Signing: &config.Signing{SSHKey: key, Timeout: time.Second}}
	if _, err := gitSigningOptions(cfg); err == nil {
		t.Fatal("missing key accepted at startup")
	}
	if err := os.WriteFile(key, []byte("public key contents checked by signer"), 0600); err != nil {
		t.Fatal(err)
	}
	if opts, err := gitSigningOptions(cfg); err != nil || len(opts) != 1 {
		t.Fatalf("readable key rejected: %v %v", opts, err)
	}
}
