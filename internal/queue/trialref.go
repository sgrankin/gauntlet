package queue

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sgrankin/gauntlet/internal/core"
)

// trialRefName is a run's trial-ref name: the configured prefix plus the
// run ID (the batch chain uses its head run ID). The prefix is a custom
// namespace (default refs/gauntlet/trials, issue #7), deliberately not
// refs/heads/** so the push neither clutters branch UIs nor triggers
// workflows.
func (d *Daemon) trialRefName(runID string) string {
	return d.cfg.TrialRefPrefix + "/" + runID
}

// createTrialRef CAS-publishes mergeOID under the run's trial ref before
// any check starts, so the tested MergeSHA is resolvable on the remote and
// can carry a verification status. Idempotent: a re-publish of the same
// run's merge (crash-retry) is git's up-to-date no-op. A ref already
// present at a DIFFERENT SHA is an operational collision (a second daemon,
// a run-id reuse) — reported as an error, never force-overwritten (a trial
// ref is immutable). The latency of this synchronous round trip is
// recorded on the run's span. Returns the ref name on success.
func (d *Daemon) createTrialRef(ctx context.Context, runID, mergeOID string, rootSpan trace.Span) (string, error) {
	ref := d.trialRefName(runID)
	start := d.now()
	err := d.git.CASUpdate(ctx, ref, "", mergeOID)
	rootSpan.SetAttributes(attribute.Int64("gauntlet.trialref.publish_ms", d.now().Sub(start).Milliseconds()))
	if errors.Is(err, core.ErrCASStale) {
		return "", fmt.Errorf("trial ref %s already exists at a different SHA (operational collision)", ref)
	}
	if err != nil {
		return "", err
	}
	return ref, nil
}

// deleteTrialRefNow CAS-deletes a landed run's now-redundant trial ref (the
// target reaches the merge, so the ref anchors nothing) and clears
// r.trialRef so finalizeRun's retention path skips it. Best-effort: a stale
// or failed delete just leaves the ref for the boot sweep — never a reason
// to fail a landing.
func (d *Daemon) deleteTrialRefNow(ctx context.Context, r *run) {
	if r.trialRef == "" {
		return
	}
	_ = d.git.CASUpdate(ctx, r.trialRef, r.chainTip, "")
	delete(d.trialReap, r.trialRef)
	r.trialRef = ""
}

// scheduleTrialReap disposes a non-landing run's trial ref: deleted
// immediately when retention is zero, otherwise retained until
// now+retention (reapTrialRefs deletes it then) so a failed synthetic
// merge stays inspectable for a bounded window. Keyed on the merge SHA so
// the eventual delete only removes a ref that still names it.
func (d *Daemon) scheduleTrialReap(ctx context.Context, ref, mergeOID string) {
	if ref == "" {
		return
	}
	if d.cfg.TrialRefRetention <= 0 {
		_ = d.git.CASUpdate(ctx, ref, mergeOID, "")
		return
	}
	d.trialReap[ref] = trialReapEntry{sha: mergeOID, at: d.now().Add(d.cfg.TrialRefRetention)}
}

// reapTrialRefs CAS-deletes every retained trial ref whose retention
// window has elapsed. Called once per reconcile tick (cheap — no remote
// round trip unless something is actually due). The CAS key is the stored
// merge SHA, so a ref an operator or another daemon recreated/changed is
// left alone; a stale result means someone already changed it, so the
// entry is dropped either way.
func (d *Daemon) reapTrialRefs(ctx context.Context) {
	if len(d.trialReap) == 0 {
		return
	}
	now := d.now()
	for ref, e := range d.trialReap {
		if now.Before(e.at) {
			continue
		}
		err := d.git.CASUpdate(ctx, ref, e.sha, "")
		if err == nil || errors.Is(err, core.ErrCASStale) {
			delete(d.trialReap, ref)
		}
		// A transient error (network) leaves the entry for a later tick.
	}
}
