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
	"path/filepath"
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

	// defaultHooksPolicy is applied to a target's hooks-policy whenever
	// the target has at least one hook and left hooks-policy unset (see
	// Target.HooksPolicy and applyDefaults) — "queue" reproduces the
	// pre-policy behavior (internal/hooks, hooks v2's decision ledger):
	// every landing's hooks run, in order, none ever dropped.
	defaultHooksPolicy = "queue"

	// defaultSummarizeModel matches internal/summarize.DefaultModel
	// (duplicated here, not imported, per this file's existing pattern of
	// owning its own defaults — see defaultGitHubTokenEnv et al.):
	// Sonnet-class, per the operator decision to move the default off
	// Haiku now that Effort (below) makes the intelligence/cost tradeoff
	// configurable; prompt quality for this task was validated live
	// against claude-sonnet-5.
	defaultSummarizeModel     = "claude-sonnet-5"
	defaultSummarizeAPIKeyEnv = "ANTHROPIC_API_KEY"

	// defaultSummarizeEffort is applied whenever the "summarize" section
	// is present and effort is left unset — "medium" balances quality
	// against the per-call cost/latency this synchronous call adds to
	// every clean trial (see defaultSummarizeTimeout below). Once
	// defaulted, Summarize.Effort is never "" for a loaded config — see
	// validSummarizeEfforts and validate()'s check below.
	defaultSummarizeEffort = "medium"

	// defaultSummarizeTimeout bounds the one Messages API call
	// Config.MergeBody makes, synchronously, on the single-threaded
	// reconcile loop, before that trial's checks even start (closing-review
	// FIX 2) — every target's reconciliation stalls behind it. Kept well
	// under defaultPoll so a slow-but-not-hung summarizer call never eats
	// a whole poll interval; see the Summarize.Timeout field doc and
	// README.md's Summaries section for the full contract.
	defaultSummarizeTimeout = 5 * time.Second

	// defaultMaxBatch, defaultWindow, and defaultOnBatchRed are the
	// phase-5 per-target queue-mode defaults (docs/plans/phase5.md §4.1),
	// applied only for their own mode (batch/batch/batch respectively;
	// Window/speculate below), same "only default within the relevant
	// section" pattern as defaultHooksPolicy above.
	defaultMaxBatch    = 8
	defaultOnBatchRed  = "serial"
	defaultWindow      = 4
	maxAllowedMaxBatch = 64 // sane safety valve, not mandated by phase5.md §4 — see MaxBatch's field doc
	maxAllowedWindow   = 32 // sane safety valve, not mandated by phase5.md §4 — see Window's field doc

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

	// defaultReadyTimeout and defaultIdleTTL are Service's per-field
	// defaults (docs/plans/services-impl.md §2.4), applied by
	// checks.go's applyServiceDefaults before a Service is ever hashed via
	// servicekey.go's ServiceKey — see that function's doc for why the
	// ordering matters. defaultMaxInstances is the Services daemon block's
	// default hard count cap (§2.3), applied only when the block is enabled
	// (len(Services.Allow) > 0).
	defaultReadyTimeout = 60 * time.Second
	defaultIdleTTL      = 30 * time.Minute
	defaultMaxInstances = 8

	// defaultServicesRuntime is the daemon services block's default
	// container runtime (docs/plans/services-impl.md §Amendments A3),
	// applied only when the executor is "local" — see Services.Runtime's
	// field doc for why "container" executor kind is never defaulted here.
	defaultServicesRuntime = "docker"
)

// validSummarizeEfforts are the legal Summarize.Effort values, per the
// claude-api skill's output_config.effort reference: "low"/"medium"/"high"
// are broadly supported, "xhigh" and "max" only on newer Sonnet/Opus-tier
// models (which includes defaultSummarizeModel). validate() checks against
// this set; "" is impossible for a loaded config because applyDefaults
// always fills it in first when the "summarize" section is present.
// validHooksPolicies are the legal Target.HooksPolicy values
// (internal/hooks.Policy, duplicated here per this file's existing pattern
// of owning its own defaults/valid-sets — see validSummarizeEfforts).
// validate() rejects any other value for a target that has hooks.
var validHooksPolicies = map[string]bool{
	"queue":    true,
	"coalesce": true,
	"cancel":   true,
}

var validSummarizeEfforts = map[string]bool{
	// "none" omits the effort field from API requests entirely — the escape
	// hatch for models that reject output_config.effort (e.g. claude-haiku-4-5).
	"none":   true,
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
}

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

	// AutoRetryErrors gates the phase-B auto-retry-once amendment (DESIGN.md
	// decision ledger, "Auto-retry once on infra-error parks";
	// docs/plans/scale.md §5): an OutcomeError park — executor unreachable,
	// service-ensure failure, a service dying mid-run; never a red verdict,
	// never a trial conflict — is automatically cleared and re-queued
	// exactly once per (ref, SHA), through the same machinery an operator's
	// Slack :recycle:/API/CLI retry uses (internal/queue's
	// maybeAutoRetry/clearParkAndRetry). Defaults to true (applyDefaults
	// below).
	//
	// A *bool, not a plain bool, for the same reason as Summarize's
	// pointer-ness (see that field's doc): kdl-go leaves an absent bool
	// property at its zero value ("false"), indistinguishable from an
	// operator explicitly writing `auto-retry-errors false`. Only a pointer
	// lets applyDefaults tell "never written" (nil, default to true) apart
	// from "written false" (non-nil, respected) without stomping the
	// latter.
	AutoRetryErrors *bool `kdl:"auto-retry-errors"`

	History   History   `kdl:"history"`   // Path=="" ⇒ disabled
	Dashboard Dashboard `kdl:"dashboard"` // Bind=="" ⇒ disabled
	GitHub    GitHub    `kdl:"github"`    // Repo=="" ⇒ disabled
	Slack     Slack     `kdl:"slack"`     // Channel=="" ⇒ disabled
	OTLP      OTLP      `kdl:"otlp"`      // Endpoint=="" ⇒ no-op (phase-1 default)
	Executor  Executor  `kdl:"executor"`  // Kind=="" ⇒ "local"
	Services  Services  `kdl:"services"`  // len(Allow)==0 ⇒ disabled

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

	// AllowedUsers optionally restricts who may issue reaction commands
	// (:recycle: retry, :x: cancel) to these Slack member IDs ("U…"/"W…"):
	//
	//	allowed-users "U025FTHN3" "U0987ZYXWV"
	//
	// Empty (the default) keeps the open behavior: anyone who can react in
	// the channel commands the queue. Only inbound command minting is
	// gated; outbound posting is unaffected.
	AllowedUsers []string `kdl:"allowed-users"`
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
	Mounts  []Mount `kdl:"mount,multiple"`
}

// Cache is one persistent named cache volume mounted into the container
// executor.
type Cache struct {
	Name string `kdl:",arg"`
	Path string `kdl:"path"`
}

// reservedResultDir is the fixed in-container path the writable result-dir
// mount is bound to — keep in sync with
// internal/executor/container.go's containerResultDir. Duplicated rather
// than imported: this package deliberately doesn't depend on
// internal/executor (see Params.Caches's doc there for the reverse of this
// same boundary).
const reservedResultDir = "/gauntlet"

// Mount is one host bind mount into the container executor, alongside the
// trial tree and the named cache volumes above (DESIGN.md decision ledger,
// "Generic container mounts"). The motivating case is the host docker
// socket, for repos whose checks run testcontainers — but this is a plain,
// unopinionated bind mount, not a docker-socket-specific knob: whatever else
// a repo's checks need visible from the host filesystem goes here too.
//
// Both Host and Path are required, must be absolute, and must not contain
// ':' (validate() below — the container runtime's "-v host:path[:ro]" argv
// syntax has no escape for it). Path may not be at or under the executor's
// own Workdir or reservedResultDir, the fixed in-container result-dir mount
// (internal/executor/container.go's containerResultDir) — both are the
// executor's own contract with every check and must not be silently
// shadowed, even partially, by an operator-configured mount.
//
// README's "Container executor" section spells out the trust implication of
// mounting the docker socket specifically: it hands every check — i.e.
// anyone who can push a for/ ref — full control of the host docker daemon,
// which is root-equivalent on most setups. That's an operator choice this
// type merely enables; it isn't gated here.
type Mount struct {
	Host     string `kdl:",arg"`     // absolute host path (file or dir)
	Path     string `kdl:"path"`     // absolute in-container path
	ReadOnly bool   `kdl:"readonly"` // default false (read-write)
}

// Services gates whether repo-declared services (config.Service, checks.go)
// may run on this box (docs/plans/services.md §7). Absent ⇒ Allow nil ⇒ a
// check spec declaring service/needs is REJECTED at run time (loud, like a
// malformed check — CheckSpec.RequiresServices() is how the queue detects
// this). Presence is signalled by len(Allow) > 0, same pattern as
// GitHub.Repo/Slack.Channel/etc.
type Services struct {
	// Allow lists the driver kinds permitted on this box; phase A supports
	// only "container" — validate() rejects "artifact"/"oci-unpack" as
	// reserved for a future release (mirroring Target.OnBatchRed's
	// "bisect" handling above), rather than silently ignoring them.
	Allow []string `kdl:"allow"`

	// MaxInstances hard-caps the number of live service instances this
	// daemon's pool will create (docs/plans/services.md §7 "Resource
	// honesty": count only, not memory/CPU). Defaults to
	// defaultMaxInstances (8) when the block is enabled and this is left
	// zero.
	MaxInstances int `kdl:"max-instances"`

	// Runtime is the container runtime the services pool uses to create
	// instances: "docker" or "podman" in phase A (docs/plans/services-impl.md
	// §Amendments A3). This field exists because "executor local" has no
	// runtime of its own, and local-executor-plus-containerized-services is
	// a first-class deployment shape — but it is ONLY CONSULTED when the
	// executor kind is "local" (cmd/gauntlet wires this at boot, chunk 3 of
	// docs/plans/services-impl.md). When the executor kind is "container",
	// the executor's own Runtime wins instead (it must already be
	// docker/podman — Apple `container` networking is a hard-fail there),
	// and applyDefaults deliberately leaves this field undefaulted in that
	// case so validate() can tell "operator wrote a conflicting value" from
	// "operator never wrote one" — see validate()'s cross-check.
	Runtime string `kdl:"runtime"`
}

// Summarize configures the optional Claude-written merge-commit body
// enricher (internal/summarize). A nil *Daemon.Summarize (the node absent
// from the document) disables it entirely; see the field doc on Daemon for
// why presence, not any single field's non-emptiness, is the enable
// signal.
//
// The summary is generated synchronously, on the reconcile loop, right
// before a trial's merge commit is built (queue/reconcile.go): the merge
// commit must carry it, and landing the already-tested SHA forbids amending
// the commit later to attach one after the fact. It fires on every clean
// trial, not just landings that go on to succeed — a trial rejected by a
// later check still paid for one summarize call. See Timeout below and
// README.md's Summaries section for the full stall contract.
type Summarize struct {
	Model     string `kdl:"model"`       // default defaultSummarizeModel
	APIKeyEnv string `kdl:"api-key-env"` // default "ANTHROPIC_API_KEY"

	// Effort is the output_config.effort value sent with every summarize
	// call (see internal/summarize.Params.Effort) — "low", "medium",
	// "high", "xhigh", or "max". Defaults to "medium" whenever the
	// "summarize" section is present; validate() rejects any other
	// value, so a loaded config's Effort is never "".
	Effort string `kdl:"effort"`

	// Timeout bounds the single Messages API call per trial (default 5s).
	// Because that call runs synchronously on gauntlet's single-threaded
	// reconcile loop, before checks start, this timeout bounds a stall of
	// the ENTIRE loop — every target, not just the one being summarized —
	// for up to its duration on every clean trial. Keep it well under
	// poll-interval.
	Timeout time.Duration `kdl:"timeout,format:units"`
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

	// HooksPolicy controls what happens to this target's hook backlog
	// when landings outpace hook execution (e.g. deploys slower than
	// merges) — internal/hooks.Policy: "queue" (default; every landing's
	// hooks run, none dropped), "coalesce" (a newer landing queued behind
	// an older one drops the older; what's already running finishes
	// undisturbed), or "cancel" (coalesce, plus the currently running
	// landing's hooks are cancelled mid-hook for the newer one). Only
	// meaningful when Hooks is non-empty — applyDefaults only defaults it
	// (to "queue") in that case, and validate() rejects it being set on a
	// target with no hooks at all, since there is no backlog to have a
	// policy about.
	HooksPolicy string `kdl:"hooks-policy"`

	// Mode selects this target's queueing discipline (docs/plans/phase5.md
	// §0, §4): "serial" (default) tests and lands one candidate at a time,
	// byte-for-byte the phase-1 behavior; "batch" merges up to MaxBatch
	// queued candidates into one --no-ff chain and runs a single check
	// suite over the combined tree; "speculate" pipelines up to Window
	// runs, each testing its own candidate chained onto the predicted
	// (not-yet-landed) tip of the run ahead of it.
	//
	// "" is the zero value and means "serial" everywhere Mode is read.
	// Unlike HooksPolicy, applyDefaults deliberately does NOT normalize ""
	// to "serial" here: a target with no mode configured must keep
	// reporting Mode=="" so a zero Target{} (and internal/queue's
	// lane-per-target refactor, phase5.md P5-C) sees serial without any
	// special-casing — "the fields' zero value already means serial".
	Mode string `kdl:"mode"` // serial (default) | batch | speculate

	// MaxBatch caps how many queued candidates one batch run combines
	// into a single --no-ff chain and check suite (docs/plans/phase5.md
	// §4.1). Legal only when Mode=="batch" — validate() rejects it being
	// set for any other mode. Defaults to 8 when Mode=="batch" and left
	// unset (zero).
	//
	// Bounded to [1, maxAllowedMaxBatch] (64) — a cap chosen here, not
	// mandated by the plan: each additional batch member costs one more
	// synchronous Config.MergeBody/summarize call on the reconcile loop
	// before checks even start (docs/plans/phase5.md §9's documented
	// stall cost), so an unbounded max-batch could stall the whole daemon
	// for many multiples of summarize.timeout on a single refill.
	MaxBatch int `kdl:"max-batch"`

	// Window is the speculation pipeline depth: up to this many runs are
	// in flight at once for the target (docs/plans/phase5.md §4.1, §4.2 —
	// a fixed v1 window; see WindowStart/WindowMax/WindowHalveOnRed below
	// for the reserved adaptive-governor knobs). Legal only when
	// Mode=="speculate"; defaults to 4 when left unset (zero).
	//
	// Bounded to [1, maxAllowedWindow] (32) — a cap chosen here, not
	// mandated by the plan. Each speculative run executes at most one
	// check at a time, so Window is ALSO the maximum number of concurrent
	// check processes/containers this target drives against the build
	// executor (docs/plans/phase5.md §10 amendment 4): an operator sizing
	// window is sizing builder concurrency, not just queue depth.
	Window int `kdl:"window"`

	// OnBatchRed selects the batch red-recovery strategy
	// (docs/plans/phase5.md §2.6): "serial" (default) re-queues every
	// batch member unparked and re-forms them one at a time on the next
	// refill until the culprit is found and parked, then batching
	// resumes; "bisect" is a documented growth path only. Legal only
	// when Mode=="batch".
	//
	// "bisect" is accepted here so config stays forward-compatible, but
	// phase 5 does NOT implement it: LoadDaemon rejects a target that
	// sets on-batch-red "bisect" with a "reserved for a future release"
	// error, rather than silently running it as "serial".
	OnBatchRed string `kdl:"on-batch-red"` // serial (default) | bisect (reserved, rejected)

	// WindowStart, WindowMax, and WindowHalveOnRed reserve the config
	// surface for a future adaptive speculation-window governor
	// (Zuul-style: start small, grow on green, halve on red;
	// docs/plans/phase5.md §4.2). Phase 5 implements only the fixed
	// Window above; setting any of these three is rejected at load with
	// a "reserved for a future release" error (same rationale as
	// OnBatchRed's "bisect") so a config that names them fails loudly
	// instead of silently no-opping.
	WindowStart      int  `kdl:"window-start"`
	WindowMax        int  `kdl:"window-max"`
	WindowHalveOnRed bool `kdl:"window-halve-on-red"`
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
	// AutoRetryErrors defaults unconditionally to true (see field doc):
	// like LogRetention above, there is no "section absent -> disabled"
	// state to preserve — the knob's only job is letting an operator opt
	// OUT of a behavior that's on by default.
	if d.AutoRetryErrors == nil {
		v := true
		d.AutoRetryErrors = &v
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

	// Services: only defaulted when the block is enabled (len(Allow) > 0),
	// same "required field non-empty is the enable signal" pattern as
	// GitHub/Slack/etc above.
	if len(d.Services.Allow) > 0 {
		if d.Services.MaxInstances == 0 {
			d.Services.MaxInstances = defaultMaxInstances
		}
		// Runtime is only defaulted under executor "local" (A3): under
		// "container", the executor's own Runtime wins at cmd-wiring time,
		// and defaulting this field here would manufacture a false
		// "conflicting services.runtime" in validate() below whenever an
		// operator sets executor.runtime to "podman" without ever writing
		// services.runtime themselves. This must run after Executor.Kind is
		// resolved above.
		if d.Executor.Kind == "local" && d.Services.Runtime == "" {
			d.Services.Runtime = defaultServicesRuntime
		}
	}

	// HooksPolicy only defaults (to "queue") for a target that actually
	// has hooks — same "required field non-empty is the enable signal"
	// pattern as GitHub/Slack/etc above. A target with no hooks keeps
	// HooksPolicy exactly as parsed ("" if absent), so validate() can
	// still tell "the node was never written" from "queue was written
	// explicitly" and reject the former when hooks are also absent.
	for i := range d.Targets {
		if len(d.Targets[i].Hooks) > 0 && d.Targets[i].HooksPolicy == "" {
			d.Targets[i].HooksPolicy = defaultHooksPolicy
		}

		// Mode's own knobs default only within their own mode — same
		// "required field non-empty is the enable signal" pattern as
		// HooksPolicy above and the phase-2/3 sections in Daemon. Mode
		// itself is never defaulted (see its field doc): "" stays "".
		switch d.Targets[i].Mode {
		case "batch":
			if d.Targets[i].MaxBatch == 0 {
				d.Targets[i].MaxBatch = defaultMaxBatch
			}
			if d.Targets[i].OnBatchRed == "" {
				d.Targets[i].OnBatchRed = defaultOnBatchRed
			}
		case "speculate":
			if d.Targets[i].Window == 0 {
				d.Targets[i].Window = defaultWindow
			}
		}
	}

	if d.Summarize != nil {
		if d.Summarize.Model == "" {
			d.Summarize.Model = defaultSummarizeModel
		}
		if d.Summarize.APIKeyEnv == "" {
			d.Summarize.APIKeyEnv = defaultSummarizeAPIKeyEnv
		}
		if d.Summarize.Effort == "" {
			d.Summarize.Effort = defaultSummarizeEffort
		}
		if d.Summarize.Timeout == 0 {
			d.Summarize.Timeout = defaultSummarizeTimeout
		}
	}
}

// pathAtOrUnder reports whether path is exactly reserved or a path
// underneath it, after cleaning both operands (so a trailing slash, a
// repeated separator, or a "/." spelling of the same path can't dodge the
// comparison). An empty reserved never matches — Executor.Workdir is only
// ever "" for kind "local", where mounts have no effect at all (see
// cmd/gauntlet's buildExecutor), and a "" root would otherwise wrongly
// match every absolute path via the separator-prefix check below.
func pathAtOrUnder(path, reserved string) bool {
	if reserved == "" {
		return false
	}
	path = filepath.Clean(path)
	reserved = filepath.Clean(reserved)
	return path == reserved || strings.HasPrefix(path, reserved+string(filepath.Separator))
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

		// hooks-policy is only meaningful alongside at least one hook —
		// see HooksPolicy's field doc and applyDefaults above, which
		// leaves it "" (never defaulted) for a target with no hooks so
		// this can tell "never written" from "written explicitly".
		if len(t.Hooks) == 0 {
			if t.HooksPolicy != "" {
				return fmt.Errorf("target %q: hooks-policy set without any hooks", t.Name)
			}
		} else if !validHooksPolicies[t.HooksPolicy] {
			return fmt.Errorf("target %q: hooks-policy must be one of queue, coalesce, cancel, got %q", t.Name, t.HooksPolicy)
		}

		// Mode + its mode-scoped knobs (docs/plans/phase5.md §4.1): each
		// knob is legal only under its own mode, matching hooks-policy's
		// "catch config mistakes" strictness above.
		switch t.Mode {
		case "", "serial":
			if t.MaxBatch != 0 {
				return fmt.Errorf("target %q: max-batch requires mode \"batch\"", t.Name)
			}
			if t.Window != 0 {
				return fmt.Errorf("target %q: window requires mode \"speculate\"", t.Name)
			}
			if t.OnBatchRed != "" {
				return fmt.Errorf("target %q: on-batch-red requires mode \"batch\"", t.Name)
			}
		case "batch":
			if t.Window != 0 {
				return fmt.Errorf("target %q: window is only valid for mode \"speculate\", not \"batch\"", t.Name)
			}
			if t.MaxBatch < 1 || t.MaxBatch > maxAllowedMaxBatch {
				return fmt.Errorf("target %q: max-batch must be between 1 and %d, got %d", t.Name, maxAllowedMaxBatch, t.MaxBatch)
			}
			switch t.OnBatchRed {
			case "serial":
				// v1-implemented; see field doc.
			case "bisect":
				// Reserved growth path (docs/plans/phase5.md §2.6, §9
				// non-goals): validated but rejected at load rather than
				// silently running as "serial".
				return fmt.Errorf("target %q: on-batch-red \"bisect\" is reserved for a future release, not implemented in phase 5", t.Name)
			default:
				return fmt.Errorf("target %q: on-batch-red must be \"serial\" or \"bisect\", got %q", t.Name, t.OnBatchRed)
			}
		case "speculate":
			if t.MaxBatch != 0 {
				return fmt.Errorf("target %q: max-batch is only valid for mode \"batch\", not \"speculate\"", t.Name)
			}
			if t.OnBatchRed != "" {
				return fmt.Errorf("target %q: on-batch-red is only valid for mode \"batch\", not \"speculate\"", t.Name)
			}
			if t.Window < 1 || t.Window > maxAllowedWindow {
				return fmt.Errorf("target %q: window must be between 1 and %d, got %d", t.Name, maxAllowedWindow, t.Window)
			}
		default:
			return fmt.Errorf("target %q: mode must be one of \"\", \"serial\", \"batch\", \"speculate\", got %q", t.Name, t.Mode)
		}

		// Reserved adaptive-window-governor knobs (docs/plans/phase5.md
		// §4.2): parsed for forward compatibility, always rejected in
		// phase 5, regardless of Mode.
		if t.WindowStart != 0 {
			return fmt.Errorf("target %q: window-start is reserved for a future adaptive-window governor, not implemented in phase 5", t.Name)
		}
		if t.WindowMax != 0 {
			return fmt.Errorf("target %q: window-max is reserved for a future adaptive-window governor, not implemented in phase 5", t.Name)
		}
		if t.WindowHalveOnRed {
			return fmt.Errorf("target %q: window-halve-on-red is reserved for a future adaptive-window governor, not implemented in phase 5", t.Name)
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
	for _, m := range d.Executor.Mounts {
		if m.Host == "" {
			return fmt.Errorf("executor: mount: host must not be empty")
		}
		if !filepath.IsAbs(m.Host) {
			return fmt.Errorf("executor: mount %q: host must be an absolute path", m.Host)
		}
		if m.Path == "" {
			return fmt.Errorf("executor: mount %q: path must not be empty", m.Host)
		}
		if !filepath.IsAbs(m.Path) {
			return fmt.Errorf("executor: mount %q: path must be an absolute path, got %q", m.Host, m.Path)
		}
		// ':' in either half silently corrupts the container runtime's
		// "-v host:path[:ro]" argv syntax (internal/executor/container.go's
		// runArgs just concatenates with ":") — the worst failure shape,
		// since it misparses rather than erroring. Absolute Linux paths
		// essentially never legitimately contain one.
		if strings.Contains(m.Host, ":") {
			return fmt.Errorf("executor: mount %q: host must not contain ':'", m.Host)
		}
		if strings.Contains(m.Path, ":") {
			return fmt.Errorf("executor: mount %q: path must not contain ':', got %q", m.Host, m.Path)
		}
		// Reserved in-container paths are the executor's own contract with
		// every check (the trial tree at Workdir, the result dir at
		// reservedResultDir — keep in sync with
		// internal/executor/container.go's containerResultDir) and must
		// never be silently shadowed by an operator mount, including by a
		// mount at some path UNDER one of them: a nested bind is legal to
		// docker/podman/container and would partially, silently shadow the
		// trial tree or result dir exactly as much as an exact-path
		// collision would. pathAtOrUnder compares cleaned paths so a
		// trailing slash, "//", or "/." spelling of the same path can't
		// bypass the guard.
		if pathAtOrUnder(m.Path, d.Executor.Workdir) {
			return fmt.Errorf("executor: mount %q: path %q is at or under executor workdir %q", m.Host, m.Path, d.Executor.Workdir)
		}
		if pathAtOrUnder(m.Path, reservedResultDir) {
			return fmt.Errorf("executor: mount %q: path %q is at or under the reserved result-dir mount %q", m.Host, m.Path, reservedResultDir)
		}
	}

	if len(d.Services.Allow) > 0 {
		for _, a := range d.Services.Allow {
			switch a {
			case "container":
				// v1-implemented; see field doc.
			case "artifact", "oci-unpack":
				// Reserved growth path (docs/plans/services.md §6):
				// validated but rejected at load rather than silently
				// no-opping, same "reserved for a future release"
				// treatment as Target.OnBatchRed's "bisect" above.
				return fmt.Errorf("services: allow %q is reserved for a future release, not implemented in phase A", a)
			default:
				return fmt.Errorf("services: allow must be \"container\", got %q", a)
			}
		}
		if d.Services.MaxInstances < 1 {
			return fmt.Errorf("services: max-instances must be at least 1, got %d", d.Services.MaxInstances)
		}
		// Runtime cross-check (A3): under "container", the executor's own
		// runtime wins, so services.runtime is legal only if it agrees with
		// it (or was never written — applyDefaults leaves it "" in that
		// case). Under "local" (and any other executor kind, defensively),
		// applyDefaults has already filled it in, so only docker/podman
		// ever reach here.
		if d.Executor.Kind == "container" {
			if d.Services.Runtime != "" && d.Services.Runtime != d.Executor.Runtime {
				return fmt.Errorf("services: runtime %q conflicts with executor runtime %q", d.Services.Runtime, d.Executor.Runtime)
			}
		} else if d.Services.Runtime != "docker" && d.Services.Runtime != "podman" {
			return fmt.Errorf("services: runtime must be \"docker\" or \"podman\", got %q", d.Services.Runtime)
		}
	}

	if d.Summarize != nil {
		if d.Summarize.Model == "" {
			return fmt.Errorf("summarize: model must not be empty")
		}
		if d.Summarize.APIKeyEnv == "" {
			return fmt.Errorf("summarize: api-key-env must not be empty")
		}
		if !validSummarizeEfforts[d.Summarize.Effort] {
			return fmt.Errorf("summarize: effort must be one of low, medium, high, xhigh, max, got %q", d.Summarize.Effort)
		}
		if d.Summarize.Timeout <= 0 {
			return fmt.Errorf("summarize: timeout must be positive, got %s", d.Summarize.Timeout)
		}
	}

	return nil
}
