// Package llm runs isolated Codex processes for model-backed features.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Codex struct {
	Model, Token, Executable string
	Auth, APIURL, Effort     string
}

type Request struct {
	Prompt    string
	Schema    []byte
	Config    []string
	TracePath string
}

func (c Codex) Run(ctx context.Context, r Request) ([]byte, error) {
	dir, err := os.MkdirTemp("", "gauntlet-codex-")
	if err != nil {
		return nil, fmt.Errorf("create Codex workspace")
	}
	defer os.RemoveAll(dir)
	schemaPath := filepath.Join(dir, "schema.json")
	outputPath := filepath.Join(dir, "response.json")
	if err := os.WriteFile(schemaPath, r.Schema, 0600); err != nil {
		return nil, fmt.Errorf("write response schema")
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		return nil, fmt.Errorf("create Codex home")
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
	if c.Auth == "api-key" {
		// Codex reads API credentials from its isolated home; the token never
		// appears in argv or the candidate's workspace/environment.
		cmd.Env = cmd.Env[:len(cmd.Env)-1]
		auth, _ := json.Marshal(map[string]string{"OPENAI_API_KEY": c.Token})
		if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0600); err != nil {
			return nil, fmt.Errorf("write isolated Codex authentication")
		}
		if c.APIURL != "" && c.APIURL != "https://api.openai.com/v1" {
			cmd.Args = append(cmd.Args, "--config", "model_provider=\"gauntlet\"", "--config", "model_providers.gauntlet={name=\"Gauntlet\",base_url="+strconv.Quote(c.APIURL)+",wire_api=\"responses\",requires_openai_auth=true}")
		}
	}
	cmd.Stdin = strings.NewReader(r.Prompt)
	for _, setting := range r.Config {
		cmd.Args = append(cmd.Args, "--config", setting)
	}
	if c.Effort != "" && c.Effort != "none" {
		cmd.Args = append(cmd.Args, "--config", "model_reasoning_effort="+strconv.Quote(c.Effort))
	}
	trace := &limitedBuffer{limit: 1 << 20}
	cmd.Args = append(cmd.Args, "--json")
	cmd.Stdout = trace
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("Codex execution failed")
	}
	if r.TracePath != "" {
		// Evidence stays alongside the run logs and follows their retention.
		_ = os.WriteFile(r.TracePath, trace.buf.Bytes(), 0600)
	}
	f, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("Codex returned no response")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(data) > 8192 {
		return nil, fmt.Errorf("invalid Codex response")
	}
	return data, nil
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.buf.Len() < b.limit {
		b.buf.Write(p[:min(len(p), b.limit-b.buf.Len())])
	}
	return n, nil
}
