package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadDeployDaemon writes body as a complete daemon config (the minimal
// required preamble plus body) and loads it, returning LoadDaemon's result
// verbatim so both the accept and reject tables can use it.
func loadDeployDaemon(t *testing.T, body string) (*Daemon, error) {
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
	return LoadDaemon(path)
}

// TestLoadDaemon_DeployExample is the design doc's own three-environment
// example (docs/architecture/deployment.md), loaded verbatim: it is the config
// grammar's acceptance criterion, so a change that silently stops parsing
// it fails here.
func TestLoadDaemon_DeployExample(t *testing.T) {
	d, err := loadDeployDaemon(t, `
deploy {
    environment "dev" {
        source "main"
        track
    }
    environment "prod" {
        source env="dev"
        track
        max-parallel 4
        on-desired-move "finish"
    }
    environment "prod2" {
        source env="dev"
        track
        nodes "migrate" "app1"
    }
}
`)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	envs := d.Deploy.Environments
	if len(envs) != 3 {
		t.Fatalf("Environments = %+v, want 3", envs)
	}

	dev := envs[0]
	if dev.Name != "dev" {
		t.Errorf("envs[0].Name = %q, want %q", dev.Name, "dev")
	}
	if dev.Source.Branch != "main" || dev.Source.Env != "" {
		t.Errorf("dev.Source = %+v, want branch main", dev.Source)
	}
	if dev.Track == nil {
		t.Error("dev.Track = nil, want present (a bare `track` node)")
	}
	// Defaults reach an environment that wrote neither knob.
	if dev.MaxParallel != defaultDeployMaxParallel {
		t.Errorf("dev.MaxParallel = %d, want default %d", dev.MaxParallel, defaultDeployMaxParallel)
	}
	if dev.OnDesiredMove != defaultOnDesiredMove {
		t.Errorf("dev.OnDesiredMove = %q, want default %q", dev.OnDesiredMove, defaultOnDesiredMove)
	}

	prod := envs[1]
	if prod.Source.Env != "dev" || prod.Source.Branch != "" {
		t.Errorf("prod.Source = %+v, want env=dev", prod.Source)
	}
	if prod.MaxParallel != 4 {
		t.Errorf("prod.MaxParallel = %d, want 4", prod.MaxParallel)
	}
	if prod.OnDesiredMove != "finish" {
		t.Errorf("prod.OnDesiredMove = %q, want finish", prod.OnDesiredMove)
	}

	prod2 := envs[2]
	if got := strings.Join(prod2.Nodes, ","); got != "migrate,app1" {
		t.Errorf("prod2.Nodes = %v, want [migrate app1]", prod2.Nodes)
	}
	if prod2.Source.Env != "dev" {
		t.Errorf("prod2.Source = %+v, want env=dev", prod2.Source)
	}
}

// TestLoadDaemon_DeployAbsent pins the disabled state: no block at all
// leaves zero environments AND no defaults anywhere — the same "only
// default within an enabled section" rule as services/github.
func TestLoadDaemon_DeployAbsent(t *testing.T) {
	d, err := loadDeployDaemon(t, "")
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if len(d.Deploy.Environments) != 0 {
		t.Fatalf("Environments = %+v, want none (disabled)", d.Deploy.Environments)
	}
}

// TestLoadDaemon_DeployEmptyBlock: a written-but-empty `deploy {}` is the
// same disabled state as no block at all — presence of environments, not
// presence of the node, is the enable signal.
func TestLoadDaemon_DeployEmptyBlock(t *testing.T) {
	d, err := loadDeployDaemon(t, "deploy {\n}\n")
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if len(d.Deploy.Environments) != 0 {
		t.Fatalf("Environments = %+v, want none (disabled)", d.Deploy.Environments)
	}
}

func TestLoadDaemon_DeployAccepts(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		check func(t *testing.T, e Environment)
	}{
		{
			name: "manual env omits track",
			body: `
deploy {
    environment "prod" {
        source "main"
    }
}
`,
			check: func(t *testing.T, e Environment) {
				if e.Track != nil {
					t.Errorf("Track = %+v, want nil (manual)", e.Track)
				}
			},
		},
		{
			name: "bare track",
			body: `
deploy {
    environment "dev" {
        source "main"
        track
    }
}
`,
			check: func(t *testing.T, e Environment) {
				if e.Track == nil {
					t.Error("Track = nil, want present")
				}
			},
		},
		{
			name: "max-parallel 4",
			body: `
deploy {
    environment "dev" {
        source "main"
        max-parallel 4
    }
}
`,
			check: func(t *testing.T, e Environment) {
				if e.MaxParallel != 4 {
					t.Errorf("MaxParallel = %d, want 4", e.MaxParallel)
				}
			},
		},
		{
			name: "on-desired-move cancel",
			body: `
deploy {
    environment "dev" {
        source "main"
        on-desired-move "cancel"
    }
}
`,
			check: func(t *testing.T, e Environment) {
				if e.OnDesiredMove != "cancel" {
					t.Errorf("OnDesiredMove = %q, want cancel", e.OnDesiredMove)
				}
			},
		},
		{
			name: "nodes subgraph",
			body: `
deploy {
    environment "dev" {
        source "main"
        nodes "migrate" "app1"
    }
}
`,
			check: func(t *testing.T, e Environment) {
				if len(e.Nodes) != 2 || e.Nodes[0] != "migrate" || e.Nodes[1] != "app1" {
					t.Errorf("Nodes = %v, want [migrate app1]", e.Nodes)
				}
			},
		},
		{
			name: "branch source with slashes",
			body: `
deploy {
    environment "dev" {
        source "release/v2"
    }
}
`,
			check: func(t *testing.T, e Environment) {
				if e.Source.Branch != "release/v2" {
					t.Errorf("Source.Branch = %q, want release/v2", e.Source.Branch)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := loadDeployDaemon(t, tt.body)
			if err != nil {
				t.Fatalf("LoadDaemon: %v", err)
			}
			if len(d.Deploy.Environments) != 1 {
				t.Fatalf("Environments = %+v, want exactly 1", d.Deploy.Environments)
			}
			tt.check(t, d.Deploy.Environments[0])
		})
	}
}

func TestLoadDaemon_DeployRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring of the error
	}{
		{
			name: "missing source",
			body: `
deploy {
    environment "dev" {
        track
    }
}
`,
			want: `environment "dev": source is required`,
		},
		{
			name: "both source forms",
			body: `
deploy {
    environment "dev" {
        source "main" env="other"
    }
    environment "other" {
        source "main"
    }
}
`,
			want: "mutually exclusive",
		},
		{
			name: "source is a full ref",
			body: `
deploy {
    environment "dev" {
        source "refs/heads/main"
    }
}
`,
			want: "must be a branch name, not a ref",
		},
		{
			name: "unknown source env",
			body: `
deploy {
    environment "dev" {
        source env="nope"
    }
}
`,
			want: `source env "nope": no such environment declared`,
		},
		{
			name: "self cycle",
			body: `
deploy {
    environment "dev" {
        source env="dev"
    }
}
`,
			want: "dependency cycle",
		},
		{
			name: "two cycle",
			body: `
deploy {
    environment "a" {
        source env="b"
    }
    environment "b" {
        source env="a"
    }
}
`,
			want: "dependency cycle",
		},
		{
			name: "three cycle",
			body: `
deploy {
    environment "a" {
        source env="b"
    }
    environment "b" {
        source env="c"
    }
    environment "c" {
        source env="a"
    }
}
`,
			want: "dependency cycle",
		},
		{
			name: "duplicate env name",
			body: `
deploy {
    environment "dev" {
        source "main"
    }
    environment "dev" {
        source "other"
    }
}
`,
			want: `environment "dev": duplicate`,
		},
		{
			name: "empty env name",
			body: `
deploy {
    environment "" {
        source "main"
    }
}
`,
			want: "name must not be empty",
		},
		{
			name: "env name with slash",
			body: `
deploy {
    environment "a/b" {
        source "main"
    }
}
`,
			want: "must not contain '/'",
		},
		{
			name: "env name with dotdot",
			body: `
deploy {
    environment "a..b" {
        source "main"
    }
}
`,
			want: "not a valid ref name",
		},
		{
			name: "env name with glob",
			body: `
deploy {
    environment "a*b" {
        source "main"
    }
}
`,
			want: "not a valid ref name",
		},
		{
			name: "env name with colon",
			body: `
deploy {
    environment "a:b" {
        source "main"
    }
}
`,
			want: "not a valid ref name",
		},
		{
			name: "env name with whitespace",
			body: `
deploy {
    environment "a b" {
        source "main"
    }
}
`,
			want: "not a valid ref name",
		},
		{
			name: "unknown on-desired-move",
			body: `
deploy {
    environment "dev" {
        source "main"
        on-desired-move "later"
    }
}
`,
			want: `on-desired-move must be "finish" or "cancel"`,
		},
		{
			name: "negative max-parallel",
			body: `
deploy {
    environment "dev" {
        source "main"
        max-parallel -1
    }
}
`,
			want: "max-parallel must be between 1 and 64",
		},
		{
			name: "max-parallel over the cap",
			body: `
deploy {
    environment "dev" {
        source "main"
        max-parallel 65
    }
}
`,
			want: "max-parallel must be between 1 and 64",
		},
		{
			name: "duplicate nodes entry",
			body: `
deploy {
    environment "dev" {
        source "main"
        nodes "migrate" "migrate"
    }
}
`,
			want: `nodes "migrate": duplicate`,
		},
		{
			name: "empty nodes entry",
			body: `
deploy {
    environment "dev" {
        source "main"
        nodes "migrate" ""
    }
}
`,
			want: "nodes: name must not be empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadDeployDaemon(t, tt.body)
			if err == nil {
				t.Fatalf("LoadDaemon succeeded, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestLoadDaemon_DeployTrackTakesNoArgument pins the spelling the kdl-go
// binding forces (see DeployTrack's doc): `track` is a bare presence node,
// and `track true` is a loud error rather than a second accepted spelling.
func TestLoadDaemon_DeployTrackTakesNoArgument(t *testing.T) {
	_, err := loadDeployDaemon(t, `
deploy {
    environment "dev" {
        source "main"
        track true
    }
}
`)
	if err == nil {
		t.Fatal("LoadDaemon accepted `track true`, want an error (track is a bare presence node)")
	}
}
