package queue

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
)

// candidatePrefix is the fixed portion of the candidate ref grammar:
// "refs/heads/for/<target>/<rest>".
const candidatePrefix = "refs/heads/for/"

// parseCandidateRef parses a candidate ref's grammar. If <rest> (the
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
	if before, after, ok := strings.Cut(remainder, "/"); ok {
		user = before
		topic = after
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

// checkIgnoredRefs scans refs for well-formed candidate refs (the for/...
// grammar) whose target segment names no configured target — a common
// misconfiguration (a typo'd target name, or a target retired from config
// while stale for/ refs linger). Emits core.EventIgnoredRef once per
// (ref, SHA), not every tick, via d.ignoredRefs — pruned here of any ref no
// longer present, so it can't grow unboundedly over a long-running
// daemon's lifetime.
func (d *Daemon) checkIgnoredRefs(ctx context.Context, refs map[string]string) {
	configured := make(map[string]bool, len(d.cfg.Targets))
	for _, t := range d.cfg.Targets {
		configured[t.Name] = true
	}

	seen := make(map[string]bool)
	for ref, sha := range refs {
		target, _, _, ok := parseCandidateRef(ref)
		if !ok || configured[target] {
			continue
		}
		seen[ref] = true
		if d.ignoredRefs[ref] == sha {
			continue // already reported for this SHA
		}
		d.ignoredRefs[ref] = sha
		d.emit(ctx, core.Event{
			Kind:      core.EventIgnoredRef,
			At:        d.now(),
			Target:    target,
			Candidate: core.Candidate{Ref: ref, Target: target, SHA: sha},
			Detail:    fmt.Sprintf("target %q is not configured", target),
		})
	}
	for ref := range d.ignoredRefs {
		if !seen[ref] {
			delete(d.ignoredRefs, ref)
		}
	}
}

// reconcileTarget syncs candidate state, consumes results, and refills the
// target lane. All queue state changes occur on this goroutine.
func (d *Daemon) reconcileTarget(ctx context.Context, t config.Target, refs map[string]string) {
	// seedParksOnce runs in ReconcileOnce before drainCommands: a first-tick
	// operator cancel must not have its "cancelled by operator" provenance
	// silently overwritten by a same-tick seed for the same ref.
	targetTip := refs[targetRefName(t)]
	cands := discoverCandidates(t.Name, refs)
	for ref, c := range d.external {
		if c.Target == t.Name {
			cands[ref] = c
		}
	}
	d.reviewEmergency(ctx, t, refs, cands)
	d.policyAdmission(ctx, t, cands, targetTip)
	d.syncBookkeeping(ctx, t, cands)
	if d.reconcileEmergency(ctx, t, targetTip, cands) {
		return
	}
	if _, paused := d.controls.Pauses[t.Name]; paused {
		permitted := false
		for _, c := range cands {
			if c.OverridePause {
				permitted = true
			}
		}
		if !permitted {
			return
		}
	}

	d.prepareCircuitProbe(ctx, t.Name, cands)
	if l := d.lanes[t.Name]; l != nil && len(l.runs) > 0 {
		if d.advanceLane(ctx, t, targetTip, cands, l) {
			return
		}
	}
	d.refillLane(ctx, t, targetTip, cands)
}

// syncBookkeeping assigns FIFO positions and drops vanished or changed
// parks and retry budgets.
func (d *Daemon) syncBookkeeping(ctx context.Context, t config.Target, cands map[string]core.Candidate) {
	order := d.order[t.Name]
	if order == nil {
		order = make(map[string]int64)
		d.order[t.Name] = order
	}
	done := d.done[t.Name]
	if done == nil {
		done = make(map[string]parkEntry)
		d.done[t.Name] = done
	}

	for ref := range order {
		if _, ok := cands[ref]; !ok {
			delete(order, ref)
		}
	}
	for ref, entry := range done {
		c, present := cands[ref]
		// A missing review may simply have lost readiness. Keep its failure
		// verdict until a different revision or request returns; ordinary
		// deleted refs retain their existing withdraw-and-requeue semantics.
		if !present && entry.Version != "" {
			continue
		}
		if !present || c.SHA != entry.SHA || c.Version != entry.Version {
			delete(done, ref)
		}
	}
	// A missing ref or changed SHA renews the infrastructure retry budget.
	if autoRetried := d.autoRetried[t.Name]; autoRetried != nil {
		for ref, sha := range autoRetried {
			if c, ok := cands[ref]; !ok || c.SHA != sha {
				delete(autoRetried, ref)
			}
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
		if parked, ok := done[ref]; ok && parked.SHA == cands[ref].SHA {
			continue // parked at this SHA already (e.g. seeded at boot): not a real "queued" transition
		}
		d.emit(ctx, core.Event{Kind: core.EventQueued, At: d.now(), Target: t.Name, Candidate: cands[ref]})
	}
}

// seedParksOnce restores only red terminal outcomes from history. Current
// candidate state is checked by syncBookkeeping.
func (d *Daemon) seedParksOnce(target string) {
	if d.seeded[target] {
		return
	}
	d.seeded[target] = true

	if d.cfg.SeedParks == nil {
		return
	}
	for _, seed := range d.cfg.SeedParks(target) {
		if !isRedOutcome(seed.Outcome) {
			continue // a landed or skipped ref was never sticky; don't seed one
		}
		m := d.done[target]
		if m == nil {
			m = make(map[string]parkEntry)
			d.done[target] = m
		}
		m[seed.Ref] = parkEntry{SHA: seed.SHA, Version: seed.Version, Outcome: seed.Outcome, Reason: seed.Reason, At: seed.At, RunID: seed.RunID}
	}
}

// isRedOutcome reports whether o is one of the park-worthy "red family"
// outcomes (matching the script DSL's cmdAssertSlotParked and park's own
// semantics, below): a ref parks on Rejected, Conflict, or Error, never on
// Landed or Skipped.
func isRedOutcome(o core.Outcome) bool {
	switch o {
	case core.OutcomeRejected, core.OutcomeConflict, core.OutcomeError:
		return true
	default:
		return false
	}
}

// pickHead returns the queue head: the candidate with the smallest order
// (tie-broken lexically by ref) whose current SHA is not parked in done.
// ok is false if every candidate is parked or none exist.
func (d *Daemon) pickHead(target string, cands map[string]core.Candidate) (core.Candidate, bool) {
	picked := d.pickUpTo(target, cands, 1, nil)
	if len(picked) == 0 {
		return core.Candidate{}, false
	}
	return picked[0], true
}

// pickUpTo selects at most n runnable candidates in FIFO order, placing
// prerequisites before children. Parked or missing prerequisites block
// their dependents. Negative n is unlimited; inFlight prerequisites may
// supply predicted bases.
func (d *Daemon) pickUpTo(target string, cands map[string]core.Candidate, n int, inFlight map[string]bool) []core.Candidate {
	order := d.order[target]
	done := d.done[target]

	var refs []string
	for ref, c := range cands {
		if c.AdmissionBlocked != "" || c.SkipChecks {
			continue
		}
		if _, paused := d.controls.Pauses[target]; paused && !c.OverridePause {
			continue
		}

		if parked, ok := done[ref]; ok && parked.SHA == c.SHA {
			continue
		}
		if inFlight[ref] {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		ui, uj := d.urgent(target, refs[i], cands), d.urgent(target, refs[j], cands)
		if ui != uj {
			if d.urgentBurst[target] >= 3 {
				return !ui
			}
			return ui
		}
		if order[refs[i]] != order[refs[j]] {
			return order[refs[i]] < order[refs[j]]
		}
		return refs[i] < refs[j]
	})
	selected := make(map[string]bool)
	var out []core.Candidate
	// Stable topological selection: a dependency may be older or newer in
	// FIFO order, but its child can never leap over a parked prerequisite.
	for len(out) < len(refs) && (n < 0 || len(out) < n) {
		progress := false
		for _, ref := range refs {
			c := cands[ref]
			if selected[ref] || (c.DependsOn != "" && !selected[c.DependsOn] && !inFlight[c.DependsOn]) {
				continue
			}
			out = append(out, c)
			selected[ref] = true
			progress = true
			if n >= 0 && len(out) == n {
				break
			}
		}
		if !progress {
			break
		}
	}
	return out
}

// pickNext returns the single next queued candidate in FIFO order
// (pickUpTo's one-result specialization): excludes parked (ref, SHA)
// entries and any ref already in inFlight. ok is
// false if nothing is left to pick (queue drained or every remaining
// candidate is parked/in-flight). Speculate's refill (refillSpeculate)
// calls this once per window slot, growing inFlight as each pick chains in.
func (d *Daemon) pickNext(target string, cands map[string]core.Candidate, inFlight map[string]bool) (core.Candidate, bool) {
	picked := d.pickUpTo(target, cands, 1, inFlight)
	if len(picked) == 0 {
		return core.Candidate{}, false
	}
	return picked[0], true
}

// runInvalidated checks every member's revision and metadata. Only the lane
// head checks the actual target tip; later runs depend on their predicted
// predecessors and are invalidated as a suffix.
func runInvalidated(r *run, laneIndex int, targetTip string, cands map[string]core.Candidate) (bool, string) {
	for _, m := range r.members {
		if cur, ok := cands[m.cand.Ref]; !ok || cur.SHA != m.cand.SHA || cur.Version != m.cand.Version {
			return true, fmt.Sprintf("candidate ref %s moved or vanished mid-run (Invariant 5)", m.cand.Ref)
		}
	}
	if laneIndex == 0 && targetTip != r.baseOID {
		return true, fmt.Sprintf("target %s moved mid-run (Invariant 5)", r.target)
	}
	return false, ""
}

// runRejectOutcome derives the terminal Outcome and Detail for a run whose
// verdict just turned red (verdictRejected/verdictErrored) from the run's
// explicitly recorded root failure (run.culprit) — never from whichever
// result happened to be recorded last, an ordering parallel completion
// makes meaningless.
func runRejectOutcome(r *run) (core.Outcome, string) {
	res := r.results[r.culprit]
	if res.Err != nil {
		return core.OutcomeError, fmt.Sprintf("check %q: %v", res.Name, res.Err)
	}
	return core.OutcomeRejected, fmt.Sprintf("check %q failed", res.Name)
}

// advanceLane consumes checks front to back, discards invalid suffixes, and
// lands the green prefix. A failure on the real base parks the culprit;
// predicted-base failures requeue for verification on the actual target.
func (d *Daemon) advanceLane(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate, lane *lane) bool {
	// (a) Validity sweep — before consuming any check result, a move must
	// be caught before a stale verdict is consumed.
	for i, r := range lane.runs {
		if bad, reason := runInvalidated(r, i, targetTip, cands); bad {
			d.invalidateSuffix(ctx, t, lane, i, reason)
			return true
		}
	}

	// (b) Advance each surviving run's current check (non-blocking; each
	// run steps its own checks sequentially).
	for _, r := range lane.runs {
		for _, m := range r.members {
			if m.cand.OverridePause {
				r.overridePause = true
				if !m.cand.SkipChecks {
					r.pauseOverrideReason = m.cand.Requester + ": " + m.cand.RequestReason
				}
			}
		}
		d.prepareEmergency(r)
		d.advanceChecks(ctx, t, r)
	}

	// (c) Bubble: a run that just went red. Only lane index 0 parks on a red
	// verdict — its base is the REAL target tip (runInvalidated's own
	// rule), so a red there is proven against reality. A red at index i>0
	// has a PREDICTED base (a predecessor's own chainTip, never a real ref,
	// per refillSpeculate) — that predecessor might itself be the one at
	// fault, so parking index i here would risk sticking a possibly-innocent
	// candidate with a false rejection. Instead it Skips unparked with a
	// Detail saying so explicitly, and re-queues; it re-forms at the front
	// of a future window and, if it's STILL red once it's actually testing
	// against the real target tip (index 0), parks for real at that point
	// via this same branch. See docs/architecture/queue-modes.md ("Red bubble:
	// only index 0 parks") for the full rationale.
	//
	// A genuine multi-member batch (len(members) > 1) instead takes the
	// serial-fallback path (finishBatchRed): we don't know which member is
	// guilty, so nothing parks there either — a batch formed with exactly
	// one member (max-batch 1, or a queue that only offered one candidate)
	// is NOT a "batch" for this purpose and takes the normal single-culprit
	// park, since max-batch 1 degrades to serial behavior byte for byte.
	//
	// Invariant, load-bearing for the index-0-only-parks rule below: a
	// genuine multi-member batch run is always at lane index 0, because
	// batch mode holds at most one run in the lane (refillLane's own "lane
	// busy" guard for every mode but speculate) and speculate only ever
	// builds one-member runs (startRun). The switch is shaped to stay safe
	// even if that invariant ever broke — the multi-member case routes to
	// finishBatchRed (no park) regardless of i — but a future mode that
	// chains multi-member runs at depth > 1 must revisit this step's
	// predicted-base reasoning for batches too, not just rely on that
	// fallback.
	for i, r := range lane.runs {
		if r.verdict == verdictRejected || r.verdict == verdictErrored {
			switch {
			case len(r.members) > 1:
				d.finishBatchRed(ctx, t, r)
			case i == 0:
				outcome, detail := runRejectOutcome(r)
				d.finishRun(ctx, t, r, outcome, detail, true)
			default:
				head := lane.runs[0].members[0].cand
				detail := fmt.Sprintf("red on predicted base (behind %s@%s); re-queuing for retest", head.Topic, head.SHA)
				d.finishRun(ctx, t, r, core.OutcomeSkipped, detail, false)
			}
			d.invalidateSuffix(ctx, t, lane, i+1, "pipeline bubble")
			lane.runs = lane.runs[:i]
			return true
		}
	}

	// (d) Land the contiguous green prefix, FIFO.
	if d.controls.Uncertain || d.circuitBlocked(t.Name) {
		return false
	}
	concluded := false
	for len(lane.runs) > 0 && lane.runs[0].verdict == verdictGreen {
		d.landRun(ctx, t, lane.runs[0])
		lane.runs = lane.runs[1:]
		concluded = true
	}
	return concluded
}

// invalidateSuffix cancels and Skips (unparked) every run in
// lane.runs[i:] — the suffix invalidated by a validity-sweep failure or a
// pipeline bubble — then truncates lane.runs to lane.runs[:i]. Degenerate
// for serial/batch (lane.runs has at most one run): i is 0 from the
// validity sweep (the lane's one run invalidates) or len(lane.runs) from
// the bubble step (nothing behind index 0 to invalidate — a no-op slice).
// Speculate is where a real suffix behind index i gets truncated.
func (d *Daemon) invalidateSuffix(ctx context.Context, t config.Target, lane *lane, i int, reason string) {
	for _, r := range lane.runs[i:] {
		d.cancelRun(r)
		d.finishRun(ctx, t, r, core.OutcomeSkipped, reason, false)
	}
	lane.runs = lane.runs[:i]
}

// chainLink is one candidate's link in the merge-commit chain (see
// docs/architecture/queue-modes.md, "The merge-commit chain"): the --no-ff merge
// commit itself, the tree it was tested against, and the candidate it links
// in. len(lane.runs[*].members) is 1 for serial/speculate; batch chains up
// to Target.MaxBatch links via repeated buildChainLink calls (startBatchRun),
// each one's base the previous call's mergeOID — chain_test.go proves the
// underlying mechanics against real git independent of any caller.
type chainLink struct {
	mergeOID string
	treeOID  string
	cand     core.Candidate
}

// buildChainLink replays a candidate and creates its landing commit.
// onClean supplies the run ID before the message is built. Dirty trials
// have no commit.
func (d *Daemon) buildChainLink(ctx, rootCtx context.Context, targetName, base string, cand core.Candidate, onClean func(trial core.TrialMerge) (runID string)) (chainLink, core.TrialMerge, error) {
	return d.buildChainLinkPrecomputed(ctx, rootCtx, targetName, base, cand, onClean, nil)
}

// buildChainLinkPrecomputed uses precomputed legacy message bodies when
// supplied; nil computes them on demand.
func (d *Daemon) buildChainLinkPrecomputed(ctx, rootCtx context.Context, targetName, base string, cand core.Candidate, onClean func(trial core.TrialMerge) (runID string), precomputed map[string]string) (chainLink, core.TrialMerge, error) {
	_, trialSpan := obs.StartTrialMerge(rootCtx, d.tr)
	linear := d.linearTarget(targetName)
	var trial core.TrialMerge
	var err error
	if linear {
		trial, err = d.git.(core.LinearGitRepo).ReplayTree(ctx, base, cand.SHA, cand.SourceBase)
	} else {
		trial, err = d.git.MergeTree(ctx, base, cand.SHA)
	}
	if err != nil {
		obs.EndSpan(trialSpan, err)
		return chainLink{}, trial, fmt.Errorf("merge-tree: %w", err)
	}
	if !trial.Clean {
		obs.EndSpan(trialSpan, nil)
		return chainLink{}, trial, nil
	}
	obs.EndSpan(trialSpan, nil)

	runID := onClean(trial)

	// Best-effort per Config.MergeBody's contract (daemon.go): called at
	// most once per trial, right here, before the message (and therefore
	// the merge commit) is built. No timeout is applied at this layer —
	// that's cmd's job — and no error path exists to check: a nil or
	// empty-string-returning hook behaves identically to no summarizer at
	// all. When precomputed is non-nil, the call already happened
	// (concurrently, ahead of this chain loop — see startBatchRun and
	// precomputeMergeBodies); consuming its result here keeps this
	// candidate's body identical to what a direct inline call would have
	// returned, without paying its latency serially.
	var body string
	if precomputed != nil {
		body = precomputed[cand.Ref]
	} else if !linear && d.cfg.MergeBody != nil {
		body = d.cfg.MergeBody(ctx, cand, base)
	}

	msg, err := buildMergeMessage(d.cfg.MergeMessage, messageFields{Topic: cand.Topic, User: cand.User, Ref: cand.Ref, Target: targetName, RunID: runID}, body)
	if err != nil {
		return chainLink{}, trial, fmt.Errorf("merge-message template: %w", err)
	}
	var mergeOID string
	if linear {
		if cand.Message != "" {
			msg = strings.TrimRight(cand.Message, "\n") + "\n"
			if cand.Source != "gerrit" {
				msg += "\n"
			}
			// Gerrit's Change-Id must remain in the final footer block.
			// Append provenance to that block, not a new paragraph.
			if cand.ReviewURL != "" {
				msg += "Reviewed-on: " + cand.ReviewURL + "\n"
			}
			msg += fmt.Sprintf("Gauntlet-Ref: %s\nGauntlet-Run: %s\n", cand.Ref, runID)
		} else if reader, ok := d.git.(interface {
			CommitMessage(context.Context, string) (string, error)
		}); ok {
			sourceMessage, readErr := reader.CommitMessage(ctx, cand.SHA)
			if readErr != nil {
				return chainLink{}, trial, readErr
			}
			msg = sourceMessage + "\n\n" + fmt.Sprintf("Gauntlet-Ref: %s\nGauntlet-Run: %s\n", cand.Ref, runID)
		}
		msg += fmt.Sprintf("Gauntlet-Source: %s\n", cand.SHA)
		if cand.Version != "" {
			msg += "Gauntlet-Version: " + cand.Version + "\n"
		}
		mergeOID, err = d.git.(core.LinearGitRepo).LinearCommit(ctx, trial.TreeOID, base, cand.SHA, msg, d.cfg.Committer)
	} else {
		mergeOID, err = d.git.CommitTree(ctx, trial.TreeOID, []string{base, cand.SHA}, msg, d.cfg.Committer)
	}
	if err != nil {
		return chainLink{}, trial, fmt.Errorf("commit-tree: %w", err)
	}
	return chainLink{mergeOID: mergeOID, treeOID: trial.TreeOID, cand: cand}, trial, nil
}

// specChanged reports changes in check-spec bytes or presence. Two
// unreadable specs count as unchanged; final spec loading rejects the run.
func (d *Daemon) specChanged(ctx context.Context, prevTree, newTree string) bool {
	prev, prevErr := d.git.ReadFileFromTree(ctx, prevTree, d.cfg.CheckSpec)
	next, nextErr := d.git.ReadFileFromTree(ctx, newTree, d.cfg.CheckSpec)
	switch {
	case prevErr != nil && nextErr != nil:
		return false // absent on both sides: no change
	case prevErr != nil || nextErr != nil:
		return true // the spec appeared or disappeared
	default:
		return !bytes.Equal(prev, next)
	}
}

// refillLane admits work to an empty lane according to the target mode.
// Batch-red recovery temporarily selects serial admission.
func (d *Daemon) refillLane(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
	// The admission boundary (issue #8): once draining, admit no new
	// candidate and extend no speculation window. Already-admitted runs
	// (advanceLane/advanceChecks/landing) keep running to a terminal
	// outcome — the drain set is what was in flight at drain entry, and it
	// is finite because nothing new enters here. Queue refs for
	// unadmitted candidates stay untouched and re-queue on the next start.
	if d.draining {
		return
	}
	l := d.lanes[t.Name]

	if t.Mode == "speculate" {
		d.refillSpeculate(ctx, t, targetTip, cands, l)
		return
	}

	if l != nil && len(l.runs) > 0 {
		return // serial/batch: at most one run in flight; lane busy
	}
	if d.controls.Uncertain || d.circuitBlocked(t.Name) {
		return
	}
	if t.Mode == "batch" && !d.batchFallback[t.Name] {
		d.refillBatch(ctx, t, targetTip, cands)
		return
	}
	d.refillSerialOne(ctx, t, targetTip, cands)
}

// refillSerialOne admits one candidate on the actual target, recovering
// already-landed candidates before building a trial.
func (d *Daemon) refillSerialOne(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
	cand, ok := d.pickHead(t.Name, cands)
	if !ok {
		return
	}

	landed, err := d.candidateLanded(ctx, t, cand, targetTip)
	if err != nil {
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "is-ancestor: "+err.Error(), nil)
		return
	}
	if landed {
		d.recoverLanded(ctx, t, cand, targetTip)
		return
	}

	d.startRun(ctx, t, targetTip, cand, false, "")
}

// refillBatch selects up to MaxBatch candidates and builds a trial on the
// actual target.
func (d *Daemon) refillBatch(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
	// Defensive only: production config (config.LoadDaemon) always
	// defaults/validates MaxBatch >= 1 for Mode=="batch". A hand-built
	// queue.Config (as tests may construct) that leaves it zero still
	// gets correct, if degenerate, one-at-a-time batch behavior rather
	// than an empty pick every tick.
	maxBatch := max(t.MaxBatch, 1)
	if d.controls.Circuits[t.Name].Backoff > 0 {
		maxBatch = 1
	}
	if limit := d.batchRecovery[t.Name]; limit > 0 {
		maxBatch = min(maxBatch, limit)
	}

	picked := d.pickUpTo(t.Name, cands, maxBatch, nil)
	if len(picked) == 0 {
		return
	}

	head := picked[0]
	landed, err := d.candidateLanded(ctx, t, head, targetTip)
	if err != nil {
		d.rejectPreMerge(ctx, t, head, core.OutcomeError, "is-ancestor: "+err.Error(), nil)
		return
	}
	if landed {
		d.recoverLanded(ctx, t, head, targetTip)
		return
	}

	d.startBatchRun(ctx, t, targetTip, picked)
}

// startBatchRun builds an ordered commit chain, stopping at a conflict or
// check-spec boundary. The resulting tip runs one suite; each member
// retains its own landing record.
func (d *Daemon) startBatchRun(ctx context.Context, t config.Target, targetTip string, picked []core.Candidate) {
	// The root span starts here, before any trial-merge, exactly as
	// serial's startRun — one shared span for the batch's single run.
	rootCtx, rootSpan := obs.StartRun(ctx, d.tr, "", t.Name, picked[0], "")

	sources := make([]string, len(picked))
	for i, cand := range picked {
		sources[i] = cand.SHA
	}
	release, err := core.RetainSources(ctx, d.git, sources...)
	if err != nil {
		for i, cand := range picked {
			var span trace.Span
			if i == 0 {
				span = rootSpan
			}
			d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "retain sources: "+err.Error(), span)
		}
		return
	}
	defer release()

	// Summarize against the current target before synthetic chain bases exist.
	reqs := make([]mergeBodyRequest, len(picked))
	for i, cand := range picked {
		reqs[i] = mergeBodyRequest{cand: cand, base: targetTip}
	}
	var precomputedBodies map[string]string
	if t.Landing != "squash" {
		precomputedBodies = precomputeMergeBodies(ctx, d.cfg.MergeBody, reqs)
	}

	var (
		runID  string
		links  []chainLink
		trials []core.TrialMerge
	)
	base := targetTip
	specTree := targetTip // ReadFileFromTree accepts any commit-ish; the target tip itself is valid as member 0's "before" side

chain:
	for _, cand := range picked {
		link, trial, err := d.buildChainLinkPrecomputed(ctx, rootCtx, t.Name, base, cand, func(trial core.TrialMerge) string {
			if runID == "" {
				// Minted from the FIRST member's trial tree, exactly as
				// serial's startRun mints its single run ID — reused
				// verbatim as the batch's BatchID.
				runID = newRunID(d.now(), trial.TreeOID)
				rootSpan.SetAttributes(attribute.String(obs.AttrRunID, runID))
			}
			d.emit(ctx, core.Event{Kind: core.EventTrialClean, At: d.now(), Target: t.Name, Candidate: cand, RunID: runID})
			return runID
		}, precomputedBodies)

		switch {
		case err != nil:
			if len(links) == 0 {
				d.rejectPreMerge(ctx, t, cand, core.OutcomeError, err.Error(), rootSpan)
				return
			}
			d.rejectPreMerge(ctx, t, cand, core.OutcomeError, err.Error(), nil)
			break chain
		case !trial.Clean:
			detail := "trial merge conflict: " + strings.Join(trial.Conflicts, ", ")
			if len(links) == 0 {
				d.rejectPreMerge(ctx, t, cand, core.OutcomeConflict, detail, rootSpan)
				return
			}
			d.rejectPreMerge(ctx, t, cand, core.OutcomeConflict, detail, nil)
			break chain
		}

		links = append(links, link)
		trials = append(trials, trial)
		base = link.mergeOID

		changed := d.specChanged(ctx, specTree, trial.TreeOID)
		specTree = trial.TreeOID
		if changed {
			break chain // member included, batch ends here (the spec-change boundary)
		}
	}

	if len(links) == 0 {
		// Unreachable given the loop's own return statements above (the only
		// way to fall through with no links is the head candidate failing,
		// which already returned) — kept as a defensive guard against a
		// future refactor of the loop above.
		return
	}

	d.finishBatchStart(ctx, t, targetTip, runID, links, trials, rootCtx, rootSpan)
}

// finishBatchStart parses the tip's spec, prepares its workspace, and
// starts the batch. Setup failures park members with individual records.
func (d *Daemon) finishBatchStart(ctx context.Context, t config.Target, base, runID string, links []chainLink, trials []core.TrialMerge, rootCtx context.Context, rootSpan trace.Span) {
	chainTip := links[len(links)-1].mergeOID
	tipTree := trials[len(trials)-1].TreeOID
	rootSpan.SetAttributes(attribute.String(obs.AttrMergeSHA, chainTip))

	sources := make([]string, len(links))
	for i, link := range links {
		sources[i] = link.cand.SHA
	}
	release, err := core.RetainSources(ctx, d.git, sources...)
	if err != nil {
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "retain sources: "+err.Error(), rootSpan)
		return
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()

	// Pin the normalized chain and base before reading them. Source leases
	// separately protect the original members, which squash history cannot reach.
	if err := d.git.Pin(ctx, chainTip); err != nil {
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "pin trial chain: "+err.Error(), rootSpan)
		return
	}

	specData, err := d.git.ReadFileFromTree(ctx, tipTree, d.cfg.CheckSpec)
	if err != nil {
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err), rootSpan)
		return
	}
	spec, err := config.ParseChecks(specData)
	if err != nil {
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err), rootSpan)
		return
	}
	// Spec-load gates, same as startRun's (see SpecRejectReason).
	if reason := SpecRejectReason(spec, d.cfg.Services != nil, d.cfg.KnownExecutorProfile, d.cfg.ImageCapableProfile, d.cfg.ReceiptNotes != nil); reason != "" {
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeRejected, reason, rootSpan)
		return
	}

	policyCandidates := make([]core.Candidate, len(links))
	for i, link := range links {
		policyCandidates[i] = link.cand
	}
	if err := d.executionPolicy(ctx, t, base, chainTip, policyCandidates, spec); err != nil {
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeRejected, err.Error(), rootSpan)
		return
	}

	// Isolated mode defers materialization to each node (issue #9); shared
	// mode exports the chain tip once here, as today (see startRun's
	// matching block for the full rationale).
	var dir string
	if !spec.Isolated() {
		dir, err = os.MkdirTemp(d.cfg.WorkDir, "gauntlet-trial-")
		if err != nil {
			d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "export tree: mkdir temp: "+err.Error(), rootSpan)
			return
		}
		if err := d.git.ExportTree(ctx, tipTree, dir); err != nil {
			_ = os.RemoveAll(dir)
			d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "export tree: "+err.Error(), rootSpan)
			return
		}
		if err := d.restoreMtimes(ctx, chainTip, dir, rootSpan); err != nil {
			_ = os.RemoveAll(dir)
			d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "restore mtimes: "+err.Error(), rootSpan)
			return
		}
	}

	// One trial ref + one status at the chain-tip merge for the whole
	// batch (issue #7), published as the LAST fallible pre-check step so
	// no earlier rejection leaks a ref (a batch verifies one suite over
	// the combined tree, so member candidates get no separate verification
	// statuses — attribution stays in the record/dashboard/status URL).
	var trialRef string
	if d.cfg.TrialRefs {
		ref, err := d.createTrialRef(ctx, runID, chainTip, rootSpan)
		if err != nil {
			_ = os.RemoveAll(dir)
			d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "publish trial ref: "+err.Error(), rootSpan)
			return
		}
		trialRef = ref
	}

	now := d.now()
	members := make([]runMember, len(links))
	prevBase := base
	for i, link := range links {
		members[i] = runMember{
			cand:     link.cand,
			mergeOID: link.mergeOID,
			rec: &core.RunRecord{
				RunID:     memberRunID(runID, i),
				Target:    t.Name,
				Candidate: link.cand,
				BaseOID:   prevBase,
				MergeSHA:  link.mergeOID,
				Trial:     trials[i],
				StartedAt: now,
				BatchID:   runID,
				Position:  i,
				BatchSize: len(links),
			},
		}
		prevBase = link.mergeOID
	}

	transferred = true
	r := &run{
		releaseSources: release,
		target:         t.Name,
		members:        members,
		baseOID:        base,
		chainTip:       chainTip,
		chainTree:      tipTree,
		batchID:        runID,
		runID:          runID,
		dir:            dir,
		checks:         buildRunNodes(spec),
		maxParallel:    effectiveMaxParallel(spec),
		inflight:       make(map[string]*checkInFlight),
		results:        make(map[string]core.CheckResult),
		readyAt:        make(map[string]time.Time),
		imageRefs:      make(map[string]string, len(spec.Images)),
		services:       spec.Services,
		verdict:        verdictNone,
		isolated:       spec.Isolated(),
		trialRef:       trialRef,
		rootCtx:        rootCtx,
		rootSpan:       rootSpan,
	}
	// One pending verification status at the chain tip, carrying the head
	// member's record (Position 0), before checks start.
	if d.cfg.TrialRefs {
		d.emit(ctx, core.Event{
			Kind: core.EventTrialMerged, At: d.now(), Target: t.Name,
			Candidate: members[0].cand, RunID: runID, MergeSHA: r.chainTip,
		})
	}
	l := d.lanes[t.Name]
	if l == nil {
		l = &lane{}
		d.lanes[t.Name] = l
	}
	l.runs = append(l.runs, r)
	for _, m := range r.members {
		if m.cand.OverridePause {
			r.overridePause = true
			if !m.cand.SkipChecks {
				r.pauseOverrideReason = m.cand.Requester + ": " + m.cand.RequestReason
			}
		}
	}
	d.prepareEmergency(r)
	d.advanceChecks(ctx, t, r) // starts the ready roots (just checks[0] at max-parallel 1)
}

// rejectBatch records a setup failure and parks every member without
// running checks.
func (d *Daemon) rejectBatch(ctx context.Context, t config.Target, base, runID string, links []chainLink, trials []core.TrialMerge, outcome core.Outcome, detail string, rootSpan trace.Span) {
	// Mirrors rejectRun: a chain rejected after finishBatchStart's pin site
	// never reaches finalizeRun, so its tip pin is released here. Call
	// sites before the pin (or the pin failure itself) unpin a ref that
	// never existed — a no-op by Unpin's contract.
	d.unpin(ctx, links[len(links)-1].mergeOID)
	now := d.now()
	prevBase := base
	var lastRec *core.RunRecord
	for i, link := range links {
		rec := &core.RunRecord{
			RunID:     memberRunID(runID, i),
			Target:    t.Name,
			Candidate: link.cand,
			BaseOID:   prevBase,
			MergeSHA:  link.mergeOID,
			Trial:     trials[i],
			Outcome:   outcome,
			Detail:    detail,
			StartedAt: now,
			EndedAt:   now,
			BatchID:   runID,
			Position:  i,
			BatchSize: len(links),
		}
		prevBase = link.mergeOID
		lastRec = rec
		d.park(t.Name, link.cand, outcome, detail, rec.RunID)
		d.emit(ctx, core.Event{
			Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
			Candidate: link.cand, RunID: rec.RunID, Record: rec, Detail: detail,
		})
		d.maybeAutoRetry(ctx, t.Name, link.cand, outcome) // see autoretry.go
	}
	obs.EndRun(rootSpan, lastRec)
}

// finishBatchRed skips the failed batch without parking individual members:
// the culprit is unknown. Serial fallback tests them independently.
func (d *Daemon) finishBatchRed(ctx context.Context, t config.Target, r *run) {
	checkName := "?"
	if r.culprit != "" {
		checkName = r.culprit
	}
	detail := fmt.Sprintf("batch %s red on check %q; attribution unconfirmed; checking smaller groups", r.batchID, checkName)
	if r.pauseOverrideReason != "" {
		d.clearEmergency(t.Name, "pause override stopped: "+detail)
	}
	if r.emergencyReason != "" {
		detail = "EMERGENCY: checks waived; required provenance failed; " + r.emergencyReason + "; " + detail
		d.clearEmergency(t.Name, "emergency stopped: "+detail)
	}
	now := d.now()

	for i := range r.members {
		m := &r.members[i]
		m.rec.Outcome = core.OutcomeSkipped
		m.rec.Detail = detail
		m.rec.EndedAt = now
	}

	d.finalizeRun(ctx, r)

	for i := range r.members {
		m := &r.members[i]
		d.emit(ctx, core.Event{
			Kind:      core.EventSkipped,
			At:        now,
			Target:    t.Name,
			Candidate: m.cand,
			RunID:     m.rec.RunID,
			Record:    m.rec,
			Detail:    detail,
		})
	}

	if t.OnBatchRed == "bisect" {
		limit := max(1, len(r.members)/2)
		// Hypotheses choose a smaller real prefix. They cannot park a member
		// or transfer the failed batch's verdict onto that prefix.
		for _, result := range r.results {
			for _, ref := range result.SuspectedRefs {
				for i, m := range r.members {
					if m.cand.Ref == ref {
						limit = min(limit, max(1, i))
					}
				}
			}
		}
		d.batchRecovery[t.Name] = limit
	} else {
		d.batchFallback[t.Name] = true
	}
}

// refillSpeculate fills the window on predicted predecessor tips. Missing
// or parked prerequisites block admission; failed predictions are retried
// on the actual target.
func (d *Daemon) refillSpeculate(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate, l *lane) {
	if d.controls.Uncertain || d.circuitBlocked(t.Name) {
		return
	}
	if d.controls.Circuits[t.Name].Backoff > 0 {
		if l != nil && len(l.runs) > 0 {
			return
		}
		d.refillSerialOne(ctx, t, targetTip, cands)
		return
	}
	// Defensive only: production config (config.LoadDaemon) always
	// defaults/validates Window >= 1 for Mode=="speculate". Mirrors
	// refillBatch's maxBatch guard for a hand-built queue.Config.
	window := max(t.Window, 1)

	var runs []*run
	if l != nil {
		runs = l.runs
	}
	if len(runs) >= window {
		return
	}

	inFlight := make(map[string]bool, len(runs))
	for _, r := range runs {
		inFlight[r.members[0].cand.Ref] = true
	}

	// base starts at the live target tip (the head run's own base); once the
	// lane already holds runs, it becomes the last run's chainTip — a
	// predicted predecessor, not yet pushed anywhere. predTopic/predSHA name
	// that predecessor for the conflict-detail message below.
	base := targetTip
	var predTopic, predSHA string
	if n := len(runs); n > 0 {
		base = runs[n-1].chainTip
		predTopic = runs[n-1].members[0].cand.Topic
		predSHA = runs[n-1].members[0].cand.SHA
	}

	for len(runs) < window {
		cand, ok := d.pickNext(t.Name, cands, inFlight)
		if !ok {
			return // queue drained; refill later as candidates arrive
		}

		predicted := len(runs) > 0 // base is a predecessor's chainTip, not the live target tip

		if len(runs) == 0 {
			// IsAncestor recovery (Invariant 4) only applies to a fresh,
			// wholly empty lane — exactly serial/batch's own head-pick-only
			// rule: a mid-window member that's somehow already landed is
			// caught the same way once it becomes the head of a future
			// empty-lane refill.
			landed, err := d.candidateLanded(ctx, t, cand, targetTip)
			if err != nil {
				d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "is-ancestor: "+err.Error(), nil)
				return
			}
			if landed {
				d.recoverLanded(ctx, t, cand, targetTip)
				return
			}
		}

		var conflictDetail string
		if predicted {
			conflictDetail = fmt.Sprintf("conflicts with in-flight %s@%s (predicted base)", predTopic, predSHA)
		}

		r, ok := d.startRun(ctx, t, base, cand, predicted, conflictDetail)
		if !ok {
			// conflict or infra error: at the head (predicted==false) this
			// candidate parked; at a predicted position it Skipped unparked
			// and will re-queue instead. Either way, stop extending the
			// window this tick.
			return
		}

		l = d.lanes[t.Name] // startRun created it on the first successful run
		runs = l.runs
		inFlight[cand.Ref] = true
		base = r.chainTip
		predTopic = cand.Topic
		predSHA = cand.SHA
	}
}

// startRun builds one trial and starts its check graph. Setup failures on
// the actual target park; failures on a predicted base requeue. Each run
// pins its tested tip until finalization or confirmed remote reachability.
func (d *Daemon) startRun(ctx context.Context, t config.Target, base string, cand core.Candidate, predicted bool, conflictDetail string) (*run, bool) {
	// The run's root span starts here, before MergeTree, so trial-merge is
	// correctly parented as its child rather than orphaned under ctx.
	// run.id and merge.sha aren't known yet — StartRun gets empty
	// placeholders — and
	// are backfilled onto the very same span via SetAttributes once each is
	// minted below; span.SetAttributes updating an already-set key is
	// standard OTel behavior, so no obs API change is needed for this.
	rootCtx, rootSpan := obs.StartRun(ctx, d.tr, "", t.Name, cand, "")

	release, err := core.RetainSources(ctx, d.git, cand.SHA)
	if err != nil {
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "retain source: "+err.Error(), rootSpan)
		return nil, false
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()

	var runID string
	link, trial, err := d.buildChainLink(ctx, rootCtx, t.Name, base, cand, func(trial core.TrialMerge) string {
		// Run ID from the trial *tree* OID, not the merge commit OID — a
		// commit OID hashes its own message, and the message must carry
		// this run ID in its Gauntlet-Run trailer, so an ID derived from
		// the commit OID would be circular. See docs/architecture/queue.md ("Run
		// identity") for the full three-part ID scheme.
		//
		// Minted here, before EventTrialClean, and reused verbatim for the
		// rest of the run: channels join every event for a run by RunID
		// (Slack threading, ghstatus's target_url), so an EventTrialClean
		// emitted without one breaks that join for the run's entire
		// lifetime.
		runID = newRunID(d.now(), trial.TreeOID)
		rootSpan.SetAttributes(attribute.String(obs.AttrRunID, runID))
		d.emit(ctx, core.Event{Kind: core.EventTrialClean, At: d.now(), Target: t.Name, Candidate: cand, RunID: runID})
		return runID
	})
	if err != nil {
		// buildChainLink's error here (MergeTree or CommitTree failing
		// outright, before any trial tree/merge commit exists) against a
		// PREDICTED base proves nothing about cand — same false-negative-park
		// risk a trial CONFLICT has below, and the same category
		// advanceLane's bubble step already folds into its predicted-base
		// no-park branch for a post-merge check ERROR (verdictErrored
		// treated identically to verdictRejected at lane index >0). Skip
		// unparked instead of parking on an unproven base. See
		// docs/architecture/queue-modes.md ("Conflict against a predicted base is
		// a skip, not a park").
		if predicted {
			d.skipPreMergePredicted(ctx, t, cand, "predicted-base build error (retesting once real): "+err.Error(), rootSpan)
			return nil, false
		}
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, err.Error(), rootSpan)
		return nil, false
	}
	if !trial.Clean {
		detail := "trial merge conflict: " + strings.Join(trial.Conflicts, ", ")
		if conflictDetail != "" {
			detail = conflictDetail + ": " + strings.Join(trial.Conflicts, ", ")
		}
		// A trial CONFLICT against a PREDICTED base (refillSpeculate's
		// non-head window members — base is a predecessor's own unpushed
		// chainTip, never a real ref) is a prediction-derived verdict in
		// exactly the sense already covered for a post-merge red at lane
		// index >0 (advanceLane's bubble step): the predecessor might be at
		// fault, or might never even land, so this base might never become
		// real. Parking cand here would be the same false-negative park.
		// Skip unparked instead — cand re-queues and re-forms in a later
		// window; if it's STILL conflicting once it actually reaches lane
		// index 0 against the REAL target tip, it parks there for real.
		if predicted {
			d.skipPreMergePredicted(ctx, t, cand, detail+"; re-queuing for retest against the real tip", rootSpan)
			return nil, false
		}
		d.rejectPreMerge(ctx, t, cand, core.OutcomeConflict, detail, rootSpan)
		return nil, false
	}
	rootSpan.SetAttributes(attribute.String(obs.AttrMergeSHA, link.mergeOID))

	// Pin the merge commit before anything reads through it: the spec read,
	// the export, and every check resolving GAUNTLET_*_SHA through
	// GAUNTLET_GIT_DIR all depend on this otherwise-unreferenced object
	// surviving git maintenance for the run's whole lifetime — reachability
	// is part of the check contract, not a gc.pruneExpire timing accident.
	// Every terminal path releases it: finalizeRun for a run that got this
	// far, rejectRun below for the pre-check failures, landedPins (via the
	// next Fetch) for a landing.
	if err := d.git.Pin(ctx, link.mergeOID); err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "pin trial merge: "+err.Error(), rootSpan)
		return nil, false
	}

	specData, err := d.git.ReadFileFromTree(ctx, trial.TreeOID, d.cfg.CheckSpec)
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err), rootSpan)
		return nil, false
	}
	spec, err := config.ParseChecks(specData)
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err), rootSpan)
		return nil, false
	}
	// Spec-load gates (SpecRejectReason): services declared with no
	// services block, an unknown executor profile, or a candidate-built
	// image on a non-container profile are configuration errors rejected
	// before any command starts — never a red verdict mid-run
	// (Config.KnownExecutorProfile's / ImageCapableProfile's docs;
	// docs/architecture/services.md, "The model: a cache entry, not a
	// supervised unit").
	if reason := SpecRejectReason(spec, d.cfg.Services != nil, d.cfg.KnownExecutorProfile, d.cfg.ImageCapableProfile, d.cfg.ReceiptNotes != nil); reason != "" {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeRejected, reason, rootSpan)
		return nil, false
	}

	if err := d.executionPolicy(ctx, t, base, link.mergeOID, []core.Candidate{cand}, spec); err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeRejected, err.Error(), rootSpan)
		return nil, false
	}

	// Workspace materialization (issue #9). SHARED mode (the default):
	// one writable export for the whole run, created here and handed to
	// every node — today's behavior. ISOLATED mode: no shared export;
	// each node materializes its own private copy in startCheck once it
	// holds an execution slot (r.dir stays "", so no node ever sees
	// another's mutations). Trial-tree export dirs are created under
	// cfg.WorkDir when set; os.MkdirTemp treats "" as the OS temp dir, and
	// sweeping WorkDir at startup is cmd's job.
	var dir string
	if !spec.Isolated() {
		dir, err = os.MkdirTemp(d.cfg.WorkDir, "gauntlet-trial-")
		if err != nil {
			d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "export tree: mkdir temp: "+err.Error(), rootSpan)
			return nil, false
		}
		if err := d.git.ExportTree(ctx, trial.TreeOID, dir); err != nil {
			_ = os.RemoveAll(dir)
			d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "export tree: "+err.Error(), rootSpan)
			return nil, false
		}
		if err := d.restoreMtimes(ctx, link.mergeOID, dir, rootSpan); err != nil {
			_ = os.RemoveAll(dir)
			d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "restore mtimes: "+err.Error(), rootSpan)
			return nil, false
		}
	}

	// Publish the trial ref (issue #7) as the LAST fallible pre-check
	// step, so every earlier rejection (bad spec, capability gate, export
	// failure) happens before any ref exists — a ref is published only for
	// a run that actually starts checks, and the run struct built just
	// below always owns it and disposes it on every terminal path. A
	// publish failure is infrastructure, not a bad candidate: OutcomeError.
	var trialRef string
	if d.cfg.TrialRefs {
		ref, err := d.createTrialRef(ctx, runID, link.mergeOID, rootSpan)
		if err != nil {
			_ = os.RemoveAll(dir)
			d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "publish trial ref: "+err.Error(), rootSpan)
			return nil, false
		}
		trialRef = ref
	}

	rec := &core.RunRecord{
		RunID:      runID,
		Target:     t.Name,
		Candidate:  cand,
		BaseOID:    base,
		MergeSHA:   link.mergeOID,
		Trial:      trial,
		StartedAt:  d.now(),
		Speculated: predicted,
	}
	transferred = true
	r := &run{
		releaseSources: release,
		target:         t.Name,
		members:        []runMember{{cand: cand, mergeOID: link.mergeOID, rec: rec}},
		baseOID:        base,
		chainTip:       link.mergeOID,
		chainTree:      trial.TreeOID,
		predicted:      predicted,
		batchID:        "",
		runID:          runID,
		dir:            dir,
		checks:         buildRunNodes(spec),
		maxParallel:    effectiveMaxParallel(spec),
		inflight:       make(map[string]*checkInFlight),
		results:        make(map[string]core.CheckResult),
		readyAt:        make(map[string]time.Time),
		imageRefs:      make(map[string]string, len(spec.Images)),
		services:       spec.Services,
		verdict:        verdictNone,
		isolated:       spec.Isolated(),
		trialRef:       trialRef,
		rootCtx:        rootCtx,
		rootSpan:       rootSpan,
	}
	// The merge is published (when enabled): its MergeSHA now carries the
	// pending verification status. Emitted before checks start, after
	// EventTrialClean (which fires pre-CommitTree, without a MergeSHA).
	if d.cfg.TrialRefs {
		d.emit(ctx, core.Event{
			Kind: core.EventTrialMerged, At: d.now(), Target: t.Name,
			Candidate: cand, RunID: runID, MergeSHA: r.chainTip,
		})
	}
	l := d.lanes[t.Name]
	if l == nil {
		l = &lane{}
		d.lanes[t.Name] = l
	}
	l.runs = append(l.runs, r)
	for _, m := range r.members {
		if m.cand.OverridePause {
			r.overridePause = true
			if !m.cand.SkipChecks {
				r.pauseOverrideReason = m.cand.Requester + ": " + m.cand.RequestReason
			}
		}
	}
	d.prepareEmergency(r)
	d.advanceChecks(ctx, t, r) // starts the ready roots (just checks[0] at max-parallel 1)
	return r, true
}
