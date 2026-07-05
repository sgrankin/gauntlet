package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// exampleDaemonPath and exampleChecksPath point at the fixtures that double
// as the repo's example config files (docs/plans/phase1.md §4). Both live
// at the repo root, two levels up from this package.
const (
	exampleDaemonPath = "../../gauntlet.kdl"
	exampleChecksPath = "../../.gauntlet.kdl"
)

func TestLoadDaemon_Example(t *testing.T) {
	d, err := LoadDaemon(exampleDaemonPath)
	if err != nil {
		t.Fatalf("LoadDaemon(%s): %v", exampleDaemonPath, err)
	}
	if d.Remote != "https://github.com/acme/widgets.git" {
		t.Errorf("Remote = %q", d.Remote)
	}
	if d.Poll != 10*time.Second {
		t.Errorf("Poll = %v, want 10s", d.Poll)
	}
	if d.CheckSpec != ".gauntlet.kdl" {
		t.Errorf("CheckSpec = %q", d.CheckSpec)
	}
	if d.Committer.Name != "Gauntlet" || d.Committer.Email != "gauntlet@ci.acme.example" {
		t.Errorf("Committer = %+v", d.Committer)
	}
	if d.MergeMsg != "Merge {{.Topic}} ({{.User}})" {
		t.Errorf("MergeMsg = %q", d.MergeMsg)
	}
	wantTargets := []Target{
		{Name: "main", Branch: "main"},
		{Name: "release", Branch: "release/v2"},
	}
	if len(d.Targets) != len(wantTargets) {
		t.Fatalf("Targets = %+v, want %+v", d.Targets, wantTargets)
	}
	for i, want := range wantTargets {
		if d.Targets[i] != want {
			t.Errorf("Targets[%d] = %+v, want %+v", i, d.Targets[i], want)
		}
	}
}

func TestParseChecks_Example(t *testing.T) {
	data, err := os.ReadFile(exampleChecksPath)
	if err != nil {
		t.Fatalf("reading %s: %v", exampleChecksPath, err)
	}
	cs, err := ParseChecks(data)
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	want := []Check{
		{Name: "lint", Command: []string{"golangci-lint", "run"}},
		{Name: "test", Command: []string{"go", "test", "./..."}},
		{Name: "build", Command: []string{"go", "build", "./..."}},
	}
	if len(cs.Checks) != len(want) {
		t.Fatalf("Checks = %+v, want %+v", cs.Checks, want)
	}
	for i, w := range want {
		got := cs.Checks[i]
		if got.Name != w.Name || strings.Join(got.Command, " ") != strings.Join(w.Command, " ") {
			t.Errorf("Checks[%d] = %+v, want %+v", i, got, w)
		}
	}
}

func TestLoadDaemon_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.Poll != defaultPoll {
		t.Errorf("Poll = %v, want default %v", d.Poll, defaultPoll)
	}
	if d.CheckSpec != defaultCheckSpec {
		t.Errorf("CheckSpec = %q, want default %q", d.CheckSpec, defaultCheckSpec)
	}
}

func TestLoadDaemon_DurationParsing(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
poll-interval "1h30m"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	want := 90 * time.Minute
	if d.Poll != want {
		t.Errorf("Poll = %v, want %v", d.Poll, want)
	}
}

func TestLoadDaemon_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		kdl     string
		wantErr string // substring the error message must contain
	}{
		{
			name: "missing remote",
			kdl: `
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
`,
			wantErr: "remote",
		},
		{
			name: "poll<=0 given explicitly",
			kdl: `
remote "https://example.com/repo.git"
poll-interval "-5s"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
`,
			wantErr: "poll-interval",
		},
		{
			name: "no targets",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
`,
			wantErr: "target",
		},
		{
			name: "target missing branch",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main"
`,
			wantErr: `target "main"`,
		},
		{
			name: "target name containing slash",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "rel/v2" branch="release/v2"
`,
			wantErr: `target "rel/v2"`,
		},
		{
			name: "empty committer name",
			kdl: `
remote "https://example.com/repo.git"
committer {
    email "gauntlet@example.com"
}
target "main" branch="main"
`,
			wantErr: "committer",
		},
		{
			name: "empty committer email",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
}
target "main" branch="main"
`,
			wantErr: "committer",
		},
		{
			name: "non-parsing merge-message template",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
merge-message "Merge {{.Topic"
target "main" branch="main"
`,
			wantErr: "merge-message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := dir + "/gauntlet.kdl"
			if err := os.WriteFile(path, []byte(tc.kdl), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDaemon(path)
			if err == nil {
				t.Fatalf("LoadDaemon: got nil error, want one containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("LoadDaemon error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadDaemon_MissingFile(t *testing.T) {
	if _, err := LoadDaemon("/nonexistent/gauntlet.kdl"); err == nil {
		t.Fatal("LoadDaemon: got nil error for missing file")
	}
}

func TestParseChecks_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		kdl     string
		wantErr string
	}{
		{
			name:    "empty check spec",
			kdl:     ``,
			wantErr: "no checks defined",
		},
		{
			name: "check without command",
			kdl: `
check "test" {
}
`,
			wantErr: `check "test"`,
		},
		{
			name: "duplicate check names",
			kdl: `
check "test" {
    command "go" "test" "./..."
}
check "test" {
    command "go" "vet" "./..."
}
`,
			wantErr: `check "test"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseChecks([]byte(tc.kdl))
			if err == nil {
				t.Fatalf("ParseChecks: got nil error, want one containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseChecks error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
