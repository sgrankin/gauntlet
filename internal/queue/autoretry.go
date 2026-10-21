package queue

// This file implements the auto-retry-once mechanism (DESIGN.md decision
// ledger, "Auto-retry once on infra-error parks"): a narrow, scoped
// exception to the "no unbounded retry loops" rule. Two pressures
// manufacture OutcomeError parks that a single automatic retry absorbs
// without a human: cold-service ready-timeouts (see docs/architecture/services.md,
// "Failure semantics") and evictable builders (see docs/architecture/scaling.md,
// "The one real prerequisite: auto-requeue on infra errors"). Scope is
// deliberately narrow: only OutcomeError (never OutcomeRejected — a red
// verdict is an author problem — and never OutcomeConflict), exactly once
// per (ref, SHA), gated by Config.AutoRetryErrors (default true, see that
// field's doc).

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
// The once-per-SHA guard is checkpointed with parks and queue order. A vanished
// ref or new SHA clears its old budget during reconciliation.
func (d *Daemon) maybeAutoRetry(ctx context.Context, target string, cand core.Candidate, outcome core.Outcome) {
	if outcome != core.OutcomeError || !d.cfg.AutoRetryErrors {
		return
	}
	// No re-admission while draining (issue #8): an auto-retry would put
	// fresh work into the finite drain set. The park stands; a restart
	// resumes normal auto-retry against the still-parked (ref, SHA).
	if d.draining {
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
