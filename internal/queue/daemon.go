// Package queue is the reconcile loop: the per-target state machine that
// drives a candidate from "queued ref" through trial merge, named checks,
// and land (or park). It knows nothing about how checks run, how events reach a human, or how git
// plumbing works underneath — it sees only core.GitRepo, core.Executor, and
// core.Channel, which is the mechanism for Invariant 8 (executor/channel
// agnosticism).
//
// The reconcile pass (ReconcileOnce) is single-threaded and never overlaps
// itself: the only other goroutines are per-check executor runs, which
// communicate back solely by sending once on a one-shot result channel that
// ReconcileOnce reads non-blockingly. There are no locks; every test can
// therefore control exactly when a pass happens and when each check's
// verdict lands.
package queue

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"text/template"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
	"github.com/sgrankin/gauntlet/internal/services"
)

// Config is the subset of the admin daemon config (internal/config.Daemon)
// the queue itself needs: which targets to reconcile, where each
// candidate's own check spec lives in its trial tree, the committer identity
// for merge commits, and the merge-message template. Remote and Poll are
// cmd-level concerns — dialing the core.GitRepo and driving Run's tick
// channel — the queue is agnostic to both.
type Config struct {
	// Targets are the target branches to reconcile, keyed by name in the
	// candidate ref grammar (see docs/design/core.md, "Candidate ref grammar").
	Targets []config.Target

	// CheckSpec is the path, within each candidate's trial tree, of the
	// repo-side check spec to read and parse (config.ParseChecks).
	CheckSpec string

	// Committer is the identity used for every merge commit.
	Committer core.Identity

	// MergeMessage is the Go text/template subject line for merge commits.
	// Empty selects the built-in default (message.go), which degrades to
	// omit an empty user rather than being used as-is.
	MergeMessage string

	// MergeBody optionally builds a prose body for the merge commit
	// message (internal/summarize, a Claude-written summary of what the
	// branch did), inserted between the subject and the Gauntlet-*
	// trailers. nil disables it entirely — no body paragraph, ever.
	// Called at most once per trial, immediately before
	// buildMergeMessage; its return is trimmed and, if non-empty, becomes
	// the body.
	//
	// This is best-effort by contract: the queue never retries it, never
	// treats an error or empty return as a failure, and never blocks a
	// landing on it — a body-less message is exactly as valid as one with
	// a summary. Bounding the call (a timeout, a real deadline) is the
	// caller's job, not the queue's: passing a ctx with no deadline here
	// would let a hung MergeBody wedge the whole reconcile loop, since
	// ReconcileOnce is never concurrent with itself. cmd's wiring closure
	// is where that policy belongs (queue stays policy-free per this
	// package's own doc comment).
	MergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string

	// WorkDir is the directory trial-tree export dirs are created under:
	// os.MkdirTemp(WorkDir, ...). Empty selects the OS default temp dir —
	// this field only lets an operator (or a test) pin exports to a known,
	// sweepable location; the queue itself never sweeps WorkDir (that's
	// cmd's job).
	WorkDir string

	// LogDir is the directory full per-check log files are written under
	// (DESIGN.md "Full per-check log files"): each check's job gets
	// LogPath = filepath.Join(LogDir, runID, "<seq>-<sanitized-name>.log")
	// — seq is the check's 1-based position in the spec, so two names that
	// sanitize identically never alias onto one file (reconcile.go) —
	// and the executor tees the check's combined output there in addition
	// to the tail-capped in-band CheckResult.Output. Empty disables log
	// files entirely (CheckJob.LogPath stays "" for every check).
	// Unlike WorkDir's trial export
	// dirs, log files under LogDir are never removed by the queue: they
	// outlive their run by design (the dashboard's "full log" link, an
	// API/MCP path) — retention/pruning is a separate mechanism, not the
	// reconcile loop's job.
	LogDir string

	// SeedParks, if non-nil, is consulted once per target — on that
	// target's very first reconcileTarget call this Daemon instance ever
	// makes, never again — to pre-seed d.done (the park list) from run
	// history before that first pass's own pick even happens (park
	// persistence across restarts).
	//
	// This is efficiency state, never correctness state (DESIGN.md's
	// decision ledger, "SQLite for history only", sharpened): Invariant 4's
	// crash recovery already reconstructs every correctness-relevant fact
	// from refs alone, with or without this. What a restart loses today is
	// purely a pointless re-test of a SHA already proven red before the
	// crash — SeedParks exists only to skip that, by asking history
	// (internal/history's LatestTerminalPerRef) for each candidate ref's
	// most recent verdict. Every seed is filtered twice before it can do
	// anything: reconcileTarget only keeps seeds whose Outcome is
	// red-family (Rejected/Conflict/Error) — a landed or skipped ref is
	// never seeded — and then the very next call, syncBookkeeping's
	// existing SHA-currency check, drops any seed whose ref has since
	// vanished or moved to a new SHA, exactly as it already does to a live
	// park on a re-push. A stale or missing db therefore costs at most some
	// avoidable re-tests; it can never manufacture a landing or suppress a
	// real one. nil (the default) disables seeding entirely.
	SeedParks func(target string) []ParkSeed

	// Services is the shared-services pool this daemon consumes for checks
	// declaring `needs` (see docs/design/services.md, "The model: a cache
	// entry, not a supervised unit"). nil ⇒ services
	// disabled entirely: hooks and needs-free checks are unaffected either
	// way, but a check spec that itself declares service/needs is rejected
	// loudly at parse time (config.CheckSpec.RequiresServices, the gating
	// check right after every config.ParseChecks call site) rather than
	// silently running without its dependency.
	Services ServicePool

	// Slots is the daemon-wide execution-capacity semaphore (the operator's
	// `max-executions` cap — see core.Slots): the scheduler TryAcquires one
	// slot per check before starting it and the check's goroutine releases
	// it after executor cleanup, so a ready check on a saturated host
	// simply stays ready (accruing CheckResult.Waited) until a later tick
	// finds a slot. nil means unlimited — the zero-config default and the
	// pre-cap behavior exactly. The SAME instance should be handed to the
	// hooks Runner (and any future image-build machinery) so every bounded
	// execution on the host shares one budget.
	Slots *core.Slots

	// KnownExecutorProfile reports whether a repo-selected executor
	// profile name (config.Check.Executor / config.Image.Executor)
	// resolves to a configured profile. Consulted once per run at spec
	// load, right beside the RequiresServices gate: a spec naming an
	// unknown profile is rejected loudly before any of its commands
	// start — a configuration error, never a red check verdict. nil means
	// no named profiles exist, so only the default ("", never consulted)
	// is legal. This is a predicate, not an executor registry, on
	// purpose: the queue core stays executor-agnostic (Invariant 8) —
	// actual routing lives in the Executor implementation (executor.Mux).
	KnownExecutorProfile func(name string) bool

	// HistoryMtimes enables the deterministic-mtimes pass after every
	// trial export (config's `export { mtimes "history" }`): the exported
	// tree's file timestamps are rewritten to history-derived committer
	// times via GitRepo.RestoreMtimes before any check can observe the
	// tree. A pass failure is an infrastructure error (OutcomeError park)
	// — never a silent wall-clock tree claiming stable-cache metadata.
	HistoryMtimes bool

	// ImageCapableProfile reports whether profile name ("" = the default
	// executor) can run a candidate-built image — i.e. is a container
	// profile. Gated at spec load like KnownExecutorProfile: a check
	// naming an image (config.Check.Image) on a local-kind profile can
	// never work (there is no rootfs to swap), so it rejects before any
	// command starts. nil means no profile can (a daemon wired without
	// this predicate predates or disables candidate images).
	ImageCapableProfile func(name string) bool

	// AutoRetryErrors enables the auto-retry-once behavior (DESIGN.md
	// decision ledger, "Auto-retry once on infra-error parks"; see also
	// docs/design/scaling.md, "The one real prerequisite: auto-requeue on
	// infra errors"): an OutcomeError park is automatically
	// cleared and re-queued exactly once per (ref, SHA) — maybeAutoRetry
	// (autoretry.go), called from every OutcomeError park site in
	// reconcile.go. False is this package's own zero-value default,
	// matching queue's policy-free stance (this package documents defaults,
	// it doesn't opinionate on them): internal/config/daemon.go's
	// Daemon.AutoRetryErrors defaults to true at the config-loading layer,
	// and cmd/gauntlet wires the resolved value straight through.
	AutoRetryErrors bool

	// TrialRefs enables trial-ref publication (issue #7, config's `github
	// { trial-ref-prefix ... }`): after CommitTree, the run's chain-tip
	// merge is CAS-published under an immutable remote ref
	// (TrialRefPrefix/<run-id>) so its MergeSHA is resolvable on the
	// remote and can carry a verification commit status. Off ⇒ today's
	// behavior verbatim (no ref, no EventTrialMerged/EventVerified,
	// candidate-SHA statuses). A publish failure is an infrastructure
	// error (OutcomeError park), never a candidate rejection.
	TrialRefs bool

	// TrialRefPrefix is the ref namespace trial merges publish under —
	// e.g. "refs/gauntlet/trials" (the default; a custom namespace,
	// deliberately NOT refs/heads/**, to avoid UI clutter and workflow
	// triggers — see the issue #7 spike). Each run's ref is
	// TrialRefPrefix + "/" + runID. Meaningful only when TrialRefs is set.
	TrialRefPrefix string

	// TrialRefRetention is how long a NON-landing run's trial ref is kept
	// after its terminal outcome before the reaper CAS-deletes it, so a
	// failed synthetic merge stays inspectable briefly. A landing deletes
	// its ref immediately (the target now reaches the merge). Zero means
	// delete on terminal, no retention. Meaningful only when TrialRefs is
	// set.
	TrialRefRetention time.Duration
}

// ServicePool is the subset of *services.Pool the queue consumes. Its
// blocking methods (EnsureAll/AnyDead) MUST be called only from a
// check-execution goroutine (see docs/design/services.md, "Lifecycle:
// ensure, release, reap") — reconcile.go's startCheck wrapper
// is the only call site; ReconcileOnce/advanceLane/refillLane/startRun never
// call any of these. Safe for concurrent use — *services.Pool satisfies this
// structurally, with no explicit `var _ ServicePool = (*services.Pool)(nil)`
// needed.
type ServicePool interface {
	// EnsureAll resolves every name in needs against svcs to a ready
	// instance, BLOCKING (create + up-to-ReadyTimeout ready-poll). Errors
	// map to CheckResult.Err (park-as-error, never a verdict — see
	// docs/design/services.md, "Failure semantics").
	EnsureAll(ctx context.Context, svcs []config.Service, needs []string) (services.Ensured, error)

	// Release drops one reference per key in e and touches its last-used
	// clock. Never destroys; the reaper does.
	Release(e services.Ensured)

	// AnyDead probe-alives every instance in e, BLOCKING. Callers MUST call
	// this only on a FAILED check — a passing check never re-probes.
	AnyDead(ctx context.Context, e services.Ensured) bool

	// ArmReaper marks the pool's reaper live (idempotent) — called once,
	// after the first full ReconcileOnce pass completes.
	ArmReaper()
}

// ParkSeed is one candidate ref's park state as derived from run history at
// boot (Config.SeedParks). Fields mirror parkEntry's own shape; exported so
// cmd's history-backed SeedParks closure can build one without importing
// any unexported queue type.
type ParkSeed struct {
	Ref     string
	SHA     string
	Outcome core.Outcome
	Reason  string
	At      time.Time

	// RunID is the terminal run that produced this verdict, mirrored
	// straight through to parkEntry.RunID (seedParksOnce) — "" for history
	// predating this field, in which case the seeded park simply renders
	// unlinked on the dashboard, same as any other RunID-less park.
	RunID string
}

// checkInFlight is one currently-running check within an in-flight run: its
// cancel func (for a ref/target move, Invariant 5) and the one-shot channel
// its executor goroutine reports back on. A run holds up to
// run.maxParallel of these at once (run.inflight).
type checkInFlight struct {
	name   string
	cancel context.CancelFunc
	result chan core.CheckResult
	span   trace.Span
	start  time.Time

	// waited is how long the check sat ready but slotless before this
	// start (run.readyAt) — stamped onto CheckResult.Waited when the
	// result is consumed, so history can tell capacity starvation from a
	// slow command.
	waited time.Duration
}

// runVerdict is a run's aggregate check-verdict-so-far, set by
// advanceChecks and consumed by advanceLane's bubble (reject/error) and
// prefix-land (green) steps — state advanceLane can read back after
// advanceChecks returns.
type runVerdict int

const (
	verdictNone     runVerdict = iota // still running, or advanced to the next check this tick
	verdictGreen                      // every check Passed/Skipped; ready to land
	verdictRejected                   // a check Failed
	verdictErrored                    // a check reported CheckResult.Err
)

// runMember is one candidate within a run and its chain link. len(run.members)
// is 1 for serial/speculate; batch grows it to N — up to Target.MaxBatch
// chained links, one check suite over the chain tip's tree.
type runMember struct {
	cand     core.Candidate
	mergeOID string          // this member's own --no-ff link (== run.chainTip for len(members)==1)
	rec      *core.RunRecord // per-member terminal record
}

// run is the daemon's entire in-flight state for one run within a target's
// lane (Invariant 4: "in-flight state is (slot, tested SHA, executor
// run-id)"). It is reconstructible from ground truth on every tick except
// for inflight, which is the one piece of state that can't be rederived
// without rerunning checks — exactly why losing it (a crash) costs at most
// a rerun, never correctness.
type run struct {
	target    string
	members   []runMember // len 1 for serial/speculate; up to Target.MaxBatch for batch
	baseOID   string      // target tip (or, non-head speculate, a predicted predecessor chainTip) this run's chain was built onto
	chainTip  string      // the tested merge commit == members[len-1].mergeOID (== members[0].mergeOID for len(members)==1)
	predicted bool        // true iff baseOID is an unpushed predicted commit (speculate, non-head); always false for serial/batch and speculate's own head run
	batchID   string      // "" unless batch; shared across member records (runID reused verbatim)
	runID     string
	dir       string // exported trial tree; removed on every terminal transition
	checks    []config.Check

	// maxParallel is how many of this run's checks may be in flight at
	// once — the spec's max-parallel with zero already normalized to 1 at
	// run construction, so schedulers never re-interpret the default. At 1
	// with no `after` edges, scheduling degenerates to the pre-parallelism
	// contract exactly: one check at a time, in declaration order.
	maxParallel int

	// inflight holds every currently-running check by name; empty both
	// before the first start and once the verdict is determined. Mutated
	// only on the reconcile goroutine (the per-check goroutines communicate
	// solely via their one-shot result channels).
	inflight map[string]*checkInFlight

	// results holds every finished check's result by name. rec.Checks is
	// deliberately NOT built incrementally from these: materializeChecks
	// fills every member's record once, in spec-declaration order — the
	// durable per-check identity history/seq/log filenames key on — when
	// the run concludes (green, red, or cancelled).
	results map[string]core.CheckResult

	// readyAt stamps when a ready check first found no free execution slot
	// (Config.Slots exhausted), so its eventual CheckResult.Waited can
	// report capacity starvation. Entries are removed when the check
	// starts; a check that starts immediately never appears here.
	readyAt map[string]time.Time

	// culprit is the first check (in spec-consumption order) to finish
	// red or errored — the run's explicit root failure. "" while the run
	// is healthy. Set once; never inferred from whichever result happened
	// to land last (parallel completion makes that ordering meaningless).
	culprit string

	// imageRefs maps image name -> the build node's VALIDATED immutable
	// identity (config.CheckSpec.Images; nil when the spec declares
	// none). Written when an "image:<name>" node's result is consumed
	// green, read by startCheck to stamp consumer jobs. A consumer can
	// only become ready once its implicit edge onto the build node is
	// green, so a ready consumer always finds its ref here.
	imageRefs map[string]string

	// materialized guards materializeChecks's one-time fill of the member
	// records' Checks slices.
	materialized bool

	// services is spec.Services verbatim — set once in startRun/
	// finishBatchStart, read-only for the rest of the run's life: no
	// cross-goroutine mutation, no race. startCheck's per-check goroutine
	// reads it (alongside the started check's own Needs) to resolve
	// `needs` against declared Service specs.
	services []config.Service

	verdict runVerdict // set by advanceChecks, consumed by advanceLane

	// verifiedEmitted guards the once-per-run EventVerified emit (the
	// verdict-goes-green transition, issue #7): advanceChecks can re-run
	// the green-detection block on a tick where the run isn't landed the
	// same pass, so the emit is edge-triggered, not level.
	verifiedEmitted bool

	// trialRef, when non-empty, is the published immutable remote ref
	// naming this run's chain-tip merge (issue #7's trial-ref publication,
	// Config.TrialRefs). Set once in startRun/finishBatchStart after the
	// CAS-create push confirms; CAS-deleted at land (landRun) and, for a
	// non-landing terminal, retained for Config.TrialRefRetention then
	// reaped. Empty when the feature is off or the merge never published.
	trialRef string

	rootCtx  context.Context
	rootSpan trace.Span
}

// lane is a target's in-flight pipeline, FIFO: runs[0] is the head (next to
// land). Serial and batch hold ≤ 1 run; speculate grows it up to
// Target.Window. A nil/absent lane, or one with an empty runs slice, is an
// idle target — reconstructible from refs every tick, no durable state.
type lane struct {
	runs []*run
}

// Daemon is the reconcile loop over N target branches on one core.GitRepo.
// The zero value is not usable; construct with New.
type Daemon struct {
	git   core.GitRepo
	exec  core.Executor
	chans []core.Channel
	tr    trace.Tracer
	cfg   Config
	now   func() time.Time

	// order assigns each candidate ref (per target) a monotonically
	// increasing sequence number the first time it's observed — the FIFO
	// key, tie-broken lexically by ref. done is a park list, sticky per
	// (ref, SHA): entries clear only when the
	// ref's SHA changes, the ref vanishes, or a CommandRetry clears it
	// explicitly (command.go), never when some other candidate lands. Both
	// are keyed by target name, then by ref, and are fully reconstructible
	// from ground truth — losing them (a restart) costs at most some
	// re-tests, never correctness.
	order map[string]map[string]int64
	done  map[string]map[string]parkEntry
	seq   int64

	// autoRetried is the once-per-(ref,SHA) auto-retry budget for
	// OutcomeError parks (autoretry.go's maybeAutoRetry):
	// target -> ref -> the SHA already auto-retried. Same shape and same
	// reconstructible-after-restart argument as done above — losing this
	// (a restart) only re-grants one already-spent auto-retry per
	// still-parked ref, never an unbounded retry loop. syncBookkeeping
	// prunes it in lockstep with done: a vanished ref or a moved SHA drops
	// its entry, so a new SHA on the same ref always gets a fresh budget.
	autoRetried map[string]map[string]string

	// ignoredRefs dedupes core.EventIgnoredRef: ref -> last-emitted-for SHA,
	// pruned of vanished refs every tick
	// so it can't grow without bound over a long-running daemon's lifetime.
	ignoredRefs map[string]string

	// lanes holds each target's in-flight pipeline. A nil/absent entry, or
	// one whose runs slice is empty, is an idle target.
	lanes map[string]*lane

	// batchFallback is batch mode's in-memory red-recovery flag (see
	// docs/design/queue-modes.md, "Red-recovery: serial fallback"):
	// target -> true while a prior batch run for it went
	// red and no landing has occurred since. refillLane consults it to
	// force the next refill into refillSerialOne (one candidate at a time,
	// normal single-culprit park semantics) instead of refillBatch;
	// landRun deletes the entry on any successful landing for that target,
	// resuming batching. Reconstructible in the sense that losing it (a
	// restart) costs at most one extra batch-red round before the culprit
	// is found again — never a correctness issue (no ref reflects this
	// flag; it's pure scheduling policy, not state any Invariant depends
	// on).
	batchFallback map[string]bool

	// seeded marks, per target, whether Config.SeedParks has already been
	// consulted for it: set true the first time reconcileTarget
	// runs for that target, regardless of whether SeedParks is nil or
	// returned anything — so seeding is attempted at most once per target
	// per Daemon lifetime, never on a later restart-free reconcile pass.
	seeded map[string]bool

	// reaperArmed marks whether Config.Services.ArmReaper has already been
	// called: set true at the
	// end of the first ReconcileOnce pass that completes a full sweep over
	// every target, never again. Left false forever when Config.Services is
	// nil. Unlike seeded (per-target), this is once per Daemon lifetime,
	// full stop — the reaper must not run until every target's in-flight
	// work from before a restart has had one whole pass to re-ensure (and
	// so refcount) whatever it still needs (see docs/design/services.md,
	// "Lifecycle: ensure, release, reap").
	reaperArmed bool

	// landedPins maps chain-tip merge OIDs to their target's ref name for
	// runs whose GC pin (core.GitRepo.Pin) must outlive the run itself:
	// landings (the chain becomes locally reachable through
	// refs/remotes/origin/<branch> only once a Fetch reflects the push, and
	// post-land hooks may export the merge from a backlog well after
	// landing) and ambiguous target-push failures (the push may have taken
	// effect server-side despite the client-visible error — landRun's
	// Skip-not-park rationale). landRun registers the pin here instead of
	// letting finalizeRun release it; ReconcileOnce releases an entry only
	// once the tick's fetched target ref actually REACHES the tip
	// (IsAncestor) — "a Fetch succeeded" alone isn't the anchor, e.g. a
	// lagging read replica can serve the pre-push tip. An entry that never
	// anchors (a genuinely failed ambiguous push, a target force-pushed to
	// discard its landing) is deliberately retained: one stale ref per rare
	// event, swept — like every crash-stranded pin — by the next startup's
	// pin sweep (Invariant 4: stranded pins protect nothing a fresh process
	// still needs). In-memory only, reconstructible in that same weak sense.
	landedPins map[string]string

	// trialReap holds published trial refs (issue #7) awaiting deletion
	// after their run's non-landing terminal — keyed by ref name, valued
	// by the merge SHA the ref names and the instant it may be reaped
	// (now + Config.TrialRefRetention). reapTrialRefs CAS-deletes each on
	// or after its instant, keyed on the stored SHA so a ref recreated or
	// changed by another daemon/operator is never removed. A landing
	// deletes its ref immediately (landRun) and never enters here.
	// In-memory only: crash orphans are swept at boot by cmd, mirroring
	// the pin sweep.
	trialReap map[string]trialReapEntry

	// idleSince is buildSnapshot's own tracked idle-transition instant (see
	// docs/design/scaling.md, "Axis 2 — park the builder"; and
	// Snapshot.IdleSince's own doc): zero while the
	// queue is busy, stamped with the tick's snap.At the moment every target
	// goes idle, and held steady across however many idle ticks follow.
	// Reconcile-goroutine-only, like order/done/lanes above — buildSnapshot
	// is the sole reader and writer, and it only ever runs there.
	idleSince time.Time

	// tick counts completed ReconcileOnce passes, solely to rotate the
	// target-iteration starting offset when a daemon-wide execution cap is
	// configured (see ReconcileOnce): under slot contention, a fixed
	// config-order iteration would let the first target's ready checks
	// permanently absorb every freed slot. Uncapped daemons never rotate,
	// keeping event ordering byte-identical to the pre-cap behavior.
	tick int64

	// snap holds the most recently published Snapshot; nil until the first
	// successful ReconcileOnce pass completes.
	snap atomic.Pointer[Snapshot]
}

// Snapshot returns the most recently published Snapshot, or nil if no
// ReconcileOnce pass has completed yet. Safe for concurrent use from any
// goroutine — the dashboard and history depth-sampler's intended callers.
func (d *Daemon) Snapshot() *Snapshot { return d.snap.Load() }

// New constructs a Daemon. now is injected so tests can control run-ID
// timestamps deterministically; a nil now defaults to time.Now.
func New(git core.GitRepo, exec core.Executor, chans []core.Channel, cfg Config, now func() time.Time) (*Daemon, error) {
	if git == nil {
		return nil, fmt.Errorf("queue: git repo is required")
	}
	if exec == nil {
		return nil, fmt.Errorf("queue: executor is required")
	}
	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("queue: at least one target is required")
	}
	if cfg.CheckSpec == "" {
		return nil, fmt.Errorf("queue: check spec path is required")
	}
	// config.LoadDaemon validates the committer too, but queue.Config is a
	// distinct type any caller can assemble by hand; without this check an
	// empty identity would surface only at the first CommitTree call.
	if cfg.Committer.Name == "" || cfg.Committer.Email == "" {
		return nil, fmt.Errorf("queue: committer identity (name and email) is required")
	}
	if cfg.MergeMessage != "" {
		if _, err := template.New("merge-message").Parse(cfg.MergeMessage); err != nil {
			return nil, fmt.Errorf("queue: merge-message template: %w", err)
		}
	}
	seen := make(map[string]bool, len(cfg.Targets))
	for _, t := range cfg.Targets {
		if t.Name == "" || t.Branch == "" {
			return nil, fmt.Errorf("queue: target must have both name and branch")
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("queue: duplicate target %q", t.Name)
		}
		seen[t.Name] = true
	}
	if now == nil {
		now = time.Now
	}

	return &Daemon{
		git:           git,
		exec:          exec,
		chans:         chans,
		tr:            obs.Tracer(),
		cfg:           cfg,
		now:           now,
		order:         make(map[string]map[string]int64),
		done:          make(map[string]map[string]parkEntry),
		autoRetried:   make(map[string]map[string]string),
		ignoredRefs:   make(map[string]string),
		lanes:         make(map[string]*lane),
		batchFallback: make(map[string]bool),
		seeded:        make(map[string]bool),
		landedPins:    make(map[string]string),
		trialReap:     make(map[string]trialReapEntry),
	}, nil
}

// trialReapEntry is one retained trial ref: the merge SHA it names (the
// CAS-delete key) and the earliest instant it may be reaped.
type trialReapEntry struct {
	sha string
	at  time.Time
}

// headRun returns the head run of target's lane (lane.runs[0]) — the run a
// single in-flight-run map lookup would have returned pre-lane-refactor —
// or nil if the target is idle (lane nil/absent or empty). Serial/batch
// hold at most one run, so this is the whole lane; speculate's window
// makes it "next to land."
func (d *Daemon) headRun(target string) *run {
	l := d.lanes[target]
	if l == nil || len(l.runs) == 0 {
		return nil
	}
	return l.runs[0]
}

// Run drives the reconcile loop until ctx is done or tick is closed, calling
// ReconcileOnce once per tick. A ReconcileOnce error is reported as a
// channel EventError (it is not target-specific, so it carries no
// Candidate) and does not stop the loop — the next tick tries again.
//
// Run performs one extra ReconcileOnce immediately, before ever waiting on
// tick: without it, park-seeding, discovery, and command draining would sit
// idle for up to a full poll interval after every restart for no reason —
// tick's first value is otherwise cfg.Poll away. Errors from this initial
// pass are reported exactly like a tick's (an EventError, loop keeps going);
// ctx.Done() firing before it completes still returns ctx.Err() as normal.
func (d *Daemon) Run(ctx context.Context, tick <-chan time.Time) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := d.ReconcileOnce(ctx); err != nil {
		d.emit(ctx, core.Event{Kind: core.EventError, At: d.now(), Detail: err.Error()})
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-tick:
			if !ok {
				return nil
			}
			if err := d.ReconcileOnce(ctx); err != nil {
				d.emit(ctx, core.Event{Kind: core.EventError, At: d.now(), Detail: err.Error()})
			}
		}
	}
}

// ReconcileOnce runs one full, non-blocking reconcile pass over every
// target: Fetch, ListRefs (the tick's snapshot of ground truth), seed each
// target's park list from Config.SeedParks exactly once (seeded first,
// before draining commands — see below), drain inbound commands, flag any
// candidate ref naming an unconfigured target, then per-target
// state-machine advancement (reconcile.go), and finally publish a Snapshot
// of the resulting state. See docs/design/core.md ("The reconcile pass")
// for the full mechanism.
//
// Seeding runs before drainCommands, not after (as it did when it lived
// inside reconcileTarget, called only once drainCommands had already
// returned for the whole tick). seedParksOnce itself is idempotent per
// target regardless of order, but a first-tick operator CommandCancel and a
// first-tick seed can both name the very same ref — whichever of the two
// writes d.done[target][ref] LAST wins, and only drainCommands's write
// carries the "cancelled by operator" Detail (cancelDetail, command.go)
// that provenance depends on. Seeding first means any first-tick cancel is
// applied afterward and so always wins.
//
// On an early error (Fetch or ListRefs failing) ReconcileOnce returns before
// seeding, draining commands, checking ignored refs, reconciling any
// target, or publishing a snapshot — the previously published Snapshot (if
// any) stays current, its staleness visible via Snapshot().At.
//
// ctx is also the parent for any run's root span and, transitively (via
// context.WithCancel), for the context each check goroutine runs under —
// which is why a run started in one ReconcileOnce call must keep working
// correctly when a *later* call passes a different ctx value: only ctx's
// cancellation (not its identity) matters for children the run already
// started. Callers that want in-flight checks to survive across ticks
// (every production caller, via Run) should pass a ctx that isn't cancelled
// between ticks; only cancel it to shut the whole daemon down.
func (d *Daemon) ReconcileOnce(ctx context.Context) error {
	if err := d.git.Fetch(ctx); err != nil {
		return fmt.Errorf("queue: fetch: %w", err)
	}
	refs, err := d.git.ListRefs(ctx)
	if err != nil {
		return fmt.Errorf("queue: list refs: %w", err)
	}

	// Release each deferred pin (landedPins) whose landing this tick's
	// fetched ground truth now anchors: the tip must be REACHABLE from the
	// target's remote-tracking ref, not merely fetched-after — a lagging
	// replica can serve the pre-push tip, and an ambiguous push failure may
	// never have landed at all. Unanchored entries simply wait (or, if they
	// never anchor, ride until startup's sweep — see the field's doc); an
	// IsAncestor error likewise just retries next tick.
	for oid, refName := range d.landedPins {
		tip, ok := refs[refName]
		if !ok {
			continue
		}
		if anchored, err := d.git.IsAncestor(ctx, oid, tip); err != nil || !anchored {
			continue
		}
		d.unpin(ctx, oid)
		delete(d.landedPins, oid)
	}

	for _, t := range d.cfg.Targets {
		d.seedParksOnce(t.Name)
	}
	d.drainCommands(ctx, refs)
	d.checkIgnoredRefs(ctx, refs)

	// Per-tick rotation of the target starting offset — but only under a
	// configured execution cap, where it matters: when slots are scarce,
	// whichever target is visited first grabs the freed ones, and a fixed
	// order would starve later targets ("no specific fairness algorithm is
	// prescribed, but one large graph must not permanently consume every
	// released slot"). Uncapped daemons keep the fixed config order and
	// its byte-identical event stream.
	d.tick++
	off := 0
	if d.cfg.Slots != nil {
		off = int(d.tick % int64(len(d.cfg.Targets)))
	}
	for i := range d.cfg.Targets {
		d.reconcileTarget(ctx, d.cfg.Targets[(off+i)%len(d.cfg.Targets)], refs)
	}

	// Reap trial refs whose retention window elapsed (issue #7). No-op
	// (and no remote round trip) unless the feature is on and something is
	// actually due.
	d.reapTrialRefs(ctx)

	// Arm the services reaper once this pass has swept every target: by
	// now, any in-flight work recovered from a restart has had this
	// whole pass to re-ensure (and so refcount) everything it still needs,
	// so the reaper can never race a just-recovered instance out from under
	// it. No-op forever when Config.Services is nil.
	if !d.reaperArmed && d.cfg.Services != nil {
		d.cfg.Services.ArmReaper()
		d.reaperArmed = true
	}

	d.snap.Store(d.buildSnapshot(refs))
	return nil
}

// emit reports ev to every configured channel, in order. Channel.Emit must
// not block the reconcile loop (its contract), so a slow/misbehaving
// channel can't wedge this loop — but an error is never silently
// discarded: most channels' Emit never fails
// (log, dashboard's no-op, slack's non-blocking outbox), but
// history.Store.Emit is a real synchronous sqlite write and can return a
// real error — e.g. a hook_runs FK violation, if d.chans is ever
// constructed with the hooks Runner ahead of history (see chans'
// construction in cmd/gauntlet/main.go for why that ordering is
// load-bearing). A durability-marker write failing silently would be a
// crash-discoverability gap, so it's logged instead.
// One unrate-limited log line is fine: a channel Emit failing is not
// expected in steady state, so unlike per-check output this can't itself
// become the noise problem.
func (d *Daemon) emit(ctx context.Context, ev core.Event) {
	for _, ch := range d.chans {
		if err := ch.Emit(ctx, ev); err != nil {
			fmt.Fprintf(os.Stderr, "queue: channel emit error (kind=%d target=%s run=%s): %v\n", ev.Kind, ev.Target, ev.RunID, err)
		}
	}
}
