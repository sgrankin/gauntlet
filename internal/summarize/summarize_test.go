package summarize

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/llm"
)

type fakeGit struct {
	commits         []gitx.CommitInfo
	logErr, diffErr error
}

func (g fakeGit) Log(context.Context, string, string) ([]gitx.CommitInfo, error) {
	return g.commits, g.logErr
}
func (g fakeGit) DiffStat(context.Context, string, string) (string, error) {
	return "source.go | 3 ++-", g.diffErr
}

type runnerFunc func(context.Context, llm.Request) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, r llm.Request) ([]byte, error) { return f(ctx, r) }

func TestMergeBody(t *testing.T) {
	g := fakeGit{commits: []gitx.CommitInfo{{Subject: "fix timeout", Body: "Keep requests bounded."}}}
	s := New(Params{Git: g, Runner: runnerFunc(func(ctx context.Context, r llm.Request) ([]byte, error) {
		for _, want := range []string{"fix timeout", "Keep requests bounded.", "source.go", "Topic: fix", "Author: alice", "untrusted"} {
			if !strings.Contains(r.Prompt, want) {
				t.Errorf("prompt missing %q", want)
			}
		}
		if len(r.Config) != 0 {
			t.Fatal("summaries must not enable tools")
		}
		if !bytes.Contains(r.Schema, []byte(`"summary"`)) {
			t.Fatal("missing summary schema")
		}
		return []byte(`{"summary":" Bound requests to avoid hangs. "}`), nil
	})})
	if got := s.MergeBody(context.Background(), core.Candidate{Topic: "fix", User: "alice", SHA: "tip"}, "base"); got != "Bound requests to avoid hangs." {
		t.Fatal(got)
	}
}

func TestMergeBodyDegrades(t *testing.T) {
	for _, name := range []string{"no commits", "git log", "diffstat", "runner error", "invalid JSON", "empty summary", "timeout"} {
		t.Run(name, func(t *testing.T) {
			g := fakeGit{commits: []gitx.CommitInfo{{Subject: "fix"}}}
			switch name {
			case "no commits":
				g.commits = nil
			case "git log":
				g.logErr = errors.New("git unavailable")
			case "diffstat":
				g.diffErr = errors.New("diff unavailable")
			}
			var log bytes.Buffer
			s := New(Params{Git: g, Timeout: 10 * time.Millisecond, Log: &log, Runner: runnerFunc(func(ctx context.Context, r llm.Request) ([]byte, error) {
				switch name {
				case "no commits", "git log", "diffstat":
					t.Fatal("runner called without evidence")
				case "runner error":
					return nil, errors.New("unavailable")
				case "invalid JSON":
					return []byte("bad"), nil
				case "timeout":
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return []byte(`{"summary":""}`), nil
			})})
			if got := s.MergeBody(context.Background(), core.Candidate{SHA: "tip"}, "base"); got != "" {
				t.Fatal(got)
			}
			if log.Len() == 0 {
				t.Fatal("degradation was not logged")
			}
		})
	}
}
