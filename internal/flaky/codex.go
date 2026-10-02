package flaky

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Codex authenticates with a ChatGPT service-account access token. Each call
// has an empty workspace and home, without the daemon's credentials or config.
type Codex struct {
	Model, Token, Executable string
	MaxOutputBytes           int
}

func (c Codex) Classify(ctx context.Context, job core.CheckJob, res core.CheckResult) (Decision, error) {
	dir, err := os.MkdirTemp("", "gauntlet-failure-review-")
	if err != nil {
		return Decision{}, fmt.Errorf("create classifier workspace")
	}
	defer os.RemoveAll(dir)
	schemaPath := filepath.Join(dir, "schema.json")
	outputPath := filepath.Join(dir, "decision.json")
	if err := os.WriteFile(schemaPath, schema, 0600); err != nil {
		return Decision{}, fmt.Errorf("write decision schema")
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		return Decision{}, fmt.Errorf("create classifier home")
	}
	cmd := exec.CommandContext(ctx, c.Executable, "exec",
		"--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check",
		"--sandbox", "read-only", "--config", "features.shell_tool=false",
		"--config", "web_search=\"disabled\"", "--model", c.Model,
		"--output-schema", schemaPath, "--output-last-message", outputPath, "-",
	)
	cmd.Dir = dir
	// Keep only runtime/network trust settings. Candidate-controlled output
	// must never gain an agent with the daemon's other operator credentials.
	for _, name := range []string{"PATH", "SYSTEMROOT", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "CODEX_HOME="+home, "CODEX_ACCESS_TOKEN="+c.Token)
	cmd.Stdin = strings.NewReader(instructions + "\n\nFailure JSON:\n" + evidence(job, res, c.MaxOutputBytes))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return Decision{}, fmt.Errorf("Codex classification failed")
	}
	f, err := os.Open(outputPath)
	if err != nil {
		return Decision{}, fmt.Errorf("Codex returned no decision")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(data) > 8192 {
		return Decision{}, fmt.Errorf("invalid Codex decision")
	}
	return decodeDecision(data)
}
