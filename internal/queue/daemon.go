// Package queue reconciles candidate revisions, runs checks, and lands
// verified history. ReconcileOnce is single-threaded. Check workers return
// results through channels; snapshots and drain requests provide the
// concurrent interfaces.
package queue

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/flaky"
	"github.com/sgrankin/gauntlet/internal/obs"
	"github.com/sgrankin/gauntlet/internal/policy"
	"github.com/sgrankin/gauntlet/internal/services"
)

// Config supplies queue policy and dependencies. Remote connection and
// polling belong to the command package.
type Config struct {
	Policy         *policy.Engine
	CircuitBreaker *config.CircuitBreaker
	SourceGitDir   string
	ControlPath    string
	AllowEmergency bool
	// FailureReview retries selected failed checks before publishing a red result.
	FailureReview *flaky.Retrier

	// Reviews is an optional forge admission adapter. Its snapshots augment
	// ordinary queue refs; the core retains the same scheduler and CAS land.
	Reviews core.ReviewSource
	// Targets are the target branches to reconcile, keyed by name in the
	// candidate ref grammar (see docs/architecture/queue.md, "Candidate ref grammar").
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

	// MergeBody supplies an optional legacy merge message body. Empty results
	// do not fail a run; the caller must bound this synchronous operation with
	// a deadline.
	MergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string

	// WorkDir holds temporary trial exports. Empty uses the OS temp directory;
	// startup cleanup belongs to the command package.
	WorkDir string

	// LogDir holds full check logs named by run, spec position, and sanitized
	// check name. Empty disables file logging. Retention is managed outside
	// the queue.
	LogDir string

	// SeedParks restores red verdicts once per target at startup. Seeds are
	// discarded when the candidate revision or metadata changes. A missing
	// history store only causes retesting.
	SeedParks func(target string) []ParkSeed

	// Services resolves check needs. Nil rejects specs declaring services or
	// needs.
	Services ServicePool

	// Slots limits simultaneous executions. Ready checks wait when full;
	// workers release their slots after cleanup. Nil is unlimited. Share it
	// with hooks and deployment runners.
	Slots *core.Slots

	// KnownExecutorProfile validates named profiles before commands run. Nil
	// permits only the default profile.
	KnownExecutorProfile func(name string) bool

	// HistoryMtimes restores history-derived timestamps after export. Failure
	// is an infrastructure error.
	HistoryMtimes bool

	// ImageCapableProfile identifies profiles that can run candidate-built
	// images. Nil permits none.
	ImageCapableProfile func(name string) bool

	// AutoRetryErrors grants one retry per ref/SHA for infrastructure
	// failures. Loaded daemon configs default it to true; the queue's zero
	// value is false.
	AutoRetryErrors bool

	// TrialRefs publishes the tested tip before checks, making it resolvable
	// for remote verification statuses. Publication failure is an
	// infrastructure error.
	TrialRefs bool

	// TrialRefPrefix names the namespace for TrialRefs: <prefix>/<run-id>. It
	// must not overlap candidate refs.
	TrialRefPrefix string

	// TrialRefRetention keeps failed trial refs inspectable until the reaper
	// removes them. Zero deletes on termination; landed refs are deleted
	// immediately.
	TrialRefRetention time.Duration

	// ReceiptNotes requires a receipt node and publishes its payload before
	// the target CAS. Nil disables publication and rejects specs with
	// receipts.
	ReceiptNotes *config.ReceiptNotes
}

// ServicePool resolves and releases shared services. Blocking methods must
// run in check workers, never on the reconcile loop. Implementations must
// be safe for concurrent use.
type ServicePool interface {
	// EnsureAll resolves every name in needs against svcs to a ready
	// instance, BLOCKING (create + up-to-ReadyTimeout ready-poll). Errors
	// map to CheckResult.Err (park-as-error, never a verdict — see
	// docs/architecture/services.md, "Failure semantics").
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
	Version string
	Ref     string
	SHA     string
	Outcome core.Outcome
	Reason  string
	At      time.Time

	// RunID links the park to its terminal history record. Empty for older
	// history.
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
	emergencyReason     string
	pauseOverrideReason string
	controlPrepared     bool
	overridePause       bool
	releaseSources      func()

	target    string
	members   []runMember // len 1 for serial/speculate; up to Target.MaxBatch for batch
	baseOID   string      // target tip (or, non-head speculate, a predicted predecessor chainTip) this run's chain was built onto
	chainTip  string      // the tested merge commit == members[len-1].mergeOID (== members[0].mergeOID for len(members)==1)
	chainTree string      // the exact tested tree OID (chainTip^{tree}); what shared-mode export and isolated materialization both archive — NOT the commit, to avoid export-subst divergence
	predicted bool        // true iff baseOID is an unpushed predicted commit (speculate, non-head); always false for serial/batch and speculate's own head run
	batchID   string      // "" unless batch; shared across member records (runID reused verbatim)
	runID     string
	dir       string // exported trial tree; removed on every terminal transition
	checks    []config.Check

	// maxParallel bounds running checks; defaults are resolved when the run is
	// created.
	maxParallel int

	// inflight is owned by the reconcile loop. Workers send results through
	// the stored channels.
	inflight map[string]*checkInFlight

	// results holds completed checks. materializeChecks writes them to member
	// records in spec order once the run concludes.
	results map[string]core.CheckResult

	// readyAt stamps when a ready check first found no free execution slot
	// (Config.Slots exhausted), so its eventual CheckResult.Waited can
	// report capacity starvation. Entries are removed when the check
	// starts; a check that starts immediately never appears here.
	readyAt map[string]time.Time

	// culprit is the first failure consumed in spec order. It is set once,
	// independently of worker completion order.
	culprit string

	// imageRefs holds validated build identities. Consumer dependency edges
	// ensure each identity exists before use.
	imageRefs map[string]string

	// materialized guards materializeChecks's one-time fill of the member
	// records' Checks slices.
	materialized bool

	// receiptPayload and receiptProducer hold a validated receipt until
	// landing. A speculative run may finish validation without ever
	// publishing.
	receiptPayload  []byte
	receiptProducer string

	// receiptBlobSHA and receiptPublished are filled by landRun immediately
	// after a successful PublishNote call, mirroring onto every member's
	// RunRecord (core.RunRecord.ReceiptBlob/ReceiptPublished) right before
	// each EventLanded — see landRun for the exact sequencing.
	receiptBlobSHA   string
	receiptPublished string

	// services is the immutable spec snapshot shared with check workers.
	services []config.Service

	// isolated gives each node a private export of chainTree. In that mode dir
	// is empty.
	isolated bool

	verdict runVerdict // set by advanceChecks, consumed by advanceLane

	// verifiedEmitted makes the all-green event edge-triggered across repeated
	// ticks.
	verifiedEmitted bool

	// trialRef names the published tested tip. It is deleted on landing or
	// retained for the configured failure interval.
	trialRef string

	rootCtx  context.Context
	rootSpan trace.Span
}

// receiptPublishedFresh and receiptPublishedAlready are run.receiptPublished's
// (and core.RunRecord.ReceiptPublished's) small vocabulary — landRun's own
// distinction between a freshly created note commit and PublishNote's
// idempotent AlreadyPublished outcome (issue #13). "" (the zero value) means
// "no receipt-notes policy, or the run never reached landing".
const (
	receiptPublishedFresh   = "published"
	receiptPublishedAlready = "already-present"
)

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
	controls controlState
	external map[string]core.Candidate
	git      core.GitRepo
	exec     core.Executor
	chans    []core.Channel
	tr       trace.Tracer
	cfg      Config
	now      func() time.Time

	// order assigns FIFO sequence numbers; done holds parks by target and ref.
	// Parks clear on revision/metadata changes or retry. Ordinary refs also
	// clear on deletion; inactive reviews keep their failure verdict.
	order map[string]map[string]int64
	done  map[string]map[string]parkEntry
	seq   int64

	// autoRetried records the ref/SHA pairs that spent their infrastructure
	// retry. Restarting renews that budget.
	autoRetried map[string]map[string]string

	// ignoredRefs dedupes core.EventIgnoredRef: ref -> last-emitted-for SHA,
	// pruned of vanished refs every tick
	// so it can't grow without bound over a long-running daemon's lifetime.
	ignoredRefs map[string]string

	// lanes holds each target's in-flight pipeline. A nil/absent entry, or
	// one whose runs slice is empty, is an idle target.
	lanes map[string]*lane

	// batchFallback selects serial retries after a red batch, until the next
	// successful landing.
	urgentBurst   map[string]int
	batchRecovery map[string]int
	batchFallback map[string]bool

	// seeded prevents reading park history more than once per target.
	seeded map[string]bool

	// reaperArmed delays service reaping until the first complete pass has
	// restored active references.
	reaperArmed bool

	// landedPins retains trial tips after successful or ambiguous target
	// pushes. Release only after fetch proves target reachability; lagging
	// replicas are not sufficient. Unresolved pins survive until the startup
	// sweep.
	landedPins map[string]string

	// trialReap holds failed trial refs until their retention deadline.
	// Deletion uses the recorded SHA; startup sweeps crash orphans.
	trialReap map[string]trialReapEntry

	// idleSince tracks the queue's transition to idle; only buildSnapshot
	// reads and writes it.
	idleSince time.Time

	// tick rotates target admission under slot contention, preventing config-
	// order starvation.
	tick int64

	// snap holds the most recently published Snapshot; nil until the first
	// successful ReconcileOnce pass completes.
	snap atomic.Pointer[Snapshot]

	// Drain requests are mutex-protected; syncDrainRequest transfers them to
	// reconcile-owned state. Draining stops admission and automatic retries,
	// leaving a finite active set.
	drainReqMu    sync.Mutex
	drainReq      bool
	drainReqDL    time.Time
	draining      bool
	drainSince    time.Time
	drainDeadline time.Time
}

// Snapshot returns the most recently published Snapshot, or nil if no
// ReconcileOnce pass has completed yet. Safe for concurrent use from any
// goroutine — the dashboard and history depth-sampler's intended callers.
func (d *Daemon) Snapshot() *Snapshot { return d.snap.Load() }

// New constructs a Daemon. now is injected so tests can control run-ID
// timestamps deterministically; a nil now defaults to time.Now.
func New(git core.GitRepo, exec core.Executor, chans []core.Channel, cfg Config, now func() time.Time) (*Daemon, error) {
	if cfg.Policy == nil {
		cfg.Policy = policy.Default()
	}
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
		if t.Landing == "squash" {
			if _, ok := git.(core.LinearGitRepo); !ok {
				return nil, fmt.Errorf("queue: linear git backend required for target %q", t.Name)
			}
		}
		if cfg.Reviews != nil && t.Landing != "squash" {
			return nil, fmt.Errorf("queue: review sources require squash landings")
		}
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

	controls, err := loadControls(cfg.ControlPath)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		controls:      controls,
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
		urgentBurst:   make(map[string]int),
		batchRecovery: make(map[string]int),
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
			// A graceful drain (issue #8) exits cleanly the moment its
			// finite admitted set has emptied — no force, no lost work.
			// cmd then drains the hook backlog and tears down in order.
			if d.drainComplete() {
				return nil
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
// of the resulting state. See docs/architecture/queue.md ("The reconcile pass")
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
	// Fold any external drain request into the tick's live state first, so
	// admission gating and this tick's Snapshot agree (issue #8).
	d.syncDrainRequest()
	if err := d.git.Fetch(ctx); err != nil {
		return fmt.Errorf("queue: fetch: %w", err)
	}
	d.external = make(map[string]core.Candidate)
	if d.cfg.Reviews != nil {
		candidates, err := d.cfg.Reviews.Candidates(ctx)
		if err != nil {
			return fmt.Errorf("queue: review admission: %w", err)
		}
		for _, c := range candidates {
			if c.Source == "" || c.Ref == "" || c.SHA == "" {
				return fmt.Errorf("queue: incomplete review candidate")
			}
			d.external[c.Ref] = c
		}
	}
	refs, err := d.git.ListRefs(ctx)
	if err != nil {
		return fmt.Errorf("queue: list refs: %w", err)
	}
	for ref, c := range d.external {
		if _, exists := refs[ref]; exists {
			return fmt.Errorf("queue: review slot collides with remote ref %s", ref)
		}
		refs[ref] = c.SHA
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
	d.observeInfrastructure(ev)
	if feedback, ok := d.cfg.Reviews.(interface {
		Feedback(context.Context, core.Event) error
	}); ok {
		fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := feedback.Feedback(fctx, ev)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "queue: review feedback: %v\n", err)
		}
	}

	for _, ch := range d.chans {
		if err := ch.Emit(ctx, ev); err != nil {
			fmt.Fprintf(os.Stderr, "queue: channel emit error (kind=%d target=%s run=%s): %v\n", ev.Kind, ev.Target, ev.RunID, err)
		}
	}
}
