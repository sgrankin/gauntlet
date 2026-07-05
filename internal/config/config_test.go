package config

import (
	"os"
	"reflect"
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
		// reflect.DeepEqual rather than != : Target now carries a Hooks
		// slice (post-land hooks), which makes it non-comparable with ==.
		if !reflect.DeepEqual(d.Targets[i], want) {
			t.Errorf("Targets[%d] = %+v, want %+v", i, d.Targets[i], want)
		}
	}

	// phase-2/3 sections (docs/plans/phase23.md §3).
	if d.History.Path != "/var/lib/gauntlet/history.db" {
		t.Errorf("History.Path = %q", d.History.Path)
	}
	if d.History.SampleEvery != 10*time.Second {
		t.Errorf("History.SampleEvery = %v, want 10s", d.History.SampleEvery)
	}
	if d.History.DepthRetention != 336*time.Hour {
		t.Errorf("History.DepthRetention = %v, want 336h", d.History.DepthRetention)
	}
	if d.Dashboard.Bind != "localhost:8080" {
		t.Errorf("Dashboard.Bind = %q", d.Dashboard.Bind)
	}
	if d.Dashboard.URL != "https://gauntlet.internal.example" {
		t.Errorf("Dashboard.URL = %q", d.Dashboard.URL)
	}
	if d.GitHub.Repo != "acme/widgets" {
		t.Errorf("GitHub.Repo = %q", d.GitHub.Repo)
	}
	if d.GitHub.TokenEnv != "GITHUB_TOKEN" {
		t.Errorf("GitHub.TokenEnv = %q", d.GitHub.TokenEnv)
	}
	if d.GitHub.APIURL != "https://api.github.com" {
		t.Errorf("GitHub.APIURL = %q", d.GitHub.APIURL)
	}
	if d.Slack.Channel != "C0123456789" {
		t.Errorf("Slack.Channel = %q", d.Slack.Channel)
	}
	if d.Slack.AppTokenEnv != "SLACK_APP_TOKEN" {
		t.Errorf("Slack.AppTokenEnv = %q", d.Slack.AppTokenEnv)
	}
	if d.Slack.BotTokenEnv != "SLACK_BOT_TOKEN" {
		t.Errorf("Slack.BotTokenEnv = %q", d.Slack.BotTokenEnv)
	}
	if d.OTLP.Endpoint != "localhost:4318" {
		t.Errorf("OTLP.Endpoint = %q", d.OTLP.Endpoint)
	}
	if !d.OTLP.Insecure {
		t.Errorf("OTLP.Insecure = false, want true")
	}
	if d.Executor.Kind != "container" {
		t.Errorf("Executor.Kind = %q", d.Executor.Kind)
	}
	if d.Executor.Runtime != "container" {
		t.Errorf("Executor.Runtime = %q", d.Executor.Runtime)
	}
	if d.Executor.Image != "ghcr.io/acme/ci:latest" {
		t.Errorf("Executor.Image = %q", d.Executor.Image)
	}
	if d.Executor.Workdir != "/workspace" {
		t.Errorf("Executor.Workdir = %q", d.Executor.Workdir)
	}
	wantCaches := []Cache{
		{Name: "gocache", Path: "/root/.cache/go-build"},
		{Name: "gomodcache", Path: "/go/pkg/mod"},
	}
	if len(d.Executor.Caches) != len(wantCaches) {
		t.Fatalf("Executor.Caches = %+v, want %+v", d.Executor.Caches, wantCaches)
	}
	for i, want := range wantCaches {
		if d.Executor.Caches[i] != want {
			t.Errorf("Executor.Caches[%d] = %+v, want %+v", i, d.Executor.Caches[i], want)
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

	// All phase-2/3 sections are absent from this config; each should come
	// back disabled (zero-valued) except Executor.Kind, which always
	// defaults to "local" regardless of whether the "executor" node is
	// present at all.
	if d.History.Path != "" {
		t.Errorf("History.Path = %q, want empty (disabled)", d.History.Path)
	}
	if d.History.SampleEvery != 0 {
		t.Errorf("History.SampleEvery = %v, want 0 (not defaulted when disabled)", d.History.SampleEvery)
	}
	if d.History.DepthRetention != 0 {
		t.Errorf("History.DepthRetention = %v, want 0 (not defaulted when disabled)", d.History.DepthRetention)
	}
	if d.Dashboard.Bind != "" {
		t.Errorf("Dashboard.Bind = %q, want empty (disabled)", d.Dashboard.Bind)
	}
	if d.Dashboard.URL != "" {
		t.Errorf("Dashboard.URL = %q, want empty (not defaulted when disabled)", d.Dashboard.URL)
	}
	if d.GitHub.Repo != "" {
		t.Errorf("GitHub.Repo = %q, want empty (disabled)", d.GitHub.Repo)
	}
	if d.Slack.Channel != "" {
		t.Errorf("Slack.Channel = %q, want empty (disabled)", d.Slack.Channel)
	}
	if d.OTLP.Endpoint != "" {
		t.Errorf("OTLP.Endpoint = %q, want empty (disabled)", d.OTLP.Endpoint)
	}
	if d.Executor.Kind != "local" {
		t.Errorf("Executor.Kind = %q, want default %q", d.Executor.Kind, "local")
	}
	if d.Executor.Runtime != "" {
		t.Errorf("Executor.Runtime = %q, want empty (only defaulted for container executor)", d.Executor.Runtime)
	}
	if d.Summarize != nil {
		t.Errorf("Summarize = %+v, want nil (disabled, node absent)", d.Summarize)
	}
}

// TestLoadDaemon_SummarizeDefaults covers the presence rule that makes
// Summarize different from every other optional section: an empty
// "summarize {}" node (no field set inside it) must still count as
// enabled — enabled means "the node is present", not "some field is
// non-empty" (every field here has its own default).
func TestLoadDaemon_SummarizeDefaults(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
summarize {
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.Summarize == nil {
		t.Fatal("Summarize = nil, want non-nil (node present, even though empty)")
	}
	if d.Summarize.Model != defaultSummarizeModel {
		t.Errorf("Summarize.Model = %q, want default %q", d.Summarize.Model, defaultSummarizeModel)
	}
	if d.Summarize.APIKeyEnv != defaultSummarizeAPIKeyEnv {
		t.Errorf("Summarize.APIKeyEnv = %q, want default %q", d.Summarize.APIKeyEnv, defaultSummarizeAPIKeyEnv)
	}
	if d.Summarize.Timeout != defaultSummarizeTimeout {
		t.Errorf("Summarize.Timeout = %v, want default %v", d.Summarize.Timeout, defaultSummarizeTimeout)
	}
}

// TestLoadDaemon_SummarizeExplicitValues covers every field set explicitly,
// overriding all three defaults.
func TestLoadDaemon_SummarizeExplicitValues(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
summarize {
    model "claude-opus-4-8"
    api-key-env "MY_ANTHROPIC_KEY"
    timeout "30s"
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.Summarize == nil {
		t.Fatal("Summarize = nil, want non-nil")
	}
	if d.Summarize.Model != "claude-opus-4-8" {
		t.Errorf("Summarize.Model = %q", d.Summarize.Model)
	}
	if d.Summarize.APIKeyEnv != "MY_ANTHROPIC_KEY" {
		t.Errorf("Summarize.APIKeyEnv = %q", d.Summarize.APIKeyEnv)
	}
	if d.Summarize.Timeout != 30*time.Second {
		t.Errorf("Summarize.Timeout = %v, want 30s", d.Summarize.Timeout)
	}
}

func TestLoadDaemon_TargetHooks(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    hook "deploy" {
        command "make" "deploy"
    }
    hook "notify" {
        command "curl" "-X" "POST" "https://example.com/notify"
    }
}
target "release" branch="release/v2"
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if len(d.Targets) != 2 {
		t.Fatalf("Targets = %+v, want 2 targets", d.Targets)
	}

	main := d.Targets[0]
	wantHooks := []Hook{
		{Name: "deploy", Command: []string{"make", "deploy"}},
		{Name: "notify", Command: []string{"curl", "-X", "POST", "https://example.com/notify"}},
	}
	if len(main.Hooks) != len(wantHooks) {
		t.Fatalf("main.Hooks = %+v, want %+v", main.Hooks, wantHooks)
	}
	for i, want := range wantHooks {
		got := main.Hooks[i]
		if got.Name != want.Name || strings.Join(got.Command, " ") != strings.Join(want.Command, " ") {
			t.Errorf("main.Hooks[%d] = %+v, want %+v", i, got, want)
		}
	}

	// A target with no hook nodes at all must come back with a nil/empty
	// Hooks slice, not an error — hooks are opt-in per target.
	release := d.Targets[1]
	if len(release.Hooks) != 0 {
		t.Errorf("release.Hooks = %+v, want empty", release.Hooks)
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
			name: "duplicate target branch",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main"    branch="shared"
target "release" branch="shared"
`,
			wantErr: `branch "shared" already used by target "main"`,
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
		{
			// Semantic validation: History enabled (Path set) but an
			// explicit non-positive sample-every.
			name: "history with non-positive sample-every",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
history "/tmp/history.db" {
    sample-every "-5s"
}
`,
			wantErr: "history",
		},
		{
			// Semantic validation: History enabled (Path set) but an
			// explicit non-positive depth-retention.
			name: "history with non-positive depth-retention",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
history "/tmp/history.db" {
    depth-retention "-336h"
}
`,
			wantErr: "history",
		},
		{
			// Structural: kdl-go itself rejects the unexpected property,
			// naming the "dashboard" node in its own error.
			name: "dashboard with unexpected property",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
dashboard "localhost:8080" {
    url "https://example.com"
    bogus "nope"
}
`,
			wantErr: "dashboard",
		},
		{
			// Semantic validation: Repo not in "owner/name" form.
			name: "github repo missing slash",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
github "widgets"
`,
			wantErr: "github",
		},
		{
			// Semantic validation: Repo has a slash but an empty name half
			// ("owner/"). Contains("/") alone would wrongly accept this.
			name: "github repo empty name",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
github "widgets/"
`,
			wantErr: "github",
		},
		{
			// Semantic validation: Repo has a slash but an empty owner half
			// ("/name"). Contains("/") alone would wrongly accept this.
			name: "github repo empty owner",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
github "/widgets"
`,
			wantErr: "github",
		},
		{
			// Structural: kdl-go rejects the unexpected property under
			// "slack" (mirrors the dashboard/otlp structural cases above).
			name: "slack with unexpected property",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
slack "C0123456789" {
    app-token-env "SLACK_APP_TOKEN"
    bogus "nope"
}
`,
			wantErr: "slack",
		},
		{
			// Structural: kdl-go rejects the unexpected property under
			// "otlp".
			name: "otlp with unexpected property",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
otlp "localhost:4318" {
    insecure true
    bogus "nope"
}
`,
			wantErr: "otlp",
		},
		{
			// Semantic validation: container executor requires an image.
			name: "executor container without image",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container"
`,
			wantErr: "executor",
		},
		{
			// Semantic validation: unrecognized executor kind.
			name: "executor with unknown kind",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "kubernetes"
`,
			wantErr: "executor",
		},
		{
			// Semantic validation: a hook's command must not be empty.
			name: "target hook missing command",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    hook "deploy" {
    }
}
`,
			wantErr: `target "main": hook "deploy"`,
		},
		{
			// Semantic validation: duplicate hook names within one target.
			name: "target duplicate hook names",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    hook "deploy" {
        command "make" "deploy"
    }
    hook "deploy" {
        command "make" "redeploy"
    }
}
`,
			wantErr: `target "main": hook "deploy": duplicate`,
		},
		{
			// Semantic validation: cache entry missing its required path.
			name: "executor cache missing path",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    cache "gocache"
}
`,
			wantErr: "executor",
		},
		{
			// Semantic validation: an explicit non-positive timeout is
			// rejected even though Timeout has a default (the default only
			// applies when the field is the zero value, i.e. unset).
			name: "summarize with non-positive timeout",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
summarize {
    timeout "-5s"
}
`,
			wantErr: "summarize",
		},
		{
			// Structural: kdl-go rejects the unexpected property under
			// "summarize" (mirrors the dashboard/otlp/slack structural
			// cases above).
			name: "summarize with unexpected property",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
summarize {
    model "claude-haiku-4-5"
    bogus "nope"
}
`,
			wantErr: "summarize",
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
