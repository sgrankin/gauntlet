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
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Hook is one named command run, in order, against a target's landed tree.
type Hook struct {
	Name    string
	Command []string
}

// Params configures a Runner.
type Params struct {
	// Hooks maps target name -> its ordered hooks. A target absent from
	// this map (or mapped to an empty/nil slice) has no hooks and every
	// landing for it is a no-op.
	Hooks map[string][]Hook

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
	hooks   map[string][]Hook
	git     core.GitRepo
	exec    core.Executor
	notify  func(context.Context, core.Event)
	workDir string
	log     io.Writer

	queue chan core.Event
	cmds  chan core.Command
}

// New returns a Runner configured by p. It performs no I/O; Run drains the
// landing queue until its context is done.
func New(p Params) *Runner {
	logw := p.Log
	if logw == nil {
		logw = os.Stderr
	}
	return &Runner{
		hooks:   p.Hooks,
		git:     p.Git,
		exec:    p.Exec,
		notify:  p.Emit,
		workDir: p.WorkDir,
		log:     logw,
		queue:   make(chan core.Event, queueBuffer),
		cmds:    make(chan core.Command),
	}
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
	}
	return nil
}

// Commands returns a channel that never yields: hooks have no inbound
// command vocabulary in phase 4.
func (r *Runner) Commands() <-chan core.Command {
	return r.cmds
}

// Run drains the landing queue serially until ctx is done. Landings are
// already serialized upstream (one lane, FIFO), so serial draining here
// never creates a backlog under normal operation; it does mean a slow
// hook's remaining siblings (and any later landing) wait behind it — by
// design, so one landing's hooks running long never races a later
// landing's hooks against the same target's deployment target.
func (r *Runner) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-r.queue:
			r.runLanding(ctx, ev)
		}
	}
}

// runLanding runs every hook configured for ev.Target, in order, against a
// fresh export of the landed merge commit's tree, stopping at the first
// failure (a later deploy step shouldn't run if an earlier one failed) and
// always cleaning up the export directory. A hook's outcome never reaches
// back into the landing or the queue — only the notification fan-out
// (Params.Emit) and the log.
func (r *Runner) runLanding(ctx context.Context, ev core.Event) {
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

	for _, h := range hs {
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
		}
		result := r.exec.RunCheck(ctx, job)

		if r.notify != nil {
			r.notify(ctx, core.Event{
				Kind:      core.EventHookFinished,
				At:        time.Now(),
				Target:    ev.Target,
				Candidate: rec.Candidate,
				RunID:     rec.RunID,
				CheckName: h.Name,
				Check:     &result,
			})
		}

		if result.Status == core.CheckFailed || result.Err != nil {
			fmt.Fprintf(r.log, "hooks: %s: hook %q failed, stopping remaining hooks for run=%s\n", ev.Target, h.Name, rec.RunID)
			return
		}
	}
}
