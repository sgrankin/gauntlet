package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

func gitSigningOptions(cfg *config.Daemon) ([]gitx.Option, error) {
	if cfg.Signing == nil {
		return nil, nil
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return nil, fmt.Errorf("signing: ssh-keygen is required")
	}
	key, err := os.Open(cfg.Signing.SSHKey)
	if err != nil {
		return nil, fmt.Errorf("signing: key file: %w", err)
	}
	defer key.Close()
	stat, err := key.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("signing: key must be a readable regular file")
	}
	return []gitx.Option{gitx.WithSSHSigning(cfg.Signing.SSHKey, cfg.Signing.Timeout)}, nil
}
