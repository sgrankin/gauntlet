package queue

// This file implements the phase-B auto-retry-once amendment (DESIGN.md
// decision ledger, "Auto-retry once on infra-error parks"): a narrow,
// scoped exception to phase 1's §9.2 "no unbounded retry loops" ruling.
// Two phase-B pressures manufacture OutcomeError parks that a single
// automatic retry absorbs without a human: cold-service ready-timeouts
// (docs/plans/services.md §7, "recorded phase-B candidate") and, later,
// evictable builders (docs/plans/scale.md §5). Scope is deliberately
// narrow: only OutcomeError (never OutcomeRejected — a red verdict is an
// author problem — and never OutcomeConflict), exactly once per (ref,
// SHA), gated by Config.AutoRetryErrors (default true, see that field's
// doc).

import (
	"context"

	"github.com/sgrankin/gauntlet/internal/core"
)

// autoRetryQueuedDetail and autoRetryDetail are the Detail strings an
// automatic retry's EventQueued/EventRetryRequested carry, distinguishing
// them — in the dashboard, Slack (which ignores EventRetryRequested's
// content but still renders EventQueued), and history — from an operator's
// own CommandRetry (command.go's "retry: park cleared" / unset Detail).
const (
	autoRetryQueuedDetail = "auto-retry: park cleared (infra error)"
	autoRetryDetail       = "automatic retry: infra error, once per SHA"
)

// maybeAutoRetry auto-retries cand's just-parked (target, cand.Ref) exactly
// once per cand.SHA, when outcome is OutcomeError and Config.AutoRetryErrors
// is enabled: it delegates to clearParkAndRetry (command.go), the same
// clear+emit machinery an operator's CommandRetry drives, so Slack
// threading, history's retry_intents suppression, and the dashboard all
// treat this identically to a human retry — only the Detail text (above)
// tells the two apart.
//
// Call this AFTER the call site's own park+terminal-event emit — every
// OutcomeError park site in reconcile.go (finishRun, rejectPreMerge,
// rejectRun, rejectBatch) does. The automatic EventQueued/EventRetryRequested
// must follow the EventError they are retrying, never precede it, so a
// channel or log rendering events in arrival order reads "errored, then
// auto-retried" — never the reverse.
//
// The once-per-SHA guard (d.autoRetried) is in-memory only, by design: a
// daemon restart resets it, which only re-grants one already-spent
// auto-retry per still-parked ref — bounded by restarts, never an unbounded
// retry-every-tick loop (the very thing §9.2 ruled out; this amendment
// grants exactly one extra attempt, not a loop). syncBookkeeping
// (reconcile.go) prunes stale entries — a vanished ref, or one that moved
// to a new SHA — in lockstep with d.done, so this map never grows without
// bound over a long daemon lifetime, and a fresh SHA on the same ref always
// gets its own fresh budget (requirement 3 of this feature's test suite).
func (d *Daemon) maybeAutoRetry(ctx context.Context, target string, cand core.Candidate, outcome core.Outcome) {
	if outcome != core.OutcomeError || !d.cfg.AutoRetryErrors {
		return
	}
	m := d.autoRetried[target]
	if m == nil {
		m = make(map[string]string)
		d.autoRetried[target] = m
	}
	if m[cand.Ref] == cand.SHA {
		return // already auto-retried this exact (ref, SHA) once; stays parked for a human
	}
	m[cand.Ref] = cand.SHA

	d.clearParkAndRetry(ctx, target, cand.Ref, autoRetryQueuedDetail, autoRetryDetail)
}
