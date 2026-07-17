// Package hooks implements core.Channel as a post-land hook runner
// (DESIGN.md's decision ledger, "Deployments as post-land hooks"): ordered
// commands run against the landed merge commit's tree, via the same
// core.Executor machinery a check runs on. This is a hard scope boundary —
// gauntlet never grows a CD system here. A hook that needs more than "run
// this command against the landed tree and tell me if it failed" (health
// checks, rollback, progressive delivery, ...) hands off to a real CD system
// (Argo CD, a terraform pipeline, whatever the environment runs); gauntlet
// itself stops at "ran the command, reported the verdict".
package hooks

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
)

// Hook is one named command run, in order, against a target's landed tree.
type Hook struct {
	Name    string
	Command []string
}

// Policy controls what happens to a target's hook backlog when landings
// outpace hook execution (e.g. deploys slower than merges). Hooks always
// run serially — one landing's hooks at a time, never two landings'
// concurrently, for any policy — Policy only decides what happens to
// entries still waiting behind the one currently running when a newer
// landing for the *same target* shows up.
type Policy string

const (
	// PolicyQueue runs every landing's hooks, in arrival order: the
	// original (pre-policy) behavior, and the default. Nothing is ever
	// dropped; a burst of landings just makes the backlog longer.
	PolicyQueue Policy = "queue"

	// PolicyCoalesce drops queued (not yet started) landings for a target
	// once a newer landing for that same target is also queued behind
	// them — only the newest queued landing's hooks run next. The
	// landing currently *running* always finishes undisturbed; only the
	// backlog behind it is collapsed. Each dropped landing is logged (one
	// line, no fabricated hook results/events — see applyBacklogPolicy).
	PolicyCoalesce Policy = "coalesce"

	// PolicyCancel is PolicyCoalesce plus: the currently *running*
	// landing's hook execution is cancelled — its in-flight
	// core.Executor.RunCheck call's ctx is cancelled — as soon as a newer
	// landing for the same target arrives, rather than waiting for it to
	// finish. The cancelled hook still gets a normal EventHookFinished,
	// carrying the Err result the executor returns on cancellation (a
	// cancel is operationally a failure-shaped event — channels already
	// know how to render it) with Detail noting what superseded it; the
	// landing's remaining hooks are skipped via the same
	// stop-at-first-failure path an ordinary hook failure takes. See
	// execLanding for the mid-hook cancellation mechanics.
	PolicyCancel Policy = "cancel"
)

// Params configures a Runner.
type Params struct {
	// Hooks maps target name -> its ordered hooks. A target absent from
	// this map (or mapped to an empty/nil slice) has no hooks and every
	// landing for it is a no-op.
	Hooks map[string][]Hook

	// Policies maps target name -> its backlog Policy. A target absent
	// from this map (or mapped to "") gets PolicyQueue, the default —
	// this mirrors internal/config's own default so a Runner built
	// without Policies at all reproduces the pre-policy behavior exactly.
	Policies map[string]Policy

	// Git provides ExportTree, used to materialize each landing's merge
	// commit into a scratch directory for hooks to run against. git
	// archive (the only implementation, internal/gitx) accepts any
	// tree-ish including a commit SHA, so the landed Record.MergeSHA is
	// passed to it directly — no commit-to-tree resolution needed.
	Git core.GitRepo

	// Exec runs each hook's command exactly like a check
	// (core.Executor.RunCheck): the GAUNTLET_* env contract carries the
	// landed coordinates (base/merge/candidate SHA, ref) for free.
	Exec core.Executor

	// HistoryMtimes mirrors queue.Config.HistoryMtimes for the hook
	// export: hooks run against the landed merge's tree, and the same
	// metadata determinism applies (config's `export { mtimes "history" }`
	// covers every trial materialization, not one executor's).
	HistoryMtimes bool

	// Slots is the daemon-wide execution-capacity semaphore hooks share
	// with candidate checks (the operator's `max-executions` cap —
	// core.Slots): each hook's RunCheck holds one slot for its whole
	// execution. Runner runs on its own goroutine, so unlike the queue's
	// non-blocking TryAcquire this waits (a hook behind a saturated host
	// starts late rather than never). nil means unlimited.
	Slots *core.Slots

	// Emit fans a hook-outcome event (EventHookFinished) out to the
	// daemon's other channels. Each channel renders it differently: the
	// log channel renders every hook result,
	// pass and fail; Slack renders only failures, as a standalone channel
	// message (no thread to reply on — the run's root ts is already
	// forgotten by hook time); ghstatus deliberately ignores
	// EventHookFinished entirely — the commit status describes the
	// landing, and a post-land hook failure must not repaint an
	// already-green landing red (the CD hand-off boundary, DESIGN.md's
	// decision ledger). Built by cmd from the same channel slice
	// queue.Daemon uses, minus this Runner itself (no self-feedback).
	Emit func(context.Context, core.Event)

	// WorkDir is the scratch directory each landing's tree is exported
	// into (a fresh subdirectory per landing, removed once that
	// landing's hooks finish).
	WorkDir string

	// LogDir, mirroring queue.Config.LogDir (DESIGN.md "Full per-check log
	// files"), is the root each hook's full combined-output log is written
	// under: runLanding assigns each hook's CheckJob.LogPath to
	// <LogDir>/<runID>/hook-<seq>-<sanitized name>.log.zst, where runID is
	// the landing's RunID and seq is the hook's 1-based position within its
	// target's configured order. Landing hook logs into the SAME per-run
	// directory a check's own logs already live under means retention
	// (cmd/gauntlet's pruneLogFiles, keyed on that directory's mtime) covers
	// them for free — no separate sweep needed. Empty (the default)
	// preserves the exact pre-parity behavior: every job.LogPath stays "",
	// so the executor writes no log file at all, exactly as if LogDir were
	// never wired up.
	LogDir string

	// Log receives one line per dropped landing (queue full), export
	// failure, or hook failure. Defaults to os.Stderr when nil.
	Log io.Writer
}

// queueBuffer bounds Runner's inbound landing queue. Landings are already
// serialized by the reconcile loop (one lane, FIFO), so this is generous
// headroom for a burst of landings arriving faster than Run can drain them
// (e.g. hooks briefly paused, or one landing's hooks running long) — not a
// steady-state expectation.
const queueBuffer = 64

var _ core.Channel = (*Runner)(nil)

// Runner is a core.Channel that runs post-land hooks. Its Emit only ever
// enqueues EventLanded events carrying a non-nil Record — every other event
// kind is ignored, per core.Channel's "channels ignore unknown kinds"
// contract (internal/channel/log.go) — since a hook stage only ever fires
// off a landing. Commands never yields: hooks have no inbound command
// vocabulary.
type Runner struct {
	hooks    map[string][]Hook
	policies map[string]Policy
	git      core.GitRepo
	slots    *core.Slots // shared daemon-wide execution cap; nil = unlimited
	mtimes   bool        // Params.HistoryMtimes
	exec     core.Executor
	notify   func(context.Context, core.Event)
	workDir  string
	logDir   string
	log      io.Writer

	// tr is gauntlet's shared tracer (obs.Tracer()), grabbed once at
	// construction exactly like queue.Daemon does (cmd/gauntlet/main.go's
	// obs.InstallProvider-before-New ordering applies here too: hooks.New is
	// only ever called after InstallProvider in main's run()). Used to give
	// each hook's RunCheck call its own "check" span, mirroring
	// internal/queue/reconcile.go's startCheck — the same
	// obs.StartCheck/EndCheck pair, since a hook's CheckJob/CheckResult
	// shape is identical to a check's. With no provider installed, every
	// span is a no-op, so this costs nothing when OTel isn't configured.
	tr trace.Tracer

	queue chan core.Event
	cmds  chan core.Command

	// drain is closed by Drain to begin a graceful hook-backlog drain
	// (issue #8): Run finishes every landing already queued, then returns.
	// drained is closed by Run once that is complete, so Drain can block
	// the caller until the backlog is truly empty. The two Onces guard
	// each close against a repeated/racing Drain and a force overlap.
	drain            chan struct{}
	drained          chan struct{}
	closeDrainOnce   sync.Once
	closeDrainedOnce sync.Once

	// monitors holds, for each target currently executing a PolicyCancel
	// landing, the inbox execLanding's monitor goroutine is watching for
	// a superseding landing. Emit consults this (under mu) to decide
	// whether to wake a monitor; execLanding registers/deregisters its
	// inbox for the duration of that one landing's hooks. A target is
	// only ever present here while a PolicyCancel landing for it is
	// actually running — never populated ahead of time and never left
	// stale afterward — so a signal can only ever mean "a landing arrived
	// while this one was running", never a leftover from some earlier,
	// already-finished landing (see execLanding).
	mu       sync.Mutex
	monitors map[string]chan core.Event

	// live is the in-memory state Snapshot/SnapshotAll read: zero-valued
	// (live.Running == false) whenever no landing's hooks are currently
	// executing. runLanding sets it once, on
	// entry, for the whole duration of one landing's hooks (mkdir/export
	// included, not just the RunCheck calls themselves) and clears it via
	// defer on every return path; setCurrentHook updates CurrentHook/
	// HookIndex before each hook's RunCheck. Guarded by mu alongside
	// monitors — hook execution is globally serial, so at most one
	// goroutine is ever writing this at a time, but Snapshot/SnapshotAll
	// read it from arbitrary caller goroutines (dashboard/MCP handlers).
	live LiveState
}

// LiveState is a snapshot of what hooks.Runner is doing for one target right
// now (in-memory only — deliberately not persisted; history.Store.
// HookRunSummaries is the durable counterpart). Running is true iff a
// landing's hooks are actively executing for Target this instant;
// CurrentHook/HookIndex/HookCount/StartedAt describe
// that landing's hook chain (CurrentHook/HookIndex zero-valued before the
// first hook starts; StartedAt is when this landing's hook execution began,
// held fixed across every hook in its chain, not reset per hook). Since hook
// execution is globally serial (one Runner goroutine, at most one
// landing's hooks running at any instant, across every target at once), at
// most one target can ever have Running == true at a time.
//
// BacklogDepth is the whole Runner's cross-target queue length (len(Runner.
// queue)) at the moment of the snapshot — approximate by design (a live
// gauge, not a durable count): it can move between the read and the caller
// using it, and it counts every target's pending landings, not just
// Target's.
type LiveState struct {
	Target       string
	Running      bool
	CurrentHook  string
	HookIndex    int
	HookCount    int
	StartedAt    time.Time
	BacklogDepth int
}

// New returns a Runner configured by p. It performs no I/O; Run drains the
// landing queue until its context is done.
func New(p Params) *Runner {
	logw := p.Log
	if logw == nil {
		logw = os.Stderr
	}
	return &Runner{
		hooks:    p.Hooks,
		policies: p.Policies,
		git:      p.Git,
		exec:     p.Exec,
		slots:    p.Slots,
		mtimes:   p.HistoryMtimes,
		notify:   p.Emit,
		workDir:  p.WorkDir,
		logDir:   p.LogDir,
		log:      logw,
		tr:       obs.Tracer(),
		queue:    make(chan core.Event, queueBuffer),
		cmds:     make(chan core.Command),
		monitors: make(map[string]chan core.Event),
		drain:    make(chan struct{}),
		drained:  make(chan struct{}),
	}
}

// policy returns target's configured backlog Policy, defaulting to
// PolicyQueue when target has none configured (nil Policies map, or an
// absent/empty entry) — mirrors internal/config's own default so a Runner
// built without Policies at all reproduces the pre-policy behavior
// exactly.
func (r *Runner) policy(target string) Policy {
	if p, ok := r.policies[target]; ok && p != "" {
		return p
	}
	return PolicyQueue
}

// Emit enqueues ev for Run's drainer if it is a landing carrying a Record —
// the only shape Runner acts on — and ignores every other event kind. It
// never blocks the reconcile loop: Emit is called synchronously from
// queue.Daemon's own emit fan-out, so once queueBuffer is full (Run is stuck
// on a slow hook, or has fallen behind), further events are logged and
// dropped rather than waited on.
func (r *Runner) Emit(ctx context.Context, ev core.Event) error {
	if ev.Kind != core.EventLanded || ev.Record == nil {
		return nil
	}
	select {
	case r.queue <- ev:
	default:
		fmt.Fprintf(r.log, "hooks: queue full (%d), dropping landing target=%s run=%s\n", queueBuffer, ev.Target, ev.RunID)
		return nil
	}
	// PolicyCancel additionally wakes up execLanding's monitor goroutine,
	// if one is currently watching this target (i.e. a PolicyCancel
	// landing for it is running right now), so it can cancel that
	// in-flight landing's hooks immediately rather than waiting for Run's
	// drainer to next reach the queue (which only happens between
	// landings, not mid-hook). No monitor registered — the common case,
	// nothing currently running for this target to cancel — is a no-op,
	// not a missed signal: ev is already durably queued above, and Run's
	// drainer will apply PolicyCancel's coalescing to the backlog once it
	// gets there regardless.
	r.mu.Lock()
	inbox := r.monitors[ev.Target]
	r.mu.Unlock()
	if inbox != nil {
		select {
		case inbox <- ev:
		default:
		}
	}
	return nil
}

// Commands returns a channel that never yields: hooks have no inbound
// command vocabulary. Hook cancellation is deliberately out-of-band (dashboard/MCP call
// Runner.CancelCurrent directly, not through a core.Command) — a hook
// stage has no candidate ref for a ref-addressed command to name.
func (r *Runner) Commands() <-chan core.Command {
	return r.cmds
}

// Run drains the landing queue serially until ctx is done: hooks always
// run one landing at a time, for every target, regardless of Policy — a
// slow hook only ever delays what's queued behind it, never a concurrent
// run. What Policy changes is what "what's queued behind it" means: each
// iteration reads one landing plus, non-blockingly, every other landing
// already sitting in the queue (drainAvailable) into a batch, applies
// every present target's Policy to that batch (applyBacklogPolicy —
// PolicyQueue keeps everything, PolicyCoalesce/PolicyCancel keep only the
// newest per target), and then executes what's left, in order.
//
// A burst that arrives faster than a batch's hooks can run simply forms
// the next batch once this one drains — Policy is applied fresh each
// cycle, so a target's backlog never accumulates unboundedly under
// PolicyCoalesce/PolicyCancel even if landings keep arriving throughout.
//
// GUARD: this loop's single goroutine draining r.queue is what makes
// hook execution globally serial — across every target at once, not merely
// one landing per target at a time — which history.Store.writeHookResult
// (internal/history/store.go) depends on for its count-in-transaction
// sequence numbering to be race-free. If this ever changes (e.g. one Run
// goroutine per target, or execLanding calls fanned out concurrently),
// that seq-by-COUNT approach must be revisited first.
func (r *Runner) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.drain:
			// Graceful drain (issue #8): the queue has stopped landing
			// (cmd calls this only after the queue's own drain completed),
			// so every landing whose hooks are owed is already sitting in
			// r.queue. Run the whole remaining backlog — the entire queued
			// backlog is part of the drain set, since hooks have no
			// post-restart replay and dropping them would be silent
			// permanent loss — then signal completion and exit. A force
			// (ctx cancel) mid-drain still short-circuits via ctx.Err().
			r.runBacklog(ctx)
			r.closeDrainedOnce.Do(func() { close(r.drained) })
			return nil
		case ev := <-r.queue:
			batch := append([]core.Event{ev}, r.drainAvailable()...)
			for _, e := range r.applyBacklogPolicy(batch) {
				if ctx.Err() != nil {
					return nil
				}
				r.execLanding(ctx, e)
			}
		}
	}
}

// runBacklog executes every landing currently queued, applying each
// target's backlog Policy once over the whole set — the drain-time
// counterpart of the Run loop's per-burst processing.
func (r *Runner) runBacklog(ctx context.Context) {
	batch := r.drainAvailable()
	if len(batch) == 0 {
		return
	}
	for _, e := range r.applyBacklogPolicy(batch) {
		if ctx.Err() != nil {
			return
		}
		r.execLanding(ctx, e)
	}
}

// Drain begins a graceful hook-backlog drain and BLOCKS until the backlog
// is empty (Run has finished it) or ctx is cancelled (a force). Idempotent
// — a repeated call is a no-op close and waits on the same completion.
// cmd calls this only after the queue has fully drained, so no new landing
// can arrive after the close.
func (r *Runner) Drain(ctx context.Context) {
	r.closeDrainOnce.Do(func() { close(r.drain) })
	select {
	case <-r.drained:
	case <-ctx.Done():
	}
}

// drainAvailable returns every event currently sitting in r.queue without
// blocking — the rest of a burst that arrived alongside the one Run's
// select already received.
func (r *Runner) drainAvailable() []core.Event {
	var out []core.Event
	for {
		select {
		case ev := <-r.queue:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// applyBacklogPolicy partitions batch by target and applies each target's
// Policy independently (two targets' backlogs never affect one another,
// even within the same batch): PolicyQueue keeps every entry for that
// target, in arrival order; PolicyCoalesce and PolicyCancel keep only the
// last (newest) entry for that target and log one line per dropped entry
// — no EventHookFinished or any other fabricated per-hook result for a
// landing whose hooks never ran at all. Overall arrival order is preserved
// among the entries that survive.
//
// This only ever collapses landings that are still queued (never started)
// as of this batch. PolicyCancel's additional behavior — cancelling a
// landing that is already *running* — happens in execLanding, not here;
// applyBacklogPolicy has no notion of "currently running".
func (r *Runner) applyBacklogPolicy(batch []core.Event) []core.Event {
	lastIdx := make(map[string]int, len(batch))
	for i, ev := range batch {
		switch r.policy(ev.Target) {
		case PolicyCoalesce, PolicyCancel:
			lastIdx[ev.Target] = i
		}
	}

	out := make([]core.Event, 0, len(batch))
	for i, ev := range batch {
		switch r.policy(ev.Target) {
		case PolicyCoalesce, PolicyCancel:
			if i != lastIdx[ev.Target] {
				newer := batch[lastIdx[ev.Target]]
				fmt.Fprintf(r.log, "hooks: coalesced landing %s, superseded by %s\n",
					landingRef(ev), landingRef(newer))
				continue
			}
		}
		out = append(out, ev)
	}
	return out
}

// execLanding runs ev's hooks (runLanding), adding PolicyCancel's mid-hook
// cancellation mechanics on top when ev.Target is configured for it; every
// other Policy is a direct passthrough with the parent ctx and no extra
// goroutine.
//
// Cancellation granularity: for the whole time this landing's hooks are
// running (not just between hooks), a monitor goroutine watches an inbox
// registered in r.monitors[ev.Target] and cancels ctx the instant Emit
// wakes it with a newer landing for the same target — including mid-hook,
// since ctx is the same one passed to core.Executor.RunCheck below, and
// LocalExecutor's RunCheck already ties the child process's lifetime to
// ctx (internal/executor/local.go, exec.CommandContext + cmd.Cancel). A
// between-hook-only check would miss exactly this case — a hook that runs
// long is the whole reason a backlog policy matters — so the monitor
// goroutine, not a peek between hook commands, is what PolicyCancel
// actually requires.
//
// The inbox is registered in r.monitors only for the duration of this one
// landing's hooks (deferred deregistration below), and only this landing's
// own execLanding call ever registers it — never ahead of time, never left
// behind afterward. That's what makes Emit's wake-up signal unambiguous:
// by construction it can only ever mean "a landing for this target arrived
// while this landing was the one running", never a stale leftover from
// some earlier, already-finished landing, and never ev's own Emit call
// (which ran, and completed, well before this registration — the whole
// reason ev is running now is that it was dequeued from r.queue after
// that).
func (r *Runner) execLanding(parent context.Context, ev core.Event) {
	if r.policy(ev.Target) != PolicyCancel {
		r.runLanding(parent, ev, nil)
		return
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	inbox := make(chan core.Event, 1)
	r.mu.Lock()
	r.monitors[ev.Target] = inbox
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.monitors, ev.Target)
		r.mu.Unlock()
	}()

	supersede := make(chan core.Event, 1)
	stop := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-stop:
		case newer := <-inbox:
			fmt.Fprintf(r.log, "hooks: %s: cancelling in-flight landing run=%s, superseded by %s\n",
				ev.Target, ev.RunID, landingRef(newer))
			supersede <- newer
			cancel()
		}
	}()

	r.runLanding(ctx, ev, supersede)
	close(stop)
	<-monitorDone
}

// CancelCurrent cancels target's currently-running landing's hooks, if one
// is running right now, and reports whether it found one to signal — an
// operator-triggered counterpart to PolicyCancel's own automatic
// supersede-cancel, wrapped
// around the exact same mechanism: r.monitors[target] is only ever
// non-nil for the duration of a PolicyCancel landing's execLanding call
// (see that method's doc), so this only ever does anything for a target
// configured with PolicyCancel that has a landing in flight this instant.
// For PolicyQueue/PolicyCoalesce, or a PolicyCancel target that's currently
// idle, there is nothing running to interrupt — CancelCurrent returns false,
// a normal, expected outcome, not an error (there is no separate
// cancellation mechanism to fall back to for those cases; runLanding's
// stop-at-first-failure behavior only ever triggers on a real check
// failure or a genuine supersede).
//
// The synthetic Event sent through the monitor's inbox plays the same role
// execLanding's own newer-landing signal does — its landingRef becomes the
// "superseded by ..." Detail runLanding attaches to the interrupted hook's
// EventHookFinished (cancelDetail's queue-side counterpart: an operator, not
// another landing, is what superseded it here).
//
// TOCTOU caveat, inherited verbatim from Emit's own supersede pattern (a
// map read under mu, the send outside it): the landing can finish normally
// in the window between this method's map read and its send — the monitors
// entry is only deleted after the monitor goroutine exits, so the send can
// still land in the (cap-1) inbox and this reports true ("cancelled") for
// hooks that had, by then, already completed on their own. Functionally
// harmless (cancel() on an already-done context is a no-op; no work is
// disturbed), but callers should read true as "a cancel signal was
// delivered", not a guarantee the hook was actually interrupted mid-flight.
func (r *Runner) CancelCurrent(target string) bool {
	r.mu.Lock()
	inbox := r.monitors[target]
	r.mu.Unlock()
	if inbox == nil {
		return false
	}

	ev := core.Event{
		Target:    target,
		Candidate: core.Candidate{Topic: "operator-cancel", SHA: "manual"},
		Detail:    "cancelled by operator",
	}
	select {
	case inbox <- ev:
		return true
	default:
		return false // a cancellation is already in flight for this target
	}
}

// Snapshot reports target's current LiveState: ok is true iff a landing's
// hooks are executing for target this instant, in which case the returned
// LiveState describes it fully. When ok
// is false, the returned LiveState still carries Target and BacklogDepth
// (the queue length is target-agnostic — meaningful regardless of whether
// target itself has anything running) but every hook-specific field is
// zero.
func (r *Runner) Snapshot(target string) (LiveState, bool) {
	r.mu.Lock()
	live := r.live
	r.mu.Unlock()

	depth := len(r.queue)
	if live.Target != target || !live.Running {
		return LiveState{Target: target, BacklogDepth: depth}, false
	}
	live.BacklogDepth = depth
	return live, true
}

// SnapshotAll returns every target with a landing currently executing hooks
// — at most one entry, by the same global-serial guarantee Snapshot's doc
// describes, but returned as a slice so callers (dashboard, MCP) don't need
// a special case for "is anything running right now" versus "what,
// specifically" — an empty slice, not a special sentinel, means nothing is
// running anywhere.
func (r *Runner) SnapshotAll() []LiveState {
	r.mu.Lock()
	live := r.live
	r.mu.Unlock()

	if !live.Running {
		return nil
	}
	live.BacklogDepth = len(r.queue)
	return []LiveState{live}
}

// setLive marks a landing's hook execution as started for the duration of
// runLanding's remaining body (mkdir/export included, not just the RunCheck
// calls) — cleared by the deferred clearLive in runLanding on every return
// path, so Running never reads stale-true after a landing concludes for any
// reason (success, export failure, ctx cancellation).
func (r *Runner) setLive(target string, hookCount int) {
	r.mu.Lock()
	r.live = LiveState{Target: target, Running: true, HookCount: hookCount, StartedAt: time.Now()}
	r.mu.Unlock()
}

// setCurrentHook updates the in-flight landing's CurrentHook/HookIndex
// immediately before that hook's RunCheck call — called only while
// live.Running is already true (setLive having run first), so this never
// resets StartedAt or HookCount.
func (r *Runner) setCurrentHook(index int, name string) {
	r.mu.Lock()
	r.live.CurrentHook = name
	r.live.HookIndex = index
	r.mu.Unlock()
}

// clearLive resets live to its zero value (Running == false), the
// counterpart to setLive — always called via defer in runLanding so it
// fires on every return path.
func (r *Runner) clearLive() {
	r.mu.Lock()
	r.live = LiveState{}
	r.mu.Unlock()
}

// landingRef formats ev as "<topic>@<shortSHA>" for log lines and
// EventHookFinished.Detail — a compact, human-scannable identifier for
// which landing superseded which.
func landingRef(ev core.Event) string {
	return ev.Candidate.Topic + "@" + shortSHA(ev.Candidate.SHA)
}

// shortSHA truncates a full SHA to git's usual abbreviation length.
// Shorter inputs (e.g. test fixtures) pass through unchanged.
func shortSHA(sha string) string {
	const n = 8
	if len(sha) > n {
		return sha[:n]
	}
	return sha
}

// hookLogPath builds one hook's full per-check log file path (DESIGN.md
// "Full per-check log files", extended here to hooks for log/history
// parity with checks): <LogDir>/<runID>/hook-<seq>-<sanitized name>.log.zst.
// seq is the hook's 1-based position within its target's configured order,
// matching the seq history.Store's hooks table records for it. The hook's
// (free-form, config-owned) name is sanitized the same way a check's name
// is (core.SanitizeName) before becoming a path component; the fixed
// "hook-" prefix plus the ".log.zst" suffix additionally guarantees the
// sanitized name can never make the final component resolve to "." or "..".
//
// Landing this in <LogDir>/<runID>/ — the exact same per-run directory
// queue/reconcile.go's job.LogPath assignment already writes that run's
// check logs into — is deliberate: cmd/gauntlet's retention sweep
// (pruneLogFiles) prunes whole per-run directories by mtime, so a hook log
// dropped into a run's existing directory is covered by that same sweep for
// free, without pruneLogFiles needing to know hooks exist at all.
//
// Returns "" when r.logDir is empty (Params.LogDir unset): the pre-parity
// default of "no log files written at all", identical in shape to
// queue.Config.LogDir's own empty-string fallback.
func (r *Runner) hookLogPath(runID string, seq int, name string) string {
	if r.logDir == "" {
		return ""
	}
	return filepath.Join(r.logDir, runID, fmt.Sprintf("hook-%d-%s.log.zst", seq, core.SanitizeName(name)))
}

// runLanding runs every hook configured for ev.Target, in order, against a
// fresh export of the landed merge commit's tree, stopping at the first
// failure (a later deploy step shouldn't run if an earlier one failed) and
// always cleaning up the export directory. A hook's outcome never reaches
// back into the landing or the queue — only the notification fan-out
// (Params.Emit) and the log.
//
// A real landing (!rec.Recovered) sets the in-memory live-state gauge
// (setLive/clearLive) for the whole call and emits EventHookStarted
// immediately before each hook's RunCheck — history.Store durably upserts
// the "owed" row from the first one; a recovery-synthesized landing
// (rec.Recovered) instead emits one EventHookSkipped and returns before any
// of that, never touching live state at all (there is nothing running to
// report). This gates on Recovered specifically, not on MergeSHA being
// empty: queue/reconcile.go's recoverLanded may still populate MergeSHA on
// a Recovered record (a best-effort lookup of the merge that already
// landed, for history/tooling), and that must never be mistaken here for
// "safe to run hooks" — hooks (e.g. a deploy step) must never auto-run a
// second time for a landing whose hooks may already have run, or be
// running, before the crash.
//
// supersede is PolicyCancel's channel from execLanding: non-nil only when
// ev.Target is configured for PolicyCancel, in which case a hook whose
// RunCheck comes back with a non-nil Err checks it (non-blockingly) for
// the landing that triggered the cancellation, to attach a "superseded
// by ..." Detail to that hook's EventHookFinished. nil for every other
// Policy — receiving from a nil channel in a select with a default case
// always falls through, so this is safe to pass through unconditionally.
func (r *Runner) runLanding(ctx context.Context, ev core.Event, supersede <-chan core.Event) {
	hs := r.hooks[ev.Target]
	if len(hs) == 0 {
		return
	}
	rec := ev.Record

	if rec.Recovered {
		// A synthesized crash-recovery landing (queue/reconcile.go's
		// recoverLanded doc: "no merge ever happens" on that path, so
		// BaseOID/Trial stay zero-valued): the merge commit already landed
		// in an earlier pass, and even when recoverLanded's best-effort
		// FindLandingMerge lookup did identify it (MergeSHA may be
		// non-empty here), hooks are never auto-run against it — see this
		// function's own doc comment above for why.
		const detail = "recovered landing; hooks not run"
		fmt.Fprintf(r.log, "hooks: %s: recovered landing, skipping hooks run=%s\n", ev.Target, rec.RunID)
		// A durable marker (history.Store upserts hook_runs with
		// skipped=1) so this landing's hooks read as "skipped (recovery)"
		// on every surface, rather than only reaching the log line above —
		// kept verbatim alongside this event, not replaced by it.
		if r.notify != nil {
			r.notify(context.WithoutCancel(ctx), core.Event{
				Kind:      core.EventHookSkipped,
				At:        time.Now(),
				Target:    ev.Target,
				Candidate: rec.Candidate,
				RunID:     rec.RunID,
				Detail:    detail,
				HookCount: len(hs),
			})
		}
		return
	}

	r.setLive(ev.Target, len(hs))
	defer r.clearLive()

	dir, err := os.MkdirTemp(r.workDir, "landing-")
	if err != nil {
		fmt.Fprintf(r.log, "hooks: %s: create export dir: %v\n", ev.Target, err)
		return
	}
	defer os.RemoveAll(dir)

	if err := r.git.ExportTree(ctx, rec.MergeSHA, dir); err != nil {
		fmt.Fprintf(r.log, "hooks: %s: export tree %s: %v\n", ev.Target, rec.MergeSHA, err)
		return
	}
	if r.mtimes {
		// Same determinism the checks got; a failure skips the hooks (with
		// the log line as the trace) rather than running them against a
		// tree whose metadata isn't what the config promises.
		if _, err := r.git.RestoreMtimes(ctx, rec.MergeSHA, dir); err != nil {
			fmt.Fprintf(r.log, "hooks: %s: restore mtimes %s: %v\n", ev.Target, rec.MergeSHA, err)
			return
		}
	}

	for i, h := range hs {
		if ctx.Err() != nil {
			return
		}
		r.setCurrentHook(i, h.Name)

		job := core.CheckJob{
			RunID:     rec.RunID,
			Target:    ev.Target,
			Name:      "hook:" + h.Name,
			Command:   h.Command,
			Dir:       dir,
			BaseSHA:   rec.BaseOID,
			MergeSHA:  rec.MergeSHA,
			Candidate: rec.Candidate,
			LogPath:   r.hookLogPath(rec.RunID, i+1, h.Name),
		}

		// EventHookStarted fires before this hook's RunCheck, on this
		// single execution goroutine (globally serial) —
		// history.Store upserts the durable "owed" row on the first hook
		// of this landing (ON CONFLICT(run_id) DO NOTHING), synchronously,
		// before any hook subprocess starts; live consumers (dashboard,
		// MCP) render every hook's start via Snapshot instead, since
		// dashboard is pull-only (api.go).
		if r.notify != nil {
			r.notify(context.WithoutCancel(ctx), core.Event{
				Kind:      core.EventHookStarted,
				At:        time.Now(),
				Target:    ev.Target,
				Candidate: rec.Candidate,
				RunID:     rec.RunID,
				CheckName: h.Name,
				HookIndex: i,
				HookCount: len(hs),
			})
		}

		// Give this hook its own "check" span, mirroring queue's startCheck
		// (internal/queue/reconcile.go) exactly — same obs.StartCheck/
		// EndCheck pair, same CheckJob/CheckResult shape.
		// There is no run-level parent span here (hooks have no
		// obs.StartRun-shaped root the way a queue run does), so this is a
		// standalone span per hook rather than a child of one; with no OTel
		// provider installed it's a no-op regardless (obs.Tracer's doc).
		spanCtx, span := obs.StartCheck(ctx, r.tr, job.Name)
		// One daemon-wide execution slot per hook, held for the whole
		// RunCheck (executor child cleanup included), same budget candidate
		// checks draw from. Blocking is correct here — this is the hooks
		// runner's own goroutine — but an abort during the wait (shutdown,
		// PolicyCancel superseding this landing) must produce the same
		// Err-carrying result an abort during RunCheck produces:
		// EventHookStarted already fired (durably upserting the owed
		// hook_runs row), so returning silently would leave a
		// started-never-finished row indistinguishable from a crash and
		// hide the supersession from every channel. The invariant is
		// "every EventHookStarted is followed by an EventHookFinished",
		// however the hook ended.
		var result core.CheckResult
		if err := r.slots.Acquire(spanCtx); err != nil {
			result = core.CheckResult{Name: job.Name, Command: job.Command, Err: fmt.Errorf("waiting for an execution slot: %w", err)}
		} else {
			result = r.exec.RunCheck(spanCtx, job)
			r.slots.Release()
		}
		obs.EndCheck(span, result)

		// A non-nil Err (as opposed to a verdict CheckFailed) is the only
		// case PolicyCancel's cancellation produces (LocalExecutor's
		// ctx.Err()-takes-precedence rule, internal/executor/local.go),
		// so it's also the only case worth checking supersede for. A
		// non-blocking receive: supersede carries at most one value ever
		// (execLanding sends then immediately cancels), and is nil
		// entirely for every Policy but PolicyCancel.
		var detail string
		if result.Err != nil {
			select {
			case newer := <-supersede:
				detail = "superseded by " + landingRef(newer)
			default:
			}
		}

		if r.notify != nil {
			// context.WithoutCancel: notification delivery must not be
			// short-circuited by the very cancellation this hook's
			// EventHookFinished is reporting — channels are expected to
			// render this event (it's operationally a failure), not
			// silently drop it because ctx happened to already be Done.
			r.notify(context.WithoutCancel(ctx), core.Event{
				Kind:      core.EventHookFinished,
				At:        time.Now(),
				Target:    ev.Target,
				Candidate: rec.Candidate,
				RunID:     rec.RunID,
				CheckName: h.Name,
				Check:     &result,
				Detail:    detail,
			})
		}

		if result.Status == core.CheckFailed || result.Err != nil {
			msg := fmt.Sprintf("hooks: %s: hook %q failed, stopping remaining hooks for run=%s", ev.Target, h.Name, rec.RunID)
			if detail != "" {
				msg += " (" + detail + ")"
			}
			fmt.Fprintln(r.log, msg)
			return
		}
	}
}
