// Package config parses gauntlet's two KDL config files into plain structs:
// the admin-written daemon config and the repo-side check spec. This is the
// only package that touches KDL, so the config language stays swappable
// (docs/plans/phase1.md §9.8) and callers depend on the structs and
// LoadDaemon/ParseChecks signatures, never on kdl-go directly.
//
// kdl-go's unmarshaler has thin validation (no required-field or
// non-negative-value enforcement), so every exported load function here runs
// a Go-side validation pass afterward; its errors name the offending
// node/field.
package config

import (
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	kdl "github.com/sblinch/kdl-go"

	"github.com/sgrankin/gauntlet/internal/core"
)

// defaultPoll and defaultCheckSpec are applied when the corresponding node
// is absent from the daemon config.
const (
	defaultPoll      = 10 * time.Second
	defaultCheckSpec = ".gauntlet.kdl"
)

// Defaults for the phase-2/3 optional sections (docs/plans/phase23.md §3);
// applied only when the section is enabled (its required key non-empty) and
// the defaulted field is unset.
const (
	defaultGitHubTokenEnv = "GITHUB_TOKEN"
	defaultGitHubAPIURL   = "https://api.github.com"
	defaultSlackAppEnv    = "SLACK_APP_TOKEN"
	defaultSlackBotEnv    = "SLACK_BOT_TOKEN"
	defaultExecutorKind   = "local"
	defaultRuntime        = "container"
	defaultWorkdir        = "/workspace"

	// defaultSummarizeModel matches internal/summarize.DefaultModel
	// (duplicated here, not imported, per this file's existing pattern of
	// owning its own defaults — see defaultGitHubTokenEnv et al.): a
	// Haiku-class model, per the claude-api skill's guidance for a cheap
	// 2-6 sentence summarization task.
	defaultSummarizeModel     = "claude-haiku-4-5"
	defaultSummarizeAPIKeyEnv = "ANTHROPIC_API_KEY"
	defaultSummarizeTimeout   = 10 * time.Second

	// defaultDepthRetention is how long queue_depth samples are kept before
	// the depth sampler prunes them (docs/plans phase23 E1). Runs/checks
	// have no retention bound — this applies to the depth series only.
	defaultDepthRetention = 14 * 24 * time.Hour

	// defaultLogRetention is how long full per-check log directories
	// (Config.LogDir/<runID>/, DESIGN.md "Full per-check log files") are
	// kept before cmd/gauntlet's prune sweep deletes them — 30 days.
	// Unlike depth-retention, this applies unconditionally: cmd/gauntlet
	// always wires Config.LogDir (chunk F-b), so this default always
	// matters, not only when some optional section is enabled.
	defaultLogRetention = 30 * 24 * time.Hour
)

// Daemon is the admin-written daemon config (docs/plans/phase1.md §4): one
// remote, the reconcile cadence, the committer identity used for merge
// commits, the merge-message template, and the target branches to
// reconcile. The phase-2/3 sections (docs/plans/phase23.md §3) are all
// optional value structs (not pointers): kdl-go leaves a struct-typed field
// at its zero value when the corresponding node is absent from the document
// (confirmed against kdl-go's unmarshalNodesToStruct, which only visits
// nodes actually present), so "section present" is encoded as "its required
// key is non-empty" rather than a nil check.
type Daemon struct {
	Remote    string        `kdl:"remote"`
	Poll      time.Duration `kdl:"poll-interval,format:units"`
	CheckSpec string        `kdl:"check-spec"`
	Committer core.Identity `kdl:"committer"`
	MergeMsg  string        `kdl:"merge-message"`
	Targets   []Target      `kdl:"target,multiple"`

	// LogRetention bounds how long full per-check log directories survive
	// under cmd/gauntlet's <state>/logs (DESIGN.md "Full per-check log
	// files"); cmd/gauntlet's prune sweep deletes any run-log directory
	// older than this. Unconditional (unlike History/Dashboard/...): full
	// logging is always wired up in cmd/gauntlet, so this always defaults
	// (30 days) rather than only defaulting when some section is "enabled".
	LogRetention time.Duration `kdl:"log-retention,format:units"`

	History   History   `kdl:"history"`   // Path=="" ⇒ disabled
	Dashboard Dashboard `kdl:"dashboard"` // Bind=="" ⇒ disabled
	GitHub    GitHub    `kdl:"github"`    // Repo=="" ⇒ disabled
	Slack     Slack     `kdl:"slack"`     // Channel=="" ⇒ disabled
	OTLP      OTLP      `kdl:"otlp"`      // Endpoint=="" ⇒ no-op (phase-1 default)
	Executor  Executor  `kdl:"executor"`  // Kind=="" ⇒ "local"

	// Summarize is a pointer, unlike every other optional section above:
	// every one of its fields has its own default (Model, APIKeyEnv,
	// Timeout), so "required field non-empty" can't serve as the
	// presence signal the way GitHub.Repo/Slack.Channel/etc. do — a
	// user-written "summarize {}" with nothing set must still count as
	// enabled. kdl-go only allocates a pointer-typed child-node field
	// when the node is actually present in the document (confirmed
	// against its own unmarshal tests), so nil here means "the summarize
	// node is absent" unambiguously, independent of what's inside it.
	Summarize *Summarize `kdl:"summarize"`
}

// History configures the optional SQLite run-history store
// (docs/plans/phase23.md §4.1). Path=="" disables it.
type History struct {
	Path           string        `kdl:",arg"`
	SampleEvery    time.Duration `kdl:"sample-every,format:units"`    // default = Poll
	DepthRetention time.Duration `kdl:"depth-retention,format:units"` // default 14 days; queue_depth only
}

// Dashboard configures the optional read-only web dashboard
// (docs/plans/phase23.md §4.2). Bind=="" disables it.
type Dashboard struct {
	Bind string `kdl:",arg"` // "localhost:8080"; "" disables
	URL  string `kdl:"url"`  // §9.3: optional public base URL for outbound links
}

// GitHub configures the optional commit-status channel
// (docs/plans/phase23.md §4.3). Repo=="" disables it.
type GitHub struct {
	Repo     string `kdl:",arg"`      // "owner/name"
	TokenEnv string `kdl:"token-env"` // default "GITHUB_TOKEN"
	APIURL   string `kdl:"api-url"`   // default "https://api.github.com"
}

// Slack configures the optional Slack channel (docs/plans/phase23.md §4.4).
// Channel=="" disables it.
type Slack struct {
	Channel     string `kdl:",arg"`          // channel ID
	AppTokenEnv string `kdl:"app-token-env"` // default "SLACK_APP_TOKEN"
	BotTokenEnv string `kdl:"bot-token-env"` // default "SLACK_BOT_TOKEN"
}

// OTLP configures the optional OTLP trace exporter (docs/plans/phase23.md
// §4.6). Endpoint=="" leaves tracing a no-op (the phase-1 default).
type OTLP struct {
	Endpoint string `kdl:",arg"`
	Insecure bool   `kdl:"insecure"`
}

// Executor selects the check-execution backend (docs/plans/phase23.md §4.5).
// Kind=="" defaults to "local" (the phase-1 in-process executor); "container"
// requires Image.
type Executor struct {
	Kind    string  `kdl:",arg"`    // "local" (default) | "container"
	Runtime string  `kdl:"runtime"` // "docker"|"podman"|"container"; default "container"
	Image   string  `kdl:"image"`   // required when Kind=="container"
	Workdir string  `kdl:"workdir"` // default "/workspace"
	Caches  []Cache `kdl:"cache,multiple"`
}

// Cache is one persistent named cache volume mounted into the container
// executor.
type Cache struct {
	Name string `kdl:",arg"`
	Path string `kdl:"path"`
}

// Summarize configures the optional Claude-written merge-commit body
// enricher (internal/summarize). A nil *Daemon.Summarize (the node absent
// from the document) disables it entirely; see the field doc on Daemon for
// why presence, not any single field's non-emptiness, is the enable
// signal.
type Summarize struct {
	Model     string        `kdl:"model"`                // default defaultSummarizeModel
	APIKeyEnv string        `kdl:"api-key-env"`          // default "ANTHROPIC_API_KEY"
	Timeout   time.Duration `kdl:"timeout,format:units"` // default 10s
}

// Target is one target branch the daemon reconciles candidates onto. Name
// is the queue-grammar name parsed out of candidate refs
// (refs/heads/for/<name>/...) and must not contain '/'; Branch is the
// actual git branch and may (docs/plans/phase1.md §9.3).
type Target struct {
	Name   string `kdl:",arg"`
	Branch string `kdl:"branch"`

	// Hooks are this target's post-land hooks (DESIGN.md's decision
	// ledger, "Deployments as post-land hooks"; internal/hooks), run in
	// order against the landed tree once a candidate lands. Nil/empty
	// means no hooks — the phase-1/2/3 behavior, unchanged.
	Hooks []Hook `kdl:"hook,multiple"`
}

// Hook is one named command a target runs, in order, once a candidate
// lands onto it. It carries only the command — internal/hooks defines what
// running it means (executor, environment, stop-on-failure, notification),
// same separation as config.Check vs. the queue's check execution.
type Hook struct {
	Name    string   `kdl:",arg"`
	Command []string `kdl:"command,child"`
}

// LoadDaemon reads, parses, and validates the daemon config at path.
func LoadDaemon(path string) (*Daemon, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var d Daemon
	if err := kdl.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	d.applyDefaults()
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &d, nil
}

func (d *Daemon) applyDefaults() {
	// Poll's zero value is indistinguishable from "node absent" (kdl-go
	// unmarshals a missing node into the field's zero value); an explicit
	// negative poll-interval is not, so validate() still rejects it after
	// this default is applied.
	if d.Poll == 0 {
		d.Poll = defaultPoll
	}
	if d.CheckSpec == "" {
		d.CheckSpec = defaultCheckSpec
	}
	// LogRetention defaults unconditionally (see its doc): there is no
	// "log-retention section absent -> disabled" state to preserve, unlike
	// every phase-2/3 section below.
	if d.LogRetention == 0 {
		d.LogRetention = defaultLogRetention
	}

	// History: SampleEvery defaults to the reconcile cadence. Only meaningful
	// (and only defaulted) when history is enabled.
	if d.History.Path != "" && d.History.SampleEvery == 0 {
		d.History.SampleEvery = d.Poll
	}
	if d.History.Path != "" && d.History.DepthRetention == 0 {
		d.History.DepthRetention = defaultDepthRetention
	}

	// Dashboard: URL defaults to an http:// URL built from Bind (§9.3) —
	// outbound links (e.g. GitHub target_url) must not point at a bind
	// address like "0.0.0.0:8080" or "localhost:8080" in a way that's
	// unreachable from outside, but absent an explicit URL that's the best
	// available default.
	if d.Dashboard.Bind != "" && d.Dashboard.URL == "" {
		d.Dashboard.URL = "http://" + d.Dashboard.Bind
	}

	if d.GitHub.Repo != "" {
		if d.GitHub.TokenEnv == "" {
			d.GitHub.TokenEnv = defaultGitHubTokenEnv
		}
		if d.GitHub.APIURL == "" {
			d.GitHub.APIURL = defaultGitHubAPIURL
		}
	}

	if d.Slack.Channel != "" {
		if d.Slack.AppTokenEnv == "" {
			d.Slack.AppTokenEnv = defaultSlackAppEnv
		}
		if d.Slack.BotTokenEnv == "" {
			d.Slack.BotTokenEnv = defaultSlackBotEnv
		}
	}

	// Executor.Kind always defaults to "local", regardless of whether the
	// "executor" node was present at all (an absent node ⇒ local executor,
	// matching phase-1 behavior). Runtime/Workdir only matter for the
	// container executor, so only default them in that case.
	if d.Executor.Kind == "" {
		d.Executor.Kind = defaultExecutorKind
	}
	if d.Executor.Kind == "container" {
		if d.Executor.Runtime == "" {
			d.Executor.Runtime = defaultRuntime
		}
		if d.Executor.Workdir == "" {
			d.Executor.Workdir = defaultWorkdir
		}
	}

	if d.Summarize != nil {
		if d.Summarize.Model == "" {
			d.Summarize.Model = defaultSummarizeModel
		}
		if d.Summarize.APIKeyEnv == "" {
			d.Summarize.APIKeyEnv = defaultSummarizeAPIKeyEnv
		}
		if d.Summarize.Timeout == 0 {
			d.Summarize.Timeout = defaultSummarizeTimeout
		}
	}
}

func (d *Daemon) validate() error {
	if d.Remote == "" {
		return fmt.Errorf("remote: must not be empty")
	}
	if d.Poll <= 0 {
		return fmt.Errorf("poll-interval: must be positive, got %s", d.Poll)
	}
	if d.LogRetention <= 0 {
		return fmt.Errorf("log-retention: must be positive, got %s", d.LogRetention)
	}
	if d.Committer.Name == "" {
		return fmt.Errorf("committer: name must not be empty")
	}
	if d.Committer.Email == "" {
		return fmt.Errorf("committer: email must not be empty")
	}
	if _, err := template.New("merge-message").Parse(d.MergeMsg); err != nil {
		return fmt.Errorf("merge-message: %w", err)
	}
	if len(d.Targets) == 0 {
		return fmt.Errorf("no target defined")
	}
	seen := make(map[string]bool, len(d.Targets))
	seenBranch := make(map[string]string, len(d.Targets)) // branch -> owning target name
	for _, t := range d.Targets {
		if t.Name == "" {
			return fmt.Errorf("target: name must not be empty")
		}
		if strings.Contains(t.Name, "/") {
			return fmt.Errorf("target %q: name must not contain '/'", t.Name)
		}
		if t.Branch == "" {
			return fmt.Errorf("target %q: branch missing", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("target %q: duplicate", t.Name)
		}
		seen[t.Name] = true
		// Two targets on the same branch would contend via CAS (phase-1
		// review finding O2): reject at config load instead.
		if owner, ok := seenBranch[t.Branch]; ok {
			return fmt.Errorf("target %q: branch %q already used by target %q", t.Name, t.Branch, owner)
		}
		seenBranch[t.Branch] = t.Name

		seenHook := make(map[string]bool, len(t.Hooks))
		for _, h := range t.Hooks {
			if h.Name == "" {
				return fmt.Errorf("target %q: hook: name must not be empty", t.Name)
			}
			if len(h.Command) == 0 {
				return fmt.Errorf("target %q: hook %q: command must not be empty", t.Name, h.Name)
			}
			if seenHook[h.Name] {
				return fmt.Errorf("target %q: hook %q: duplicate", t.Name, h.Name)
			}
			seenHook[h.Name] = true
		}
	}

	if d.History.Path != "" && d.History.SampleEvery <= 0 {
		return fmt.Errorf("history: sample-every must be positive, got %s", d.History.SampleEvery)
	}
	if d.History.Path != "" && d.History.DepthRetention <= 0 {
		return fmt.Errorf("history: depth-retention must be positive, got %s", d.History.DepthRetention)
	}

	if d.GitHub.Repo != "" {
		owner, name, ok := strings.Cut(d.GitHub.Repo, "/")
		if !ok || owner == "" || name == "" {
			return fmt.Errorf("github: repo must be in \"owner/name\" form, got %q", d.GitHub.Repo)
		}
	}

	switch d.Executor.Kind {
	case "local":
		// no further requirements
	case "container":
		if d.Executor.Image == "" {
			return fmt.Errorf("executor: image must not be empty for kind \"container\"")
		}
	default:
		return fmt.Errorf("executor: kind must be \"local\" or \"container\", got %q", d.Executor.Kind)
	}
	for _, c := range d.Executor.Caches {
		if c.Name == "" {
			return fmt.Errorf("executor: cache: name must not be empty")
		}
		if c.Path == "" {
			return fmt.Errorf("executor: cache %q: path must not be empty", c.Name)
		}
	}

	if d.Summarize != nil {
		if d.Summarize.Model == "" {
			return fmt.Errorf("summarize: model must not be empty")
		}
		if d.Summarize.APIKeyEnv == "" {
			return fmt.Errorf("summarize: api-key-env must not be empty")
		}
		if d.Summarize.Timeout <= 0 {
			return fmt.Errorf("summarize: timeout must be positive, got %s", d.Summarize.Timeout)
		}
	}

	return nil
}
