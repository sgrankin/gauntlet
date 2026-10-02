package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Signing enables daemon-owned SSH signatures on generated landing commits.
// SSHKey may name a private key or an agent-backed public key file.
type Signing struct {
	SSHKey  string        `kdl:"ssh-key"`
	Timeout time.Duration `kdl:"timeout,format:units"`
}

func (s *Signing) validate() error {
	if !filepath.IsAbs(s.SSHKey) || strings.ContainsAny(s.SSHKey, "\x00\r\n") {
		return fmt.Errorf("signing: ssh-key must be an absolute key-file path")
	}
	if s.Timeout <= 0 || s.Timeout > time.Minute {
		return fmt.Errorf("signing: timeout must be >0 and <=1m")
	}
	return nil
}
