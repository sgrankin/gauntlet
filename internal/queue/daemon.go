// Package queue is the reconcile loop: the per-target state machine that
// drives a candidate from "queued ref" through trial merge, named checks,
// and land (or park), per docs/plans/phase1.md §3 (amended by §9). It knows
// nothing about how checks run, how events reach a human, or how git
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
	"sync/atomic"
	"text/template"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
)

// Config is the subset of the admin daemon config (internal/config.Daemon)
// the queue itself needs: which targets to reconcile, where each
// candidate's own check spec lives in its trial tree, the committer identity
// for merge commits, and the merge-message template. Remote and Poll are
// cmd-level concerns — dialing the core.GitRepo and driving Run's tick
// channel — the queue is agnostic to both.
type Config struct {
	// Targets are the target branches to reconcile, keyed by name in the
	// candidate ref grammar (docs/plans/phase1.md §9.3).
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

	// WorkDir is the directory trial-tree export dirs are created under
	// (docs/plans/phase23.md §10, F2): os.MkdirTemp(WorkDir, ...). Empty
	// selects the OS default temp dir, exactly the phase-1 behavior — this
	// field only lets an operator (or a test) pin exports to a known,
	// sweepable location; the queue itself never sweeps WorkDir (that's
	// cmd's job, D7).
	WorkDir string

	// LogDir is the directory full per-check log files are written under
	// (DESIGN.md "Full per-check log files"): each check's job gets
	// LogPath = filepath.Join(LogDir, runID, sanitized-check-name+".log"),
	// and the executor tees the check's combined output there in addition
	// to the tail-capped in-band CheckResult.Output. Empty disables log
	// files entirely (CheckJob.LogPath stays "" for every check),
	// preserving the exact pre-F-a behavior. Unlike WorkDir's trial export
	// dirs, log files under LogDir are never removed by the queue: they
	// outlive their run by design (the dashboard's "full log" link, an
	// API/MCP path) — retention/pruning is a separate mechanism, not the
	// reconcile loop's job.
	LogDir string
}

// checkInFlight is the currently-running check within an in-flight run: its
// cancel func (for a ref/target move, Invariant 5) and the one-shot channel
// its executor goroutine reports back on.
type checkInFlight struct {
	name   string
	cancel context.CancelFunc
	result chan core.CheckResult
	span   trace.Span
	start  time.Time
}

// run is the daemon's entire in-flight state for one target (Invariant 4:
// "in-flight state is (slot, tested SHA, executor run-id)"). It is
// reconstructible from ground truth on every tick except for cur, which is
// the one piece of state that can't be rederived without rerunning checks —
// exactly why losing it (a crash) costs at most a rerun, never correctness.
type run struct {
	target   string
	cand     core.Candidate
	baseOID  string // target tip this run's trial merge was built onto
	mergeOID string // the tested merge commit (Invariant 1)
	runID    string
	dir      string // exported trial tree; removed on every terminal transition
	checks   []config.Check
	idx      int // index into checks of the current (or next) check

	rec *core.RunRecord

	rootCtx  context.Context
	rootSpan trace.Span

	cur *checkInFlight // nil between checks (never observable mid-tick: land follows immediately)
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
	// (ref, SHA) (docs/plans/phase1.md §9.1): entries clear only when the
	// ref's SHA changes, the ref vanishes, or a CommandRetry clears it
	// explicitly (command.go), never when some other candidate lands. Both
	// are keyed by target name, then by ref, and are fully reconstructible
	// from ground truth — losing them (a restart) costs at most some
	// re-tests, never correctness.
	order map[string]map[string]int64
	done  map[string]map[string]parkEntry
	seq   int64

	// ignoredRefs dedupes core.EventIgnoredRef (docs/plans/phase23.md §10,
	// O4): ref -> last-emitted-for SHA, pruned of vanished refs every tick
	// so it can't grow without bound over a long-running daemon's lifetime.
	ignoredRefs map[string]string

	// runs holds the single in-flight run per target, if any.
	runs map[string]*run

	// snap holds the most recently published Snapshot (docs/plans/phase23.md
	// §2.1); nil until the first successful ReconcileOnce pass completes.
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
		git:         git,
		exec:        exec,
		chans:       chans,
		tr:          obs.Tracer(),
		cfg:         cfg,
		now:         now,
		order:       make(map[string]map[string]int64),
		done:        make(map[string]map[string]parkEntry),
		ignoredRefs: make(map[string]string),
		runs:        make(map[string]*run),
	}, nil
}

// Run drives the reconcile loop until ctx is done or tick is closed, calling
// ReconcileOnce once per tick. A ReconcileOnce error is reported as a
// channel EventError (it is not target-specific, so it carries no
// Candidate) and does not stop the loop — the next tick tries again.
func (d *Daemon) Run(ctx context.Context, tick <-chan time.Time) error {
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
// target: Fetch, ListRefs (the tick's snapshot of ground truth, per §3 step
// 1), drain inbound commands (docs/plans/phase23.md §2.2), flag any
// candidate ref naming an unconfigured target (§10, O4), then per-target
// state-machine advancement (reconcile.go), and finally publish a Snapshot
// of the resulting state (§2.1).
//
// On an early error (Fetch or ListRefs failing) ReconcileOnce returns before
// draining commands, checking ignored refs, reconciling any target, or
// publishing a snapshot — the previously published Snapshot (if any) stays
// current, its staleness visible via Snapshot().At.
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

	d.drainCommands(ctx)
	d.checkIgnoredRefs(ctx, refs)

	for _, t := range d.cfg.Targets {
		d.reconcileTarget(ctx, t, refs)
	}

	d.snap.Store(d.buildSnapshot(refs))
	return nil
}

// emit reports ev to every configured channel. Channel.Emit must not block
// the reconcile loop (its contract) and errors are not actionable here, so
// they're discarded.
func (d *Daemon) emit(ctx context.Context, ev core.Event) {
	for _, ch := range d.chans {
		_ = ch.Emit(ctx, ev)
	}
}
