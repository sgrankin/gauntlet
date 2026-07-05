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

	"github.com/sgrankin/gauntlet/internal/core"
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

	// Emit fans a hook-outcome event (EventHookFinished) out to the
	// daemon's other channels. Each channel renders it differently
	// (closing-review FIX 1): the log channel renders every hook result,
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
	exec     core.Executor
	notify   func(context.Context, core.Event)
	workDir  string
	logDir   string
	log      io.Writer

	queue chan core.Event
	cmds  chan core.Command

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
		notify:   p.Emit,
		workDir:  p.WorkDir,
		logDir:   p.LogDir,
		log:      logw,
		queue:    make(chan core.Event, queueBuffer),
		cmds:     make(chan core.Command),
		monitors: make(map[string]chan core.Event),
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
// command vocabulary in phase 4.
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
func (r *Runner) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
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

	if rec.MergeSHA == "" {
		// A synthesized crash-recovery landing (queue/reconcile.go's
		// recoverLanded doc: "no merge ever happens" on that path, so
		// BaseOID/MergeSHA/Trial stay zero-valued): the merge commit
		// already landed in an earlier pass and its SHA was never
		// captured here, so there is no tree to export hooks against.
		fmt.Fprintf(r.log, "hooks: %s: landing has no merge SHA (recovered landing), skipping hooks run=%s\n", ev.Target, rec.RunID)
		return
	}

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

	for i, h := range hs {
		if ctx.Err() != nil {
			return
		}

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
		result := r.exec.RunCheck(ctx, job)

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
