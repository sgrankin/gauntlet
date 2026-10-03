package queue

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
	"github.com/sgrankin/gauntlet/internal/policy"
)

// stampReceiptRecords mirrors r.receiptBlobSHA/r.receiptPublished (and
// d.cfg.ReceiptNotes.Ref) onto every member's RunRecord — the common record
// path every terminal event and history write reads from. Called exactly
// once, from landRun, right after a successful PublishNote and before the
// target CAS is even attempted: that ordering is what makes an orphaned
// publication (the target CAS then fails or the process crashes) keep its
// provenance instead of recording as if nothing was ever published — the
// orphan is precisely the case that most needs this data for diagnosis.
// Never called when d.cfg.ReceiptNotes is nil (landRun's own gate), so an
// unconfigured policy's records stay all-empty, unchanged.
func (d *Daemon) stampReceiptRecords(r *run) {
	for i := range r.members {
		r.members[i].rec.ReceiptRef = d.cfg.ReceiptNotes.Ref
		r.members[i].rec.ReceiptBlob = r.receiptBlobSHA
		r.members[i].rec.ReceiptPublished = r.receiptPublished
	}
}

// landRun is the generalized land: one CAS push lands the whole chain, then
// a per-member slot delete + terminal event, FIFO. len(r.members)==1 for
// serial/speculate: one push, one delete, one event. A batch run has N
// members: one push, N deletes, N EventLanded — one per candidate, in FIFO
// order.
//
// A stale target CAS means the target moved between trial and land — Skip,
// keep every member's slot, retry next tick (Invariant 2). A stale
// slot-delete CAS means the author re-pushed between land and delete — the
// landed commit still holds exactly the tested SHA (Invariant 1), that
// member's slot simply survives at its new SHA and re-queues naturally
// (Invariant 3); the run is still a Landed outcome.
func (d *Daemon) landRun(ctx context.Context, t config.Target, r *run) {
	if _, paused := d.controls.Pauses[t.Name]; paused && !r.overridePause {
		return
	}
	_, landSpan := obs.StartLand(r.rootCtx, d.tr)
	members := make([]core.Candidate, len(r.members))
	for i, m := range r.members {
		members[i] = m.cand
	}
	var reviewMembers []core.Candidate
	for _, member := range members {
		if member.Source != "" {
			reviewMembers = append(reviewMembers, member)
		}
	}
	var fresh map[string]policy.Input
	if source, ok := d.cfg.Reviews.(interface {
		ValidatePolicyInputs(context.Context, []core.Candidate, bool) (map[string]policy.Input, error)
	}); ok && len(reviewMembers) > 0 {
		var err error
		fresh, err = source.ValidatePolicyInputs(ctx, reviewMembers, r.emergencyReason != "")
		if err != nil {
			obs.EndSpan(landSpan, err)
			d.finishRun(ctx, t, r, core.OutcomeSkipped, "review changed before land: "+err.Error(), false)
			return
		}
	}
	if d.cfg.Reviews != nil && fresh == nil {
		for _, m := range r.members {
			if m.cand.Source != "" {
				var err error
				if r.emergencyReason != "" {
					if validator, ok := d.cfg.Reviews.(interface {
						ValidateEmergency(context.Context, core.Candidate) error
					}); ok {
						err = validator.ValidateEmergency(ctx, m.cand)
					} else {
						err = fmt.Errorf("forge does not support honest emergency validation")
					}
				} else {
					err = d.cfg.Reviews.Validate(ctx, m.cand)
				}
				if err != nil {
					obs.EndSpan(landSpan, err)
					d.finishRun(ctx, t, r, core.OutcomeSkipped, "review changed before land: "+err.Error(), false)
					return
				}
			}
		}
	}
	for _, m := range r.members {
		var prepared []policy.Input
		if fresh != nil && m.cand.Source != "" {
			input, ok := fresh[m.cand.Ref]
			if !ok {
				err := fmt.Errorf("fresh policy snapshot missing for %s", m.cand.Ref)
				obs.EndSpan(landSpan, err)
				d.finishRun(ctx, t, r, core.OutcomeSkipped, err.Error(), false)
				return
			}
			prepared = append(prepared, input)
		}
		if err := d.evaluatePolicy(ctx, t, m.cand, r.baseOID, members, m.rec.Checks, "landing", r.emergencyReason != "", r.overridePause, prepared...); err != nil {
			obs.EndSpan(landSpan, err)
			d.finishRun(ctx, t, r, core.OutcomeRejected, "landing policy: "+err.Error(), true)
			return
		}
	}

	// Receipt-notes publication (issue #13) is a GATE on landing, not a
	// parallel step: when policy is enabled, this run is structurally
	// unable to reach the target CAS below without publication first
	// confirmed — the call sits in this same function, immediately before
	// that CAS, so there is no path from here to a landed target that
	// skips it. Publishes on r.chainTip, the tested CHAIN TIP merge commit
	// (== the batch's own chain tip for a batch run, whose single receipt
	// node already keyed capture to this same run) — never the candidate
	// SHA, never chainTree. A speculative run's captured payload sits
	// harmlessly on r.receiptPayload until (unless) it reaches HERE as the
	// lane head; a non-head or later-invalidated run never calls this.
	if d.cfg.ReceiptNotes != nil {
		res, err := d.git.PublishNote(ctx, d.cfg.ReceiptNotes.Ref, r.chainTip, r.receiptPayload, d.cfg.Committer)
		if err != nil {
			// Any failure here — transport, contention exhaustion, or a
			// fail-closed ErrNoteConflict — is infrastructure: OutcomeError
			// park, subject to the existing auto-retry-once, target CAS
			// never attempted, nothing lands. ErrNoteConflict gets its own
			// wording naming the invariant it violates (two disjoint
			// receipts computed for the same tested SHA), distinct from an
			// ordinary transport error.
			detail := "publish receipt note: " + err.Error()
			if errors.Is(err, core.ErrNoteConflict) {
				detail = "publish receipt note: invariant violation (a different receipt already exists for this tested SHA): " + err.Error()
			}
			obs.EndSpan(landSpan, err)
			d.finishRun(ctx, t, r, core.OutcomeError, detail, true)
			return
		}
		// Both Published (fresh) and !Published (AlreadyPublished) are
		// landing successes — AlreadyPublished is substantive (an
		// idempotent crash-retried or duplicate publish of the SAME
		// receipt), logged distinctly here via the span attribute
		// EndRun/runAttributes attaches from the record below, and
		// mirrored onto every member's RunRecord for history's v12
		// provenance columns.
		r.receiptBlobSHA = res.NoteBlobSHA
		r.receiptPublished = receiptPublishedFresh
		if !res.Published {
			r.receiptPublished = receiptPublishedAlready
		}
		// Stamp provenance onto every member's record IMMEDIATELY — before
		// the target CAS below is even attempted, not only after it
		// succeeds. A publish that then loses the target race (stale CAS,
		// crash, a disjoint concurrent writer — the orphan case documented
		// in DESIGN.md's decision ledger) still confirms a real note exists
		// for r.chainTip; recording that here means the orphan's own
		// history/API/span rows carry ref+blob+published for diagnosis,
		// same as a landed run's, rather than reading as if nothing was
		// ever published. finishRun's own per-member loop (the stale-CAS
		// and error paths both route through it) only ever touches
		// Outcome/Detail/EndedAt, never these fields, so a value stamped
		// here survives untouched to every terminal path. Never the
		// payload itself — r.receiptPayload stays in-memory only.
		d.stampReceiptRecords(r)
	}

	err := d.git.CASUpdate(ctx, targetRefName(t), r.baseOID, r.chainTip)
	if err != nil {
		// Stale or not, a failed target push must Skip — never park
		// (Invariant 2: "fail cleanly and trigger re-trial"). The non-stale
		// case matters because a real push can fail ambiguously (a
		// client-visible error after the update actually took effect
		// server-side); parking would freeze the slot and keep the
		// IsAncestor recovery path — which exists precisely to heal that
		// ambiguity — from ever running. Skipping lets the next tick
		// re-derive ground truth either way: recovery-delete if the push
		// landed, a fresh trial if it didn't.
		obs.EndSpan(landSpan, err)
		detail := "target moved before land; slot kept, retried next tick"
		if !errors.Is(err, core.ErrCASStale) {
			detail = "land: push target: " + err.Error() + "; slot kept, retried next tick"
			// The ambiguous case in the flesh: the push may have taken
			// effect server-side, making this chain the real target tip —
			// so its pin must not be released until ground truth settles.
			// Registering in landedPins makes finalizeRun (below, via
			// finishRun) leave the pin alone; the release loop then unpins
			// once a fetch shows the tip anchored, or retains the pin for
			// startup's sweep if the push genuinely failed (the field's
			// doc has the accounting). A definite stale lease needs none of
			// this: the push did NOT happen, and the re-trial pins its own
			// fresh chain.
			d.landedPins[r.chainTip] = targetRefName(t)
		}
		d.finishRun(ctx, t, r, core.OutcomeSkipped, detail, false)
		return
	}

	// The landing's GC pin is handed to landedPins rather than released
	// here: the chain becomes locally reachable (via the remote-tracking
	// target ref) only once a Fetch reflects this push, and post-land hooks
	// may export the merge from a backlog well after this call — see the
	// field's doc. finalizeRun's own unpin skips registered runs for
	// exactly this reason.
	d.landedPins[r.chainTip] = targetRefName(t)

	// Any successful landing for this target clears batch-red serial
	// fallback, resuming batching on the next refill —
	// whether this landing is an ordinary batch, or one round of the
	// fallback's own one-at-a-time walk. delete is a no-op when the flag
	// was never set (every non-batch mode, or a batch target that never
	// went red).
	delete(d.batchFallback, t.Name)
	delete(d.batchRecovery, t.Name)

	for i := range r.members {
		m := &r.members[i]
		var delErr error
		if m.cand.Source == "" {
			delErr = d.git.CASUpdate(ctx, m.cand.Ref, m.cand.SHA, "")
		} else {
			delErr = d.cfg.Reviews.Landed(ctx, m.cand, m.mergeOID)
		}
		detail := ""
		switch {
		case errors.Is(delErr, core.ErrCASStale):
			detail = "candidate re-pushed before slot delete; slot survives at new SHA and re-queues"
		case delErr != nil:
			detail = "land: delete slot: " + delErr.Error()
		}
		urgent := m.cand.Urgent
		if revision, ok := d.controls.Urgent[m.cand.Ref]; ok && revision.SHA == m.cand.SHA && revision.Version == m.cand.Version {
			urgent = true
		}
		if urgent {
			d.urgentBurst[t.Name]++
		} else {
			d.urgentBurst[t.Name] = 0
		}
		m.rec.Outcome = core.OutcomeLanded
		memberDetail := detail
		if r.emergencyReason != "" {
			memberDetail = "EMERGENCY: checks bypassed; " + r.emergencyReason + "; " + detail
		}
		if r.pauseOverrideReason != "" {
			memberDetail = "PAUSE OVERRIDE: " + r.pauseOverrideReason + "; " + memberDetail
		}
		m.rec.Detail = memberDetail
		m.rec.EndedAt = d.now()
		// Receipt provenance (issue #13) is already on m.rec by now —
		// stampReceiptRecords ran right after the publish, above, before
		// this CAS was even attempted, specifically so it isn't lost on
		// the paths that never reach this loop (a stale/failed target CAS
		// routes through finishRun instead). "" on every field when the
		// policy is off, unchanged.
		d.emit(ctx, core.Event{
			Kind:      core.EventLanded,
			At:        d.now(),
			Target:    t.Name,
			Candidate: m.cand,
			RunID:     m.rec.RunID,
			Record:    m.rec,
			Detail:    m.rec.Detail,
		})
	}
	if r.controlPrepared {
		d.clearEmergency(t.Name, "")
	}
	// Deliberate ordering exception vs. the other terminal paths: finishRun
	// finalizes (root-span end, export-dir removal) *before* emitting its
	// terminal event, but here each member's EventLanded is emitted above,
	// before finalizeRun runs — the delete-then-emit-per-member FIFO shape.
	// Unobservable today (no event or record carries the dir path,
	// and spans are no-op), but don't write a consumer — or a batch-chunk
	// extension — that assumes the dir is gone by EventLanded time.
	// The trial ref is now redundant — the target reaches the merge — so
	// delete it immediately (issue #7). Clears r.trialRef so finalizeRun's
	// retention path skips it.
	d.deleteTrialRefNow(ctx, r)

	obs.EndSpan(landSpan, nil) // the land itself (target push) succeeded regardless of any slot-delete outcome
	d.finalizeRun(ctx, r)
}

// finishRun finalizes every member's RunRecord (outcome/detail/end time),
// optionally parks each (ref, SHA), finalizes the run once (root span end,
// dir removal — finalizeRun), and emits one terminal event per member, in
// member order (mirroring landRun's own per-member-then-once-per-run
// shape). len(r.members)==1 for serial/speculate and for a batch-red run
// (which advanceLane routes to finishBatchRed instead, never here) — so
// this loop is a one-element loop in every case except a move/target
// invalidation of a genuine multi-member batch (invalidateSuffix, park
// always false there): every member of an invalidated batch must Skip and
// re-queue, none of them singled out.
func (d *Daemon) finishRun(ctx context.Context, t config.Target, r *run, outcome core.Outcome, detail string, park bool) {
	if (r.emergencyReason != "" || r.pauseOverrideReason != "") && outcome != core.OutcomeSkipped && outcome != core.OutcomeLanded {
		d.clearEmergency(t.Name, "emergency stopped: "+detail)
	}
	// A run concluded by its own verdict already materialized its records
	// in advanceChecks (idempotent guard); this covers the externally
	// concluded paths (move/cancel/skip), whose records carry whatever
	// finished before the abort.
	d.materializeChecks(r)
	for i := range r.members {
		m := &r.members[i]
		m.rec.Outcome = outcome
		memberDetail := detail
		if r.emergencyReason != "" {
			memberDetail = "EMERGENCY: checks bypassed; " + r.emergencyReason + "; " + detail
		}
		if r.pauseOverrideReason != "" {
			memberDetail = "PAUSE OVERRIDE: " + r.pauseOverrideReason + "; " + memberDetail
		}
		m.rec.Detail = memberDetail
		m.rec.EndedAt = d.now()

		if park {
			d.park(t.Name, m.cand, outcome, detail, m.rec.RunID)
		}
	}

	d.finalizeRun(ctx, r)

	for i := range r.members {
		m := &r.members[i]
		d.emit(ctx, core.Event{
			Kind:      eventKindForOutcome(outcome),
			At:        d.now(),
			Target:    t.Name,
			Candidate: m.cand,
			RunID:     m.rec.RunID,
			Record:    m.rec,
			Detail:    detail,
		})
		// See autoretry.go: only a member that was actually
		// just parked (park==true) is eligible — maybeAutoRetry itself
		// no-ops for anything but OutcomeError, but gating on park here too
		// avoids ever consulting/clearing a stale, unrelated park entry for
		// a member this call never touched (the park==false invalidation
		// path above never calls d.park for these members at all).
		if park {
			d.maybeAutoRetry(ctx, t.Name, m.cand, outcome)
		}
	}
}

// finalizeRun performs the once-per-run cleanup that must happen exactly
// once regardless of member count: ends the root span (its summary
// attributes/status come from the head member's
// RunRecord — the representative summary for a batch's shared span; each
// member's own full record is still what's emitted per terminal event) and
// removes the exported trial dir. Per-check log files
// (LogDir, if configured) are deliberately never touched here: they
// outlive the run by design (DESIGN.md "Full per-check log files") —
// retention is a separate, later prune mechanism, not this state machine's
// job. It does not touch the lane: lane.runs is truncated by
// advanceLane/invalidateSuffix, the sole mutators of that slice, at the
// same call sites that already know the removal boundary.
//
// It also releases the run's GC pin (created at startRun/finishBatchStart's
// pin site) — unless landRun already handed the pin to landedPins, which it
// does before finalizing: a landed chain must stay pinned until the next
// successful Fetch anchors it through the remote-tracking target ref. The
// handoff registration itself is the authority here, not an outcome field a
// caller might set without going through landRun — so a hypothetical future
// path that marks members Landed some other way still releases its pin
// rather than leaking it, and a path that registers the handoff never
// double-releases.
func (d *Daemon) finalizeRun(ctx context.Context, r *run) {
	if r.releaseSources != nil {
		r.releaseSources()
	}
	obs.EndRun(r.rootSpan, r.members[0].rec)

	if _, deferred := d.landedPins[r.chainTip]; !deferred {
		d.unpin(ctx, r.chainTip)
	}

	// A landing already deleted its trial ref (landRun, clearing
	// r.trialRef); any other terminal disposes by retention — deleted now
	// when retention is zero, otherwise kept briefly for diagnosis and
	// reaped by reapTrialRefs. No-op when the feature is off.
	if r.trialRef != "" {
		d.scheduleTrialReap(ctx, r.trialRef, r.chainTip)
	}

	if r.dir != "" {
		_ = os.RemoveAll(r.dir)
	}
}

// restoreMtimes runs the optional deterministic-mtimes pass
// (Config.HistoryMtimes) over a freshly exported trial dir, keyed off the
// chain-tip MERGE COMMIT — never the bare tree, whose export carries no
// history — and records the walk's cost on the run's root span. No-op when
// the feature is off; an error is the caller's OutcomeError (the tree must
// never silently run with wall-clock metadata while the config promises
// otherwise).
func (d *Daemon) restoreMtimes(ctx context.Context, mergeOID, dir string, rootSpan trace.Span) error {
	if !d.cfg.HistoryMtimes {
		return nil
	}
	stats, err := d.git.RestoreMtimes(ctx, mergeOID, dir)
	if err != nil {
		return err
	}
	rootSpan.SetAttributes(
		attribute.Int("gauntlet.mtimes.entries", stats.Commits),
		attribute.Int("gauntlet.mtimes.paths", stats.Paths),
	)
	return nil
}

// unpin best-effort-releases oid's GC pin. The error is deliberately
// dropped, mirroring finalizeRun's own os.RemoveAll: a pin that outlives
// its run merely protects some garbage until startup's pin sweep clears
// it, which is strictly better than any failure handling this could add.
func (d *Daemon) unpin(ctx context.Context, oid string) {
	_ = d.git.Unpin(ctx, oid)
}

// recoverLanded finishes slot deletion or forge acknowledgement for a
// revision already in target history. Its recovered record has no checks
// and must not trigger hooks.
func (d *Daemon) recoverLanded(ctx context.Context, t config.Target, cand core.Candidate, targetTip string) {
	mergeSHA, _ := d.git.FindLandingMerge(ctx, targetTip, cand.SHA)
	if t.Landing == "squash" {
		version := cand.Version
		if cand.Source != "" {
			version = "*"
		}
		mergeSHA, _ = d.git.(core.LinearGitRepo).FindLanding(ctx, targetTip, cand.Ref, cand.SHA, version)
	}
	var delErr error
	if cand.Source == "" {
		delErr = d.git.CASUpdate(ctx, cand.Ref, cand.SHA, "")
	} else {
		delErr = d.cfg.Reviews.Landed(ctx, cand, mergeSHA)
	}
	if delErr != nil && !errors.Is(delErr, core.ErrCASStale) {
		return // transient; retry next tick
	}
	now := d.now()
	runID := newRunID(now, cand.SHA)
	detail := "candidate already ancestor of target; checks not re-run"
	if t.Landing == "squash" {
		detail = "candidate already recorded in target history; checks not re-run"
	}
	rec := &core.RunRecord{
		RunID:     runID,
		Target:    t.Name,
		Candidate: cand,
		MergeSHA:  mergeSHA,
		Outcome:   core.OutcomeLanded,
		Detail:    detail,
		StartedAt: now,
		EndedAt:   now,
		Recovered: true,
	}
	d.emit(ctx, core.Event{
		Kind: core.EventLanded, At: now, Target: t.Name, Candidate: cand,
		RunID: runID, Record: rec, Detail: detail,
	})
}

// rejectPreMerge parks cand and emits its terminal event for an outcome
// decided before any merge commit exists (a trial-merge conflict, or an
// infra error before CommitTree succeeds): no check ever ran and no run
// object was ever created, so there's nothing to cancel. rootSpan is the
// run's root span if one was already started (startRun starts it before
// MergeTree) — nil for the one outcome that precedes even that (an
// IsAncestor infra error, before it's known whether a trial will even be
// attempted).
func (d *Daemon) rejectPreMerge(ctx context.Context, t config.Target, cand core.Candidate, outcome core.Outcome, detail string, rootSpan trace.Span) {
	now := d.now()
	// These outcomes precede a clean trial, so no merge commit — and for a
	// conflict or MergeTree failure not even a trial tree — exists to name
	// the run after; the candidate's own SHA is the next best stable,
	// human-correlatable stand-in.
	runID := newRunID(now, cand.SHA)
	rec := &core.RunRecord{
		RunID: runID, Target: t.Name, Candidate: cand,
		Outcome: outcome, Detail: detail,
		StartedAt: now, EndedAt: now,
	}
	d.park(t.Name, cand, outcome, detail, runID)
	if rootSpan != nil {
		obs.EndRun(rootSpan, rec)
	}
	d.emit(ctx, core.Event{
		Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
		Candidate: cand, RunID: runID, Record: rec, Detail: detail,
	})
	d.maybeAutoRetry(ctx, t.Name, cand, outcome) // see autoretry.go
}

// skipPreMergePredicted is rejectPreMerge's twin for startRun's two
// pre-merge failure branches — a trial CONFLICT or a buildChainLink infra
// ERROR — when predicted is true: the base being tested against is a
// predecessor's own unpushed chainTip, never the real target tip, so the
// verdict proves nothing about cand itself (the predecessor might be at
// fault, or might never even land). Parking here would be exactly the
// false-negative park already avoided for a post-merge red at lane index >0
// (advanceLane's bubble step) — this mirrors that branch's shape precisely:
// no park, RunRecord.Outcome is OutcomeSkipped (not the raw conflict/error
// outcome), EventSkipped (not EventTrialConflict/EventError), and no
// maybeAutoRetry call (finishRun's own park==true gate has the same effect
// for its callers; there is simply no park here to gate on). cand's slot is
// untouched by this call, so it re-queues and re-forms in a later window;
// if it's still bad once it genuinely reaches lane index 0 against the REAL
// target tip, it parks there for real via rejectPreMerge. See
// docs/design/queue-modes.md ("Red bubble: only index 0 parks" and
// "Conflict against a predicted base is a skip, not a park") for the
// rationale this generalizes.
func (d *Daemon) skipPreMergePredicted(ctx context.Context, t config.Target, cand core.Candidate, detail string, rootSpan trace.Span) {
	now := d.now()
	runID := newRunID(now, cand.SHA)
	rec := &core.RunRecord{
		RunID: runID, Target: t.Name, Candidate: cand,
		Outcome: core.OutcomeSkipped, Detail: detail,
		StartedAt: now, EndedAt: now,
	}
	if rootSpan != nil {
		obs.EndRun(rootSpan, rec)
	}
	d.emit(ctx, core.Event{
		Kind: core.EventSkipped, At: now, Target: t.Name,
		Candidate: cand, RunID: runID, Record: rec, Detail: detail,
	})
}

// rejectRun parks cand and emits its terminal event for an outcome decided
// after the merge commit exists but before any check ran (a missing/invalid
// check spec, or an export failure). rootSpan is always non-nil here: every
// call site follows a successful StartRun.
func (d *Daemon) rejectRun(ctx context.Context, t config.Target, cand core.Candidate, runID, baseOID, mergeOID string, trial core.TrialMerge, outcome core.Outcome, detail string, rootSpan trace.Span) {
	// Every rejectRun call site sits after startRun's pin site (the pin
	// failure itself included — unpinning a never-created pin is a no-op),
	// and this run never reaches finalizeRun, so the pin is released here.
	d.unpin(ctx, mergeOID)
	now := d.now()
	rec := &core.RunRecord{
		RunID: runID, Target: t.Name, Candidate: cand,
		BaseOID: baseOID, MergeSHA: mergeOID, Trial: trial,
		Outcome: outcome, Detail: detail,
		StartedAt: now, EndedAt: now,
	}
	d.park(t.Name, cand, outcome, detail, runID)
	if rootSpan != nil {
		obs.EndRun(rootSpan, rec)
	}
	d.emit(ctx, core.Event{
		Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
		Candidate: cand, RunID: runID, Record: rec, Detail: detail,
	})
	d.maybeAutoRetry(ctx, t.Name, cand, outcome) // see autoretry.go
}

// parkEntry records why a (ref, SHA) is parked — its terminal outcome, a
// human-readable reason, and when — feeding the dashboard snapshot's
// ParkedEntry. Sticky per (ref, SHA): cleared only when the ref's SHA
// changes, the ref vanishes, or a CommandRetry clears it explicitly
// (command.go) — never when some other candidate lands.
type parkEntry struct {
	Version string
	SHA     string
	Outcome core.Outcome
	Reason  string
	At      time.Time

	// RunID is the terminal RunRecord that parked this (ref, SHA) — the
	// dashboard's /t/{target} Parked table links its outcome tag to
	// /run/{RunID} when this is non-empty. Every live call site
	// (finishRun, rejectPreMerge, rejectRun, rejectBatch, the two
	// command.go cancel paths) has its own record's RunID in scope by
	// construction; only a boot-time seed (seedParksOnce, from history
	// predating this field) can leave it "".
	RunID string
}

// park marks cand's (ref, SHA) as parked for target, recording outcome,
// detail, and the RunID of the run that decided it as the park's reason: it
// will not be re-tested until the ref's SHA changes, the ref vanishes, or a
// CommandRetry clears it.
func (d *Daemon) park(target string, cand core.Candidate, outcome core.Outcome, detail, runID string) {
	m := d.done[target]
	if m == nil {
		m = make(map[string]parkEntry)
		d.done[target] = m
	}
	m[cand.Ref] = parkEntry{SHA: cand.SHA, Version: cand.Version, Outcome: outcome, Reason: detail, At: d.now(), RunID: runID}
}

func eventKindForOutcome(o core.Outcome) core.EventKind {
	switch o {
	case core.OutcomeLanded:
		return core.EventLanded
	case core.OutcomeRejected:
		return core.EventRejected
	case core.OutcomeConflict:
		return core.EventTrialConflict
	case core.OutcomeSkipped:
		return core.EventSkipped
	default: // core.OutcomeError
		return core.EventError
	}
}

// runIDTimeFormat is the UTC timestamp portion of a run ID: yyyymmddThhmmssZ.
const runIDTimeFormat = "20060102T150405Z"

// A random starting point separates same-second runs across process restarts.
// The package-level counter also separates daemon instances in one process.
var runIDCounter = func() *atomic.Uint64 {
	counter := new(atomic.Uint64)
	counter.Store(1<<62 + rand.Uint64()>>2)
	return counter
}()

// newRunID combines start time, a randomly seeded sequence, and the trial tree.
func newRunID(t time.Time, oid string) string {
	if len(oid) > 12 {
		oid = oid[:12]
	}
	seq := runIDCounter.Add(1)
	return fmt.Sprintf("%s-%d-%s", t.UTC().Format(runIDTimeFormat), seq, oid)
}

// memberRunID derives a batch member's own RunRecord.RunID at position pos
// within a batch whose bare (chain-mint-time) run ID is batchRunID.
//
// Bug fixed here: every member of a batch used to share batchRunID verbatim
// as its RunRecord.RunID too (not just as BatchID) — and history's runs
// table PRIMARY KEYs on run_id with INSERT OR REPLACE (internal/history/
// schema.sql), so writing N members with the same RunID silently replaced
// the first N-1 rows with the last, gutting the batch-members history/
// dashboard feature for both green (N landed) and red (N skipped) batches.
//
// Fix: position 0 (the head member) keeps the bare batchRunID verbatim — the
// mid-run EventCheckStarted/EventCheckFinished events (one per check, shared
// across the whole batch, keyed on run.runID) and the Slack root's
// trial-clean tracking key both stay anchored to a real, single-member
// identity. Every later member (pos > 0) gets a distinct
// "<batchRunID>-mN" suffix, so each lands its own history row.
// BatchID stays batchRunID, unsuffixed, for every member (unchanged) — it's
// the join key BatchMembers and Slack's batch-aware root lookup use.
func memberRunID(batchRunID string, pos int) string {
	if pos == 0 {
		return batchRunID
	}
	return fmt.Sprintf("%s-m%d", batchRunID, pos)
}
