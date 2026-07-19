// Package config parses gauntlet's two KDL config files into plain structs:
// the admin-written daemon config and the repo-side check spec. This is the
// only package that touches KDL, so the config language stays swappable and
// callers depend on the structs and LoadDaemon/ParseChecks signatures, never
// on kdl-go directly.
//
// kdl-go's unmarshaler has thin validation (no required-field or
// non-negative-value enforcement), so every exported load function here runs
// a Go-side validation pass afterward; its errors name the offending
// node/field.
package config

import (
	"fmt"
	"net/url"
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

// Defaults for the optional sections below; applied only when the section
// is enabled (its required key non-empty) and the defaulted field is unset.
const (
	defaultGitHubTokenEnv    = "GITHUB_TOKEN"
	defaultGitHubAPIURL      = "https://api.github.com"
	defaultTrialRefPrefix    = "refs/gauntlet/trials"
	defaultTrialRefRetention = 24 * time.Hour

	// defaultReceiptNotesRef and defaultReceiptNotesMaxBytes are
	// GitHub.ReceiptNotes's per-field defaults, applied only when the
	// `receipt-notes` block is present (see that field's doc) and the
	// field is left unset. maxAllowedReceiptBytes is the hard ceiling
	// max-bytes may not exceed even when explicitly set — see
	// ReceiptNotes's doc for why this one caps rather than just defaults.
	defaultReceiptNotesRef      = "refs/notes/gauntlet/receipts"
	defaultReceiptNotesMaxBytes = 65536
	maxAllowedReceiptBytes      = 1 << 20 // 1 MiB
	defaultShutdown             = "drain"
	defaultSlackAppEnv          = "SLACK_APP_TOKEN"
	defaultSlackBotEnv          = "SLACK_BOT_TOKEN"
	defaultExecutorKind         = "local"
	defaultRuntime              = "container"
	defaultWorkdir              = "/workspace"

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
	// per-target queue-mode defaults, applied only for their own mode
	// (batch/batch/batch respectively; Window/speculate below), same
	// "only default within the relevant section" pattern as
	// defaultHooksPolicy above.
	defaultMaxBatch    = 8
	defaultOnBatchRed  = "serial"
	defaultWindow      = 4
	maxAllowedMaxBatch = 64 // sane safety valve, not a hard requirement — see MaxBatch's field doc
	maxAllowedWindow   = 32 // sane safety valve, not a hard requirement — see Window's field doc

	// defaultDepthRetention is how long queue_depth samples are kept before
	// the depth sampler prunes them. Runs/checks have no retention bound —
	// this applies to the depth series only.
	defaultDepthRetention = 14 * 24 * time.Hour

	// defaultLogRetention is how long full per-check log directories
	// (Config.LogDir/<runID>/, DESIGN.md "Full per-check log files") are
	// kept before cmd/gauntlet's prune sweep deletes them — 30 days.
	// Unlike depth-retention, this applies unconditionally: cmd/gauntlet
	// always wires Config.LogDir, so this default always matters, not only
	// when some optional section is enabled.
	defaultLogRetention = 30 * 24 * time.Hour

	// defaultReadyTimeout and defaultIdleTTL are Service's per-field
	// defaults, applied by checks.go's applyServiceDefaults before a
	// Service is ever hashed via servicekey.go's ServiceKey — see that
	// function's doc for why the ordering matters. defaultMaxInstances is
	// the Services daemon block's default hard count cap, applied only
	// when the block is enabled (len(Services.Allow) > 0).
	defaultReadyTimeout = 60 * time.Second
	defaultIdleTTL      = 30 * time.Minute
	defaultMaxInstances = 8

	// defaultServicesRuntime is the daemon services block's default
	// container runtime, applied only when the executor is "local" — see
	// Services.Runtime's field doc for why "container" executor kind is
	// never defaulted here.
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

// Daemon is the admin-written daemon config: one remote, the reconcile
// cadence, the committer identity used for merge commits, the merge-message
// template, and the target branches to reconcile. The optional sections
// below are all optional value structs (not pointers): kdl-go leaves a
// struct-typed field
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

	// AutoRetryErrors gates the auto-retry-once behavior (DESIGN.md decision
	// ledger, "Auto-retry once on infra-error parks"): an OutcomeError
	// park — executor unreachable, service-ensure failure, a service dying
	// mid-run; never a red verdict,
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

	// Shutdown selects the first-SIGTERM behavior (issue #8): "drain"
	// (the default) stops admitting new candidates and lets the in-flight
	// set finish before exiting — a second signal forces the immediate
	// kill; "kill" restores the legacy behavior where the first signal
	// cancels everything at once. Empty defaults to "drain".
	Shutdown string `kdl:"shutdown"`

	History   History   `kdl:"history"`   // Path=="" ⇒ disabled
	Dashboard Dashboard `kdl:"dashboard"` // Bind=="" ⇒ disabled
	GitHub    GitHub    `kdl:"github"`    // Repo=="" ⇒ disabled
	Slack     Slack     `kdl:"slack"`     // Channel=="" ⇒ disabled
	OTLP      OTLP      `kdl:"otlp"`      // Endpoint=="" ⇒ no-op (the default)
	Services  Services  `kdl:"services"`  // len(Allow)==0 ⇒ disabled

	// Executors is the raw parse target for every `executor` node —
	// applyDefaults splits it into Executor (the default profile: the one
	// kind-less legacy block, or an implicit local executor when none is
	// written) and Profiles (each `executor "name" kind="..."` block).
	// Consumers read Executor/Profiles, never this.
	Executors []Executor `kdl:"executor,multiple"`

	// Executor is the resolved DEFAULT profile: what checks that name no
	// `executor` in the repo spec run on, and what post-land hooks run on.
	// Exactly the pre-profiles field, so a hand-assembled Daemon (tests)
	// and every existing consumer keep working unchanged.
	Executor Executor `kdl:"-"`

	// Profiles is the resolved named executor profiles (the repo spec's
	// `executor "name"` vocabulary, issue #3). Defining a profile is what
	// allows a repo to select it; there is no separate allow-list.
	// Selecting one grants the check every capability attached to it
	// (mounts, env, sockets), so prefer several small profiles over one
	// all-powerful default — see docs/config.md.
	Profiles []Executor `kdl:"-"`

	// Export configures trial-tree materialization (every export: check
	// trees, image-build trees, hook trees). Zero value = defaults (no
	// deterministic-mtimes pass).
	Export Export `kdl:"export"`

	// MaxExecutions is the daemon-wide cap on concurrently executing
	// bounded commands — candidate checks and post-land hooks, across
	// every target, mode, speculation window, and executor profile. It
	// lives at the top level (not on any one executor block) because it is
	// a property of the host, not of a profile: one core.Slots budget
	// covers everything. Zero (unset) means unlimited — the compatibility
	// default; production deployments should set an explicit value sized
	// to the host. Long-lived shared service containers do not count;
	// their own instance limits apply.
	MaxExecutions int `kdl:"max-executions"`

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

// History configures the optional SQLite run-history store. Path=="" disables
// it.
type History struct {
	Path           string        `kdl:",arg"`
	SampleEvery    time.Duration `kdl:"sample-every,format:units"`    // default = Poll
	DepthRetention time.Duration `kdl:"depth-retention,format:units"` // default 14 days; queue_depth only
}

// Dashboard configures the optional read-only web dashboard. Bind=="" disables
// it.
type Dashboard struct {
	Bind string `kdl:",arg"` // "localhost:8080"; "" disables
	URL  string `kdl:"url"`  // optional public base URL for outbound links
}

// GitHub configures the optional GitHub integration. Repo=="" disables it.
// Two authentication modes, mutually exclusive:
//
//   - token-env (the default, "GITHUB_TOKEN"): a static PAT read from the
//     environment; it authenticates the commit-status channel only, and
//     git keeps whatever ambient auth (SSH, credential helper) the host
//     already has — today's behavior, unchanged.
//   - auth "app": GitHub App installation tokens (issue #6). The same
//     refreshable provider authenticates BOTH the status channel and git
//     fetch/push against the configured remote, which must be an HTTPS
//     URL canonicalizing to the same host and owner/repo as this block —
//     a mismatch is a startup error, never a silent fallback to ambient
//     auth.
type GitHub struct {
	Repo     string      `kdl:",arg"`      // "owner/name"
	TokenEnv string      `kdl:"token-env"` // default "GITHUB_TOKEN" (unless Auth set)
	APIURL   string      `kdl:"api-url"`   // default "https://api.github.com"
	Auth     *GitHubAuth `kdl:"auth"`      // nil ⇒ static-token mode

	// TrialRefPrefix, when non-empty, enables trial-ref publication
	// (issue #7): each run's tested synthetic merge is published under an
	// immutable ref TrialRefPrefix/<run-id> so GitHub can display and
	// status exactly the bytes that ran, and the commit status moves to
	// the MERGE SHA (verification), not the candidate SHA. Empty ⇒
	// disabled (today's candidate-SHA behavior). The default when the
	// `trial-refs` node is present but bare is refs/gauntlet/trials — a
	// CUSTOM namespace, deliberately NOT refs/heads/**, to avoid branch-UI
	// clutter and workflow triggers (a refs/heads/** prefix is allowed but
	// documented as trigger-prone).
	TrialRefPrefix string `kdl:"-"`

	// TrialRefRetention is how long a non-landing run's trial ref is kept
	// for diagnosis before the reaper deletes it (a landing deletes its
	// ref at once). Zero ⇒ delete on terminal. Meaningful only when
	// TrialRefPrefix is set.
	TrialRefRetention time.Duration `kdl:"-"`

	// TrialRefs is the raw parse target for the optional `trial-refs`
	// child node; resolveGitHubTrialRefs lifts it into TrialRefPrefix/
	// TrialRefRetention. A pointer so presence is unambiguous (a bare
	// `trial-refs` node with defaults still enables the feature), like
	// Daemon.Summarize.
	TrialRefs *GitHubTrialRefs `kdl:"trial-refs"`

	// ReceiptNotes is the `receipt-notes { ref ...; max-bytes ... }`
	// block (issue #13): its presence — not any one field's non-emptiness
	// — enables the daemon's commitment to publish every landing's
	// receipt (CheckSpec.Receipt's captured command result) as a git note
	// on the tested merge SHA before landing, for every target this
	// GitHub block covers. A pointer for the same presence-signalling
	// reason as TrialRefs/Summarize: a bare `receipt-notes {}` with
	// nothing set must still count as enabled. Absent ⇒ disabled, zero
	// behavior change.
	//
	// This config slice (issue #13's config-surface half) only exposes
	// and validates this field; a later slice wires actual note
	// publication. The load-time consequence THIS slice does implement is
	// queue.SpecRejectReason's receipt-policy gate, both directions: a
	// spec with no receipt is rejected when this is set, and a spec
	// declaring one is rejected when this is nil (see that function's
	// doc).
	ReceiptNotes *ReceiptNotes `kdl:"receipt-notes"`
}

// GitHubTrialRefs is the `trial-refs { prefix ...; retention ... }` block.
type GitHubTrialRefs struct {
	Prefix    string        `kdl:"prefix"`    // default "refs/gauntlet/trials"
	Retention time.Duration `kdl:"retention"` // default 24h (unset/zero both take the default)
}

// ReceiptNotes is the `receipt-notes { ref ...; max-bytes ... }` block —
// see GitHub.ReceiptNotes's doc for what its presence enables. Ref is the
// git-notes ref every receipt is published under; validate() holds it to
// the same ref-name grammar as GitHub.TrialRefPrefix (issue #7's
// trial-refs validation), plus an additional requirement specific to
// notes: it must not live under "refs/heads/" (a notes payload there
// would trigger branch machinery), though any other custom namespace —
// "refs/notes/..." is conventional, not required — is allowed, matching
// trial-refs' own stance on refs/heads/**. MaxBytes bounds one receipt's
// published size; receipts are receipts, not artifacts, so this is capped
// hard at maxAllowedReceiptBytes (1 MiB), not just operator-adjustable.
type ReceiptNotes struct {
	Ref string `kdl:"ref"` // default defaultReceiptNotesRef

	// MaxBytes bounds one receipt's published size (default
	// defaultReceiptNotesMaxBytes; hard ceiling maxAllowedReceiptBytes).
	// Zero is indistinguishable from "left unset" (kdl-go unmarshals a
	// missing node into the field's zero value, like Daemon.Poll
	// elsewhere in this package) and applyDefaults fills it in before
	// validate() ever runs — so only an explicit NEGATIVE value is
	// unambiguously invalid; validate()'s "must be positive" check is
	// consequently unreachable for exactly zero, by the same design as
	// CheckSpec.MaxParallel's zero-vs-absent handling.
	MaxBytes int `kdl:"max-bytes"`
}

// GitHubAuth is the `auth "app" { ... }` block: GitHub App installation
// authentication. A pointer field for the same presence-signalling reason
// as Daemon.Summarize — the block's presence itself selects the mode. The
// private key stays in a file (never inline in config, never an
// installation token); cmd validates and loads it at startup via
// ghauth.LoadPrivateKey.
type GitHubAuth struct {
	Mode           string `kdl:",arg"`             // must be "app"
	AppID          int64  `kdl:"app-id"`           // the App's numeric ID
	InstallationID int64  `kdl:"installation-id"`  // the installation on Repo
	PrivateKeyFile string `kdl:"private-key-file"` // PEM, PKCS#1 or PKCS#8
}

// Slack configures the optional Slack channel. Channel=="" disables it.
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

// OTLP configures the optional OTLP trace exporter. Endpoint=="" leaves
// tracing a no-op (the default).
type OTLP struct {
	Endpoint string `kdl:",arg"`
	Insecure bool   `kdl:"insecure"`
}

// Executor selects the check-execution backend. Kind=="" defaults to "local"
// (the in-process executor); "container" requires Image.
// Executor is one execution profile. Two spellings share the node:
//
//	executor "container" { ... }         // legacy/default: the arg is the KIND
//	executor "ci" kind="container" { ... } // named profile: the arg is the NAME
//
// The presence of the `kind` property is the discriminator — applyDefaults
// resolves Arg into Kind (legacy) or Name (profile) and never leaves the
// raw Arg for consumers. Profile names "local" and "container" are
// rejected outright so the two spellings can never be confused; "default"
// is rejected too, since gauntlet doctor prints the default (kind-less)
// profile under exactly that label and a same-named profile would make
// its probe output ambiguous.
//
// A profile is an operational guardrail, not a sandbox: selecting one in a
// repo spec grants the check everything attached to it (mounts, fixed env,
// a docker socket). The repo side can only NAME a profile; every host
// capability stays operator-owned here — no per-check mount sources,
// runtime flags, or resource overrides can come from the repo.
type Executor struct {
	Arg  string `kdl:",arg"` // raw first argument; resolved by applyDefaults, never read downstream
	Kind string `kdl:"kind"` // "local" | "container" (property form ⇒ named profile)
	Name string `kdl:"-"`    // resolved profile name; "" = the default profile

	Runtime string  `kdl:"runtime"` // "docker"|"podman"|"container"; default "container"
	Image   string  `kdl:"image"`   // required when Kind=="container"
	Workdir string  `kdl:"workdir"` // default "/workspace"
	Caches  []Cache `kdl:"cache,multiple"`
	Mounts  []Mount `kdl:"mount,multiple"`

	// Env is fixed, operator-owned environment values every check on this
	// profile receives — non-secret topology like
	// TESTCONTAINERS_HOST_OVERRIDE, not a secrets channel. Names may not
	// collide with the GAUNTLET_* contract (validated); on any other
	// collision the gauntlet-provided values win (they're appended after
	// these). Works for both kinds: exported to the subprocess under
	// "local", passed as -e pairs under "container".
	Env []EnvVar `kdl:"env,multiple"`

	// AddHosts is container-only: one --add-host <host>:<gateway> per
	// entry (the testcontainers host.docker.internal pattern).
	AddHosts []AddHost `kdl:"add-host,multiple"`

	// Memory/CPUs are container-only resource ceilings, passed to the
	// runtime verbatim as --memory/--cpus — same syntax and plausibility
	// validation as a Service's fields of the same names. Empty emits no
	// flag (the runtime's own default).
	Memory string `kdl:"memory"`
	CPUs   string `kdl:"cpus"`

	// MaxExecutions is DEPRECATED here: the cap is daemon-wide (spanning
	// every profile), so it lives at the top level (Daemon.MaxExecutions)
	// now. The field survives on the default block only so a config
	// written when it was documented here keeps loading — resolveExecutors
	// adopts the value into the top-level field (and rejects it on a
	// named profile, or when both spellings are set).
	MaxExecutions int `kdl:"max-executions"`
}

// Export configures how trial trees are materialized on disk — an
// operator/builder-cache property, deliberately not a repo-spec one: the
// repo must not silently impose a potentially expensive history walk on
// the machine.
type Export struct {
	// Mtimes selects file-timestamp materialization. "" (absent, the
	// default): extraction wall time, the classic behavior. "history":
	// every tracked file's mtime is set to the committer time of the last
	// commit that changed that path (git-restore-mtime semantics, keyed
	// off the exact synthetic merge) — so re-exports of the same merge
	// are metadata-identical and path+metadata-keyed caches (test-result
	// caches recording stat() of opened files) stop missing on every
	// unrelated commit. Costs one bounded history walk per export; the
	// walk failing fails the trial as an infrastructure error, never a
	// silent wall-clock fallback. Any other value is a config error.
	Mtimes string `kdl:"mtimes"`
}

// AddHost is one `add-host "hostname" "gateway"` pair on a container
// executor profile.
type AddHost struct {
	Host    string `kdl:",arg"`
	Gateway string `kdl:",arg"`
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

// reservedGitDir is the fixed in-container path the daemon's bare repo is
// bind-mounted at read-only (the GAUNTLET_GIT_DIR contract) — keep in sync
// with internal/executor/container.go's containerGitDir, duplicated for the
// same no-import-of-executor reason as reservedResultDir above.
const reservedGitDir = "/gauntlet-git"

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
// own Workdir, reservedResultDir (the fixed in-container result-dir mount,
// internal/executor/container.go's containerResultDir), or reservedGitDir
// (the fixed read-only bare-repo mount, containerGitDir there) — all three
// are the executor's own contract with every check and must not be silently
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
// may run on this box. Absent ⇒ Allow nil ⇒ a check spec declaring
// service/needs is REJECTED at run time (loud, like a malformed check —
// CheckSpec.RequiresServices() is how the queue detects this). Presence is
// signalled by len(Allow) > 0, same pattern as GitHub.Repo/Slack.Channel/etc.
type Services struct {
	// Allow lists the driver kinds permitted on this box; currently only
	// "container" is implemented — validate() rejects "artifact" and
	// "oci-unpack" as reserved for a future release (mirroring
	// Target.OnBatchRed's "bisect" handling above), rather than silently
	// ignoring them.
	Allow []string `kdl:"allow"`

	// MaxInstances hard-caps the number of live service instances this
	// daemon's pool will create (count only, not memory/CPU). Defaults to
	// defaultMaxInstances (8) when the block is enabled and this is left
	// zero.
	MaxInstances int `kdl:"max-instances"`

	// Runtime is the container runtime the services pool uses to create
	// instances: currently "docker" or "podman". This field exists because
	// "executor local" has no runtime of its own, and
	// local-executor-plus-containerized-services is a first-class
	// deployment shape — but it is ONLY CONSULTED when the executor kind is
	// "local" (cmd/gauntlet wires this at boot). When the executor kind is
	// "container", the executor's own Runtime wins instead (it must already
	// be docker/podman — Apple `container` networking is a hard-fail
	// there), and applyDefaults deliberately leaves this field undefaulted
	// in that case so validate() can tell "operator wrote a conflicting
	// value" from "operator never wrote one" — see validate()'s
	// cross-check.
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
// actual git branch and may.
type Target struct {
	Name   string `kdl:",arg"`
	Branch string `kdl:"branch"`

	// Hooks are this target's post-land hooks (DESIGN.md's decision
	// ledger, "Deployments as post-land hooks"; internal/hooks), run in
	// order against the landed tree once a candidate lands. Nil/empty
	// means no hooks.
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

	// Mode selects this target's queueing discipline: "serial" (default)
	// tests and lands one candidate at a time, the baseline discipline;
	// "batch" merges up to MaxBatch queued candidates into one --no-ff
	// chain and runs a single check suite over the combined tree;
	// "speculate" pipelines up to Window runs, each testing its own
	// candidate chained onto the predicted (not-yet-landed) tip of the run
	// ahead of it.
	//
	// "" is the zero value and means "serial" everywhere Mode is read.
	// Unlike HooksPolicy, applyDefaults deliberately does NOT normalize ""
	// to "serial" here: a target with no mode configured must keep
	// reporting Mode=="" so a zero Target{} sees serial without any
	// special-casing in internal/queue's lane model — "the fields' zero
	// value already means serial".
	Mode string `kdl:"mode"` // serial (default) | batch | speculate

	// MaxBatch caps how many queued candidates one batch run combines
	// into a single --no-ff chain and check suite. Legal only when
	// Mode=="batch" — validate() rejects it being set for any other mode.
	// Defaults to 8 when Mode=="batch" and left unset (zero).
	//
	// Bounded to [1, maxAllowedMaxBatch] (64) — a cap chosen here, not a
	// hard requirement: each additional batch member costs one more
	// synchronous Config.MergeBody/summarize call on the reconcile loop
	// before checks even start, so an unbounded max-batch could stall the
	// whole daemon for many multiples of summarize.timeout on a single
	// refill.
	MaxBatch int `kdl:"max-batch"`

	// Window is the speculation pipeline depth: up to this many runs are
	// in flight at once for the target — a fixed window; see
	// WindowStart/WindowMax/WindowHalveOnRed below for the reserved
	// adaptive-governor knobs. Legal only when Mode=="speculate"; defaults
	// to 4 when left unset (zero).
	//
	// Bounded to [1, maxAllowedWindow] (32) — a cap chosen here, not a
	// hard requirement. Each speculative run executes at most one check at
	// a time, so Window is ALSO the maximum number of concurrent check
	// processes/containers this target drives against the build executor:
	// an operator sizing window is sizing builder concurrency, not just
	// queue depth.
	Window int `kdl:"window"`

	// OnBatchRed selects the batch red-recovery strategy: "serial"
	// (default) re-queues every batch member unparked and re-forms them
	// one at a time on the next refill until the culprit is found and
	// parked, then batching resumes; "bisect" is a reserved growth path
	// only. Legal only when Mode=="batch".
	//
	// "bisect" is accepted here so config stays forward-compatible, but is
	// not implemented: LoadDaemon rejects a target that sets on-batch-red
	// "bisect" with a "reserved for a future release" error, rather than
	// silently running it as "serial".
	OnBatchRed string `kdl:"on-batch-red"` // serial (default) | bisect (reserved, rejected)

	// WindowStart, WindowMax, and WindowHalveOnRed reserve the config
	// surface for a future adaptive speculation-window governor
	// (Zuul-style: start small, grow on green, halve on red). Only the
	// fixed Window above is implemented; setting any of these three is
	// rejected at load with a "reserved for a future release" error (same
	// rationale as OnBatchRed's "bisect") so a config that names them
	// fails loudly instead of silently no-opping.
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
	if err := d.resolveExecutors(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	d.applyDefaults()
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &d, nil
}

// resolveExecutors splits the raw Executors nodes into the default profile
// (Daemon.Executor — the single kind-less block, whose argument is the
// KIND, exactly the pre-profiles spelling) and the named profiles
// (Daemon.Profiles — each `executor "name" kind="..."` block, whose
// argument is the NAME). The `kind` property's presence is the
// discriminator; see the Executor type doc. Runs before applyDefaults so
// per-profile defaulting and validation see resolved shapes only.
func (d *Daemon) resolveExecutors() error {
	seenDefault := false
	for _, e := range d.Executors {
		if e.Kind == "" {
			// Legacy/default spelling: `executor "container" { ... }` —
			// the argument is the kind. An argument that isn't a kind word
			// is caught HERE, not left for the kind validator: it is far
			// more likely a named profile missing its kind= (or a kind
			// typo) than a deliberate default block, and "kind must be
			// local or container, got \"ci\"" would send the operator
			// hunting in exactly the wrong place.
			if e.Arg != "" && e.Arg != "local" && e.Arg != "container" {
				return fmt.Errorf("executor %q: not a kind — the argument of a kind-less executor block must be \"local\" or \"container\"; a named profile needs kind=, e.g. executor %q kind=\"container\"", e.Arg, e.Arg)
			}
			if seenDefault {
				return fmt.Errorf("executor: more than one default (kind-less) executor block; name additional ones with kind=, e.g. executor \"ci\" kind=\"container\"")
			}
			seenDefault = true
			e.Kind = e.Arg
			e.Arg = ""
			// Deprecated spelling adoption: max-executions used to live on
			// the executor block; it's daemon-wide, so the top-level field
			// is canonical now, but the old location keeps working.
			if e.MaxExecutions != 0 {
				if d.MaxExecutions != 0 {
					return fmt.Errorf("max-executions is set both at the top level and on the executor block; keep the top-level one")
				}
				d.MaxExecutions = e.MaxExecutions
				e.MaxExecutions = 0
			}
			d.Executor = e
			continue
		}
		// Named profile: `executor "ci" kind="container" { ... }`.
		if e.Arg == "" {
			return fmt.Errorf("executor: a profile with kind=%q needs a name argument", e.Kind)
		}
		if e.MaxExecutions != 0 {
			return fmt.Errorf("executor %q: max-executions is daemon-wide, not per-profile; set it at the top level", e.Arg)
		}
		if e.Arg == "local" || e.Arg == "container" {
			return fmt.Errorf("executor: profile may not be named %q — that word in the argument position means the DEFAULT executor's kind; pick another name", e.Arg)
		}
		if e.Arg == "default" {
			return fmt.Errorf("executor: profile may not be named %q — gauntlet doctor prints the default (kind-less) profile under that label; pick another name", e.Arg)
		}
		e.Name = e.Arg
		e.Arg = ""
		d.Profiles = append(d.Profiles, e)
	}
	return nil
}

// applyExecutorDefaults fills one executor profile's container-only
// defaults (Runtime, Workdir) when its kind is container. Kind itself is
// only defaulted for the default profile (a named profile's kind= is
// explicit by construction).
func applyExecutorDefaults(e *Executor) {
	if e.Kind == "container" {
		if e.Runtime == "" {
			e.Runtime = defaultRuntime
		}
		if e.Workdir == "" {
			e.Workdir = defaultWorkdir
		}
	}
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
	if d.Shutdown == "" {
		d.Shutdown = defaultShutdown
	}
	// LogRetention defaults unconditionally (see its doc): there is no
	// "log-retention section absent -> disabled" state to preserve, unlike
	// every optional section below.
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
		// TokenEnv only defaults in static-token mode: under auth "app"
		// there is no PAT, and an explicitly-set token-env alongside the
		// app block is a validation error — defaulting it here would make
		// that conflict undetectable.
		if d.GitHub.TokenEnv == "" && d.GitHub.Auth == nil {
			d.GitHub.TokenEnv = defaultGitHubTokenEnv
		}
		if d.GitHub.APIURL == "" {
			d.GitHub.APIURL = defaultGitHubAPIURL
		}
		// Trial-ref publication (issue #7): the block's presence enables
		// it; a bare `trial-refs` node takes both defaults.
		if tr := d.GitHub.TrialRefs; tr != nil {
			d.GitHub.TrialRefPrefix = tr.Prefix
			if d.GitHub.TrialRefPrefix == "" {
				d.GitHub.TrialRefPrefix = defaultTrialRefPrefix
			}
			d.GitHub.TrialRefRetention = tr.Retention
			if d.GitHub.TrialRefRetention == 0 {
				d.GitHub.TrialRefRetention = defaultTrialRefRetention
			}
		}
		// Receipt-notes policy (issue #13): the block's presence enables
		// it; a bare `receipt-notes {}` node takes both defaults — same
		// "only default within the section, only when enabled" pattern as
		// trial-refs above.
		if rn := d.GitHub.ReceiptNotes; rn != nil {
			if rn.Ref == "" {
				rn.Ref = defaultReceiptNotesRef
			}
			if rn.MaxBytes == 0 {
				rn.MaxBytes = defaultReceiptNotesMaxBytes
			}
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

	// The default profile's Kind always defaults to "local", regardless of
	// whether any "executor" node was present at all (an absent node ⇒
	// local executor). Runtime/Workdir only matter for a container
	// executor, so only default them in that case — for the default
	// profile and every named profile alike.
	if d.Executor.Kind == "" {
		d.Executor.Kind = defaultExecutorKind
	}
	applyExecutorDefaults(&d.Executor)
	for i := range d.Profiles {
		applyExecutorDefaults(&d.Profiles[i])
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
		// HooksPolicy above and the optional sections in Daemon. Mode
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
// validateAppRemote enforces issue #6's app-mode remote contract: the App
// automatically authenticates git against the configured remote, so the
// remote must be (a) HTTPS — installation tokens cannot authenticate SSH —
// (b) credential-free — the ledger already flags URL-embedded credentials
// as readable by checks through the mounted GAUNTLET_GIT_DIR — and (c) the
// SAME host and owner/repo the github block names, canonicalized. A
// mismatch is a startup error, never a silent fallback to ambient auth: an
// operator who selected app auth must never discover months later that git
// was quietly using something else.
func validateAppRemote(remote, apiURL, repo string) error {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Covers scp-style SSH syntax (git@host:owner/repo.git), which
		// url.Parse reads as a bare opaque path.
		return fmt.Errorf(`github: auth "app" requires an HTTPS remote URL, got %q`, remote)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf(`github: auth "app" requires an HTTPS remote (installation tokens cannot authenticate %s)`, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf(`github: auth "app": remote must be credential-free (found userinfo in the URL); the token is injected per operation, never persisted`)
	}

	api, err := url.Parse(apiURL)
	if err != nil || api.Host == "" {
		return fmt.Errorf("github: api-url %q does not parse as a URL", apiURL)
	}
	// github.com's API lives on its own subdomain; GHES serves the API
	// under the primary host (https://HOST/api/v3).
	wantHost := api.Host
	if strings.EqualFold(wantHost, "api.github.com") {
		wantHost = "github.com"
	}
	if !strings.EqualFold(u.Host, wantHost) {
		return fmt.Errorf("github: auth \"app\": remote host %q does not match the github block's host %q — app credentials are scoped to one host, never forwarded", u.Host, wantHost)
	}

	// EscapedPath, not Path: url.Parse decodes %2F to "/", so a remote
	// path "acme%2Fwidgets" would otherwise validate equal to owner/repo
	// "acme/widgets" — yet git sends the raw %2F, which GitHub 404s. The
	// contract is a startup error, never a runtime surprise, so compare
	// what git will actually send.
	got := strings.Trim(u.EscapedPath(), "/")
	if n := len(got); n >= 4 && strings.EqualFold(got[n-4:], ".git") {
		got = got[:n-4]
	}
	if !strings.EqualFold(got, repo) {
		return fmt.Errorf("github: auth \"app\": remote repository %q does not match the github block's %q", got, repo)
	}
	return nil
}

// validateExecutor validates one executor profile (the default profile or
// a named one); label prefixes every error ("executor" for the default,
// `executor "ci"` for a profile) so an operator can tell which block is at
// fault.
func validateExecutor(e *Executor, label string) error {
	switch e.Kind {
	case "local":
		// Container-only options on a local profile are rejected loudly
		// rather than silently no-opped ("reserved, rejected if set" house
		// style): every one of these would otherwise read as configured
		// while doing nothing.
		switch {
		case e.Image != "":
			return fmt.Errorf("%s: image is container-only (kind is \"local\")", label)
		case e.Runtime != "":
			return fmt.Errorf("%s: runtime is container-only (kind is \"local\")", label)
		case e.Workdir != "":
			return fmt.Errorf("%s: workdir is container-only (kind is \"local\")", label)
		case len(e.Caches) > 0:
			return fmt.Errorf("%s: cache is container-only (kind is \"local\")", label)
		case len(e.Mounts) > 0:
			return fmt.Errorf("%s: mount is container-only (kind is \"local\")", label)
		case len(e.AddHosts) > 0:
			return fmt.Errorf("%s: add-host is container-only (kind is \"local\")", label)
		case e.Memory != "":
			return fmt.Errorf("%s: memory is container-only (kind is \"local\")", label)
		case e.CPUs != "":
			return fmt.Errorf("%s: cpus is container-only (kind is \"local\")", label)
		}
	case "container":
		if e.Image == "" {
			return fmt.Errorf("%s: image must not be empty for kind \"container\"", label)
		}
	default:
		return fmt.Errorf("%s: kind must be \"local\" or \"container\", got %q", label, e.Kind)
	}

	// Env works for both kinds (exported to the subprocess under "local",
	// -e pairs under "container"). The GAUNTLET_* namespace is the
	// check-facing contract and may not be squatted on by config.
	for _, ev := range e.Env {
		if ev.Name == "" {
			return fmt.Errorf("%s: env: name must not be empty", label)
		}
		if strings.Contains(ev.Name, "=") {
			return fmt.Errorf("%s: env %q: name must not contain '='", label, ev.Name)
		}
		if strings.HasPrefix(ev.Name, "GAUNTLET_") {
			return fmt.Errorf("%s: env %q: the GAUNTLET_* namespace is reserved for the check contract", label, ev.Name)
		}
	}

	for _, ah := range e.AddHosts {
		if ah.Host == "" || ah.Gateway == "" {
			return fmt.Errorf("%s: add-host needs both a hostname and a gateway argument", label)
		}
		// --add-host's own syntax is host:gateway; a ':' inside either half
		// would misparse rather than error, same failure shape as mounts.
		if strings.Contains(ah.Host, ":") {
			return fmt.Errorf("%s: add-host %q: hostname must not contain ':'", label, ah.Host)
		}
	}
	if e.Memory != "" && !memoryPattern.MatchString(e.Memory) {
		return fmt.Errorf("%s: memory %q: must match %s (e.g. \"2g\")", label, e.Memory, memoryPattern.String())
	}
	if e.CPUs != "" && !cpusPattern.MatchString(e.CPUs) {
		return fmt.Errorf("%s: cpus %q: must match %s (e.g. \"1.5\")", label, e.CPUs, cpusPattern.String())
	}

	for _, c := range e.Caches {
		if c.Name == "" {
			return fmt.Errorf("%s: cache: name must not be empty", label)
		}
		if c.Path == "" {
			return fmt.Errorf("%s: cache %q: path must not be empty", label, c.Name)
		}
	}
	for _, m := range e.Mounts {
		if m.Host == "" {
			return fmt.Errorf("%s: mount: host must not be empty", label)
		}
		if !filepath.IsAbs(m.Host) {
			return fmt.Errorf("%s: mount %q: host must be an absolute path", label, m.Host)
		}
		if m.Path == "" {
			return fmt.Errorf("%s: mount %q: path must not be empty", label, m.Host)
		}
		if !filepath.IsAbs(m.Path) {
			return fmt.Errorf("%s: mount %q: path must be an absolute path, got %q", label, m.Host, m.Path)
		}
		// ':' in either half silently corrupts the container runtime's
		// "-v host:path[:ro]" argv syntax (internal/executor/container.go's
		// runArgs just concatenates with ":") — the worst failure shape,
		// since it misparses rather than erroring. Absolute Linux paths
		// essentially never legitimately contain one.
		if strings.Contains(m.Host, ":") {
			return fmt.Errorf("%s: mount %q: host must not contain ':'", label, m.Host)
		}
		if strings.Contains(m.Path, ":") {
			return fmt.Errorf("%s: mount %q: path must not contain ':', got %q", label, m.Host, m.Path)
		}
		// Reserved in-container paths are the executor's own contract with
		// every check (the trial tree at Workdir, the result dir at
		// reservedResultDir, the bare-repo mount at reservedGitDir — keep
		// in sync with internal/executor/container.go's containerResultDir
		// and containerGitDir) and must never be silently shadowed by an
		// operator mount, including by a mount at some path UNDER one of
		// them: a nested bind is legal to docker/podman/container and would
		// partially, silently shadow the trial tree or result dir exactly
		// as much as an exact-path collision would. pathAtOrUnder compares
		// cleaned paths so a trailing slash, "//", or "/." spelling of the
		// same path can't bypass the guard. Checked per profile: each
		// container profile has its own workdir.
		if pathAtOrUnder(m.Path, e.Workdir) {
			return fmt.Errorf("%s: mount %q: path %q is at or under executor workdir %q", label, m.Host, m.Path, e.Workdir)
		}
		if pathAtOrUnder(m.Path, reservedResultDir) {
			return fmt.Errorf("%s: mount %q: path %q is at or under the reserved result-dir mount %q", label, m.Host, m.Path, reservedResultDir)
		}
		if pathAtOrUnder(m.Path, reservedGitDir) {
			return fmt.Errorf("%s: mount %q: path %q is at or under the reserved git-dir mount %q", label, m.Host, m.Path, reservedGitDir)
		}
	}
	return nil
}

func pathAtOrUnder(path, reserved string) bool {
	if reserved == "" {
		return false
	}
	path = filepath.Clean(path)
	reserved = filepath.Clean(reserved)
	return path == reserved || strings.HasPrefix(path, reserved+string(filepath.Separator))
}

// SecretEnvNames returns the environment variable NAMES (never values)
// that d's configured integrations declare as operator-secret credential
// sources: github's token-env in static-token mode, slack's
// app-token-env/bot-token-env, and summarize's api-key-env — exactly the
// vocabulary issue #13's Gap 1 requires the local executor to strip from
// every CANDIDATE-CODE command environment (checks, image builds, receipt
// producers; post-land hooks are exempt — see core.CheckJob.OperatorOwned —
// being operator-owned daemon config themselves, e.g. a deploy hook driving
// `gh`). cmd/gauntlet threads this straight into executor.LocalExecutor's
// SecretEnv field at construction; callers that build a Daemon by hand
// (tests) get the same collection logic by calling this method rather than
// re-deriving it, so the two can never drift.
//
// Call after applyDefaults has run (LoadDaemon always does) so each env-var
// field already carries its resolved default rather than an unresolved "".
//
// Only a name the config ACTUALLY declares as a secret source is
// collected, never the whole vocabulary regardless of mode: github's
// TokenEnv is a credential source only in static-token mode (Auth == nil)
// — an app-mode github block puts no static token in the daemon's own
// environment at all (ghauth mints installation tokens in-process), so
// there is nothing to strip. Likewise a disabled section (Repo/Channel
// empty, Summarize nil) contributes nothing.
func (d *Daemon) SecretEnvNames() []string {
	var names []string
	if d.GitHub.Repo != "" && d.GitHub.Auth == nil && d.GitHub.TokenEnv != "" {
		names = append(names, d.GitHub.TokenEnv)
	}
	if d.Slack.Channel != "" {
		if d.Slack.AppTokenEnv != "" {
			names = append(names, d.Slack.AppTokenEnv)
		}
		if d.Slack.BotTokenEnv != "" {
			names = append(names, d.Slack.BotTokenEnv)
		}
	}
	if d.Summarize != nil && d.Summarize.APIKeyEnv != "" {
		names = append(names, d.Summarize.APIKeyEnv)
	}
	return names
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
		// Two targets on the same branch would contend via CAS: reject at
		// config load instead.
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

		// Mode + its mode-scoped knobs: each knob is legal only under its
		// own mode, matching hooks-policy's "catch config mistakes"
		// strictness above.
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
				// NOTE: reserved growth path, validated but rejected at
				// load rather than silently running as "serial" — see
				// docs/design/queue-modes.md ("Deliberately not built").
				return fmt.Errorf("target %q: on-batch-red \"bisect\" is reserved for a future release", t.Name)
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

		// NOTE: reserved adaptive-window-governor knobs, parsed for forward
		// compatibility and always rejected regardless of Mode — see
		// docs/design/queue-modes.md ("Deliberately not built").
		if t.WindowStart != 0 {
			return fmt.Errorf("target %q: window-start is reserved for a future adaptive-window governor", t.Name)
		}
		if t.WindowMax != 0 {
			return fmt.Errorf("target %q: window-max is reserved for a future adaptive-window governor", t.Name)
		}
		if t.WindowHalveOnRed {
			return fmt.Errorf("target %q: window-halve-on-red is reserved for a future adaptive-window governor", t.Name)
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
	if tr := d.GitHub.TrialRefs; tr != nil {
		if d.GitHub.Repo == "" {
			return fmt.Errorf("github: trial-refs requires the github block to name a repo")
		}
		// The resolved prefix is what the ref name is built from; validate
		// the resolved value so a bare `trial-refs {}` (defaulted) and an
		// explicit prefix are held to the same rule.
		p := d.GitHub.TrialRefPrefix
		if !strings.HasPrefix(p, "refs/") {
			return fmt.Errorf("github: trial-refs prefix must start with \"refs/\", got %q", p)
		}
		if strings.HasSuffix(p, "/") || strings.ContainsAny(p, " \t\n") {
			return fmt.Errorf("github: trial-refs prefix must have no trailing slash or whitespace, got %q", p)
		}
		// The prefix is concatenated with a run ID and pushed as a ref, so
		// it must itself be a valid ref name — reject git's forbidden
		// characters (glob metacharacters an operator might copy from a
		// ls-remote pattern included) at load, loudly, rather than let
		// every publish fail at run time and park the queue.
		if strings.ContainsAny(p, "*?[\\~^:") || strings.Contains(p, "..") || strings.Contains(p, "@{") {
			return fmt.Errorf("github: trial-refs prefix is not a valid ref name (no * ? [ \\ ~ ^ : .. @{ ), got %q", p)
		}
		if tr.Retention < 0 {
			return fmt.Errorf("github: trial-refs retention must not be negative, got %s", tr.Retention)
		}
	}
	if rn := d.GitHub.ReceiptNotes; rn != nil {
		if d.GitHub.Repo == "" {
			return fmt.Errorf("github: receipt-notes requires the github block to name a repo")
		}
		// Same ref-name grammar as trial-refs above (issue #7), reused
		// rather than re-derived, plus the notes-specific refs/heads/
		// exclusion below.
		ref := rn.Ref
		if !strings.HasPrefix(ref, "refs/") {
			return fmt.Errorf("github: receipt-notes ref must start with \"refs/\", got %q", ref)
		}
		if strings.HasSuffix(ref, "/") || strings.ContainsAny(ref, " \t\n") {
			return fmt.Errorf("github: receipt-notes ref must have no trailing slash or whitespace, got %q", ref)
		}
		if strings.ContainsAny(ref, "*?[\\~^:") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
			return fmt.Errorf("github: receipt-notes ref is not a valid ref name (no * ? [ \\ ~ ^ : .. @{ ), got %q", ref)
		}
		// A notes payload under refs/heads/ would trigger branch
		// machinery (GitHub's branch UI, push-triggered workflows) —
		// refs/notes/ is the conventional namespace, but any other
		// custom, non-heads namespace is allowed, matching trial-refs'
		// own refs/heads/** stance.
		if strings.HasPrefix(ref, "refs/heads/") {
			return fmt.Errorf("github: receipt-notes ref must not be under \"refs/heads/\" — a receipts payload there would trigger branch machinery; use a custom namespace like %q, got %q", defaultReceiptNotesRef, ref)
		}
		if rn.MaxBytes <= 0 {
			return fmt.Errorf("github: receipt-notes max-bytes must be positive, got %d", rn.MaxBytes)
		}
		if rn.MaxBytes > maxAllowedReceiptBytes {
			return fmt.Errorf("github: receipt-notes max-bytes %d exceeds the maximum of %d", rn.MaxBytes, maxAllowedReceiptBytes)
		}
	}
	if a := d.GitHub.Auth; a != nil {
		if d.GitHub.Repo == "" {
			return fmt.Errorf(`github: auth "app" requires the github block to name a repo`)
		}
		if a.Mode != "app" {
			return fmt.Errorf("github: auth mode must be \"app\", got %q", a.Mode)
		}
		if a.AppID <= 0 {
			return fmt.Errorf("github: auth \"app\": app-id must be positive, got %d", a.AppID)
		}
		if a.InstallationID <= 0 {
			return fmt.Errorf("github: auth \"app\": installation-id must be positive, got %d", a.InstallationID)
		}
		if a.PrivateKeyFile == "" {
			return fmt.Errorf("github: auth \"app\": private-key-file is required")
		}
		if d.GitHub.TokenEnv != "" {
			return fmt.Errorf(`github: token-env and auth "app" are mutually exclusive — pick one authentication mode`)
		}
		if err := validateAppRemote(d.Remote, d.GitHub.APIURL, d.GitHub.Repo); err != nil {
			return err
		}
	}

	if err := validateExecutor(&d.Executor, "executor"); err != nil {
		return err
	}
	profileNames := make(map[string]bool, len(d.Profiles))
	for i := range d.Profiles {
		p := &d.Profiles[i]
		label := fmt.Sprintf("executor %q", p.Name)
		if profileNames[p.Name] {
			return fmt.Errorf("%s: duplicate profile name", label)
		}
		profileNames[p.Name] = true
		// A named profile's kind is explicit by construction
		// (resolveExecutors requires the property), so unlike the default
		// profile it never defaults — reject anything but the two kinds.
		if p.Kind != "local" && p.Kind != "container" {
			return fmt.Errorf("%s: kind must be \"local\" or \"container\", got %q", label, p.Kind)
		}
		if err := validateExecutor(p, label); err != nil {
			return err
		}
	}
	switch d.Export.Mtimes {
	case "", "history":
	default:
		return fmt.Errorf("export: mtimes must be \"history\" (or absent), got %q", d.Export.Mtimes)
	}
	// Zero is "left unset" = unlimited (the field doc), so only a negative
	// value — never a meaningful cap — is rejected.
	if d.MaxExecutions < 0 {
		return fmt.Errorf("max-executions must not be negative, got %d", d.MaxExecutions)
	}
	switch d.Shutdown {
	case "", "drain", "kill":
	default:
		return fmt.Errorf("shutdown must be \"drain\" or \"kill\", got %q", d.Shutdown)
	}

	if len(d.Services.Allow) > 0 {
		for _, a := range d.Services.Allow {
			switch a {
			case "container":
				// v1-implemented; see field doc.
			case "artifact", "oci-unpack":
				// NOTE: reserved growth path — the artifact and oci-unpack
				// drivers were designed and deliberately left unbuilt; see
				// docs/design/services.md ("Deliberately not built").
				// Validated but rejected at load rather than silently
				// no-opping, same "reserved for a future release"
				// treatment as Target.OnBatchRed's "bisect" above.
				return fmt.Errorf("services: allow %q is reserved for a future release", a)
			default:
				return fmt.Errorf("services: allow must be \"container\", got %q", a)
			}
		}
		if d.Services.MaxInstances < 1 {
			return fmt.Errorf("services: max-instances must be at least 1, got %d", d.Services.MaxInstances)
		}
		// Runtime cross-check: under "container", the executor's own
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
			return fmt.Errorf("summarize: effort must be one of none, low, medium, high, xhigh, max, got %q", d.Summarize.Effort)
		}
		if d.Summarize.Timeout <= 0 {
			return fmt.Errorf("summarize: timeout must be positive, got %s", d.Summarize.Timeout)
		}
	}

	return nil
}
