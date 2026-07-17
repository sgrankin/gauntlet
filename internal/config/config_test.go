package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// exampleDaemonPath and exampleChecksPath point at the fixtures that double
// as the repo's example config files. Both live at the repo root, two
// levels up from this package.
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
	if d.LogRetention != 720*time.Hour {
		t.Errorf("LogRetention = %v, want 720h", d.LogRetention)
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

	// Optional sections.
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
	if want := []string{"U025FTHN3", "U0987ZYXWV"}; !reflect.DeepEqual(d.Slack.AllowedUsers, want) {
		t.Errorf("Slack.AllowedUsers = %v, want %v", d.Slack.AllowedUsers, want)
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
	// gauntlet.kdl's example "mount" line is deliberately commented out (a
	// trust-changing, default-off knob — see its own comment there); assert
	// it stays that way so an accidental de-comment can't silently start
	// parsing without this test noticing.
	if len(d.Executor.Mounts) != 0 {
		t.Errorf("Executor.Mounts = %+v, want empty (example mount is commented out)", d.Executor.Mounts)
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
	// LogRetention defaults unconditionally (no "section absent" state to
	// preserve, unlike the optional sections below) — 30 days even though
	// the "log-retention" node is absent from this config.
	if d.LogRetention != defaultLogRetention {
		t.Errorf("LogRetention = %v, want default %v", d.LogRetention, defaultLogRetention)
	}
	// AutoRetryErrors defaults unconditionally to true, same "no absent
	// state to preserve" reasoning as LogRetention above.
	if d.AutoRetryErrors == nil || !*d.AutoRetryErrors {
		t.Errorf("AutoRetryErrors = %v, want default true", d.AutoRetryErrors)
	}

	// All optional sections are absent from this config; each should come
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

// TestLoadDaemon_ExecutorMounts covers the three shapes a "mount" node can
// take: bare (arg + path, read-write by default), and with an explicit
// "readonly" property. It also proves the reserved-path guard's
// pathAtOrUnder check is a real path-component comparison, not a
// strings.HasPrefix footgun: "/workspace2" merely shares "/workspace" as a
// string prefix and must load cleanly alongside the workdir default.
func TestLoadDaemon_ExecutorMounts(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/var/run/docker.sock" path="/var/run/docker.sock"
    mount "/etc/foo" path="/foo" readonly=true
    mount "/host/sibling" path="/workspace2"
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	want := []Mount{
		{Host: "/var/run/docker.sock", Path: "/var/run/docker.sock"},
		{Host: "/etc/foo", Path: "/foo", ReadOnly: true},
		{Host: "/host/sibling", Path: "/workspace2"},
	}
	if !reflect.DeepEqual(d.Executor.Mounts, want) {
		t.Errorf("Executor.Mounts = %+v, want %+v", d.Executor.Mounts, want)
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
	if d.Summarize.Effort != defaultSummarizeEffort {
		t.Errorf("Summarize.Effort = %q, want default %q", d.Summarize.Effort, defaultSummarizeEffort)
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
    effort "high"
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
	if d.Summarize.Effort != "high" {
		t.Errorf("Summarize.Effort = %q, want %q", d.Summarize.Effort, "high")
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

	// hooks-policy defaults to "queue" whenever the target has hooks and
	// left it unset (hooks v2, backlog policies).
	if main.HooksPolicy != "queue" {
		t.Errorf("main.HooksPolicy = %q, want %q (defaulted)", main.HooksPolicy, "queue")
	}

	// A target with no hook nodes at all must come back with a nil/empty
	// Hooks slice, not an error — hooks are opt-in per target. Its
	// HooksPolicy must stay "" (never defaulted): there is no hooks
	// backlog to have a policy about.
	release := d.Targets[1]
	if len(release.Hooks) != 0 {
		t.Errorf("release.Hooks = %+v, want empty", release.Hooks)
	}
	if release.HooksPolicy != "" {
		t.Errorf("release.HooksPolicy = %q, want empty (not defaulted without hooks)", release.HooksPolicy)
	}
}

// TestLoadDaemon_TargetHooksPolicy covers hooks-policy's three legal
// explicit values (hooks v2, backlog policies) round-tripping through
// LoadDaemon unchanged.
func TestLoadDaemon_TargetHooksPolicy(t *testing.T) {
	for _, policy := range []string{"queue", "coalesce", "cancel"} {
		t.Run(policy, func(t *testing.T) {
			dir := t.TempDir()
			path := dir + "/gauntlet.kdl"
			data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    hooks-policy "` + policy + `"
    hook "deploy" {
        command "make" "deploy"
    }
}
`)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			d, err := LoadDaemon(path)
			if err != nil {
				t.Fatalf("LoadDaemon: %v", err)
			}
			if got := d.Targets[0].HooksPolicy; got != policy {
				t.Errorf("HooksPolicy = %q, want %q", got, policy)
			}
		})
	}
}

// TestLoadDaemon_TargetMode_Example covers the three-mode example (serial
// default, batch, speculate) parsing with explicit knob values
// round-tripping unchanged.
func TestLoadDaemon_TargetMode_Example(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}

target "main" branch="main"

target "release" branch="release/v2" {
    mode "batch"
    max-batch 8
    on-batch-red "serial"
}

target "staging" branch="staging" {
    mode "speculate"
    window 4
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if len(d.Targets) != 3 {
		t.Fatalf("Targets = %+v, want 3 targets", d.Targets)
	}

	main := d.Targets[0]
	if main.Mode != "" {
		t.Errorf("main.Mode = %q, want empty (serial default, never normalized)", main.Mode)
	}
	if main.MaxBatch != 0 || main.Window != 0 || main.OnBatchRed != "" {
		t.Errorf("main mode-scoped knobs = %+v, want all zero", main)
	}

	release := d.Targets[1]
	if release.Mode != "batch" {
		t.Errorf("release.Mode = %q, want %q", release.Mode, "batch")
	}
	if release.MaxBatch != 8 {
		t.Errorf("release.MaxBatch = %d, want 8", release.MaxBatch)
	}
	if release.OnBatchRed != "serial" {
		t.Errorf("release.OnBatchRed = %q, want %q", release.OnBatchRed, "serial")
	}
	if release.Window != 0 {
		t.Errorf("release.Window = %d, want 0 (unset, not legal for batch)", release.Window)
	}

	staging := d.Targets[2]
	if staging.Mode != "speculate" {
		t.Errorf("staging.Mode = %q, want %q", staging.Mode, "speculate")
	}
	if staging.Window != 4 {
		t.Errorf("staging.Window = %d, want 4", staging.Window)
	}
	if staging.MaxBatch != 0 || staging.OnBatchRed != "" {
		t.Errorf("staging batch-only knobs = %+v, want zero", staging)
	}
}

// TestLoadDaemon_TargetMode_Defaults covers MaxBatch/Window/OnBatchRed
// defaulting only within their own mode when left unset in the config
// (MaxBatch defaults to 8, Window to 4, OnBatchRed to "serial").
func TestLoadDaemon_TargetMode_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "batch-target" branch="b" {
    mode "batch"
}
target "speculate-target" branch="s" {
    mode "speculate"
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	batch := d.Targets[0]
	if batch.MaxBatch != defaultMaxBatch {
		t.Errorf("batch.MaxBatch = %d, want default %d", batch.MaxBatch, defaultMaxBatch)
	}
	if batch.OnBatchRed != defaultOnBatchRed {
		t.Errorf("batch.OnBatchRed = %q, want default %q", batch.OnBatchRed, defaultOnBatchRed)
	}
	speculate := d.Targets[1]
	if speculate.Window != defaultWindow {
		t.Errorf("speculate.Window = %d, want default %d", speculate.Window, defaultWindow)
	}
}

// TestLoadDaemon_Services covers `services { allow "container" }` parsing,
// and MaxInstances/Runtime defaulting as documented.
func TestLoadDaemon_Services(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
services {
    allow "container"
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if want := []string{"container"}; !reflect.DeepEqual(d.Services.Allow, want) {
		t.Errorf("Services.Allow = %v, want %v", d.Services.Allow, want)
	}
	if d.Services.MaxInstances != defaultMaxInstances {
		t.Errorf("Services.MaxInstances = %d, want default %d", d.Services.MaxInstances, defaultMaxInstances)
	}
	// Executor kind defaults to "local" when absent, so Runtime should
	// default too (A3).
	if d.Services.Runtime != defaultServicesRuntime {
		t.Errorf("Services.Runtime = %q, want default %q", d.Services.Runtime, defaultServicesRuntime)
	}
}

// TestLoadDaemon_Services_ContainerExecutorRuntimeWins covers A3's "no
// services.runtime written ⇒ no defaulting, no conflict" path when the
// executor is "container": Services.Runtime stays "" (never defaulted),
// and load succeeds — chunk 3's cmd wiring is what actually resolves the
// effective runtime from the executor block in that case.
func TestLoadDaemon_Services_ContainerExecutorRuntimeWins(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    runtime "podman"
    image "ghcr.io/acme/ci:latest"
}
services {
    allow "container"
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.Services.Runtime != "" {
		t.Errorf("Services.Runtime = %q, want \"\" (executor runtime wins, A3)", d.Services.Runtime)
	}
}

// TestLoadDaemon_AutoRetryErrors covers the *bool pointer-ness this field's
// doc comment argues for: an operator explicitly writing
// "auto-retry-errors false" must be respected (not silently re-defaulted
// back to true), and explicit "true" must round-trip too — proving
// applyDefaults' nil check, not the field's zero value, is what
// distinguishes "never written" from "written false".
func TestLoadDaemon_AutoRetryErrors(t *testing.T) {
	build := func(t *testing.T, extra string) *Daemon {
		t.Helper()
		dir := t.TempDir()
		path := dir + "/gauntlet.kdl"
		data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
` + extra)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		d, err := LoadDaemon(path)
		if err != nil {
			t.Fatalf("LoadDaemon: %v", err)
		}
		return d
	}

	t.Run("explicit false is respected", func(t *testing.T) {
		d := build(t, "auto-retry-errors false\n")
		if d.AutoRetryErrors == nil || *d.AutoRetryErrors {
			t.Errorf("AutoRetryErrors = %v, want explicit false", d.AutoRetryErrors)
		}
	})

	t.Run("explicit true parses", func(t *testing.T) {
		d := build(t, "auto-retry-errors true\n")
		if d.AutoRetryErrors == nil || !*d.AutoRetryErrors {
			t.Errorf("AutoRetryErrors = %v, want explicit true", d.AutoRetryErrors)
		}
	})
}

func TestLoadDaemon_DurationParsing(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
poll-interval "1h30m"
log-retention "48h"
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
	if d.LogRetention != 48*time.Hour {
		t.Errorf("LogRetention = %v, want 48h", d.LogRetention)
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
			name: "log-retention<=0 given explicitly",
			kdl: `
remote "https://example.com/repo.git"
log-retention "-720h"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
`,
			wantErr: "log-retention",
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
			// Semantic validation (hooks v2, backlog policies): hooks-policy
			// is only meaningful alongside at least one hook.
			name: "hooks-policy set without any hooks",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    hooks-policy "coalesce"
}
`,
			wantErr: `target "main": hooks-policy set without any hooks`,
		},
		{
			// Semantic validation (hooks v2): hooks-policy must be one of
			// queue/coalesce/cancel.
			name: "hooks-policy unknown value",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    hooks-policy "sometimes"
    hook "deploy" {
        command "make" "deploy"
    }
}
`,
			wantErr: `target "main": hooks-policy must be one of queue, coalesce, cancel`,
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
			name: "executor mount missing path",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/var/run/docker.sock"
}
`,
			wantErr: "executor",
		},
		{
			name: "executor mount relative host path",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "var/run/docker.sock" path="/var/run/docker.sock"
}
`,
			wantErr: "executor",
		},
		{
			name: "executor mount relative container path",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/var/run/docker.sock" path="var/run/docker.sock"
}
`,
			wantErr: "executor",
		},
		{
			// Reserved-path guard: a mount must not shadow the trial-tree
			// mount at the executor's own workdir.
			name: "executor mount collides with workdir",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    workdir "/workspace"
    mount "/host/dir" path="/workspace"
}
`,
			wantErr: "workdir",
		},
		{
			// Reserved-path guard: a mount must not shadow the fixed
			// result-dir mount at /gauntlet (containerResultDir).
			name: "executor mount collides with result dir",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/host/dir" path="/gauntlet"
}
`,
			wantErr: "result-dir",
		},
		{
			// Reserved-path guard, canonicalization: a trailing slash on the
			// mount path must not bypass the workdir collision check.
			name: "executor mount collides with workdir via trailing slash",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    workdir "/workspace"
    mount "/host/dir" path="/workspace/"
}
`,
			wantErr: "workdir",
		},
		{
			// Reserved-path guard, canonicalization: a doubled separator on
			// the mount path must not bypass the result-dir collision check.
			name: "executor mount collides with result dir via doubled separator",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/host/dir" path="//gauntlet"
}
`,
			wantErr: "result-dir",
		},
		{
			// Reserved-path guard now also rejects a mount at a subpath
			// UNDER the workdir, not just an exact match: a nested bind
			// still silently, partially shadows the trial tree.
			name: "executor mount is a subpath under workdir",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    workdir "/workspace"
    mount "/host/dir" path="/workspace/src"
}
`,
			wantErr: "workdir",
		},
		{
			// Same as above, for the result-dir mount.
			name: "executor mount is a subpath under result dir",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/host/dir" path="/gauntlet/sub"
}
`,
			wantErr: "result-dir",
		},
		{
			// Reserved-path guard: a mount must not shadow the fixed
			// read-only bare-repo mount at /gauntlet-git (containerGitDir).
			name: "executor mount collides with git dir",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/host/dir" path="/gauntlet-git"
}
`,
			wantErr: "git-dir",
		},
		{
			// Same as above, for a subpath under the git-dir mount.
			name: "executor mount is a subpath under git dir",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/host/dir" path="/gauntlet-git/sub"
}
`,
			wantErr: "git-dir",
		},
		{
			name: "executor mount host contains colon",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/host/a:b" path="/foo"
}
`,
			wantErr: "executor",
		},
		{
			name: "executor mount path contains colon",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    image "ghcr.io/acme/ci:latest"
    mount "/host/dir" path="/foo:bar"
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
		{
			// Semantic validation: effort must be one of the claude-api
			// skill's legal output_config.effort values.
			name: "summarize with invalid effort",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
summarize {
    effort "extreme"
}
`,
			wantErr: "summarize",
		},
		{
			// window is only legal with mode "speculate".
			name: "window set on a serial (default-mode) target",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    window 4
}
`,
			wantErr: "window",
		},
		{
			name: "max-batch set on a speculate target",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "speculate"
    max-batch 8
}
`,
			wantErr: "max-batch",
		},
		{
			name: "on-batch-red set on a speculate target",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "speculate"
    on-batch-red "serial"
}
`,
			wantErr: "on-batch-red",
		},
		{
			name: "unknown mode",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "yolo"
}
`,
			wantErr: "mode",
		},
		{
			// on-batch-red "bisect" is validated (a legal enum value) but
			// rejected at construction — a reserved growth path, not yet
			// implemented.
			name: "on-batch-red bisect rejected at construction",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "batch"
    on-batch-red "bisect"
}
`,
			wantErr: "reserved for a future release",
		},
		{
			name: "max-batch out of bounds (too high)",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "batch"
    max-batch 65
}
`,
			wantErr: "max-batch",
		},
		{
			name: "max-batch out of bounds (zero via explicit negative)",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "batch"
    max-batch -1
}
`,
			wantErr: "max-batch",
		},
		{
			name: "window out of bounds (too high)",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "speculate"
    window 33
}
`,
			wantErr: "window",
		},
		{
			name: "window out of bounds (negative)",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "speculate"
    window -1
}
`,
			wantErr: "window",
		},
		{
			name: "reserved window-start governor knob rejected",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "speculate"
    window-start 1
}
`,
			wantErr: "reserved for a future adaptive-window governor",
		},
		{
			name: "reserved window-max governor knob rejected",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "speculate"
    window-max 8
}
`,
			wantErr: "reserved for a future adaptive-window governor",
		},
		{
			name: "reserved window-halve-on-red governor knob rejected",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main" {
    mode "speculate"
    window-halve-on-red true
}
`,
			wantErr: "reserved for a future adaptive-window governor",
		},
		{
			name: "services allow artifact rejected",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
services {
    allow "artifact"
}
`,
			wantErr: "reserved for a future release",
		},
		{
			name: "services allow unknown value rejected",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
services {
    allow "bogus"
}
`,
			wantErr: `services: allow must be "container"`,
		},
		{
			// A literal 0 is indistinguishable from "absent" (applyDefaults
			// would fill it back in to defaultMaxInstances, same
			// zero-vs-absent ambiguity as Daemon.Poll) — a negative value
			// is unambiguous either way, same convention as the
			// poll-interval<=0 case above.
			name: "services max-instances negative rejected",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
services {
    allow "container"
    max-instances -1
}
`,
			wantErr: "max-instances must be at least 1",
		},
		{
			name: "services runtime invalid under local executor",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
services {
    allow "container"
    runtime "bogus"
}
`,
			wantErr: `runtime must be "docker" or "podman"`,
		},
		{
			name: "services runtime conflicts with container executor runtime",
			kdl: `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "container" {
    runtime "podman"
    image "ghcr.io/acme/ci:latest"
}
services {
    allow "container"
    runtime "docker"
}
`,
			wantErr: `runtime "docker" conflicts with executor runtime "podman"`,
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

// TestLoadDaemon_KitchenSink covers a daemon-side combination that had never
// been co-parsed in one config document before (test-blind-spot audit):
// services{} alongside an executor{} block carrying both cache and mount
// children, plus auto-retry-errors false, plus history/dashboard/github for
// good measure. Each of these has its own dedicated test elsewhere (e.g.
// TestLoadDaemon_ExecutorMounts, TestLoadDaemon_Services,
// TestLoadDaemon_AutoRetryErrors), but never together in one document — this
// asserts every field still lands correctly when they're all present at
// once, since kdl-go's per-node unmarshaling could in principle let one
// section's presence/defaulting interfere with another's.
//
// executor.runtime and services.runtime are both explicitly "docker" here
// (rather than relying on either default) so the runtime cross-check
// (validate()'s "services: runtime conflicts with executor runtime" guard,
// A3) is genuinely exercised rather than trivially skipped.
func TestLoadDaemon_KitchenSink(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/gauntlet.kdl"
	data := []byte(`
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
auto-retry-errors false
history "/var/lib/gauntlet/history.db" {
    sample-every "10s"
    depth-retention "336h"
}
dashboard "localhost:8080" {
    url "https://gauntlet.internal.example"
}
github "acme/widgets" {
    token-env "GITHUB_TOKEN"
    api-url "https://api.github.com"
}
executor "container" {
    runtime "docker"
    image "ghcr.io/acme/ci:latest"
    cache "gocache" path="/root/.cache/go-build"
    mount "/var/run/docker.sock" path="/var/run/docker.sock"
}
services {
    allow "container"
    max-instances 4
    runtime "docker"
}
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}

	if d.Remote != "https://example.com/repo.git" {
		t.Errorf("Remote = %q", d.Remote)
	}
	if d.Committer.Name != "Gauntlet" || d.Committer.Email != "gauntlet@example.com" {
		t.Errorf("Committer = %+v", d.Committer)
	}
	if len(d.Targets) != 1 || d.Targets[0].Name != "main" || d.Targets[0].Branch != "main" {
		t.Errorf("Targets = %+v", d.Targets)
	}

	if d.AutoRetryErrors == nil || *d.AutoRetryErrors {
		t.Errorf("AutoRetryErrors = %v, want explicit false", d.AutoRetryErrors)
	}

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

	if d.Executor.Kind != "container" {
		t.Errorf("Executor.Kind = %q", d.Executor.Kind)
	}
	if d.Executor.Runtime != "docker" {
		t.Errorf("Executor.Runtime = %q", d.Executor.Runtime)
	}
	if d.Executor.Image != "ghcr.io/acme/ci:latest" {
		t.Errorf("Executor.Image = %q", d.Executor.Image)
	}
	wantCaches := []Cache{{Name: "gocache", Path: "/root/.cache/go-build"}}
	if !reflect.DeepEqual(d.Executor.Caches, wantCaches) {
		t.Errorf("Executor.Caches = %+v, want %+v", d.Executor.Caches, wantCaches)
	}
	wantMounts := []Mount{{Host: "/var/run/docker.sock", Path: "/var/run/docker.sock"}}
	if !reflect.DeepEqual(d.Executor.Mounts, wantMounts) {
		t.Errorf("Executor.Mounts = %+v, want %+v", d.Executor.Mounts, wantMounts)
	}

	if want := []string{"container"}; !reflect.DeepEqual(d.Services.Allow, want) {
		t.Errorf("Services.Allow = %v, want %v", d.Services.Allow, want)
	}
	if d.Services.MaxInstances != 4 {
		t.Errorf("Services.MaxInstances = %d, want 4", d.Services.MaxInstances)
	}
	if d.Services.Runtime != "docker" {
		t.Errorf("Services.Runtime = %q, want docker", d.Services.Runtime)
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
		{
			name: "undeclared need",
			kdl: `
check "test" {
    command "go" "test" "./..."
    needs "mssql"
}
`,
			wantErr: `needs "mssql": no such service declared`,
		},
		{
			name: "duplicate need within a check",
			kdl: `
service "mssql" {
    image "img"
    port 1433
}
check "test" {
    command "go" "test" "./..."
    needs "mssql" "mssql"
}
`,
			wantErr: `needs "mssql": duplicate`,
		},
		{
			name: "duplicate service name",
			kdl: `
service "mssql" {
    image "img"
    port 1433
}
service "mssql" {
    image "img2"
    port 1434
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": duplicate`,
		},
		{
			// "my-db" and "my_db" are exact-string-distinct (legal under the
			// duplicate-name check above) but both mangle to
			// GAUNTLET_SVC_MY_DB_* via envSafeName — the executor's env is a
			// last-wins slice, so one would silently shadow the other's
			// endpoint without this check.
			name: "service names collide under env-var name transform",
			kdl: `
service "my-db" {
    image "img"
    port 1433
}
service "my_db" {
    image "img2"
    port 1434
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "my_db": collides with service "my-db" under env var name GAUNTLET_SVC_MY_DB_*`,
		},
		{
			name: "service missing image",
			kdl: `
service "mssql" {
    port 1433
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": image must not be empty`,
		},
		{
			name: "service port zero",
			kdl: `
service "mssql" {
    image "img"
    port 0
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": port must be between 1 and 65535`,
		},
		{
			name: "service non-positive ready-timeout",
			kdl: `
service "mssql" {
    image "img"
    port 1433
    ready-timeout "-5s"
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": ready-timeout must be positive`,
		},
		{
			name: "service non-positive idle-ttl",
			kdl: `
service "mssql" {
    image "img"
    port 1433
    idle-ttl "-5m"
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": idle-ttl must be positive`,
		},
		{
			name: "service garbage memory",
			kdl: `
service "mssql" {
    image "img"
    port 1433
    memory "lots"
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": memory "lots"`,
		},
		{
			name: "service memory with space",
			kdl: `
service "mssql" {
    image "img"
    port 1433
    memory "2 gigs"
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": memory "2 gigs"`,
		},
		{
			name: "service negative cpus",
			kdl: `
service "mssql" {
    image "img"
    port 1433
    cpus "-1"
}
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `service "mssql": cpus "-1"`,
		},
		{
			name: "after names an undeclared check",
			kdl: `
check "package" {
    command "./ci/package"
    after "unit"
}
`,
			wantErr: `check "package": after "unit": no such check declared`,
		},
		{
			name: "after self-dependency",
			kdl: `
check "unit" {
    command "./ci/unit"
    after "unit"
}
`,
			wantErr: `check "unit": after "unit": a check cannot depend on itself`,
		},
		{
			name: "after duplicate edge",
			kdl: `
check "unit" {
    command "./ci/unit"
}
check "package" {
    command "./ci/package"
    after "unit" "unit"
}
`,
			wantErr: `check "package": after "unit": duplicate`,
		},
		{
			name: "after two-node cycle",
			kdl: `
check "a" {
    command "./a"
    after "b"
}
check "b" {
    command "./b"
    after "a"
}
`,
			wantErr: `dependency cycle`,
		},
		{
			name: "after three-node cycle through a forward reference",
			kdl: `
check "a" {
    command "./a"
    after "c"
}
check "b" {
    command "./b"
    after "a"
}
check "c" {
    command "./c"
    after "b"
}
`,
			wantErr: `dependency cycle`,
		},
		{
			name: "negative max-parallel",
			kdl: `
max-parallel -2
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `max-parallel must not be negative`,
		},
		{
			name: "absurd max-parallel",
			kdl: `
max-parallel 1000
check "test" {
    command "go" "test" "./..."
}
`,
			wantErr: `max-parallel 1000 exceeds`,
		},
		{
			name: "check consumes undeclared image",
			kdl: `
check "unit" {
    command "./ci/unit"
    image "go-ci"
}
`,
			wantErr: `check "unit": image "go-ci": no such image declared`,
		},
		{
			name: "image without command",
			kdl: `
image "go-ci" {
}
check "unit" {
    command "./ci/unit"
}
`,
			wantErr: `image "go-ci": command must not be empty`,
		},
		{
			name: "duplicate image names",
			kdl: `
image "go-ci" {
    command "./a"
}
image "go-ci" {
    command "./b"
}
check "unit" {
    command "./ci/unit"
}
`,
			wantErr: `image "go-ci": duplicate`,
		},
		{
			name: "check squats on the image node prefix",
			kdl: `
check "image:go-ci" {
    command "./ci/unit"
}
`,
			wantErr: `the "image:" name prefix is reserved`,
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
