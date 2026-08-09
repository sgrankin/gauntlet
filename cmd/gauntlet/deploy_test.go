package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/deploy"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

// The seam buildDeployTracker's signature depends on: the daemon hands its
// own *gitx.Repo straight to the tracker, with no adapter.
var _ deploy.Git = (*gitx.Repo)(nil)

func loadDeployConfig(t *testing.T, body string) *config.Daemon {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gauntlet.kdl")
	data := `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
` + body
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	return cfg
}

// TestBuildDeployTracker_NilWithoutBlock: no deploy block means no
// tracker at all, so main never starts a tick goroutine — the same
// nil-means-not-configured contract as buildHooksRunner.
func TestBuildDeployTracker_NilWithoutBlock(t *testing.T) {
	cfg := loadDeployConfig(t, "")
	if tr := buildDeployTracker(cfg, nil, io.Discard); tr != nil {
		t.Fatalf("buildDeployTracker = %v with no deploy block, want nil", tr)
	}
	// A written-but-empty block is the same disabled state.
	cfg = loadDeployConfig(t, "deploy {\n}\n")
	if tr := buildDeployTracker(cfg, nil, io.Discard); tr != nil {
		t.Fatalf("buildDeployTracker = %v for an empty deploy block, want nil", tr)
	}
}

// TestBuildDeployTracker_MapsTheDesignExample walks the design doc's
// three-environment config end to end: LoadDaemon, buildDeployTracker,
// then the tracker's own published lanes — which is the only place the
// mapping is observable from outside the package.
func TestBuildDeployTracker_MapsTheDesignExample(t *testing.T) {
	cfg := loadDeployConfig(t, `
deploy {
    environment "dev" {
        source "main"
        track
    }
    environment "prod" {
        source env="dev"
        track
        max-parallel 4
        on-desired-move "cancel"
    }
    environment "prod2" {
        source env="dev"
        nodes "migrate" "app1"
    }
}
`)
	var log strings.Builder
	tr := buildDeployTracker(cfg, &stubDeployGit{}, &log)
	if tr == nil {
		t.Fatal("buildDeployTracker = nil with three environments configured")
	}
	if err := tr.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	lanes := tr.Snapshot().Lanes
	if len(lanes) != 3 {
		t.Fatalf("Lanes = %+v, want 3 in declaration order", lanes)
	}

	want := []struct {
		env    string
		mode   deploy.Mode
		source string
	}{
		{"dev", deploy.ModeTrack, "main"},
		{"prod", deploy.ModeTrack, "env=dev"},
		// No `track` node: manual, the mode an operator pins an
		// environment to during an incident.
		{"prod2", deploy.ModeManual, "env=dev"},
	}
	for i, w := range want {
		got := lanes[i]
		if got.Env != w.env || got.Mode != w.mode || got.Source != w.source {
			t.Errorf("Lanes[%d] = {Env:%q Mode:%q Source:%q}, want {%q %q %q}",
				i, got.Env, got.Mode, got.Source, w.env, w.mode, w.source)
		}
	}

	// The fields D2 consumes are carried through with their resolved
	// defaults, not dropped on the floor.
	envs := cfg.Deploy.Environments
	if envs[0].MaxParallel != 1 || envs[0].OnDesiredMove != "finish" {
		t.Errorf("dev resolved to max-parallel %d / on-desired-move %q, want 1 / finish",
			envs[0].MaxParallel, envs[0].OnDesiredMove)
	}
	if envs[1].MaxParallel != 4 || envs[1].OnDesiredMove != "cancel" {
		t.Errorf("prod resolved to max-parallel %d / on-desired-move %q, want 4 / cancel",
			envs[1].MaxParallel, envs[1].OnDesiredMove)
	}
	if strings.Join(envs[2].Nodes, ",") != "migrate,app1" {
		t.Errorf("prod2 nodes = %v, want [migrate app1]", envs[2].Nodes)
	}
}

// TestBuildDeployTracker_ManualEnvironmentIsNeverWritten: the whole
// tracker, wired the way main wires it, must not touch a manual lane's
// desired ref.
func TestBuildDeployTracker_ManualEnvironmentIsNeverWritten(t *testing.T) {
	cfg := loadDeployConfig(t, `
deploy {
    environment "prod" {
        source "main"
    }
}
`)
	git := &stubDeployGit{refs: map[string]string{"refs/heads/main": "aaa"}}
	tr := buildDeployTracker(cfg, git, io.Discard)
	if err := tr.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(git.pushed) != 0 {
		t.Fatalf("CAS pushes = %v, want none for a manual environment", git.pushed)
	}
}

// stubDeployGit is the smallest deploy.Git that lets the wiring run: a
// fixed ref picture and a record of every push attempted.
type stubDeployGit struct {
	refs   map[string]string
	pushed []string
}

func (g *stubDeployGit) ListRefs(context.Context) (map[string]string, error) { return g.refs, nil }

func (g *stubDeployGit) ListLocalRefs(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (g *stubDeployGit) CASUpdate(_ context.Context, remoteRef, _, newOID string) error {
	g.pushed = append(g.pushed, remoteRef+"="+newOID)
	return nil
}
