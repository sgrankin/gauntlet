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
// ref grammar in docs/plans/phase1.md §9.3.
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

	// Clean is reserved for the phase-4 clean-build escape hatch (Invariant
	// 7: a cache-poisoning workaround). Always false in phase 1.
	Clean bool
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

	Status CheckStatus

	// Output is the check's captured output, tail-capped (64 KiB in phase
	// 1; see docs/plans/phase1.md §9.6).
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
// that both the OTel span tree (phase 1) and a future SQLite writer / OTLP
// exporter (phase 3) build from, unchanged.
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
	// grammar) whose target segment names no configured target — a
	// misconfiguration phase 1 silently dropped (docs/plans/phase23.md §10,
	// O4). Emitted once per (ref, SHA), not every tick. Not terminal: it
	// carries no RunRecord, since no run was ever attempted. Channel
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
// executor/channel-agnostic and defines the command vocabulary); no phase-1
// channel produces one.
type Command struct {
	Kind   string
	Target string
	Ref    string
}
