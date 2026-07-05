package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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

// checkIgnoredRefs scans refs for well-formed candidate refs (the for/...
// grammar, §9.3) whose target segment names no configured target — a common
// misconfiguration (a typo'd target name, or a target retired from config
// while stale for/ refs linger) that phase 1 silently dropped
// (docs/plans/phase23.md §10, O4). Emits core.EventIgnoredRef once per
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

// reconcileTarget runs one tick's worth of the per-target state machine
// (docs/plans/phase1.md §3, generalized to a pipeline by
// docs/plans/phase5.md §2): snapshot bookkeeping, then either advance the
// target's lane or (if it's idle) try to refill it.
//
// A lane holding any run at the start of the tick claims the whole tick —
// even if every run in it concludes (lands, parks, or skips) during this
// same call: a concluding land mutates both the target and slot refs out
// from under targetTip/cands, which were snapshotted once at the top of
// this function, so immediately reusing them to start a new trial would
// trial-merge against stale ground truth (observed as re-testing the very
// candidate that had just landed). Deferring the next pick to the
// following tick, which re-Fetches/re-ListRefs, avoids that staleness
// entirely; the cost is at most one idle tick of latency per conclusion,
// negligible next to the poll interval already inherent to the loop.
// P5-F (docs/plans/phase5.md §2): dispatch matches the plan's pseudocode
// exactly now that a lane can hold more than one run. advanceLane's return
// means "something structural concluded this tick" (a land, a park, a
// suffix invalidation) — reconcileTarget defers refill to the next tick's
// fresh Fetch/ListRefs in that case, exactly as before. Otherwise — lane
// empty, OR lane non-empty but nothing concluded (a "quiet" tick: every
// surviving run is still mid-check) — refillLane runs too. For serial and
// batch this is a no-op whenever the lane already holds a run (refillLane's
// own per-mode "busy" guard, §2.5): those modes hold at most one run, so a
// quiet tick with a non-empty lane never has room to refill anyway. Only
// speculate's window actually tops up on a quiet tick with runs still
// in flight — the one behavioral change this chunk makes to the dispatch
// itself.
func (d *Daemon) reconcileTarget(ctx context.Context, t config.Target, refs map[string]string) {
	targetTip := refs[targetRefName(t)]
	cands := discoverCandidates(t.Name, refs)
	d.syncBookkeeping(ctx, t, cands)

	if l := d.lanes[t.Name]; l != nil && len(l.runs) > 0 {
		if d.advanceLane(ctx, t, targetTip, cands, l) {
			return
		}
	}
	d.refillLane(ctx, t, targetTip, cands)
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
		done = make(map[string]parkEntry)
		d.done[t.Name] = done
	}

	for ref := range order {
		if _, ok := cands[ref]; !ok {
			delete(order, ref)
		}
	}
	for ref, entry := range done {
		if c, ok := cands[ref]; !ok || c.SHA != entry.SHA {
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
		if parked, ok := done[ref]; ok && parked.SHA == c.SHA {
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

// pickUpTo returns up to n candidates in the same FIFO order as pickHead
// (smallest order, lexical ref tie-break), excluding parked (ref, SHA)
// entries and any ref in inFlight (docs/plans/phase5.md §2.5's
// pickHead-generalized "pickNext (one, excluding in-flight) + pickUpTo (N)").
// inFlight may be nil (batch's own refill: the lane is always empty when
// refillLane runs, so nothing is ever already in flight). refillSpeculate
// (via pickNext, this function's n==1 specialization) is the caller that
// actually needs a non-nil inFlight: its window can hold several runs at
// once, so each pick must exclude every ref already chained in, not just
// parked ones. The result may be shorter than n (fewer than n candidates
// queued) or empty (nothing to pick).
func (d *Daemon) pickUpTo(target string, cands map[string]core.Candidate, n int, inFlight map[string]bool) []core.Candidate {
	order := d.order[target]
	done := d.done[target]

	var refs []string
	for ref, c := range cands {
		if parked, ok := done[ref]; ok && parked.SHA == c.SHA {
			continue
		}
		if inFlight[ref] {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if order[refs[i]] != order[refs[j]] {
			return order[refs[i]] < order[refs[j]]
		}
		return refs[i] < refs[j]
	})
	if n >= 0 && len(refs) > n {
		refs = refs[:n]
	}
	out := make([]core.Candidate, len(refs))
	for i, ref := range refs {
		out[i] = cands[ref]
	}
	return out
}

// pickNext returns the single next queued candidate in FIFO order
// (pickUpTo's one-result specialization, docs/plans/phase5.md §2.5):
// excludes parked (ref, SHA) entries and any ref already in inFlight. ok is
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

// runInvalidated is the generalized Invariant-5 test (docs/plans/phase5.md
// §2.2): true (with a human-readable reason) iff any member's candidate
// ref moved or vanished, or — for the lane's head run (laneIndex==0) only —
// the real target tip moved out from under baseOID. laneIndex is always 0
// for serial/batch (lane.runs has at most one element, matching phase-1's
// unconditional baseOID==targetTip check). A speculation window's non-head
// runs (laneIndex > 0) have a *predicted* baseOID — a predecessor's
// chainTip, never a real ref — so their validity is transitive through the
// predecessor instead: if index p-1 invalidates, invalidateSuffix already
// truncates the lane at p, so index p's own baseOID is never independently
// tested against targetTip here.
func runInvalidated(r *run, laneIndex int, targetTip string, cands map[string]core.Candidate) (bool, string) {
	for _, m := range r.members {
		if cur, ok := cands[m.cand.Ref]; !ok || cur.SHA != m.cand.SHA {
			return true, fmt.Sprintf("candidate ref %s moved or vanished mid-run (Invariant 5)", m.cand.Ref)
		}
	}
	if laneIndex == 0 && targetTip != r.baseOID {
		return true, fmt.Sprintf("target %s moved mid-run (Invariant 5)", r.target)
	}
	return false, ""
}

// runRejectOutcome derives the terminal Outcome and Detail for a run whose
// verdict just turned red (verdictRejected/verdictErrored) from the last
// check result recorded against its head member — exactly the run whose
// result set the verdict, since both terminal verdicts short-circuit
// (docs/plans/phase5.md §2.3): no further check ever starts once one
// fails or errors, so rec.Checks' last entry is always the culprit.
func runRejectOutcome(r *run) (core.Outcome, string) {
	checks := r.members[0].rec.Checks
	last := checks[len(checks)-1]
	if last.Err != nil {
		return core.OutcomeError, fmt.Sprintf("check %q: %v", last.Name, last.Err)
	}
	return core.OutcomeRejected, fmt.Sprintf("check %q failed", last.Name)
}

// advanceLane walks lane's pipeline front to back for one tick
// (docs/plans/phase5.md §2.1): a validity sweep first — a move must be
// caught before a stale verdict is consumed, exactly reconcileInFlight's
// move-then-result order — then each surviving run's checks advance once,
// then a bubble check (a run that just went red parks; anything behind it
// in the lane is invalidated unparked), then the contiguous green prefix
// lands FIFO. Returns true iff this tick concluded something structural (a
// suffix invalidation, a bubble, or at least one landing) —
// reconcileTarget's signal to defer refill to the next tick's fresh Fetch.
//
// Degenerate for serial/batch: lane.runs has at most one element there, so
// the bubble step's "suffix behind the culprit" is always empty and the
// prefix-land loop runs at most once per tick. Speculate is where this
// generalizes for real (P5-F): lane.runs can be up to Target.Window deep, so
// a validity-sweep or bubble truncation can strand a genuine suffix (Skipped
// unparked, re-queuing next tick), and the prefix-land loop can drain
// several already-green runs in one tick, each land's CAS base equal to the
// prior run's own chainTip (constraint 5's structural FIFO) — this function
// itself needed no change to support either; P5-C already built it
// lane-general, proven only at depth 1 until now.
func (d *Daemon) advanceLane(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate, lane *lane) bool {
	// (a) Validity sweep — before consuming any check result, exactly
	// reconcileInFlight's move-then-result order.
	for i, r := range lane.runs {
		if bad, reason := runInvalidated(r, i, targetTip, cands); bad {
			d.invalidateSuffix(ctx, t, lane, i, reason)
			return true
		}
	}

	// (b) Advance each surviving run's current check (non-blocking; each
	// run steps its own checks sequentially, unchanged from phase 1).
	for _, r := range lane.runs {
		d.advanceChecks(ctx, t, r)
	}

	// (c) Bubble: a run that just went red. Only the culprit parks; a
	// future speculation window's suffix Skips unparked and re-queues. A
	// genuine multi-member batch (len(members) > 1) instead takes §10
	// amendment 2's serial-fallback path (finishBatchRed): we don't know
	// which member is guilty, so nothing parks there either — a batch
	// formed with exactly one member (max-batch 1, or a queue that only
	// offered one candidate) is NOT a "batch" for this purpose and takes the
	// normal single-culprit park, since §4.1 promises max-batch 1 "degrades
	// to serial behavior" byte for byte.
	for i, r := range lane.runs {
		if r.verdict == verdictRejected || r.verdict == verdictErrored {
			if len(r.members) > 1 {
				d.finishBatchRed(ctx, t, r)
			} else {
				outcome, detail := runRejectOutcome(r)
				d.finishRun(ctx, t, r, outcome, detail, true)
			}
			d.invalidateSuffix(ctx, t, lane, i+1, "pipeline bubble")
			lane.runs = lane.runs[:i]
			return true
		}
	}

	// (d) Land the contiguous green prefix, FIFO.
	concluded := false
	for len(lane.runs) > 0 && lane.runs[0].verdict == verdictGreen {
		d.landRun(ctx, t, lane.runs[0])
		lane.runs = lane.runs[1:]
		concluded = true
	}
	return concluded
}

// invalidateSuffix cancels and Skips (unparked) every run in
// lane.runs[i:] — the suffix invalidated by a validity-sweep failure
// (§2.1a) or a pipeline bubble (§2.1c) — then truncates lane.runs to
// lane.runs[:i]. Degenerate on today's ≤1-run lane: i is 0 from the
// validity sweep (the lane's one run invalidates) or len(lane.runs) from
// the bubble step (nothing behind index 0 to invalidate — a no-op slice).
func (d *Daemon) invalidateSuffix(ctx context.Context, t config.Target, lane *lane, i int, reason string) {
	for _, r := range lane.runs[i:] {
		d.cancelRun(r)
		d.finishRun(ctx, t, r, core.OutcomeSkipped, reason, false)
	}
	lane.runs = lane.runs[:i]
}

// advanceChecks is one run's check-advance step for one tick
// (docs/plans/phase5.md §2.3): structurally identical to phase-1
// reconcileInFlight's check-advance tail, minus the move/target checks
// (hoisted into advanceLane's validity sweep) and minus the inline land
// (now advanceLane's prefix-drain step). A non-blocking read of
// r.cur.result that finds nothing yet leaves r.verdict at verdictNone (r
// stays in flight). A result ends the check span, appends it to the head
// member's run record, emits EventCheckFinished, then sets r.verdict:
// Err -> errored, Failed -> rejected (both short-circuit — no further
// check starts), Passed/Skipped either starts the next check (verdict
// stays none) or, if it was the last check, sets verdict green. It never
// itself lands, parks, or finishes the run — those stay centralized in
// advanceLane's bubble/land steps.
//
// P5-F multi-run generality fix: r.cur is nil once r's verdict is fully
// determined (green/rejected/errored) and no further check was started —
// exactly the steady state of a run waiting its turn to land behind a
// still-in-flight predecessor. At lane-depth 1 this state is never
// observed by a SECOND call to advanceChecks: a run can only go green while
// sitting at lane.runs[0] (the only position that exists), and
// advanceLane's prefix-drain step lands it that very same tick, before the
// lane is ever revisited. At depth > 1, a non-head run can resolve (either
// verdict) before its predecessor does, and then sit one or more further
// ticks with cur==nil while advanceChecks keeps being called on it every
// tick regardless (advanceLane's loop iterates every surviving run
// unconditionally) — a bare `<-r.cur.result` there dereferences a nil
// *checkInFlight and panics. Discovered via TestSpeculateDepth3RaceSoak
// (concurrent releases can resolve a non-head run first); guarding here is
// a no-op for every already-passing serial/batch/speculate case, since none
// of them previously reached this function with r.cur == nil.
func (d *Daemon) advanceChecks(ctx context.Context, t config.Target, r *run) {
	if r.cur == nil {
		return // verdict already determined; waiting its turn behind a predecessor (see doc comment)
	}
	select {
	case res := <-r.cur.result:
		obs.EndCheck(r.cur.span, res)
		// §3.3: a batch's checks run once against the chain tip's tree, but
		// the result is duplicated onto every member's own RunRecord — each
		// landed/skipped row stays self-contained ("did this land green?"
		// needs no join), and BatchID/Position/BatchSize carry the "tested
		// together" truth for anyone who needs it. Serial/speculate have
		// exactly one member, so this is a one-element loop, unchanged in
		// every observable respect.
		for i := range r.members {
			r.members[i].rec.Checks = append(r.members[i].rec.Checks, res)
		}
		// Check is the just-finished result itself (docs/plans/phase23.md
		// F-a: "Event additionally carries the finished *CheckResult on
		// check-finished events"), so channels can render a per-check
		// verdict mid-run instead of waiting for the run's terminal event.
		// Candidate attribution is the run's head member (documented
		// decision, docs/plans/phase5.md P5-E): one event per check per run,
		// not one per member per check, matching startCheck's own choice
		// below and keeping channel noise independent of batch size — the
		// per-member terminal event (EventLanded/EventSkipped/EventRejected)
		// carries each member's own duplicated Checks slice regardless.
		d.emit(ctx, core.Event{Kind: core.EventCheckFinished, At: d.now(), Target: t.Name, Candidate: r.members[0].cand, RunID: r.runID, CheckName: res.Name, Check: &res})
		r.cur = nil

		switch {
		case res.Err != nil:
			r.verdict = verdictErrored
		case res.Status == core.CheckFailed:
			r.verdict = verdictRejected
		default: // CheckPassed or CheckSkipped: both count as green (§5A)
			if r.idx+1 < len(r.checks) {
				r.idx++
				d.startCheck(ctx, r)
			} else {
				r.verdict = verdictGreen
			}
		}
	default:
		// current check still running; r.verdict stays verdictNone
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
		MergeSHA:  r.chainTip,
		Candidate: r.members[0].cand,
		Clean:     false, // reserved for the phase-4 clean-build escape hatch
	}
	// F-a (DESIGN.md "Full per-check log files"): LogDir == "" preserves
	// the exact pre-F-a behavior (job.LogPath stays ""). The check name is
	// free-form config, so it's sanitized the same way container names are
	// (core.SanitizeName) before becoming a path component — the trailing
	// ".log.zst" suffix additionally guarantees the sanitized name can
	// never resolve to "." or "..". The filename is prefixed with the
	// check's 1-based position in the spec (r.idx+1), stable and matching
	// history's per-check seq column: two check names that sanitize to the
	// same string (e.g. "lint go" and "lint/go", both -> "lint-go") would
	// otherwise alias onto the same O_TRUNC'd file, with both checks'
	// history rows pointing at whichever happened to write last
	// (closing-review FIX 3).
	//
	// ".log.zst": the executor writes this file as a single zstd stream
	// (internal/executor/logfile.go's openCheckLog) — the suffix is what
	// the dashboard's handleRunLog keys on to decide whether to decompress
	// on serve (legacy plain ".log" rows from before this change keep
	// working unchanged).
	if d.cfg.LogDir != "" {
		job.LogPath = filepath.Join(d.cfg.LogDir, r.runID, fmt.Sprintf("%d-%s.log.zst", r.idx+1, core.SanitizeName(check.Name)))
	}

	result := make(chan core.CheckResult, 1)
	start := d.now()
	go func() {
		result <- d.exec.RunCheck(spanCtx, job)
	}()
	r.cur = &checkInFlight{name: check.Name, cancel: cancel, result: result, span: span, start: start}

	d.emit(ctx, core.Event{Kind: core.EventCheckStarted, At: d.now(), Target: r.target, Candidate: r.members[0].cand, RunID: r.runID, CheckName: check.Name})
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

// chainLink is one candidate's link in the merge-commit chain
// (docs/plans/phase5.md §1.2): the --no-ff merge commit itself, the tree it
// was tested against, and the candidate it links in. len(lane.runs[*].members)
// is 1 for serial/speculate; batch (P5-E, landed) chains up to Target.MaxBatch
// links via repeated buildChainLink calls (startBatchRun), each one's base
// the previous call's mergeOID — chain_test.go proves the underlying
// mechanics against real git independent of any caller.
type chainLink struct {
	mergeOID string
	treeOID  string
	cand     core.Candidate
}

// buildChainLink trial-merges cand onto base and, if the trial is clean,
// builds cand's --no-ff merge commit (docs/plans/phase5.md §1.2): this is
// tryStartTrial's merge+message+commit logic, factored out of the pick/
// recovery logic that now lives in refillLane/startRun so batch and
// speculate can each call it once per chain link.
//
// onClean, if trial.Clean, is invoked exactly once, immediately after the
// clean trial is confirmed and before any message/commit work begins — the
// same point tryStartTrial minted the run ID (from trial.TreeOID) and
// emitted EventTrialClean, before MergeBody/CommitTree ran. Its return is
// the run ID embedded in the merge message's Gauntlet-Run trailer.
// MergeBody is invoked here, per candidate, exactly as tryStartTrial did
// (constraint 9). err is any daemon-side infra failure (MergeTree,
// merge-message template, CommitTree), pre-formatted with the same stage
// prefix tryStartTrial used as its Detail string. A conflict is signalled
// by trial.Clean == false with a zero link and nil err — the caller
// distinguishes that case itself (a conflict is data, not an error).
//
// base need not be a real ref: it may be a prior chain link's mergeOID — an
// unpushed commit that exists only as a loose object in the local repo
// (docs/plans/phase5.md §1.1's spike finding, and P5-D's chain_test.go: both
// MergeTree and CommitTree resolve any commit-ish from the object store
// regardless of refs, and MergeTree detects a conflict against a chained
// base identically to one against a real ref). No change was needed in this
// function itself to support that — startBatchRun builds one multi-link
// chain per batch run; refillSpeculate (via startRun, one member at a time)
// builds one chain per window, each call's base the previous call's
// mergeOID exactly the same way.
func (d *Daemon) buildChainLink(ctx, rootCtx context.Context, targetName, base string, cand core.Candidate, onClean func(trial core.TrialMerge) (runID string)) (chainLink, core.TrialMerge, error) {
	_, trialSpan := obs.StartTrialMerge(rootCtx, d.tr)
	trial, err := d.git.MergeTree(ctx, base, cand.SHA)
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
	// all.
	var body string
	if d.cfg.MergeBody != nil {
		body = d.cfg.MergeBody(ctx, cand, base)
	}

	msg, err := buildMergeMessage(d.cfg.MergeMessage, messageFields{Topic: cand.Topic, User: cand.User, Ref: cand.Ref, Target: targetName, RunID: runID}, body)
	if err != nil {
		return chainLink{}, trial, fmt.Errorf("merge-message template: %w", err)
	}
	mergeOID, err := d.git.CommitTree(ctx, trial.TreeOID, []string{base, cand.SHA}, msg, d.cfg.Committer)
	if err != nil {
		return chainLink{}, trial, fmt.Errorf("commit-tree: %w", err)
	}
	return chainLink{mergeOID: mergeOID, treeOID: trial.TreeOID, cand: cand}, trial, nil
}

// specChanged reports whether cfg.CheckSpec's content differs between
// prevTree and newTree — the §10 amendment-3 batch-boundary test
// (docs/plans/phase5.md): while chaining, a member whose merge changes the
// check-spec content relative to the chain's tree *before* that member's
// link terminates the batch there (the member is included, tested under its
// own change; later picks start the next batch, P5-E). prevTree/newTree may
// be any tree-ish (a commit or a tree) — exactly ReadFileFromTree's own
// contract; the intended callers pass a link's base and its resulting
// trial.TreeOID.
//
// This compares file *content*, not a blob OID: ReadFileFromTree returns
// bytes, not an object ID, and content comparison is what the plan calls
// for (a byte-identical spec re-added at a different path, or vice versa,
// isn't a "change" this check cares about — only the text the parser reads
// from cfg.CheckSpec matters).
//
// ReadFileFromTree errors identically whether cfg.CheckSpec is genuinely
// absent from a tree or the read failed for some other reason (gitx wraps
// "cat-file -p <tree>:<path>" as one opaque error, git_test.go's
// TestReadFileFromTree confirms no distinct "not found" signal exists at
// this layer). Either kind of failure is treated here as "the spec is
// absent in that tree", which is the conservative direction for this use:
// a spec appearing or disappearing between two chain trees is itself
// substantive (the batch boundary should fire), and a merely transient real
// git failure would surface again, identically, the next time that tree's
// spec is read for real (startRun's own ReadFileFromTree on the eventual
// chain tip) — no signal is silently dropped by folding it into "changed"
// here.
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

// refillLane tries to fill an idle lane for one tick (docs/plans/phase5.md
// §2.5): reconcileTarget only calls this when the lane started the tick
// empty, so there's no separate "lane busy" check here — that precondition
// is enforced by the caller, exactly as tryStartTrial's implicit
// precondition was pre-refactor.
//
// Dispatches on t.Mode: "speculate" tops up the window (refillSpeculate,
// P5-F) — the one mode whose refill runs even when the lane already holds
// runs (reconcileTarget calls refillLane on every quiet tick now, not just
// an empty-lane one; see its own doc comment). Every other mode holds at
// most one run, so its branch below re-asserts that "lane busy" precondition
// explicitly rather than relying on the caller to have never called it with
// runs in flight (true pre-P5-F, no longer true once speculate exists).
// "batch" then forms a chained multi-candidate run (refillBatch) UNLESS this
// target is in batch-red serial fallback (§2.6, overridden by §10 amendment
// 2 for the event vocabulary; d.batchFallback), in which case — and for
// "serial"/"" the default — refillSerialOne runs instead, exactly
// tryStartTrial's pick-head step.
func (d *Daemon) refillLane(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
	l := d.lanes[t.Name]

	if t.Mode == "speculate" {
		d.refillSpeculate(ctx, t, targetTip, cands, l)
		return
	}

	if l != nil && len(l.runs) > 0 {
		return // serial/batch: at most one run in flight; lane busy
	}
	if t.Mode == "batch" && !d.batchFallback[t.Name] {
		d.refillBatch(ctx, t, targetTip, cands)
		return
	}
	d.refillSerialOne(ctx, t, targetTip, cands)
}

// refillSerialOne is the one-candidate-at-a-time refill: serial mode's own
// refill, AND — while d.batchFallback[t.Name] is set (§2.6, §10 amendment
// 2) — batch mode's red-recovery fallback. It deliberately is NOT "a
// size-1 batch": running through this exact path (not startBatchRun with a
// single picked candidate) means a red verdict here takes the normal
// single-culprit park + EventRejected treatment (finishRun's plain branch
// in advanceLane's bubble step, since len(members)==1), not batch-red's
// no-park EventSkipped treatment — the culprit's true rejection must come
// from a genuine serial round, per §10 amendment 2's own reasoning.
func (d *Daemon) refillSerialOne(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
	cand, ok := d.pickHead(t.Name, cands)
	if !ok {
		return
	}

	landed, err := d.git.IsAncestor(ctx, cand.SHA, targetTip)
	if err != nil {
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "is-ancestor: "+err.Error(), nil)
		return
	}
	if landed {
		d.recoverLanded(ctx, t, cand)
		return
	}

	d.startRun(ctx, t, targetTip, cand, false, "")
}

// refillBatch fills an idle lane in batch mode (docs/plans/phase5.md §2.5):
// picks up to t.MaxBatch queued candidates FIFO (pickUpTo; nothing is ever
// "in flight" to exclude here, since batch holds at most one run and
// refillLane only runs when the lane is idle), then chains them via
// startBatchRun. IsAncestor recovery (Invariant 4) is checked on the head
// pick only, exactly as serial's own refill: a mid-chain member that's
// somehow already landed is caught the same way once it becomes a future
// refill's head (§8's per-member recovery walkthrough).
func (d *Daemon) refillBatch(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
	maxBatch := t.MaxBatch
	if maxBatch < 1 {
		// Defensive only: production config (config.LoadDaemon) always
		// defaults/validates MaxBatch >= 1 for Mode=="batch". A hand-built
		// queue.Config (as tests may construct) that leaves it zero still
		// gets correct, if degenerate, one-at-a-time batch behavior rather
		// than an empty pick every tick.
		maxBatch = 1
	}

	picked := d.pickUpTo(t.Name, cands, maxBatch, nil)
	if len(picked) == 0 {
		return
	}

	head := picked[0]
	landed, err := d.git.IsAncestor(ctx, head.SHA, targetTip)
	if err != nil {
		d.rejectPreMerge(ctx, t, head, core.OutcomeError, "is-ancestor: "+err.Error(), nil)
		return
	}
	if landed {
		d.recoverLanded(ctx, t, head)
		return
	}

	d.startBatchRun(ctx, t, targetTip, picked)
}

// startBatchRun chains picked's candidates into one --no-ff merge chain
// (§1.2's buildChainLink, advancing the base to each new link) and, if at
// least the head candidate chains cleanly, starts one run testing the chain
// tip's tree against every chained member (§2.4's one-check-suite-per-batch
// shape).
//
// Chaining stops — without failing the whole batch — at the first member
// that either conflicts against the chain built so far, or hits a
// daemon-side infra failure building its link: that member parks via the
// normal per-candidate machinery (rejectPreMerge, a conflict or infra error
// before any run exists) and the batch forms from whatever chained cleanly
// before it (decide-and-document: park-and-stop, not
// park-and-skip-and-continue — simplest, and preserves FIFO, since the
// members after the parked one are never touched and simply wait for the
// next refill). If the very first candidate fails this way, no batch forms
// at all — byte-for-byte serial's own rejectPreMerge path.
//
// A member whose link changes the check spec's content relative to the
// chain built before it (specChanged) terminates the batch AFTER that
// member (§10 amendment 3, overriding §9's "future refinement" framing):
// the member is included, tested under its own change; later picks start
// the next batch.
func (d *Daemon) startBatchRun(ctx context.Context, t config.Target, targetTip string, picked []core.Candidate) {
	// F4 (docs/plans/phase23.md §10): the root span starts here, before any
	// trial-merge, exactly as serial's startRun — one shared span for the
	// batch's single run.
	rootCtx, rootSpan := obs.StartRun(ctx, d.tr, "", t.Name, picked[0], "")

	var (
		runID  string
		links  []chainLink
		trials []core.TrialMerge
	)
	base := targetTip
	specTree := targetTip // ReadFileFromTree accepts any commit-ish; the target tip itself is valid as member 0's "before" side

chain:
	for _, cand := range picked {
		link, trial, err := d.buildChainLink(ctx, rootCtx, t.Name, base, cand, func(trial core.TrialMerge) string {
			if runID == "" {
				// Minted from the FIRST member's trial tree, exactly as
				// serial's startRun mints its single run ID — reused
				// verbatim as the batch's BatchID (§3.2: "<runID> reuse is
				// fine").
				runID = newRunID(d.now(), trial.TreeOID)
				rootSpan.SetAttributes(attribute.String(obs.AttrRunID, runID))
			}
			d.emit(ctx, core.Event{Kind: core.EventTrialClean, At: d.now(), Target: t.Name, Candidate: cand, RunID: runID})
			return runID
		})

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
			break chain // §10 amendment 3: member included, batch ends here
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

// finishBatchStart is startBatchRun's back half (§2.5, §3.3): once the
// chain has at least one link, read and parse the check spec from the
// chain TIP's tree (the batch's one-check-suite-over-the-combined-tree
// shape — §9's documented "tested by the tip's own definition" caveat,
// narrowed by §10 amendment 3's spec-change boundary above), export the
// tip's tree, and produce one run whose members carry per-member
// RunRecords sharing runID as BatchID (Position/BatchSize per §3.3).
//
// Every daemon-side infra failure here (missing/invalid check spec, export
// failure) parks every already-chained member (rejectBatch) — there's no
// single "guilty" member to blame when the combined tree they were all
// chained onto can't even be read, mirroring rejectRun's per-candidate
// treatment of the identical failures in the single-candidate case.
func (d *Daemon) finishBatchStart(ctx context.Context, t config.Target, base, runID string, links []chainLink, trials []core.TrialMerge, rootCtx context.Context, rootSpan trace.Span) {
	chainTip := links[len(links)-1].mergeOID
	tipTree := trials[len(trials)-1].TreeOID
	rootSpan.SetAttributes(attribute.String(obs.AttrMergeSHA, chainTip))

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

	dir, err := os.MkdirTemp(d.cfg.WorkDir, "gauntlet-trial-")
	if err != nil {
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "export tree: mkdir temp: "+err.Error(), rootSpan)
		return
	}
	if err := d.git.ExportTree(ctx, tipTree, dir); err != nil {
		_ = os.RemoveAll(dir)
		d.rejectBatch(ctx, t, base, runID, links, trials, core.OutcomeError, "export tree: "+err.Error(), rootSpan)
		return
	}

	now := d.now()
	members := make([]runMember, len(links))
	prevBase := base
	for i, link := range links {
		members[i] = runMember{
			cand:     link.cand,
			mergeOID: link.mergeOID,
			rec: &core.RunRecord{
				RunID:     runID,
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

	r := &run{
		target:   t.Name,
		members:  members,
		baseOID:  base,
		chainTip: chainTip,
		batchID:  runID,
		runID:    runID,
		dir:      dir,
		checks:   spec.Checks,
		idx:      0,
		verdict:  verdictNone,
		rootCtx:  rootCtx,
		rootSpan: rootSpan,
	}
	l := d.lanes[t.Name]
	if l == nil {
		l = &lane{}
		d.lanes[t.Name] = l
	}
	l.runs = append(l.runs, r)
	d.startCheck(ctx, r)
}

// rejectBatch parks every member in links and emits its own terminal event
// for a batch-wide pre-check failure discovered after the chain was fully
// built (§3.3's per-member-record shape, applied to a failure path rather
// than a verdict): unlike batch-red (§10 amendment 2, finishBatchRed), this
// isn't "a check failed and we don't know who's guilty" — it's "the
// combined tree these members were chained onto can't even be
// read/parsed/exported", which is nobody's individual fault but blocks
// every member equally, so — like rejectRun's single-candidate case — every
// member parks, avoiding an unbounded retry-every-tick loop. rootSpan is
// always non-nil here: finishBatchStart's own callers always have a real
// run-in-progress span by this point.
func (d *Daemon) rejectBatch(ctx context.Context, t config.Target, base, runID string, links []chainLink, trials []core.TrialMerge, outcome core.Outcome, detail string, rootSpan trace.Span) {
	now := d.now()
	prevBase := base
	var lastRec *core.RunRecord
	for i, link := range links {
		rec := &core.RunRecord{
			RunID:     runID,
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
		d.park(t.Name, link.cand, outcome, detail)
		d.emit(ctx, core.Event{
			Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
			Candidate: link.cand, RunID: runID, Record: rec, Detail: detail,
		})
	}
	obs.EndRun(rootSpan, lastRec)
}

// finishBatchRed handles a genuine multi-member batch run (len(r.members) >
// 1) whose combined check suite went red (§2.6, overridden by §10 amendment
// 2 for the event vocabulary): we don't know which member is guilty, so
// nothing parks. Every member gets its own terminal record — Outcome
// Skipped, the shared failing checks already duplicated onto it by
// advanceChecks — and its own EventSkipped, in member order (constraint
// 10's per-candidate event contract, generalized), with a Detail naming the
// batch and the failing check. batchFallback[target] is then set so the
// next refillLane for this target walks candidates one at a time
// (refillSerialOne) until a landing clears it (landRun): the culprit's
// genuine EventRejected + park comes from ITS serial round, keeping park
// semantics honest (only a proven-red SHA ever parks) and channel rendering
// (ghstatus/Slack) truthful — every status returns to pending on the serial
// re-trial, exactly as a re-push would.
//
// A batch formed with BatchSize==1 is NOT routed here — advanceLane's
// bubble step dispatches those through the plain finishRun/park path
// instead (see its own doc comment).
func (d *Daemon) finishBatchRed(ctx context.Context, t config.Target, r *run) {
	checkName := "?"
	if checks := r.members[0].rec.Checks; len(checks) > 0 {
		checkName = checks[len(checks)-1].Name
	}
	detail := fmt.Sprintf("batch %s red on check %q; serializing", r.batchID, checkName)
	now := d.now()

	for i := range r.members {
		m := &r.members[i]
		m.rec.Outcome = core.OutcomeSkipped
		m.rec.Detail = detail
		m.rec.EndedAt = now
	}

	d.finalizeRun(r)

	for i := range r.members {
		m := &r.members[i]
		d.emit(ctx, core.Event{
			Kind:      core.EventSkipped,
			At:        now,
			Target:    t.Name,
			Candidate: m.cand,
			RunID:     r.runID,
			Record:    m.rec,
			Detail:    detail,
		})
	}

	d.batchFallback[t.Name] = true
}

// refillSpeculate tops up target's speculation window for one tick
// (docs/plans/phase5.md §2.5): unlike serial/batch, this runs whenever the
// window has room, whether the lane started the tick empty or already held
// some runs (reconcileTarget calls refillLane on every quiet tick, not just
// an empty-lane one — this is the mode that actually needs that). Each new
// run's base is the previous run's chainTip — an unpushed, PREDICTED
// predecessor — once the lane is non-empty; the very first run of an empty
// lane bases on the live target tip instead (the head run, predicted=false).
// pickNext excludes every ref already chained into the lane so one window
// never contains the same candidate twice.
//
// The first candidate whose trial conflicts against the chain built so far
// stops extending the window for this tick: that candidate parks via
// startRun's normal rejectPreMerge path, with a Detail that says so
// explicitly when the conflict is against a PREDICTION (a non-head base) —
// "conflicts with in-flight <topic>@<sha> (predicted base)" — rather than
// the generic "trial merge conflict" wording serial/batch use, since a
// predicted-base conflict is conflicting with in-flight, not-yet-landed
// work, not with anything actually on the target. Later candidates simply
// wait for a future tick, once the window has room again (a landing, a
// bubble, or the culprit's own park freeing a slot).
func (d *Daemon) refillSpeculate(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate, l *lane) {
	window := t.Window
	if window < 1 {
		// Defensive only: production config (config.LoadDaemon) always
		// defaults/validates Window >= 1 for Mode=="speculate". Mirrors
		// refillBatch's maxBatch guard for a hand-built queue.Config.
		window = 1
	}

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
			// rule (§2.5): a mid-window member that's somehow already landed
			// is caught the same way once it becomes the head of a future
			// empty-lane refill.
			landed, err := d.git.IsAncestor(ctx, cand.SHA, targetTip)
			if err != nil {
				d.rejectPreMerge(ctx, t, cand, core.OutcomeError, "is-ancestor: "+err.Error(), nil)
				return
			}
			if landed {
				d.recoverLanded(ctx, t, cand)
				return
			}
		}

		var conflictDetail string
		if predicted {
			conflictDetail = fmt.Sprintf("conflicts with in-flight %s@%s (predicted base)", predTopic, predSHA)
		}

		r, ok := d.startRun(ctx, t, base, cand, predicted, conflictDetail)
		if !ok {
			return // conflict or infra error: this candidate parked, stop extending the window this tick
		}

		l = d.lanes[t.Name] // startRun created it on the first successful run
		runs = l.runs
		inFlight[cand.Ref] = true
		base = r.chainTip
		predTopic = cand.Topic
		predSHA = cand.SHA
	}
}

// startRun builds cand's chain link (via buildChainLink) and, on a clean
// merge, reads and parses its check spec and exports its tree, producing a
// new one-run, one-member lane entry (docs/plans/phase5.md §3.2) — the
// rest of tryStartTrial's §3 step 3. Every daemon-side infra failure in
// this path (MergeTree, message template, CommitTree, ReadFileFromTree,
// ParseChecks, MkdirTemp, ExportTree) is handled uniformly: OutcomeError +
// park + EventError (OutcomeRejected for a missing/invalid check spec).
// Parking prevents an unbounded retry-every-tick loop (§9.2's explicit
// phase-1 ruling: backoff/auto-retry is phase 2), and the distinct
// EventError lets operators tell infra from red; a restart, a re-push, or
// a CommandRetry clears the park.
//
// predicted (P5-F) marks whether base is a predicted, unpushed predecessor
// chainTip rather than the live target tip — a speculation window's non-head
// member. It threads onto both run.predicted (RunSnapshot.Predicted, §3.4)
// and RunRecord.Speculated (§3.3), purely informational for the dashboard;
// the landed commit is the tested commit either way (Invariant 1). Every
// caller but refillSpeculate passes false (base is always the real target
// tip for serial's one-run lane).
//
// conflictDetail, if non-empty, replaces the default "trial merge conflict:
// ..." message on a trial-merge conflict. refillSpeculate uses it to
// document a conflict against a PREDICTION: a non-head candidate that
// conflicts with the chain built so far is conflicting with in-flight,
// not-yet-landed work, which is a materially different situation from a
// real conflict against the pushed target tip, and its park Detail must say
// so (docs/plans/phase5.md §2.5).
//
// ok reports whether a run was started; false covers every
// terminal-without-a-run outcome (conflict or any infra error) — the
// caller's signal to stop extending a window/re-pick, since the candidate
// that failed has already been parked by this call.
func (d *Daemon) startRun(ctx context.Context, t config.Target, base string, cand core.Candidate, predicted bool, conflictDetail string) (*run, bool) {
	// F4 (docs/plans/phase23.md §10): the run's root span starts here,
	// before MergeTree, so trial-merge is correctly parented as its child
	// instead of being orphaned under ctx (phase 1's bug: the root span
	// used to start only once a merge commit existed). run.id and
	// merge.sha aren't known yet — StartRun gets empty placeholders — and
	// are backfilled onto the very same span via SetAttributes once each is
	// minted below; span.SetAttributes updating an already-set key is
	// standard OTel behavior, so no obs API change is needed for this.
	rootCtx, rootSpan := obs.StartRun(ctx, d.tr, "", t.Name, cand, "")

	var runID string
	link, trial, err := d.buildChainLink(ctx, rootCtx, t.Name, base, cand, func(trial core.TrialMerge) string {
		// Run ID from the trial *tree* OID, not the merge commit OID — a
		// deliberate deviation from §9.4's letter. The merge commit's
		// message must carry a Gauntlet-Run trailer containing the run ID
		// (§3), and a commit's OID is a hash over its own message, so a
		// run ID containing mergeOID[:12] is a genuine circular dependency
		// — no commit can embed (a prefix of) its own hash. The trial
		// tree's OID is known before any commit exists, is
		// content-addressed to exactly what the checks test, and stays
		// human-correlatable (`git log --format='%H %T'` on the target
		// ties each merge commit to its tree). §9.4's other property —
		// uniqueness across restarts with no persistence — comes from the
		// timestamp, sharpened in phase 2/3 by a monotonic per-process
		// counter (§2.4) since same-second identical-tree trials would
		// otherwise mint identical IDs. Commit-to-run correlation is the
		// trailer's job; run-to-commit is RunRecord.MergeSHA's.
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
		d.rejectPreMerge(ctx, t, cand, core.OutcomeError, err.Error(), rootSpan)
		return nil, false
	}
	if !trial.Clean {
		detail := "trial merge conflict: " + strings.Join(trial.Conflicts, ", ")
		if conflictDetail != "" {
			detail = conflictDetail + ": " + strings.Join(trial.Conflicts, ", ")
		}
		d.rejectPreMerge(ctx, t, cand, core.OutcomeConflict, detail, rootSpan)
		return nil, false
	}
	rootSpan.SetAttributes(attribute.String(obs.AttrMergeSHA, link.mergeOID))

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

	// F2 (docs/plans/phase23.md §10): trial-tree export dirs are created
	// under cfg.WorkDir when it's set. os.MkdirTemp treats an empty dir
	// argument as "use the OS default temp dir" already, so this is a strict
	// superset of the phase-1 behavior; sweeping WorkDir at startup (the
	// other half of F2) is cmd's job, not the queue's (D7).
	dir, err := os.MkdirTemp(d.cfg.WorkDir, "gauntlet-trial-")
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "export tree: mkdir temp: "+err.Error(), rootSpan)
		return nil, false
	}
	if err := d.git.ExportTree(ctx, trial.TreeOID, dir); err != nil {
		_ = os.RemoveAll(dir)
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "export tree: "+err.Error(), rootSpan)
		return nil, false
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
	r := &run{
		target:    t.Name,
		members:   []runMember{{cand: cand, mergeOID: link.mergeOID, rec: rec}},
		baseOID:   base,
		chainTip:  link.mergeOID,
		predicted: predicted,
		batchID:   "",
		runID:     runID,
		dir:       dir,
		checks:    spec.Checks,
		idx:       0,
		verdict:   verdictNone,
		rootCtx:   rootCtx,
		rootSpan:  rootSpan,
	}
	l := d.lanes[t.Name]
	if l == nil {
		l = &lane{}
		d.lanes[t.Name] = l
	}
	l.runs = append(l.runs, r)
	d.startCheck(ctx, r)
	return r, true
}

// landRun is the generalized land (docs/plans/phase5.md §2.4): one CAS push
// lands the whole chain, then a per-member slot delete + terminal event,
// FIFO. len(r.members)==1 for serial/speculate: exactly phase-1's land, one
// push one delete one event. A batch run (P5-E, landed) has N members: one
// push, N deletes, N EventLanded — one per candidate, in FIFO order.
//
// A stale target CAS means the target moved between trial and land — Skip,
// keep every member's slot, retry next tick (Invariant 2). A stale
// slot-delete CAS means the author re-pushed between land and delete — the
// landed commit still holds exactly the tested SHA (Invariant 1), that
// member's slot simply survives at its new SHA and re-queues naturally
// (Invariant 3); the run is still a Landed outcome.
func (d *Daemon) landRun(ctx context.Context, t config.Target, r *run) {
	_, landSpan := obs.StartLand(r.rootCtx, d.tr)

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
		}
		d.finishRun(ctx, t, r, core.OutcomeSkipped, detail, false)
		return
	}

	// §2.6/§10 amendment 2: any successful landing for this target clears
	// batch-red serial fallback, resuming batching on the next refill —
	// whether this landing is an ordinary batch, or one round of the
	// fallback's own one-at-a-time walk. delete is a no-op when the flag
	// was never set (every non-batch mode, or a batch target that never
	// went red).
	delete(d.batchFallback, t.Name)

	for i := range r.members {
		m := &r.members[i]
		delErr := d.git.CASUpdate(ctx, m.cand.Ref, m.cand.SHA, "")
		detail := ""
		switch {
		case errors.Is(delErr, core.ErrCASStale):
			detail = "candidate re-pushed before slot delete; slot survives at new SHA and re-queues"
		case delErr != nil:
			detail = "land: delete slot: " + delErr.Error()
		}
		m.rec.Outcome = core.OutcomeLanded
		m.rec.Detail = detail
		m.rec.EndedAt = d.now()
		d.emit(ctx, core.Event{
			Kind:      core.EventLanded,
			At:        d.now(),
			Target:    t.Name,
			Candidate: m.cand,
			RunID:     r.runID,
			Record:    m.rec,
			Detail:    detail,
		})
	}
	// Deliberate ordering exception vs. the other terminal paths: finishRun
	// finalizes (root-span end, export-dir removal) *before* emitting its
	// terminal event, but here each member's EventLanded is emitted above,
	// before finalizeRun runs — §2.4's delete-then-emit-per-member FIFO
	// shape. Unobservable today (no event or record carries the dir path,
	// and spans are no-op), but don't write a consumer — or a batch-chunk
	// extension — that assumes the dir is gone by EventLanded time.
	obs.EndSpan(landSpan, nil) // the land itself (target push) succeeded regardless of any slot-delete outcome
	d.finalizeRun(r)
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
// re-queue, none of them singled out (§2.2).
func (d *Daemon) finishRun(ctx context.Context, t config.Target, r *run, outcome core.Outcome, detail string, park bool) {
	for i := range r.members {
		m := &r.members[i]
		m.rec.Outcome = outcome
		m.rec.Detail = detail
		m.rec.EndedAt = d.now()

		if park {
			d.park(t.Name, m.cand, outcome, detail)
		}
	}

	d.finalizeRun(r)

	for i := range r.members {
		m := &r.members[i]
		d.emit(ctx, core.Event{
			Kind:      eventKindForOutcome(outcome),
			At:        d.now(),
			Target:    t.Name,
			Candidate: m.cand,
			RunID:     r.runID,
			Record:    m.rec,
			Detail:    detail,
		})
	}
}

// finalizeRun performs the once-per-run cleanup that must happen exactly
// once regardless of member count (docs/plans/phase5.md §3.2): ends the
// root span (its summary attributes/status come from the head member's
// RunRecord — the representative summary for a batch's shared span; each
// member's own full record is still what's emitted per terminal event) and
// removes the exported trial dir. Per-check log files
// (LogDir, if configured) are deliberately never touched here: they
// outlive the run by design (DESIGN.md "Full per-check log files") —
// retention is a separate, later prune mechanism, not this state machine's
// job. It does not touch the lane: lane.runs is truncated by
// advanceLane/invalidateSuffix, the sole mutators of that slice, at the
// same call sites that already know the removal boundary.
func (d *Daemon) finalizeRun(r *run) {
	obs.EndRun(r.rootSpan, r.members[0].rec)

	if r.dir != "" {
		_ = os.RemoveAll(r.dir)
	}
}

// recoverLanded implements Invariant 4's crash-recovery branch: cand.SHA is
// already an ancestor of the target tip, meaning some earlier run landed it
// before a crash (or this daemon's own previous pass) interrupted slot
// cleanup. No trial ran and no check ran, but F1 (docs/plans/phase23.md
// §10) requires every terminal event to still carry a complete, non-nil
// RunRecord, so one is synthesized here: a run-ID stand-in derived from the
// candidate SHA (phase1 §9.4's stand-in rule, minted through the same
// counter as a real run ID so it can never collide with one), zero checks,
// OutcomeLanded, and a Detail explaining that checks were not re-run. As
// before, this is a pure recovery action, not a run: no merge ever happens,
// so BaseOID/MergeSHA/Trial stay zero-valued, matching the other
// pre-merge synthesized records (rejectPreMerge).
func (d *Daemon) recoverLanded(ctx context.Context, t config.Target, cand core.Candidate) {
	if delErr := d.git.CASUpdate(ctx, cand.Ref, cand.SHA, ""); delErr != nil && !errors.Is(delErr, core.ErrCASStale) {
		return // transient; retry next tick
	}
	now := d.now()
	runID := newRunID(now, cand.SHA)
	const detail = "candidate already ancestor of target; checks not re-run"
	rec := &core.RunRecord{
		RunID:     runID,
		Target:    t.Name,
		Candidate: cand,
		Outcome:   core.OutcomeLanded,
		Detail:    detail,
		StartedAt: now,
		EndedAt:   now,
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
// run's root span if one was already started (F4 starts it before
// MergeTree) — nil for the one outcome that precedes even that (an
// IsAncestor infra error, before tryStartTrial knows a trial will even be
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
	d.park(t.Name, cand, outcome, detail)
	if rootSpan != nil {
		obs.EndRun(rootSpan, rec)
	}
	d.emit(ctx, core.Event{
		Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
		Candidate: cand, RunID: runID, Record: rec, Detail: detail,
	})
}

// rejectRun parks cand and emits its terminal event for an outcome decided
// after the merge commit exists but before any check ran (a missing/invalid
// check spec, or an export failure). rootSpan is always non-nil here: every
// call site follows a successful StartRun.
func (d *Daemon) rejectRun(ctx context.Context, t config.Target, cand core.Candidate, runID, baseOID, mergeOID string, trial core.TrialMerge, outcome core.Outcome, detail string, rootSpan trace.Span) {
	now := d.now()
	rec := &core.RunRecord{
		RunID: runID, Target: t.Name, Candidate: cand,
		BaseOID: baseOID, MergeSHA: mergeOID, Trial: trial,
		Outcome: outcome, Detail: detail,
		StartedAt: now, EndedAt: now,
	}
	d.park(t.Name, cand, outcome, detail)
	if rootSpan != nil {
		obs.EndRun(rootSpan, rec)
	}
	d.emit(ctx, core.Event{
		Kind: eventKindForOutcome(outcome), At: now, Target: t.Name,
		Candidate: cand, RunID: runID, Record: rec, Detail: detail,
	})
}

// parkEntry records why a (ref, SHA) is parked — its terminal outcome, a
// human-readable reason, and when — feeding the dashboard snapshot's
// ParkedEntry (docs/plans/phase23.md §2.1, §2.3). Semantics are unchanged
// from phase1 §9.1: sticky per (ref, SHA), cleared only when the ref's SHA
// changes, the ref vanishes, or a CommandRetry clears it explicitly
// (command.go) — never when some other candidate lands.
type parkEntry struct {
	SHA     string
	Outcome core.Outcome
	Reason  string
	At      time.Time
}

// park marks cand's (ref, SHA) as parked for target, recording outcome and
// detail as the park's reason: it will not be re-tested until the ref's SHA
// changes, the ref vanishes, or a CommandRetry clears it (§9.1).
func (d *Daemon) park(target string, cand core.Candidate, outcome core.Outcome, detail string) {
	m := d.done[target]
	if m == nil {
		m = make(map[string]parkEntry)
		d.done[target] = m
	}
	m[cand.Ref] = parkEntry{SHA: cand.SHA, Outcome: outcome, Reason: detail, At: d.now()}
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

// runIDCounter is a monotonic per-process counter folded into every run ID
// (docs/plans/phase23.md §2.4). The phase-1 review (C7) found that two
// trials sharing an identical trial tree and started within the same UTC
// second — a re-push that restores previously-tested content, or two
// daemon instances racing the same candidate — mint identical run IDs
// under the timestamp+OID-prefix scheme alone. The container executor
// (phase 2/3) derives container names from run IDs, so such a collision
// would also break `--name`; the counter closes the gap regardless of
// clock resolution or tree content. Package-level (not per-Daemon) because
// the uniqueness this protects is process-wide: two Daemon instances in one
// process (as the duplicate-daemon tests construct) must not mint
// colliding IDs either.
var runIDCounter atomic.Int64

// newRunID builds a run ID: a UTC timestamp, a monotonic per-process
// sequence number, and the first 12 characters of oid — unique across
// restarts (no persistence means the same merge re-tested after a restart
// gets a new timestamp), unique within one process even for same-second
// identical-tree trials (the counter), and human-correlatable to oid.
func newRunID(t time.Time, oid string) string {
	if len(oid) > 12 {
		oid = oid[:12]
	}
	seq := runIDCounter.Add(1)
	return fmt.Sprintf("%s-%d-%s", t.UTC().Format(runIDTimeFormat), seq, oid)
}
