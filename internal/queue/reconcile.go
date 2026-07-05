package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
)

// candidatePrefix is the fixed portion of the candidate ref grammar
// (docs/plans/phase1.md §9.3): "refs/heads/for/<target>/<rest>".
const candidatePrefix = "refs/heads/for/"

// parseCandidateRef parses a candidate ref's grammar (§9.3). If <rest> (the
// portion after the target segment) has two or more slash-separated
// segments, the first is user and the remainder — slashes allowed — is
// topic (e.g. "for/main/alice/feat/foo" -> target "main", user "alice",
// topic "feat/foo"). A single segment means user=="" (solo setups) and
// topic is that segment. ok is false for anything that doesn't fit: wrong
// prefix, empty target, no topic, or an empty user/topic segment.
func parseCandidateRef(ref string) (target, user, topic string, ok bool) {
	rest, found := strings.CutPrefix(ref, candidatePrefix)
	if !found {
		return "", "", "", false
	}
	i := strings.Index(rest, "/")
	if i <= 0 {
		return "", "", "", false // no target/rest split, or empty target
	}
	target = rest[:i]
	remainder := rest[i+1:]
	if remainder == "" {
		return "", "", "", false // target with no topic at all
	}
	if j := strings.Index(remainder, "/"); j >= 0 {
		user = remainder[:j]
		topic = remainder[j+1:]
		if user == "" || topic == "" {
			return "", "", "", false
		}
	} else {
		topic = remainder
	}
	return target, user, topic, true
}

// discoverCandidates extracts every well-formed candidate ref for target out
// of refs (the tick's ListRefs snapshot).
func discoverCandidates(target string, refs map[string]string) map[string]core.Candidate {
	out := make(map[string]core.Candidate)
	for ref, sha := range refs {
		t, user, topic, ok := parseCandidateRef(ref)
		if !ok || t != target {
			continue
		}
		out[ref] = core.Candidate{Ref: ref, Target: target, User: user, Topic: topic, SHA: sha}
	}
	return out
}

// targetRefName is the git ref for a target's branch.
func targetRefName(t config.Target) string { return "refs/heads/" + t.Branch }

// reconcileTarget runs one tick's worth of the per-target state machine
// (docs/plans/phase1.md §3): snapshot bookkeeping, then either advance the
// in-flight run or (if there is none) try to start the next one.
//
// A run present at the start of the tick claims the whole tick — one lane —
// even if it concludes (lands, parks, or skips) during this same call: a
// concluding Land mutates both the target and slot refs out from under
// targetTip/cands, which were snapshotted once at the top of this function,
// so immediately reusing them to start a new trial would trial-merge
// against stale ground truth (observed as re-testing the very candidate
// that had just landed). Deferring the next pick to the following tick,
// which re-Fetches/re-ListRefs, avoids that staleness entirely; the cost is
// at most one idle tick of latency per landing, negligible next to the poll
// interval already inherent to the loop.
func (d *Daemon) reconcileTarget(ctx context.Context, t config.Target, refs map[string]string) {
	targetTip := refs[targetRefName(t)]
	cands := discoverCandidates(t.Name, refs)
	d.syncBookkeeping(ctx, t, cands)

	if r := d.runs[t.Name]; r != nil {
		d.reconcileInFlight(ctx, t, targetTip, cands, r)
		return
	}
	d.tryStartTrial(ctx, t, targetTip, cands)
}

// syncBookkeeping updates order and done against this tick's candidates
// (§9.1): drops entries for refs that vanished, clears park entries whose
// SHA changed (a re-push), and assigns a fresh sequence number — emitting
// EventQueued — to every ref seen for the first time.
func (d *Daemon) syncBookkeeping(ctx context.Context, t config.Target, cands map[string]core.Candidate) {
	order := d.order[t.Name]
	if order == nil {
		order = make(map[string]int64)
		d.order[t.Name] = order
	}
	done := d.done[t.Name]
	if done == nil {
		done = make(map[string]string)
		d.done[t.Name] = done
	}

	for ref := range order {
		if _, ok := cands[ref]; !ok {
			delete(order, ref)
		}
	}
	for ref, sha := range done {
		if c, ok := cands[ref]; !ok || c.SHA != sha {
			delete(done, ref)
		}
	}

	var newRefs []string
	for ref := range cands {
		if _, ok := order[ref]; !ok {
			newRefs = append(newRefs, ref)
		}
	}
	sort.Strings(newRefs) // deterministic sequence assignment within one batch
	for _, ref := range newRefs {
		order[ref] = d.seq
		d.seq++
		d.emit(ctx, core.Event{Kind: core.EventQueued, At: d.now(), Target: t.Name, Candidate: cands[ref]})
	}
}

// pickHead returns the queue head: the candidate with the smallest order
// (tie-broken lexically by ref) whose current SHA is not parked in done
// (§9.1). ok is false if every candidate is parked or none exist.
func (d *Daemon) pickHead(target string, cands map[string]core.Candidate) (core.Candidate, bool) {
	order := d.order[target]
	done := d.done[target]

	var refs []string
	for ref, c := range cands {
		if parked, ok := done[ref]; ok && parked == c.SHA {
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return core.Candidate{}, false
	}
	sort.Slice(refs, func(i, j int) bool {
		if order[refs[i]] != order[refs[j]] {
			return order[refs[i]] < order[refs[j]]
		}
		return refs[i] < refs[j]
	})
	return cands[refs[0]], true
}

// reconcileInFlight advances r by one tick (§3 step 2): a moved/deleted
// candidate or a moved target cancels and Skips; otherwise a non-blocking
// read of the current check's result either finds nothing yet (r survives)
// or records the verdict and either short-circuits (Err/Failed), advances to
// the next check, or lands. The bool return (true iff r is still in flight
// afterward) is informational only — reconcileTarget always returns after
// calling this once per tick either way (see its doc).
func (d *Daemon) reconcileInFlight(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate, r *run) bool {
	if cur, exists := cands[r.cand.Ref]; !exists || cur.SHA != r.cand.SHA {
		d.cancelRun(r)
		d.finishRun(ctx, t, r, core.OutcomeSkipped, fmt.Sprintf("candidate ref %s moved or vanished mid-run (Invariant 5)", r.cand.Ref), false)
		return false
	}
	if targetTip != r.baseOID {
		d.cancelRun(r)
		d.finishRun(ctx, t, r, core.OutcomeSkipped, fmt.Sprintf("target %s moved mid-run (Invariant 5)", t.Name), false)
		return false
	}

	select {
	case res := <-r.cur.result:
		obs.EndCheck(r.cur.span, res)
		r.rec.Checks = append(r.rec.Checks, res)
		d.emit(ctx, core.Event{Kind: core.EventCheckFinished, At: d.now(), Target: t.Name, Candidate: r.cand, RunID: r.runID, CheckName: res.Name})
		r.cur = nil

		switch {
		case res.Err != nil:
			d.finishRun(ctx, t, r, core.OutcomeError, fmt.Sprintf("check %q: %v", res.Name, res.Err), true)
			return false
		case res.Status == core.CheckFailed:
			d.finishRun(ctx, t, r, core.OutcomeRejected, fmt.Sprintf("check %q failed", res.Name), true)
			return false
		default: // CheckPassed or CheckSkipped: both count as green (§5A)
			if r.idx+1 < len(r.checks) {
				r.idx++
				d.startCheck(ctx, r)
				return true
			}
			d.land(ctx, t, r)
			return false
		}
	default:
		return true // current check still running
	}
}

// startCheck launches r.checks[r.idx] via the configured Executor in its own
// goroutine, which communicates back solely by sending once on the
// checkInFlight's one-shot result channel.
func (d *Daemon) startCheck(ctx context.Context, r *run) {
	check := r.checks[r.idx]
	checkCtx, cancel := context.WithCancel(r.rootCtx)
	spanCtx, span := obs.StartCheck(checkCtx, d.tr, check.Name)

	job := core.CheckJob{
		RunID:     r.runID,
		Target:    r.target,
		Name:      check.Name,
		Command:   check.Command,
		Dir:       r.dir,
		BaseSHA:   r.baseOID,
		MergeSHA:  r.mergeOID,
		Candidate: r.cand,
		Clean:     false, // reserved for the phase-4 clean-build escape hatch
	}

	result := make(chan core.CheckResult, 1)
	start := d.now()
	go func() {
		result <- d.exec.RunCheck(spanCtx, job)
	}()
	r.cur = &checkInFlight{name: check.Name, cancel: cancel, result: result, span: span, start: start}

	d.emit(ctx, core.Event{Kind: core.EventCheckStarted, At: d.now(), Target: r.target, Candidate: r.cand, RunID: r.runID, CheckName: check.Name})
}

// cancelRun aborts r's current check, if any (Invariant 5): cancels its
// context (the executor is responsible for killing the underlying process
// group, §9.5) and ends its span without waiting for the executor goroutine,
// which reports into a buffered channel nobody needs to read anymore.
func (d *Daemon) cancelRun(r *run) {
	if r.cur == nil {
		return
	}
	r.cur.cancel()
	obs.EndSpan(r.cur.span, context.Canceled)
	r.cur = nil
}

// tryStartTrial picks the queue head (if any) and starts its trial: recovery
// via IsAncestor, else MergeTree, and on a clean merge, the CommitTree +
// check-spec read + export that produces an in-flight run (§3 step 3).
//
// Every daemon-side infra failure in this path (IsAncestor, MergeTree,
// CommitTree, ExportTree, temp-dir creation) is handled uniformly:
// OutcomeError + park + EventError. Parking prevents an unbounded
// retry-every-tick loop (§9.2's explicit phase-1 ruling: backoff/auto-retry
// is phase 2), and the distinct EventError lets operators tell infra from
// red; a restart or a re-push clears the park.
func (d *Daemon) tryStartTrial(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
	cand, ok := d.pickHead(t.Name, cands)
	if !ok {
		return
	}

	landed, err := d.git.IsAncestor(ctx, cand.SHA, targetTip)
	if err != nil {
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "is-ancestor: "+err.Error())
		return
	}
	if landed {
		// Invariant 4: a run already landed before a crash interrupted slot
		// cleanup. No trial ran, so there is no RunRecord for this — it's a
		// pure recovery action, not a run.
		if delErr := d.git.CASUpdate(ctx, cand.Ref, cand.SHA, ""); delErr != nil && !errors.Is(delErr, core.ErrCASStale) {
			return // transient; retry next tick
		}
		d.emit(ctx, core.Event{
			Kind: core.EventLanded, At: d.now(), Target: t.Name, Candidate: cand,
			Detail: "recovered: candidate SHA already an ancestor of target",
		})
		return
	}

	_, trialSpan := obs.StartTrialMerge(ctx, d.tr)
	trial, err := d.git.MergeTree(ctx, targetTip, cand.SHA)
	if err != nil {
		obs.EndSpan(trialSpan, err)
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "merge-tree: "+err.Error())
		return
	}
	if !trial.Clean {
		obs.EndSpan(trialSpan, nil)
		d.rejectPreMerge(ctx, t, cand, core.OutcomeConflict, "trial merge conflict: "+strings.Join(trial.Conflicts, ", "))
		return
	}
	obs.EndSpan(trialSpan, nil)
	d.emit(ctx, core.Event{Kind: core.EventTrialClean, At: d.now(), Target: t.Name, Candidate: cand})

	// Run ID from the trial *tree* OID, not the merge commit OID — a
	// deliberate deviation from §9.4's letter. The merge commit's message
	// must carry a Gauntlet-Run trailer containing the run ID (§3), and a
	// commit's OID is a hash over its own message, so a run ID containing
	// mergeOID[:12] is a genuine circular dependency — no commit can embed
	// (a prefix of) its own hash. The trial tree's OID is known before any
	// commit exists, is content-addressed to exactly what the checks test,
	// and stays human-correlatable (`git log --format='%H %T'` on the
	// target ties each merge commit to its tree). §9.4's other property —
	// uniqueness across restarts with no persistence — comes from the
	// timestamp, exactly as specified. Commit-to-run correlation is the
	// trailer's job; run-to-commit is RunRecord.MergeSHA's.
	runID := newRunID(d.now(), trial.TreeOID)
	msg, err := buildMergeMessage(d.cfg.MergeMessage, messageFields{Topic: cand.Topic, User: cand.User, Ref: cand.Ref, Target: t.Name, RunID: runID})
	if err != nil {
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "merge-message template: "+err.Error())
		return
	}
	mergeOID, err := d.git.CommitTree(ctx, trial.TreeOID, []string{targetTip, cand.SHA}, msg, d.cfg.Committer)
	if err != nil {
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "commit-tree: "+err.Error())
		return
	}

	specData, err := d.git.ReadFileFromTree(ctx, trial.TreeOID, d.cfg.CheckSpec)
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, targetTip, mergeOID, trial, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err))
		return
	}
	spec, err := config.ParseChecks(specData)
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, targetTip, mergeOID, trial, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err))
		return
	}

	dir, err := os.MkdirTemp("", "gauntlet-trial-")
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, targetTip, mergeOID, trial, core.OutcomeError, "export tree: mkdir temp: "+err.Error())
		return
	}
	if err := d.git.ExportTree(ctx, trial.TreeOID, dir); err != nil {
		_ = os.RemoveAll(dir)
		d.rejectRun(ctx, t, cand, runID, targetTip, mergeOID, trial, core.OutcomeError, "export tree: "+err.Error())
		return
	}

	rootCtx, rootSpan := obs.StartRun(ctx, d.tr, runID, t.Name, cand, mergeOID)
	r := &run{
		target:   t.Name,
		cand:     cand,
		baseOID:  targetTip,
		mergeOID: mergeOID,
		runID:    runID,
		dir:      dir,
		checks:   spec.Checks,
		idx:      0,
		rec: &core.RunRecord{
			RunID:     runID,
			Target:    t.Name,
			Candidate: cand,
			BaseOID:   targetTip,
			MergeSHA:  mergeOID,
			Trial:     trial,
			StartedAt: d.now(),
		},
		rootCtx:  rootCtx,
		rootSpan: rootSpan,
	}
	d.runs[t.Name] = r
	d.startCheck(ctx, r)
}

// land is §3 step 4: CAS-push the target to the tested merge commit, then
// CAS-delete the candidate slot. A stale target CAS means the target moved
// between trial and land — Skip, keep the slot, retry next tick (Invariant
// 2). A stale slot-delete CAS means the author re-pushed between land and
// delete — the landed commit still holds exactly the tested SHA (Invariant
// 1), the slot simply survives at its new SHA and re-queues naturally
// (Invariant 3); the run is still a Landed outcome.
func (d *Daemon) land(ctx context.Context, t config.Target, r *run) {
	_, landSpan := obs.StartLand(r.rootCtx, d.tr)

	err := d.git.CASUpdate(ctx, targetRefName(t), r.baseOID, r.mergeOID)
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
		}
		d.finishRun(ctx, t, r, core.OutcomeSkipped, detail, false)
		return
	}

	delErr := d.git.CASUpdate(ctx, r.cand.Ref, r.cand.SHA, "")
	detail := ""
	switch {
	case errors.Is(delErr, core.ErrCASStale):
		detail = "candidate re-pushed before slot delete; slot survives at new SHA and re-queues"
	case delErr != nil:
		detail = "land: delete slot: " + delErr.Error()
	}
	obs.EndSpan(landSpan, nil) // the land itself (target push) succeeded regardless of slot-delete outcome
	d.finishRun(ctx, t, r, core.OutcomeLanded, detail, false)
}

// finishRun finalizes r's RunRecord, optionally parks (ref, SHA), ends the
// root span, removes the exported trial tree, emits the terminal event, and
// drops r from the in-flight table.
func (d *Daemon) finishRun(ctx context.Context, t config.Target, r *run, outcome core.Outcome, detail string, park bool) {
	r.rec.Outcome = outcome
	r.rec.Detail = detail
	r.rec.EndedAt = d.now()

	if park {
		d.park(t.Name, r.cand)
	}

	obs.EndRun(r.rootSpan, r.rec)

	if r.dir != "" {
		_ = os.RemoveAll(r.dir)
	}

	d.emit(ctx, core.Event{
		Kind:      eventKindForOutcome(outcome),
		At:        d.now(),
		Target:    t.Name,
		Candidate: r.cand,
		RunID:     r.rec.RunID,
		Record:    r.rec,
		Detail:    detail,
	})

	d.runs[t.Name] = nil
}

// rejectPreMerge parks cand and emits its terminal event for an outcome
// decided before any merge commit exists (a trial-merge conflict, or an
// infra error before CommitTree succeeds): no check ever ran and no run
// object was ever created, so there's nothing to cancel.
func (d *Daemon) rejectPreMerge(ctx context.Context, t config.Target, cand core.Candidate, outcome core.Outcome, detail string) {
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
	d.park(t.Name, cand)
	d.emit(ctx, core.Event{
		Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
		Candidate: cand, RunID: runID, Record: rec, Detail: detail,
	})
}

// rejectRun parks cand and emits its terminal event for an outcome decided
// after the merge commit exists but before any check ran (a missing/invalid
// check spec, or an export failure).
func (d *Daemon) rejectRun(ctx context.Context, t config.Target, cand core.Candidate, runID, baseOID, mergeOID string, trial core.TrialMerge, outcome core.Outcome, detail string) {
	now := d.now()
	rec := &core.RunRecord{
		RunID: runID, Target: t.Name, Candidate: cand,
		BaseOID: baseOID, MergeSHA: mergeOID, Trial: trial,
		Outcome: outcome, Detail: detail,
		StartedAt: now, EndedAt: now,
	}
	d.park(t.Name, cand)
	d.emit(ctx, core.Event{
		Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
		Candidate: cand, RunID: runID, Record: rec, Detail: detail,
	})
}

// park marks cand's (ref, SHA) as parked for target: it will not be
// re-tested until the ref's SHA changes or the ref vanishes (§9.1).
func (d *Daemon) park(target string, cand core.Candidate) {
	m := d.done[target]
	if m == nil {
		m = make(map[string]string)
		d.done[target] = m
	}
	m[cand.Ref] = cand.SHA
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

// runIDTimeFormat is the UTC timestamp portion of a run ID (§9.4):
// yyyymmddThhmmssZ.
const runIDTimeFormat = "20060102T150405Z"

// newRunID builds a run ID (§9.4): a UTC timestamp plus the first 12
// characters of oid, unique across restarts (no persistence means the same
// merge re-tested after a restart gets a new timestamp) and
// human-correlatable to oid.
func newRunID(t time.Time, oid string) string {
	if len(oid) > 12 {
		oid = oid[:12]
	}
	return t.UTC().Format(runIDTimeFormat) + "-" + oid
}
