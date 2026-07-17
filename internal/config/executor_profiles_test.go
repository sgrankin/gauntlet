package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadDaemonString writes kdl to a temp file and LoadDaemons it — profile
// resolution (resolveExecutors) only runs on the LoadDaemon path, so these
// tests must go through it, not hand-built structs.
func loadDaemonString(t *testing.T, kdl string) (*Daemon, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gauntlet.kdl")
	if err := os.WriteFile(path, []byte(kdl), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return LoadDaemon(path)
}

const profileTestBase = `
remote "https://example.com/repo.git"
committer name="Gauntlet" email="g@ci.example"
target "main" {
    branch "main"
}
`

func TestLoadDaemon_ExecutorProfiles(t *testing.T) {
	d, err := loadDaemonString(t, profileTestBase+`
max-executions 8
executor "container" {
    runtime "docker"
    image "ci:latest"
}
executor "host" kind="local" {
    env "DEPLOY_TARGET" "staging"
}
executor "ci" kind="container" {
    runtime "podman"
    image "go-ci:latest"
    cache "gocache" path="/root/.cache/go-build"
    mount "/var/run/docker.sock" path="/var/run/docker.sock"
    add-host "host.docker.internal" "host-gateway"
    env "TESTCONTAINERS_HOST_OVERRIDE" "host.docker.internal"
    memory "8g"
    cpus "4"
}
`)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.MaxExecutions != 8 {
		t.Errorf("MaxExecutions = %d, want 8", d.MaxExecutions)
	}
	// The kind-less block is the default profile, its arg the KIND.
	if d.Executor.Kind != "container" || d.Executor.Name != "" || d.Executor.Image != "ci:latest" {
		t.Errorf("default profile = %+v, want kind=container image=ci:latest name=\"\"", d.Executor)
	}
	if len(d.Profiles) != 2 {
		t.Fatalf("Profiles = %+v, want 2", d.Profiles)
	}
	host, ci := d.Profiles[0], d.Profiles[1]
	if host.Name != "host" || host.Kind != "local" || len(host.Env) != 1 || host.Env[0].Name != "DEPLOY_TARGET" {
		t.Errorf("host profile = %+v", host)
	}
	if ci.Name != "ci" || ci.Kind != "container" || ci.Runtime != "podman" || ci.Image != "go-ci:latest" {
		t.Errorf("ci profile = %+v", ci)
	}
	if ci.Workdir != "/workspace" {
		t.Errorf("ci.Workdir = %q, want the container default applied per profile", ci.Workdir)
	}
	if len(ci.AddHosts) != 1 || ci.AddHosts[0].Host != "host.docker.internal" || ci.AddHosts[0].Gateway != "host-gateway" {
		t.Errorf("ci.AddHosts = %+v", ci.AddHosts)
	}
	if ci.Memory != "8g" || ci.CPUs != "4" {
		t.Errorf("ci resources = %q/%q", ci.Memory, ci.CPUs)
	}
}

// TestLoadDaemon_MaxExecutionsOnExecutorBlockStillLoads: the cap was
// briefly documented on the executor block; a config written then must
// keep loading, adopted into the canonical top-level field.
func TestLoadDaemon_MaxExecutionsOnExecutorBlockStillLoads(t *testing.T) {
	d, err := loadDaemonString(t, profileTestBase+`
executor "container" {
    image "ci:latest"
    max-executions 6
}
`)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.MaxExecutions != 6 {
		t.Errorf("MaxExecutions = %d, want the executor-block value adopted", d.MaxExecutions)
	}

	if _, err := loadDaemonString(t, profileTestBase+`
max-executions 4
executor "container" {
    image "ci:latest"
    max-executions 6
}
`); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("both spellings set: err = %v, want a keep-the-top-level-one error", err)
	}

	if _, err := loadDaemonString(t, profileTestBase+`
executor "a" kind="local" {
    max-executions 6
}
`); err == nil || !strings.Contains(err.Error(), "daemon-wide") {
		t.Errorf("per-profile cap: err = %v, want a daemon-wide rejection", err)
	}
}

func TestLoadDaemon_NoExecutorNodeStillDefaultsLocal(t *testing.T) {
	d, err := loadDaemonString(t, profileTestBase)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.Executor.Kind != "local" || len(d.Profiles) != 0 {
		t.Errorf("executor = %+v profiles = %+v, want implicit local default and no profiles", d.Executor, d.Profiles)
	}
}

func TestLoadDaemon_ExecutorProfileErrors(t *testing.T) {
	cases := []struct {
		name    string
		kdl     string
		wantErr string
	}{
		{
			name:    "two kind-less blocks",
			kdl:     "executor \"local\"\nexecutor \"container\" {\n    image \"x\"\n}\n",
			wantErr: "more than one default",
		},
		{
			name:    "profile named local",
			kdl:     "executor \"local\" kind=\"local\"\n",
			wantErr: `may not be named "local"`,
		},
		{
			name:    "profile with kind but no name",
			kdl:     "executor kind=\"local\"\n",
			wantErr: "needs a name argument",
		},
		{
			name:    "duplicate profile names",
			kdl:     "executor \"a\" kind=\"local\"\nexecutor \"a\" kind=\"local\"\n",
			wantErr: `executor "a": duplicate profile name`,
		},
		{
			name:    "profile with bogus kind",
			kdl:     "executor \"a\" kind=\"warp\"\n",
			wantErr: `executor "a": kind must be`,
		},
		{
			name:    "container profile without image",
			kdl:     "executor \"a\" kind=\"container\"\n",
			wantErr: `executor "a": image must not be empty`,
		},
		{
			name:    "local profile with container-only option",
			kdl:     "executor \"a\" kind=\"local\" {\n    memory \"2g\"\n}\n",
			wantErr: `executor "a": memory is container-only`,
		},
		{
			name:    "local default with container-only option",
			kdl:     "executor \"local\" {\n    image \"x\"\n}\n",
			wantErr: "executor: image is container-only",
		},
		{
			name:    "profile env squats on the check contract",
			kdl:     "executor \"a\" kind=\"local\" {\n    env \"GAUNTLET_REF\" \"x\"\n}\n",
			wantErr: "GAUNTLET_* namespace is reserved",
		},
		{
			name:    "add-host with colon in hostname",
			kdl:     "executor \"a\" kind=\"container\" {\n    image \"x\"\n    add-host \"evil:host\" \"gw\"\n}\n",
			wantErr: "hostname must not contain ':'",
		},
		{
			name:    "profile mount under reserved git dir",
			kdl:     "executor \"a\" kind=\"container\" {\n    image \"x\"\n    mount \"/data\" path=\"/gauntlet-git/sub\"\n}\n",
			wantErr: `executor "a": mount "/data"`,
		},
		{
			name:    "negative max-executions",
			kdl:     "max-executions -1\n",
			wantErr: "max-executions must not be negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadDaemonString(t, profileTestBase+tc.kdl)
			if err == nil {
				t.Fatalf("LoadDaemon: got nil error, want one containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestParseChecks_ExecutorSelection: the repo spec may NAME a profile —
// nothing more. Resolution against the daemon's profiles happens at run
// start in the queue, not here (the spec parser can't know daemon config).
func TestParseChecks_ExecutorSelection(t *testing.T) {
	cs, err := ParseChecks([]byte(`
check "test" {
    command "./ci/test"
    executor "ci"
}
check "receipt" {
    command "./ci/receipt"
}
`))
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if cs.Checks[0].Executor != "ci" {
		t.Errorf("Checks[0].Executor = %q, want ci", cs.Checks[0].Executor)
	}
	if cs.Checks[1].Executor != "" {
		t.Errorf("Checks[1].Executor = %q, want \"\" (default)", cs.Checks[1].Executor)
	}
}
