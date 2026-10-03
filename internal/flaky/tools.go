package flaky

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

type toolContext struct {
	GitDir    string
	Revisions []string
	History   []Observation
	Check     string
}
type toolInput struct {
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path,omitempty"`
	Base     string `json:"base,omitempty"`
	Query    string `json:"query,omitempty"`
}
type toolOutput struct {
	Text string `json:"text"`
}
type readerTools struct {
	capability   toolContext
	mu           sync.Mutex
	calls, bytes int
}

func RunTools(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var cap toolContext
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &cap); err != nil {
		return err
	}
	reader := &readerTools{capability: cap}
	srv := sdk.NewServer(&sdk.Implementation{Name: "gauntlet-investigation", Version: "1"}, nil)
	for name, description := range map[string]string{
		"read_file": "Read a file blob from an allowed exact Git revision; paths do not access the host filesystem.",
		"paths":     "List tree paths at an allowed revision, filtered by an optional literal query.",
		"diff":      "Inspect a bounded diff between two allowed revisions. External diff and text conversion are disabled.",
		"history":   "Recent observed failures and actual retry outcomes for this check. Model conclusions are hypotheses.",
	} {
		sdk.AddTool(srv, &sdk.Tool{Name: name, Description: description}, func(ctx context.Context, _ *sdk.CallToolRequest, in toolInput) (*sdk.CallToolResult, toolOutput, error) {
			out, err := reader.call(ctx, name, in)
			return nil, toolOutput{out}, err
		})
	}
	return srv.Run(ctx, &sdk.StdioTransport{})
}

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (r *readerTools) allowed(sha string) bool {
	if !objectID.MatchString(sha) {
		return false
	}
	for _, rev := range r.capability.Revisions {
		if sha == rev {
			return true
		}
	}
	return false
}
func (r *readerTools) call(ctx context.Context, name string, in toolInput) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls >= 24 || r.bytes >= 256<<10 {
		return "", fmt.Errorf("investigation budget exhausted")
	}
	r.calls++
	var out []byte
	if name == "history" {
		out, _ = json.Marshal(r.capability.History)
	} else {
		if !r.allowed(in.Revision) {
			return "", fmt.Errorf("revision is outside this investigation")
		}
		args := []string{"--no-replace-objects", "--git-dir=" + r.capability.GitDir}
		switch name {
		case "read_file":
			if in.Path == "" || strings.ContainsAny(in.Path, "\x00\n\r") || strings.HasPrefix(in.Path, "/") || strings.Contains(in.Path, "..") {
				return "", fmt.Errorf("invalid tree path")
			}
			args = append(args, "cat-file", "blob", in.Revision+":"+in.Path)
		case "paths":
			args = append(args, "ls-tree", "-r", "--name-only", in.Revision)
		case "diff":
			if !r.allowed(in.Base) {
				return "", fmt.Errorf("base is outside this investigation")
			}
			args = append(args, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", in.Base, in.Revision, "--")
		default:
			return "", fmt.Errorf("unknown tool")
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", args...)
		// No credentials, inherited Git configuration, hooks, external helpers,
		// or candidate-controlled environment. git is an implementation detail,
		// never an agent-selected command.
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_LAZY_FETCH=1"}
		buffer := &limitedBuffer{limit: 32768}
		cmd.Stdout = buffer
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("Git object read failed")
		}
		out = buffer.buf.Bytes()
		if buffer.buf.Len() == buffer.limit {
			out = append(append([]byte{}, out...), []byte("\n[Output truncated at investigation byte limit]\n")...)
		}
		if name == "paths" && in.Query != "" {
			var filtered []string
			for _, path := range strings.Split(string(out), "\n") {
				if strings.Contains(path, in.Query) {
					filtered = append(filtered, path)
				}
			}
			out = []byte(strings.Join(filtered, "\n"))
		}
	}
	remaining := min(32768, (256<<10)-r.bytes)
	if len(out) > remaining {
		out = append(append([]byte{}, out[:max(0, remaining-64)]...), []byte("\n[Output truncated; absence is not evidence]\n")...)
	}
	r.bytes += len(out)
	return strings.ToValidUTF8(string(out), "�"), nil
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
