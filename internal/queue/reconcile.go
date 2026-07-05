package queue

import (
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
func (d *Daemon) reconcileTarget(ctx context.Context, t config.Target, refs map[string]string) {
	targetTip := refs[targetRefName(t)]
	cands := discoverCandidates(t.Name, refs)
	d.syncBookkeeping(ctx, t, cands)

	if l := d.lanes[t.Name]; l != nil && len(l.runs) > 0 {
		d.advanceLane(ctx, t, targetTip, cands, l)
		return
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

// runInvalidated is the generalized Invariant-5 test (docs/plans/phase5.md
// §2.2): true (with a human-readable reason) iff any member's candidate
// ref moved or vanished, or — for the lane's head run (laneIndex==0) only —
// the real target tip moved out from under baseOID. laneIndex is always 0
// in this chunk (lane.runs has at most one element, matching phase-1's
// unconditional baseOID==targetTip check); non-head runs in a future
// speculation window have a *predicted* baseOID, whose validity is
// transitive through the predecessor instead.
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
// Degenerate in this chunk: lane.runs has at most one element (serial), so
// the bubble step's "suffix behind the culprit" is always empty and the
// prefix-land loop runs at most once per tick.
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
	// future speculation window's suffix Skips unparked and re-queues.
	for i, r := range lane.runs {
		if r.verdict == verdictRejected || r.verdict == verdictErrored {
			outcome, detail := runRejectOutcome(r)
			d.finishRun(ctx, t, r, outcome, detail, true)
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
func (d *Daemon) advanceChecks(ctx context.Context, t config.Target, r *run) {
	select {
	case res := <-r.cur.result:
		obs.EndCheck(r.cur.span, res)
		m := &r.members[0]
		m.rec.Checks = append(m.rec.Checks, res)
		// Check is the just-finished result itself (docs/plans/phase23.md
		// F-a: "Event additionally carries the finished *CheckResult on
		// check-finished events"), so channels can render a per-check
		// verdict mid-run instead of waiting for the run's terminal event.
		d.emit(ctx, core.Event{Kind: core.EventCheckFinished, At: d.now(), Target: t.Name, Candidate: m.cand, RunID: r.runID, CheckName: res.Name, Check: &res})
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

// chainLink is one candidate's link in the (length-1, in this chunk)
// merge-commit chain (docs/plans/phase5.md §1.2): the --no-ff merge commit
// itself, the tree it was tested against, and the candidate it links in.
type chainLink struct {
	mergeOID string
	treeOID  string
	cand     core.Candidate
}

// buildChainLink trial-merges cand onto base and, if the trial is clean,
// builds cand's --no-ff merge commit (docs/plans/phase5.md §1.2): this is
// tryStartTrial's merge+message+commit logic, factored out of the pick/
// recovery logic that now lives in refillLane/startRun so later chunks
// (P5-D/E/F) can call it once per chain link.
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

// refillLane tries to fill an idle lane for one tick (docs/plans/phase5.md
// §2.5): reconcileTarget only calls this when the lane started the tick
// empty, so there's no separate "lane busy" check here — that precondition
// is enforced by the caller, exactly as tryStartTrial's implicit
// precondition was pre-refactor.
//
// Serial only in this chunk: config.Target has no Mode field yet (P5-A),
// so batch/speculate refill strategies (§2.5's other two switch cases)
// aren't reachable and aren't implemented — this is exactly and only
// tryStartTrial's pick-head step.
func (d *Daemon) refillLane(ctx context.Context, t config.Target, targetTip string, cands map[string]core.Candidate) {
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

	d.startRun(ctx, t, targetTip, cand)
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
func (d *Daemon) startRun(ctx context.Context, t config.Target, base string, cand core.Candidate) {
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
		return
	}
	if !trial.Clean {
		d.rejectPreMerge(ctx, t, cand, core.OutcomeConflict, "trial merge conflict: "+strings.Join(trial.Conflicts, ", "), rootSpan)
		return
	}
	rootSpan.SetAttributes(attribute.String(obs.AttrMergeSHA, link.mergeOID))

	specData, err := d.git.ReadFileFromTree(ctx, trial.TreeOID, d.cfg.CheckSpec)
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err), rootSpan)
		return
	}
	spec, err := config.ParseChecks(specData)
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeRejected, fmt.Sprintf("check spec %q: %v", d.cfg.CheckSpec, err), rootSpan)
		return
	}

	// F2 (docs/plans/phase23.md §10): trial-tree export dirs are created
	// under cfg.WorkDir when it's set. os.MkdirTemp treats an empty dir
	// argument as "use the OS default temp dir" already, so this is a strict
	// superset of the phase-1 behavior; sweeping WorkDir at startup (the
	// other half of F2) is cmd's job, not the queue's (D7).
	dir, err := os.MkdirTemp(d.cfg.WorkDir, "gauntlet-trial-")
	if err != nil {
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "export tree: mkdir temp: "+err.Error(), rootSpan)
		return
	}
	if err := d.git.ExportTree(ctx, trial.TreeOID, dir); err != nil {
		_ = os.RemoveAll(dir)
		d.rejectRun(ctx, t, cand, runID, base, link.mergeOID, trial, core.OutcomeError, "export tree: "+err.Error(), rootSpan)
		return
	}

	rec := &core.RunRecord{
		RunID:     runID,
		Target:    t.Name,
		Candidate: cand,
		BaseOID:   base,
		MergeSHA:  link.mergeOID,
		Trial:     trial,
		StartedAt: d.now(),
	}
	r := &run{
		target:    t.Name,
		members:   []runMember{{cand: cand, mergeOID: link.mergeOID, rec: rec}},
		baseOID:   base,
		chainTip:  link.mergeOID,
		predicted: false,
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
}

// landRun is the generalized land (docs/plans/phase5.md §2.4): one CAS push
// lands the whole chain, then a per-member slot delete + terminal event,
// FIFO. len(r.members)==1 in this chunk: exactly phase-1's land, one push
// one delete one event.
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

// finishRun finalizes the head member's RunRecord (outcome/detail/end
// time), optionally parks its (ref, SHA), finalizes the run (root span
// end, dir removal — finalizeRun), and emits its terminal event.
// len(r.members)==1 in this chunk, so this is exactly phase-1's finishRun
// in every observable respect; a future batch chunk (P5-E) will need to
// split the per-member fields/park/emit from the once-per-run finalizeRun
// call so a batch's terminal loop doesn't end the shared root span once
// per member.
func (d *Daemon) finishRun(ctx context.Context, t config.Target, r *run, outcome core.Outcome, detail string, park bool) {
	m := &r.members[0]
	m.rec.Outcome = outcome
	m.rec.Detail = detail
	m.rec.EndedAt = d.now()

	if park {
		d.park(t.Name, m.cand, outcome, detail)
	}

	d.finalizeRun(r)

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

// finalizeRun performs the once-per-run cleanup that must happen exactly
// once regardless of member count (docs/plans/phase5.md §3.2): ends the
// root span (its summary attributes/status come from the head member's
// RunRecord — len(members)==1 today, so this is exactly the record the run
// produced) and removes the exported trial dir. Per-check log files
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
