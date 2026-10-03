// Package core defines the shared queue, check, and deployment types.
package core

import "time"

// Candidate identifies a queue slot and its current revision. Ordinary
// slots use refs/heads/for/<target>/<user>/<topic>; forge adapters supply
// virtual slots.
type Candidate struct {
	AdmissionBlocked                      string
	SkipChecks, OverridePause             bool
	Requester, RequestReason, EmergencyID string
	RequestedCount                        int
	Ref                                   string
	Target                                string
	User                                  string
	Topic                                 string
	SHA                                   string

	// Review adapters supply Source, Message, and ReviewURL. SourceBase bounds
	// the change's delta; DependsOn names its prerequisite slot. Version
	// identifies review metadata and admission intent beyond SHA.
	Source     string
	SourceBase string
	Message    string
	ReviewURL  string
	Version    string
	DependsOn  string
	Urgent     bool
}

// TrialMerge is the result of trial-merging a Candidate onto the target tip.
type TrialMerge struct {
	// Clean reports whether the merge produced a tree with no conflicts.
	Clean bool

	// TreeOID is the resulting tree object ID. Valid only if Clean.
	TreeOID string

	// Conflicts lists the paths that conflicted. Valid only if !Clean.
	Conflicts []string
}

// CheckJob describes one named check to run against an exported trial tree.
type CheckJob struct {
	GitDir     string
	Candidates []Candidate
	// RunID is stable for the whole run and shared by every check in it.
	RunID string

	Target string
	Name   string

	// Command is the argv to execute; Command[0] is the program.
	Command []string

	// Executor names an operator-defined execution profile. Empty selects the
	// default; executor.Mux handles routing.
	Executor string

	// OperatorOwned permits access to the daemon's configured credential
	// environment. Set only for commands from operator config, such as hooks;
	// repo-defined checks, image builds, and receipts leave it false.
	OperatorOwned bool

	// ImageBuild captures EnvImageResultFile into CheckResult.Image for queue
	// validation. Mutually exclusive with Image and ReceiptCapture.
	ImageBuild bool

	// Image is the validated immutable image identity for a consumer check.
	// Container executors use it instead of their configured image.
	Image string

	// ReceiptCapture reads EnvReceiptResultFile into CheckResult.Receipt.
	// Receipts cannot report skipped and are distinct from image builds.
	ReceiptCapture bool

	// ReceiptMaxBytes bounds capture to this value plus one byte, so the queue
	// can detect overflow before the executor removes its scratch directory.
	// Zero selects the executor's internal ceiling.
	ReceiptMaxBytes int

	// DeployEnv enables the GAUNTLET_DEPLOY_* environment. DeployNode names
	// the graph node; DeployedSHA is the previously observed revision, empty
	// on the first deploy. MergeSHA is the revision being deployed.
	DeployEnv   string
	DeployNode  string
	DeployedSHA string

	// Dir is the exported trial tree the check runs against.
	Dir string

	// LogPath receives the full combined output, alongside the bounded Output
	// tail. Empty disables file logging; inability to create the file does not
	// fail the check.
	LogPath string

	// BaseSHA is the target tip the trial merge was built onto.
	BaseSHA string

	// MergeSHA is the exact tested commit, whether a normalized landing or a
	// legacy merge.
	MergeSHA string

	Candidate Candidate

	// Clean is reserved for a future cache-bypass request and is currently
	// always false.
	Clean bool

	// ServiceEnv supplies GAUNTLET_SVC_<NAME>_HOST/PORT after the built-in
	// environment. Nil for checks without needs and for hooks.
	ServiceEnv []string

	// Networks lists the service networks a container check must join. Local
	// checks use published ports and ignore it.
	Networks []string
}

// CheckStatus is a check's verdict.
type CheckStatus int

const (
	// CheckFailed includes nonzero exits and command-start failures. It is the
	// zero value so an unset result cannot pass.
	CheckFailed CheckStatus = iota

	// CheckPassed means the check exited 0 and did not report skipped.
	CheckPassed

	// CheckSkipped means the check exited 0 and explicitly reported
	// "skipped" via the result file. Distinct from CheckPassed so run
	// history doesn't lie about what actually ran.
	CheckSkipped

	// CheckBlocked means a prerequisite or the run's root failure prevented
	// execution. Unlike CheckSkipped, it is not green; BlockedBy identifies
	// the cause.
	CheckBlocked

	// CheckWaived is an explicit operator bypass, never an ordinary green result.
	CheckWaived
)

// CheckResult reports a check verdict. Status is meaningful only when Err
// is nil. Command-start failures and nonzero exits are CheckFailed; Err is
// reserved for cancellation and executor infrastructure failures.
type CheckResult struct {
	SuspectedRefs []string
	FailureKind   string
	Name          string

	// Image records the build result or the immutable image a consumer used.
	// Empty for other jobs.
	Image string

	// Receipt holds raw captured bytes until the queue validates and clears
	// them; it is never persisted. Nil means unreadable, while a non-nil empty
	// slice means an empty file.
	Receipt []byte

	// Seq is the 1-based spec position, preserving check identity and log
	// filenames when parallel completion leaves gaps. Zero lets history use
	// the slice index for older records.
	Seq int

	// Command is the argv submitted to the executor, copied onto the result by
	// the queue. Nil for older or hand-built records.
	Command []string

	Status CheckStatus

	// Output is the check's captured output, tail-capped (64 KiB).
	Output string

	// LogPath names the full output file only if the executor created it. File
	// creation failure leaves it empty without failing the check.
	LogPath string

	Duration time.Duration

	// Waited measures time ready to run but waiting for a daemon-wide
	// execution slot.
	Waited time.Duration

	// Materialized measures private workspace export and timestamp restoration
	// in isolated mode. Zero in shared mode.
	Materialized time.Duration

	// PeakRSS is peak resident memory in bytes, including reaped descendants
	// where supported. Zero means unmeasured. Resource usage never affects the
	// verdict.
	PeakRSS int64

	// UserCPU and SysCPU are process and reaped-descendant CPU times. Zero
	// means unmeasured.
	UserCPU time.Duration
	SysCPU  time.Duration

	// BlockedBy identifies failed prerequisites or the run's root failure. Set
	// only for CheckBlocked.
	BlockedBy []string

	// Err is set only for daemon-caused non-verdict failures. See Status
	// doc for the Err-vs-Status contract.
	Err error
}

// Environment variables every Executor exports to a check's process. This is
// the mechanism by which conditional execution (e.g. monorepo "only web
// changed" skips) stays the check script's job, never gauntlet config's.
const (
	// EnvBaseSHA is the target tip the trial merge was built onto (=
	// CheckJob.BaseSHA).
	EnvBaseSHA = "GAUNTLET_BASE_SHA"

	// EnvMergeSHA is the tested merge commit (= CheckJob.MergeSHA).
	EnvMergeSHA = "GAUNTLET_MERGE_SHA"

	// EnvCandidateSHA is the candidate's own commit (=
	// CheckJob.Candidate.SHA).
	EnvCandidateSHA = "GAUNTLET_CANDIDATE_SHA"

	// EnvRef is the candidate's queue-slot ref (= CheckJob.Candidate.Ref).
	EnvRef = "GAUNTLET_REF"

	// EnvResultFile is the path to a result file the executor creates
	// before running the check's command. Precedence: a non-zero exit is
	// always CheckFailed regardless of the file's contents (the file is
	// ignored on failure); on exit 0, the file containing "skipped" is
	// CheckSkipped, and an empty or absent file is CheckPassed.
	EnvResultFile = "GAUNTLET_RESULT_FILE"

	// EnvRunID namespaces external resources per run. It is shared by all
	// checks and all members of a batch.
	EnvRunID = "GAUNTLET_RUN_ID"

	// EnvImageResultFile replaces EnvResultFile for builds. The executor
	// captures an image ID or digest reference; the queue rejects missing,
	// empty, or mutable results. Nonzero exit always fails.
	EnvImageResultFile = "GAUNTLET_IMAGE_RESULT_FILE"

	// EnvReceiptResultFile replaces EnvResultFile for receipts. The executor
	// captures raw bytes, bounded by ReceiptMaxBytes+1; the queue validates
	// them. Nonzero exit always fails.
	EnvReceiptResultFile = "GAUNTLET_RECEIPT_RESULT_FILE"

	// EnvGitDir points to the daemon's object store so checks can resolve the
	// supplied SHAs. Read-only by contract; omitted when the executor has no
	// configured Git directory.
	EnvGitDir = "GAUNTLET_GIT_DIR"

	// The deploy variables are exported together when CheckJob.DeployEnv is
	// set. EnvDeployedSHA is set but empty on the first deploy, allowing
	// scripts to distinguish it from a missing deploy environment.
	EnvDeployEnv = "GAUNTLET_DEPLOY_ENV"

	// EnvDeployNode is the deploy node's name (= CheckJob.DeployNode).
	EnvDeployNode = "GAUNTLET_DEPLOY_NODE"

	// EnvDeploySHA is the desired revision, also exported as EnvMergeSHA.
	EnvDeploySHA = "GAUNTLET_DEPLOY_SHA"

	// EnvDeployedSHA is the environment's observed ref BEFORE this deploy
	// (= CheckJob.DeployedSHA); empty on its first-ever deploy, and empty
	// only then.
	EnvDeployedSHA = "GAUNTLET_DEPLOYED_SHA"
)

// Outcome is a run's final disposition.
type Outcome int

const (
	// OutcomeLanded means the tested commit reached the target. Slot deletion
	// or forge acknowledgement may still need recovery.
	OutcomeLanded Outcome = iota

	// OutcomeRejected means a check failed, or the candidate's check spec
	// was missing or invalid. The target is untouched and the (ref, SHA)
	// is parked: it will not be re-tested until the author re-pushes a
	// new SHA.
	OutcomeRejected

	// OutcomeConflict means the trial merge itself conflicted. The target
	// is untouched and the (ref, SHA) is parked, same as OutcomeRejected.
	OutcomeConflict

	// OutcomeSkipped means the ref or target moved mid-run, or a CAS race
	// was lost at land time. Nothing is parked; the slot naturally
	// re-queues and is retried at its current SHA.
	OutcomeSkipped

	// OutcomeError means a daemon-side failure occurred (see
	// CheckResult.Err). It parks the (ref, SHA) like OutcomeRejected, but
	// is reported as a distinct event so operators can tell infra
	// failures from real red checks.
	OutcomeError
)

// RunRecord is the single structured fact produced by one run: a stable ID,
// the merge tested, and the per-check verdicts. It is the source of truth
// that both the OTel span tree and the SQLite writer / OTLP exporter build
// from, unchanged.
type RunRecord struct {
	RunID     string
	Target    string
	Candidate Candidate

	// BaseOID is the target tip the trial merge was built onto.
	BaseOID string

	// MergeSHA is the tested merge commit.
	MergeSHA string

	Trial TrialMerge

	// Checks holds results in check-spec order.
	Checks []CheckResult

	Outcome Outcome

	// Detail is a human-readable explanation (e.g. the missing check-spec
	// path, or the conflicted paths) for events that don't carry enough
	// context otherwise.
	Detail string

	StartedAt time.Time
	EndedAt   time.Time

	// BatchID groups the per-member records of one batch run (empty for
	// serial and speculate). All members of a batch share it; the dashboard
	// and history use it to render "landed together as batch <id>".
	BatchID string

	// Position is this member's 0-based index within its batch (0 for
	// serial/speculate).
	Position int

	// BatchSize is the member count when BatchID is non-empty. Serial and
	// speculative in-memory records leave it zero; history normalizes it to
	// one.
	BatchSize int

	// Speculated is true iff this run was tested on a *predicted* base
	// (speculation, a non-head window member) rather than the live target
	// tip. Purely informational for the dashboard; the landed commit is the
	// tested commit either way (Invariant 1).
	Speculated bool

	// Recovered marks a landing discovered in target history without running
	// checks here. Hooks must not auto-run for recovered records, even when
	// MergeSHA is known.
	Recovered bool

	// ReceiptRef, ReceiptBlob, and ReceiptPublished record confirmed note
	// publication, even if the subsequent target CAS fails. ReceiptPublished
	// is "published" or "already-present"; empty values mean no publication.
	ReceiptRef       string
	ReceiptBlob      string
	ReceiptPublished string
}

// Identity is a git commit identity: a name and an email address.
type Identity struct {
	Name  string
	Email string
}

// EventKind classifies an Event.
type EventKind int

const (
	EventQueued EventKind = iota
	EventTrialClean
	EventTrialConflict
	EventCheckStarted
	EventCheckFinished
	EventLanded
	EventRejected
	EventSkipped
	EventError

	// EventIgnoredRef reports a well-formed candidate ref (the for/...
	// grammar) whose target segment names no configured target — a common
	// misconfiguration (a typo, or a target retired from config while
	// stale refs linger), surfaced explicitly rather than silently
	// dropped. Emitted once per (ref, SHA), not every tick. Not terminal:
	// it carries no RunRecord, since no run was ever attempted. Channel
	// implementations must ignore EventKind values they don't recognize
	// (this one included) rather than erroring — new kinds are always
	// additive.
	EventIgnoredRef

	// EventHookFinished reports one post-land hook's outcome (internal/hooks,
	// DESIGN.md's decision ledger "Deployments as post-land hooks"): Target,
	// Candidate, and RunID identify the landing; CheckName is the hook's
	// name; Check carries its CheckResult. It is additive like EventIgnoredRef
	// — channels that don't render it simply ignore it.
	EventHookFinished

	// EventHookStarted reports that one post-land hook is about to run
	// (internal/hooks's durable owed/skipped marker and live
	// observability): Target, Candidate, and RunID identify the
	// landing; CheckName is the hook's name; HookIndex is its 0-based
	// position within this landing's configured hook order and HookCount is
	// the landing's total configured hook count (see Event's HookIndex/
	// HookCount doc — meaningful only on hook events, zero on every other
	// kind). Emitted once per hook, immediately before that hook's
	// core.Executor.RunCheck call, on hooks.Runner's single execution
	// goroutine (hook execution stays globally serial — this never fires
	// from more than one goroutine at a time). history.Store upserts a
	// durable "owed" row (hook_runs) the first time this fires for a given
	// RunID — before any hook subprocess actually starts — so a crash
	// mid-chain leaves owed_count > (COUNT of finished hooks), discoverable
	// via history.Store.HookRunSummaries without gauntlet ever auto-resuming
	// anything. Additive like EventIgnoredRef: channel implementations must
	// ignore EventKind values they don't recognize (this one included)
	// rather than erroring — new kinds are always additive.
	EventHookStarted

	// EventHookSkipped reports that a recovery-synthesized landing
	// (queue/reconcile.go's recoverLanded, whose RunRecord.MergeSHA is
	// always "" — there is no merge SHA to export a tree from) skipped its
	// hooks entirely: hooks.Runner.runLanding never calls RunCheck for it at
	// all. Target, Candidate, and RunID identify the landing; Detail is a
	// human-readable reason ("recovered landing; hooks not run"); HookCount
	// is the target's configured hook count, carried for parity with
	// EventHookStarted's owed accounting (see Event's HookIndex/HookCount
	// doc). history.Store persists this as a durable hook_runs row with
	// skipped=1/skip_reason=Detail, so the landing's hooks read as
	// "skipped (recovery)" on every surface rather than being silently
	// mistaken for a stalled or crashed chain (discoverability, no
	// auto-resume). Additive like EventIgnoredRef — channel implementations
	// must ignore EventKind values they don't recognize rather than
	// erroring.
	EventHookSkipped

	// EventTrialMerged reports that a run's synthetic merge commit now
	// exists and (when trial-ref publication is enabled, issue #7) has
	// been published under an immutable remote ref — so its MergeSHA is
	// resolvable on the remote and can carry a commit status. Target,
	// Candidate, and RunID identify the run; Record is set and carries the
	// MergeSHA/BaseOID, the same *RunRecord the run's terminal event will
	// later carry (so the pending verification status posts to exactly the
	// bytes that ran). Fired once per run, after CommitTree and the ref
	// push, before any check starts — a non-terminal, additive event
	// (channels that don't render it ignore it). Unlike EventTrialClean
	// (emitted before CommitTree, when no MergeSHA exists yet), this
	// carries the merge identity, which is the whole point: the candidate
	// SHA and the tested-merge SHA are different claims.
	EventTrialMerged

	// EventVerified reports that a run's required check graph went green —
	// the tested merge is verified — BEFORE the landing CAS that makes it
	// the target tip. It is deliberately distinct from EventLanded:
	// verification is a true statement about the exact MergeSHA, whereas
	// landing is a subsequent compare-and-swap that can still fail (or be
	// skipped) if the target moved. This ordering is what lets a "tested
	// SHA must be green before the ref update" policy exist — posting
	// success only after landing could never satisfy it. Target,
	// Candidate, and RunID identify the run; Record carries the MergeSHA.
	// Fired once per run, when the verdict first becomes green. Additive
	// and non-terminal (the run's own terminal EventLanded/EventRejected
	// still follows) — channels that don't recognize it ignore it.
	EventVerified

	// EventRetryRequested reports an operator's explicit retry of a parked
	// (ref, SHA) (queue/command.go's applyRetry, a persisted retry
	// intent): Target and Candidate (Ref, SHA set; User/Topic as parsed)
	// identify what was retried, At is when. history.Store upserts a
	// retry_intents row (target, ref) -> (sha, at), latest retry wins, so a
	// daemon crash between this retry and the retried run's own terminal
	// event can't silently re-park the ref at its old, now-superseded
	// rejection (see history's LatestTerminalPerRef seed-park query, which
	// suppresses a park whose terminal predates a later retry_intents row).
	// Purely a durability signal — EventQueued, emitted alongside this in
	// the same applyRetry call, still drives every channel's live
	// rendering. Additive like EventIgnoredRef — channel implementations
	// must ignore EventKind values they don't recognize rather than
	// erroring.
	EventRetryRequested

	// EventDeployStarted reports that one environment's deploy graph run is
	// about to start (internal/deploy, docs/architecture/deployment.md): DeployEnv
	// is the environment, RunID the deploy run's own ID, DeploySHA the
	// revision being deployed (the desired ref's value) and DeployedSHA the
	// observed ref's value before this run ("" on an environment's
	// first-ever deploy). It carries no Candidate and no Target: a deploy
	// lane is addressed by environment, never by a candidate ref — which is
	// also why deploy retry/cancel are env-addressed API calls rather than
	// core.Commands. Additive like EventIgnoredRef: channel implementations
	// must ignore EventKind values they don't recognize rather than
	// erroring.
	EventDeployStarted

	// EventDeployNodeFinished reports one deploy node's outcome — the
	// deploy twin of EventCheckFinished, and it carries its result the same
	// way: CheckName is the node's name and Check its *CheckResult, so a
	// channel can render per-node verdicts mid-graph without waiting for
	// the run's terminal record. DeployEnv/RunID/DeploySHA/DeployedSHA
	// identify the run, exactly as on EventDeployStarted. A node that never
	// ran (blocked by a failed edge, or by the graph failing fast before it
	// started) emits NOTHING here — blocked rows exist only in the terminal
	// DeployRecord's Nodes slice, the same rule CheckBlocked follows in a
	// run's record.
	EventDeployNodeFinished

	// EventDeployFinished is a deploy graph run's TERMINAL event and
	// carries the finished *DeployRecord in Deploy — the deploy analogue of
	// the run-terminal events' Record, and deliberately a separate field:
	// the two consumers that treat a non-nil Record as "a finished RUN,
	// render/persist it" (internal/slack, internal/history) must not
	// mistake a deploy for one. DeployEnv/RunID/DeploySHA/DeployedSHA are
	// set here too, so a channel can identify the lane without
	// dereferencing the record. Emitted whatever the outcome — green,
	// parked on a red node, errored, or concluded externally (a `cancel`
	// policy desired move, drain) — since the lane's park state is exactly
	// what an operator needs told.
	EventDeployFinished

	// numEventKinds counts the declared kinds — KEEP LAST, and add new
	// kinds ABOVE it. The emit-site contract table (events_test.go) is
	// checked for exactly this many entries, so a kind added without a
	// contract entry fails a test instead of silently shipping an
	// unspecified shape: event shapes have broken twice already (DESIGN.md,
	// "Event shapes are the soft underbelly") and "extend the contract
	// tests first" needs a mechanism, not a habit.
	numEventKinds
)

// Event is one notification emitted to a Channel. Terminal events —
// EventLanded, EventRejected, EventTrialConflict, EventSkipped, EventError —
// carry the finished *RunRecord; the deploy subsystem's terminal event,
// EventDeployFinished, carries a *DeployRecord in Deploy instead. See
// ValidateEvent for the whole emit-site contract in executable form.
type Event struct {
	Kind EventKind
	At   time.Time

	Target    string
	Candidate Candidate

	// RunID identifies the run this event belongs to: a queue run's ID on
	// every candidate/hook event, and the DEPLOY run's own ID on the three
	// deploy kinds — one field, because "run-scoped events carry the run
	// ID" (docs/architecture/queue.md, "Event model") is a contract about
	// joinability, not about which subsystem minted the ID. The two ID
	// spaces never collide (both are unique per process) and no consumer
	// joins across them: a deploy event carries no Candidate, so nothing
	// that keys on (target, ref) sees one at all.
	RunID string

	// CheckName names the check, hook, or deploy NODE an event is about.
	CheckName string

	// HookIndex and HookCount are meaningful only on EventHookStarted
	// (both) and EventHookSkipped (HookCount only) — hooks.Runner's
	// per-landing hook accounting — and zero on every other event kind,
	// the same additive pattern as CheckName. HookIndex is a hook's
	// 0-based position within its landing's configured hook order
	// ("hook 0 of HookCount"); EventHookSkipped never sets it, since a
	// skipped landing never starts any specific hook. HookCount is the
	// landing's target's total configured hook count.
	HookIndex int
	HookCount int

	// Check carries one finished result — the just-finished check on
	// EventCheckFinished, the just-finished hook on EventHookFinished, or
	// the just-finished deploy node on EventDeployNodeFinished — so
	// channels can render per-check/per-hook/per-node verdicts (and
	// durations) mid-run without waiting for the run's terminal record. nil
	// on every other event kind. Channel implementations must nil-check
	// before dereferencing: older events, and any future EventKind, may
	// carry nil here even on what looks like a finished-check line.
	Check *CheckResult

	// Record is set on terminal events; nil otherwise. Two consumers
	// (internal/slack, internal/history) treat a non-nil Record as "this
	// is a finished run — render/persist it", so a non-terminal event must
	// NOT carry one: the trial-merge/verified events below carry their
	// merge identity in MergeSHA instead, keeping Record nil.
	Record *RunRecord

	// MergeSHA is the tested synthetic-merge commit an EventTrialMerged /
	// EventVerified describes (issue #7) — the SHA a verification commit
	// status posts to. Meaningful only on those two kinds (the same
	// additive, kind-specific pattern as CheckName/HookIndex); "" on every
	// other event, whose merge identity, when it has one, lives on Record.
	MergeSHA string

	// DeployEnv, DeploySHA and DeployedSHA are meaningful only on the three
	// deploy kinds (the same additive, kind-specific pattern as
	// CheckName/HookIndex/MergeSHA) and empty on every other event.
	// DeployEnv is the environment whose lane this is; DeploySHA is the
	// revision being deployed — the desired ref's value, and what the
	// node's own GAUNTLET_DEPLOY_SHA holds; DeployedSHA is the observed
	// ref's value BEFORE this run (GAUNTLET_DEPLOYED_SHA), empty on an
	// environment's first-ever deploy and only then. Carried on every
	// deploy event, terminal or not, so a channel can render the lane and
	// the drift it is closing without holding the terminal record.
	DeployEnv   string
	DeploySHA   string
	DeployedSHA string

	// Deploy is set on EventDeployFinished — the deploy graph run's
	// terminal record — and nil otherwise. Deliberately NOT Record: the
	// consumers that read a non-nil Record as "a finished run, render and
	// persist it" would otherwise have to learn to tell a deploy from a
	// candidate landing, and every one of them predates deploys.
	Deploy *DeployRecord

	Detail string
}

// DeployRecord is the single structured fact produced by one deploy graph
// run — the deploy twin of RunRecord, and the same kind of source of truth:
// history rows, the deploy detail page, and the terminal event all build
// from this one value rather than re-deriving anything.
//
// It is NOT correctness state. The refs remain the ground truth for what an
// environment should and does run (docs/architecture/deployment.md: desired vs
// observed); losing every record costs old detail pages and nothing else,
// which is exactly why retry re-runs the whole graph instead of resuming
// from the per-node rows here.
type DeployRecord struct {
	// Env is the environment; RunID is this graph run's own ID, the same
	// one its events carry.
	Env   string
	RunID string

	// DeploySHA is the revision this run deployed (the desired ref's value
	// when it started); DeployedSHA is the observed ref's value before it,
	// empty on an environment's first-ever deploy. The observed ref
	// advances to DeploySHA only when Outcome is OutcomeLanded.
	DeploySHA   string
	DeployedSHA string

	// Nodes holds one result per DECLARED node of this run's graph, in
	// spec-declaration order, with Seq the 1-based spec position — the same
	// durable per-node identity RunRecord.Checks carries, for the same
	// reason (history's seq column and the log filename prefix key on it).
	// A node that never ran is a CheckBlocked row naming its failed edges,
	// exactly as in a run's record.
	Nodes []CheckResult

	// Outcome reuses the run vocabulary deliberately, one word per lane
	// state an operator can act on: OutcomeLanded — the whole graph
	// finished green (skipped counts green) and the observed ref advanced;
	// OutcomeRejected — a node reported a red verdict, so the lane PARKS at
	// DeploySHA and Culprit names the node; OutcomeError — a daemon-side
	// failure (export, executor unreachable, cancelled slot wait), which
	// parks the same way but is eligible for the standing auto-retry-once
	// budget; OutcomeSkipped — the run was concluded externally (a `cancel`
	// policy desired move, daemon drain), attributing no failure to
	// anything. OutcomeConflict never occurs: a deploy has no trial merge.
	Outcome Outcome

	// Culprit names the node whose non-green result failed the graph — the
	// explicit root failure, in spec order when several finished non-green
	// in the same window, never inferred from whichever row landed last.
	// Empty when Outcome is OutcomeLanded, and when the run was concluded
	// externally with nothing to attribute.
	Culprit string

	// Detail is a human-readable explanation for the cases the rows above
	// can't carry themselves — a spec rejection (no deploy nodes in the
	// revision's tree, an environment naming a node that doesn't exist), a
	// cancellation's reason.
	Detail string

	StartedAt time.Time
	EndedAt   time.Time
}

// Command is an inbound instruction from a Channel (e.g. a Slack reaction
// meaning "retry"). It exists for Invariant 8 (the core is
// executor/channel-agnostic and defines the command vocabulary); no built-in
// channel produces one.
type Revision struct {
	Ref     string `json:"ref"`
	SHA     string `json:"sha"`
	Version string `json:"version"`
}

// Principal holds transport-established identity. Actor remains audit text.
type Principal struct {
	Source          string          `json:"source"`
	ID              string          `json:"id"`
	Authenticated   bool            `json:"authenticated"`
	AllowedByConfig bool            `json:"allowed_by_config"`
	Permission      string          `json:"permission"`
	Teams           map[string]bool `json:"teams"`
}

type Command struct {
	Principal     *Principal `json:"-"`
	RequestID     string
	Actor         string
	Reason        string
	OverridePause bool
	Revisions     []Revision
	Kind          string
	Target        string
	Ref           string
}
