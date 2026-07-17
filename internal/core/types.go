// Package core defines gauntlet's domain vocabulary: the nouns and verbs
// shared by every other package. It holds no behavior and depends on
// nothing but the standard library, so that config, gitx, executor, channel,
// obs, and queue can all depend on it without depending on each other.
package core

import "time"

// Candidate identifies one queue slot. The ref name is the durable identity —
// resubmit is re-pushing the same name, cancellation is deleting it,
// attribution is parsed from it — while SHA is simply what gets tested this
// tick and changes on every re-push.
//
// Ref has the form "refs/heads/for/<target>/<user>/<topic>". User may be ""
// for solo setups where the ref is "refs/heads/for/<target>/<topic>"; see the
// ref grammar in docs/design/core.md ("Candidate ref grammar").
type Candidate struct {
	Ref    string
	Target string
	User   string
	Topic  string
	SHA    string
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
	// RunID is stable for the whole run and shared by every check in it.
	RunID string

	Target string
	Name   string

	// Command is the argv to execute; Command[0] is the program.
	Command []string

	// Executor names the operator-defined execution profile this job runs
	// on (config.Check.Executor, copied verbatim by the queue); "" means
	// the default executor. Routing happens in executor.Mux — the daemon
	// binary's Executor implementation when profiles are configured. The
	// queue itself never interprets the name beyond a config-owned
	// known-profile predicate at spec load (Invariant 8: the core stays
	// executor-agnostic).
	Executor string

	// ImageBuild marks this job as a candidate-image BUILD
	// (config.Image): the executor exports EnvImageResultFile instead of
	// EnvResultFile and returns the file's content verbatim in
	// CheckResult.Image for the queue to validate. Never set together
	// with Image below.
	ImageBuild bool

	// Image, when non-empty, is the captured immutable identity a
	// CONSUMER check runs in — the container executor uses it in place of
	// its profile's static image. Stamped by the queue from the build
	// node's validated result; meaningless to the local executor (specs
	// naming an image are gated onto container profiles at run start).
	Image string

	// Dir is the exported trial tree the check runs against.
	Dir string

	// LogPath, if non-empty, is the file the executor tees this check's
	// combined stdout+stderr to, in full — alongside (never instead of)
	// the tail-capped in-band Output (DESIGN.md "Full per-check log
	// files"). The queue assigns it (Config.LogDir); empty means no file
	// is written at all, which is also the correct fallback when a
	// non-empty LogPath's file can't be created — see CheckResult.LogPath
	// and the executor's handling. Assigning this is purely additive: an
	// executor that doesn't know about it is free to ignore it.
	LogPath string

	// BaseSHA is the target tip the trial merge was built onto.
	BaseSHA string

	// MergeSHA is the tested merge commit (base + Candidate.SHA).
	MergeSHA string

	Candidate Candidate

	// NOTE: Clean is reserved for a future clean-build escape hatch
	// (Invariant 7: a cache-poisoning workaround) — architected for, not
	// triggerable, in the core loop itself. Always false today. See
	// docs/design/core.md ("Deliberately not built").
	Clean bool

	// ServiceEnv is extra environment (GAUNTLET_SVC_<NAME>_HOST/PORT) for
	// this check's resolved `needs`, appended after the built-in
	// GAUNTLET_* vars by every executor. nil for checks with no needs and
	// for hooks.
	//
	// NOTE: hooks cannot declare `needs` at all — internal/hooks builds
	// CheckJob with no needs grammar — a deliberate scope decision. See
	// docs/design/services.md ("Deliberately not built").
	ServiceEnv []string

	// Networks are container networks the check must join to reach its
	// services (ModeNetwork — a shared runtime, e.g. the container
	// executor). The container executor adds one --network per entry; the
	// local executor ignores it (ModePublish reaches the service at
	// 127.0.0.1 instead). nil for no-needs checks, hooks, and publish mode.
	Networks []string
}

// CheckStatus is a check's three-valued verdict.
type CheckStatus int

const (
	// CheckFailed means the check reported a real verdict of "not green":
	// a non-zero exit, or a failure to even start the command (e.g. the
	// command is missing or not executable). Both are the author's problem
	// to fix, so both park the (ref, SHA) rather than retrying forever.
	//
	// CheckFailed is deliberately the zero value: a CheckResult whose Status
	// was never assigned must read as a failure, never as a silent pass.
	CheckFailed CheckStatus = iota

	// CheckPassed means the check exited 0 and did not report skipped.
	CheckPassed

	// CheckSkipped means the check exited 0 and explicitly reported
	// "skipped" via the result file. Distinct from CheckPassed so run
	// history doesn't lie about what actually ran.
	CheckSkipped

	// CheckBlocked means the check never ran because a prerequisite —
	// an `after` edge, or the run failing before this check could start —
	// did not end green (CheckResult.BlockedBy names it). Deliberately
	// distinct from CheckSkipped: skipped is the CHECK's own successful
	// "nothing to do here" verdict and counts green, while blocked is the
	// run telling the truth that this command never executed at all. A
	// blocked result carries no Duration, Output, or start time, and a run
	// containing one is never green — the root failure is the run's
	// rejection cause, recorded explicitly, never inferred from whichever
	// result happened to finish last.
	CheckBlocked
)

// CheckResult is one check's outcome within a run.
//
// Status is meaningful only when Err is nil. Err is reserved for
// daemon-caused, non-verdict failures — context cancellation (the ref or
// target moved mid-check) or executor I/O failure (e.g. could not create a
// temp dir) — never for the checked command itself failing to run or
// exiting non-zero. Those are verdicts (CheckFailed), not Err.
type CheckResult struct {
	Name string

	// Image is the immutable image identity this row is about: for an
	// image-BUILD node, the result-file content the build produced (read
	// back verbatim by the executor, validated by the queue); for a
	// consumer check, the identity it actually ran in (stamped by the
	// queue alongside Command). "" everywhere else. Provenance — history
	// records exactly which bytes ran (issue #2's "explain what ran").
	Image string

	// Seq is the check's 1-based SPEC-DECLARATION position — the durable
	// per-check identity history's seq column and the log filename prefix
	// share. Stamped by the queue when it materializes a run's record
	// (never by executors); 0 means "unknown" (a hand-built result, or a
	// record predating this field), in which case history falls back to
	// the row's slice index — identical whenever the record is a
	// contiguous spec prefix, which every pre-parallelism record was.
	// With parallel checks an externally-concluded run's record can have
	// GAPS (a later check finished while an earlier one was still
	// running when the run was aborted), and only this field keeps the
	// stored seq aligned with the on-disk `<seq>-<name>.log.zst` prefix.
	Seq int

	// Command is the argv that was actually submitted for this check
	// (= CheckJob.Command at the time RunCheck was called), copied onto the
	// result by the queue right after the executor returns (internal/queue/
	// reconcile.go's startCheck) rather than by the Executor implementations
	// themselves. Nil for a result a test builds by hand without going
	// through startCheck (e.g. GatedExecutor.Release's caller-supplied
	// CheckResult) — history/dashboard treat that the same as an old
	// pre-v8 row that predates this field: no command echo rendered.
	Command []string

	Status CheckStatus

	// Output is the check's captured output, tail-capped (64 KiB).
	Output string

	// LogPath is set iff the executor actually wrote the full,
	// uncapped log file at CheckJob.LogPath: empty both when no file was
	// requested (CheckJob.LogPath == "") and when one was requested but
	// couldn't be created (a log-less fallback — losing the log file must
	// never fail the check itself, so that failure is silent here, not an
	// Err). Callers that want the complete record use this path; Output
	// stays the fast tail-capped inline view either way.
	LogPath string

	Duration time.Duration

	// Waited is how long the check sat READY — every `after` prerequisite
	// green, its run below max-parallel — but unable to start because the
	// daemon-wide execution cap had no free slot. Zero when it started
	// immediately (the common case, and always under an unlimited cap).
	// Recorded so an operator can tell capacity starvation from a slow
	// command: a long Duration is the check's own cost, a long Waited is
	// the host's.
	Waited time.Duration

	// BlockedBy, set only when Status is CheckBlocked, names the
	// prerequisite check(s) whose non-green end blocked this one — the
	// direct `after` edges that failed, or, for a check with no failed
	// edge of its own (it was in flight or independent when the run went
	// red), the run's root failing check. Structured rather than prose so
	// history and channels can link the culprit.
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

	// EnvRunID is the run's ID (= CheckJob.RunID), exported so a check's
	// own test harness can namespace shared external services (e.g. a
	// scratch database on a shared SQL Server) per run. This is groundwork
	// for a future shared-services design: concurrent runs — the
	// speculate window, or a batch's members, each of which runs its own
	// checks over the same shared external services — need a token that
	// distinguishes them, and the run ID is the one identity already
	// unique per run that a check couldn't otherwise see.
	EnvRunID = "GAUNTLET_RUN_ID"

	// EnvImageResultFile is the path an IMAGE-BUILD job (CheckJob.
	// ImageBuild) must write its captured immutable image identity to —
	// a local image ID (`sha256:...`, e.g. docker buildx's --iidfile
	// output) or a digest-pinned registry reference
	// (`registry/repo@sha256:...`). Exported INSTEAD of EnvResultFile:
	// builds have no skipped verdict, and the two protocols must not be
	// conflated. A non-zero exit is a build failure regardless of the
	// file; exit 0 with a missing, empty, or mutable (tag-shaped) result
	// is ALSO a build failure — the queue validates the content, the
	// executor only reads it back (CheckResult.Image).
	EnvImageResultFile = "GAUNTLET_IMAGE_RESULT_FILE"

	// EnvGitDir is a git dir (usable as GIT_DIR or `git --git-dir`) holding
	// every object the *_SHA vars above name — the daemon's own bare repo,
	// which contains the trial merge commit whether or not it ever lands.
	// The trial tree itself is exported without a .git (git archive), so
	// this is what lets an affected-only check resolve
	// `git diff $GAUNTLET_BASE_SHA $GAUNTLET_MERGE_SHA`, or derive
	// content-based cache keys (`git log -1 -- <inputs>`), without
	// maintaining its own clone. Read-only by contract: the container
	// executor mounts it :ro at a fixed in-container path; the local
	// executor exports the daemon's live repo path and trusts the check
	// (the same own-developers threat model as everything else). Absent
	// entirely when the executor wasn't told where the repo lives
	// (LocalExecutor.GitDir / executor.Params.GitDir empty — the state of
	// every hand-built executor in tests before this field existed).
	EnvGitDir = "GAUNTLET_GIT_DIR"
)

// Outcome is a run's final disposition.
type Outcome int

const (
	// OutcomeLanded means the merge commit was CAS-pushed to the target
	// and the candidate's slot was CAS-deleted.
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

	// Checks holds each check's result in the order it ran.
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

	// BatchSize is the batch's member count. Construction sites that never
	// touch batching (serial/speculate's tryStartTrial/rejectRun/
	// rejectPreMerge/recoverLanded, internal/queue) leave it at Go's zero
	// value, 0 — NOT 1 — so a consumer reading BatchSize straight off an
	// in-memory RunRecord must gate on BatchID != "" (empty for serial/
	// speculate) rather than assume "0 or 1 means a lone run". Only
	// history.Store normalizes it to 1 at write time (batchSizeOrDefault,
	// internal/history/store.go) for the persisted/queried value — a row
	// read back from history.RunRow.BatchSize (or the dashboard/CLI JSON
	// derived from it) is always >= 1.
	BatchSize int

	// Speculated is true iff this run was tested on a *predicted* base
	// (speculation, a non-head window member) rather than the live target
	// tip. Purely informational for the dashboard; the landed commit is the
	// tested commit either way (Invariant 1).
	Speculated bool

	// Recovered is true iff this record was synthesized by crash recovery
	// (queue/reconcile.go's recoverLanded) rather than produced by an
	// actual trial+check run: cand.SHA was found already landed, so no
	// merge happened and no checks ran here. MergeSHA may still be
	// populated on a Recovered record (recoverLanded best-effort looks up
	// the landing merge that already exists), but that must NOT be read as
	// "checks ran, safe to treat like a normal landing" — internal/hooks's
	// Runner gates its "run hooks vs. emit EventHookSkipped" decision on
	// this field specifically, not on MergeSHA's presence, precisely so a
	// recovered landing never auto-runs hooks (e.g. re-triggering a deploy)
	// merely because its merge SHA happened to be identifiable.
	Recovered bool
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
)

// Event is one notification emitted to a Channel. Terminal events —
// EventLanded, EventRejected, EventTrialConflict, EventSkipped, EventError —
// carry the finished *RunRecord.
type Event struct {
	Kind EventKind
	At   time.Time

	Target    string
	Candidate Candidate

	RunID     string
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
	// EventCheckFinished, or the just-finished hook on EventHookFinished —
	// so channels can render per-check/per-hook verdicts (and durations)
	// mid-run without waiting for the run's terminal RunRecord. nil on
	// every other event kind. Channel implementations must nil-check
	// before dereferencing: older events, and any future EventKind, may
	// carry nil here even on what looks like a finished-check line.
	Check *CheckResult

	// Record is set on terminal events; nil otherwise.
	Record *RunRecord

	Detail string
}

// Command is an inbound instruction from a Channel (e.g. a Slack reaction
// meaning "retry"). It exists for Invariant 8 (the core is
// executor/channel-agnostic and defines the command vocabulary); no built-in
// channel produces one.
type Command struct {
	Kind   string
	Target string
	Ref    string
}
