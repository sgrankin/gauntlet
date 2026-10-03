package queue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
)

// advanceChecks consumes completed workers in spec order, records the first
// failure, and starts ready nodes within execution limits. It does not
// decide whether the run should land or requeue.
func (d *Daemon) advanceChecks(ctx context.Context, t config.Target, r *run) {
	if r.verdict != verdictNone {
		return // already determined; waiting its turn behind a predecessor
	}

	// (1): consume EVERY available result first, in spec order — every
	// check that actually ran to completion gets its real result recorded
	// (status, duration, output, log link) and its EventCheckFinished,
	// even when a sibling that finished in the same window went red.
	// Culling before draining would throw a completed sibling's buffered
	// result away and falsely record a check that RAN as blocked.
	for i := range r.checks {
		name := r.checks[i].Name
		inf, ok := r.inflight[name]
		if !ok {
			continue
		}
		select {
		case res := <-inf.result:
			obs.EndCheck(inf.span, res)
			res.Waited = inf.waited
			// An image-build node's green is conditional on its captured
			// result validating as one IMMUTABLE reference — validated
			// HERE, before the result is stored or evented, so a bad
			// result is the build's own red verdict (one root cause;
			// consumers block on it) and never N consumer failures.
			if imgName, isImage := imageNodeName(name); isImage && res.Err == nil {
				switch res.Status {
				case core.CheckPassed:
					if ref, err := validImageRef(res.Image); err != nil {
						res.Status = core.CheckFailed
						res.Image = ""
						if res.Output != "" {
							res.Output += "\n"
						}
						res.Output += "gauntlet: " + err.Error()
					} else {
						res.Image = ref
						r.imageRefs[imgName] = ref
					}
				case core.CheckSkipped:
					// Builds have no skipped verdict (the shipped executors
					// can't produce one for an ImageBuild job), but the
					// Executor is an injected interface: a green-shaped
					// result with NO validated ref would release consumers
					// onto the profile's static image — silently different
					// bytes than claimed, the exact hole this feature
					// closes. Flip it red so the guarantee holds by
					// construction, whatever the executor did.
					res.Status = core.CheckFailed
					if res.Output != "" {
						res.Output += "\n"
					}
					res.Output += "gauntlet: an image build cannot report skipped"
				}
			}
			// The receipt node's (issue #13) green is likewise conditional
			// on its captured result validating — non-empty, readable, and
			// within the configured max-bytes ceiling — validated HERE for
			// the same one-root-cause reason as an image build above. On
			// success the validated payload moves onto the run struct (in
			// memory only, never persisted onto this CheckResult/history
			// row); the transient captured bytes are dropped either way.
			if rcpName, isReceipt := receiptNodeName(name); isReceipt && res.Err == nil {
				switch res.Status {
				case core.CheckPassed:
					maxBytes := 0
					if d.cfg.ReceiptNotes != nil {
						maxBytes = d.cfg.ReceiptNotes.MaxBytes
					}
					if payload, err := validReceipt(res.Receipt, maxBytes); err != nil {
						res.Status = core.CheckFailed
						if res.Output != "" {
							res.Output += "\n"
						}
						res.Output += "gauntlet: " + err.Error()
					} else {
						r.receiptPayload = payload
						r.receiptProducer = rcpName
					}
				case core.CheckSkipped:
					// Same defensive flip as an image build: a receipt has
					// no skipped verdict, so a green-shaped result with no
					// validated payload must not silently release the run
					// toward landing with nothing to publish.
					res.Status = core.CheckFailed
					if res.Output != "" {
						res.Output += "\n"
					}
					res.Output += "gauntlet: a receipt cannot report skipped"
				}
				res.Receipt = nil // never carried past this validation step
			}
			// OTLP node-completion histograms (issue #14), recorded AFTER
			// the image/receipt validation above so the metric's outcome
			// attribute matches the verdict history stores — a
			// validation-flipped node must not count as "passed" in
			// outcome-partitioned aggregates. (EndCheck's span status
			// above keeps its pre-existing pre-validation semantics.)
			// nodeKind is the one bit of node-name-prefix knowledge obs
			// needs but must not own itself (that convention is queue's).
			obs.RecordNode(ctx, t.Name, nodeKind(name), res)
			delete(r.inflight, name)
			r.results[name] = res
			// Check is the just-finished result itself, so channels can
			// render a per-check verdict mid-run instead of waiting for
			// the run's terminal event. Candidate attribution is the run's
			// head member: one event per check per run, not one per member
			// per check, keeping channel noise independent of batch size —
			// the per-member terminal event carries each member's own
			// duplicated Checks slice regardless.
			d.emit(ctx, core.Event{Kind: core.EventCheckFinished, At: d.now(), Target: t.Name, Candidate: r.members[0].cand, RunID: r.runID, CheckName: res.Name, Check: &res})
		default:
			// still running
		}
	}

	// (2): with the drain complete, the first non-green RESULT in spec
	// order (deterministic whatever the completion order was) becomes the
	// run's recorded root failure, and the run fails fast: everything
	// still genuinely running is cancelled — those commands' outcomes can
	// no longer matter (the run is red regardless), so burning builder
	// capacity on them buys nothing. Only never-finished checks become
	// blocked rows; same-window finishers were already recorded above.
	if r.culprit == "" {
		for i := range r.checks {
			if res, ok := r.results[r.checks[i].Name]; ok && !r.nodeGreen(res) {
				r.culprit = r.checks[i].Name
				d.cancelRun(r)
				break
			}
		}
	}

	if r.culprit != "" {
		// cancelInflight above emptied the map, so the drain is complete
		// the same tick the culprit lands; the guard is belt-and-braces
		// against a future partial-cancel policy.
		if len(r.inflight) == 0 {
			d.materializeChecks(r)
			if r.results[r.culprit].Err != nil {
				r.verdict = verdictErrored
			} else {
				r.verdict = verdictRejected
			}
		}
		return
	}

	// (3): start every ready check, spec order, up to the caps. capacity
	// is the run's own remaining max-parallel headroom this tick; a
	// daemon-cap denial consumes it too, so readyAt (the Waited stamp) is
	// only ever placed on checks that WOULD have started but for the
	// daemon-wide cap — a check merely queued behind its own run's
	// max-parallel is not starving and must not report capacity pressure.
	capacity := r.maxParallel - len(r.inflight)
	slotDenied := false
	for i := range r.checks {
		if capacity <= 0 {
			break
		}
		c := &r.checks[i]
		if _, done := r.results[c.Name]; done {
			continue
		}
		if _, running := r.inflight[c.Name]; running {
			continue
		}
		ready := true
		for _, dep := range c.After {
			// core.NodeGreen — Passed or Skipped with no Err, the same
			// results that keep a candidate green. A non-green dep can't
			// occur here (it would have set r.culprit above).
			if res, ok := r.results[dep]; !ok || !r.nodeGreen(res) {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		capacity--
		if slotDenied || !d.cfg.Slots.TryAcquire() {
			// The daemon-wide cap is saturated: this check stays ready and
			// starts on a later tick. One denial means denial for every
			// later ready check this tick too (single semaphore) — no more
			// TryAcquire calls; the scan continues only to stamp the other
			// checks inside this run's own headroom.
			slotDenied = true
			if _, stamped := r.readyAt[c.Name]; !stamped {
				r.readyAt[c.Name] = d.now()
			}
			continue
		}
		d.startCheck(ctx, r, i)
	}

	// (4): green once everything finished (and nothing red — culprit above).
	if len(r.results) == len(r.checks) && len(r.inflight) == 0 {
		d.materializeChecks(r)
		r.verdict = verdictGreen
		// The tested merge is verified — a true statement about this exact
		// MergeSHA, emitted BEFORE the landing CAS (which can still fail or
		// be skipped if the target moved). advanceChecks's verdictNone
		// guard makes this the sole green transition, so the emit is
		// once-per-run; verifiedEmitted is belt-and-braces. One member
		// carries the run's MergeSHA in every mode (batch's chain tip
		// included) — members[0].rec is the head record. Gated with
		// EventTrialMerged behind TrialRefs so disabled-mode event streams
		// stay byte-identical to today's.
		if d.cfg.TrialRefs && !r.verifiedEmitted {
			r.verifiedEmitted = true
			detail := ""
			if r.emergencyReason != "" {
				detail = "EMERGENCY: checks waived; " + r.emergencyReason
			}
			d.emit(ctx, core.Event{
				Kind: core.EventVerified, At: d.now(), Target: r.target, Detail: detail,
				Candidate: r.members[0].cand, RunID: r.runID, MergeSHA: r.chainTip,
			})
		}
	}
}

// materializeChecks writes results once, in spec order, to each member
// record. Nodes prevented from running receive CheckBlocked with the failed
// prerequisites or root culprit.
func (d *Daemon) materializeChecks(r *run) {
	if r.materialized {
		return
	}
	r.materialized = true

	var out []core.CheckResult
	for i := range r.checks {
		c := &r.checks[i]
		if res, ok := r.results[c.Name]; ok {
			// Seq: the 1-based spec position, matching the log filename
			// prefix startCheck used — load-bearing for externally
			// concluded parallel runs, whose finished set can have gaps
			// (a later check done, an earlier one aborted mid-run), so a
			// row's slice index alone would drift from its position.
			res.Seq = i + 1
			out = append(out, res)
			continue
		}
		if r.culprit == "" {
			continue // externally concluded; unstarted checks get no row
		}
		var blockedBy []string
		for _, dep := range c.After {
			if res, ok := r.results[dep]; !ok || !r.nodeGreen(res) {
				blockedBy = append(blockedBy, dep)
			}
		}
		if len(blockedBy) == 0 {
			blockedBy = []string{r.culprit}
		}
		out = append(out, core.CheckResult{Name: c.Name, Seq: i + 1, Status: core.CheckBlocked, BlockedBy: blockedBy})
	}
	for i := range r.members {
		r.members[i].rec.Checks = append([]core.CheckResult(nil), out...)
	}
}

// startCheck launches r.checks[idx] via the configured Executor in its own
// goroutine, which communicates back solely by sending once on the
// checkInFlight's one-shot result channel. The caller (advanceChecks's
// scheduler step) has already taken this check's daemon-wide execution
// slot; the goroutine releases it on exit — after RunCheck has returned,
// i.e. after executor child cleanup — so a freed slot never represents a
// still-running process or container.
func (d *Daemon) startCheck(ctx context.Context, r *run, idx int) {
	d.markCircuitProbe(r)
	check := r.checks[idx]
	checkCtx, cancel := context.WithCancel(r.rootCtx)
	spanCtx, span := obs.StartCheck(checkCtx, d.tr, check.Name)

	job := core.CheckJob{
		GitDir:    d.cfg.SourceGitDir,
		RunID:     r.runID,
		Target:    r.target,
		Name:      check.Name,
		Command:   check.Command,
		Executor:  check.Executor,
		Dir:       r.dir,
		Image:     r.imageRefs[check.Image], // "" unless this check consumes a candidate-built image (readiness guarantees the ref exists by now)
		BaseSHA:   r.baseOID,
		MergeSHA:  r.chainTip,
		Candidate: r.members[0].cand,
		Clean:     false, // reserved for a future clean-build cache escape hatch; see docs/design/core.md ("Deliberately not built")
	}
	for _, m := range r.members {
		job.Candidates = append(job.Candidates, m.cand)
	}
	// An "image:<name>" node is a BUILD: the executor swaps the result-
	// file protocol (GAUNTLET_IMAGE_RESULT_FILE) and hands the captured
	// reference back on the result for advanceChecks to validate.
	if _, isImage := imageNodeName(check.Name); isImage {
		job.ImageBuild = true
	}
	// The receipt node (issue #13) gets the same env-var-swap treatment,
	// plus the configured size ceiling so the executor's own bounded read
	// (MaxBytes+1) can detect an oversized result before this job's
	// ephemeral scratch dir is removed — the queue never gets a second
	// chance to read the file itself.
	if _, isReceipt := receiptNodeName(check.Name); isReceipt {
		job.ReceiptCapture = true
		if d.cfg.ReceiptNotes != nil {
			job.ReceiptMaxBytes = d.cfg.ReceiptNotes.MaxBytes
		}
	}
	// LogDir == "" disables per-check log files entirely (job.LogPath stays
	// ""); see DESIGN.md ("Full per-check log files"). The check name is
	// free-form config, so it's sanitized the same way container names are
	// (core.SanitizeName) before becoming a path component — the trailing
	// ".log.zst" suffix additionally guarantees the sanitized name can
	// never resolve to "." or "..". The filename is prefixed with the
	// check's 1-based SPEC-DECLARATION position (idx+1) — the durable
	// per-check identity, stable regardless of the order parallel checks
	// actually start or finish, and matching history's per-check seq
	// column (materializeChecks fills records in this same spec order):
	// two check names that sanitize to the
	// same string (e.g. "lint go" and "lint/go", both -> "lint-go") would
	// otherwise alias onto the same O_TRUNC'd file, with both checks'
	// history rows pointing at whichever happened to write last.
	//
	// ".log.zst": the executor writes this file as a single zstd stream
	// (internal/executor/logfile.go's openCheckLog) — the suffix is what
	// the dashboard's handleRunLog keys on to decide whether to decompress
	// on serve (legacy plain ".log" rows from before this change keep
	// working unchanged).
	if d.cfg.LogDir != "" {
		job.LogPath = filepath.Join(d.cfg.LogDir, r.runID, fmt.Sprintf("%d-%s.log.zst", idx+1, core.SanitizeName(check.Name)))
	}

	result := make(chan core.CheckResult, 1)
	start := d.now()
	// Waited: how long this check sat ready-but-slotless (advanceChecks
	// stamped readyAt on the first denial). Zero for the common immediate
	// start.
	var waited time.Duration
	if readyAt, ok := r.readyAt[check.Name]; ok {
		waited = start.Sub(readyAt)
		delete(r.readyAt, check.Name)
	}
	// ALL blocking service work — EnsureAll and the mid-run liveness
	// re-probe — happens here, inside this check's own goroutine, never on
	// the reconcile goroutine; see docs/design/services.md ("Lifecycle:
	// ensure, release, reap") for why that's load-bearing. needs/svcs are
	// captured by value into the closure so a later mutation of r (none
	// happens, but defensively) can't race this goroutine.
	needs := check.Needs
	svcs := r.services
	isolated := r.isolated
	chainTip := r.chainTip   // captured by value; the goroutine never touches r
	chainTree := r.chainTree // the exact tree to materialize (not the commit — export-subst)
	sources := make([]string, len(r.members))
	for i, member := range r.members {
		sources[i] = member.cand.SHA
	}
	releaseSources, sourceErr := core.RetainSources(ctx, d.git, sources...)

	go func() {
		// The execution slot advanceChecks acquired for this check is
		// released only when this goroutine exits — RunCheck has returned
		// and the executor's child cleanup is complete by then, so the
		// freed slot never represents a live process/container.
		defer d.cfg.Slots.Release()
		if sourceErr != nil {
			result <- core.CheckResult{Name: check.Name, Err: fmt.Errorf("retain check sources: %w", sourceErr)}
			return
		}
		defer releaseSources()

		// Isolated mode (issue #9): materialize this node's own private
		// copy of the chain-tip tree now that the slot is held, so the
		// daemon-wide cap bounds simultaneous archives too. A failure is
		// park-as-error, exactly like a service-ensure failure below —
		// never a fallback to a shared dir. The dir is removed after
		// RunCheck returns (child stopped), covering pass/fail/cancel.
		var materialized time.Duration
		if isolated {
			wsDir, took, err := d.materializeNode(spanCtx, chainTree, chainTip)
			if err != nil {
				result <- core.CheckResult{Name: check.Name, Command: job.Command, Err: fmt.Errorf("materialize workspace: %w", err)}
				return
			}
			defer os.RemoveAll(wsDir)
			job.Dir = wsDir
			materialized = took
			span.SetAttributes(attribute.Int64("gauntlet.workspace.materialize_ms", took.Milliseconds()))
		}

		// Command (v8, run.html's command echo): stamped onto the result
		// here rather than by the Executor implementations themselves, since
		// job.Command is already in scope at every send point below and this
		// is the one place a CheckResult crosses back from "what was run" to
		// "what history records" — see core.CheckResult.Command's doc.
		if len(needs) == 0 || d.cfg.Services == nil {
			res := d.cfg.FailureReview.Run(spanCtx, job, d.exec.RunCheck)
			res.Command = job.Command
			res.Materialized = materialized
			stampConsumedImage(&res, job)
			result <- res
			return
		}
		ens, err := d.cfg.Services.EnsureAll(spanCtx, svcs, needs) // BLOCKING, off the reconcile loop
		if err != nil {
			result <- core.CheckResult{Name: check.Name, Command: job.Command, Materialized: materialized, Err: fmt.Errorf("service ensure: %w", err)}
			return // -> verdictErrored -> OutcomeError, park-as-error (see docs/design/services.md "Failure semantics")
		}
		defer d.cfg.Services.Release(ens) // refcount--; last-used is touched on release, not ensure
		job.ServiceEnv, job.Networks = ens.Env, ens.Networks
		res := d.cfg.FailureReview.Run(spanCtx, job, func(ctx context.Context, attempt core.CheckJob) core.CheckResult {
			res := d.exec.RunCheck(ctx, attempt)
			if res.Err == nil && res.Status == core.CheckFailed && d.cfg.Services.AnyDead(ctx, ens) {
				res.Err = fmt.Errorf("service died mid-run (park-as-error); check output retained above")
			}
			return res
		})
		res.Command = job.Command
		res.Materialized = materialized
		stampConsumedImage(&res, job)
		result <- res
	}()
	r.inflight[check.Name] = &checkInFlight{name: check.Name, cancel: cancel, result: result, span: span, start: start, waited: waited}

	d.emit(ctx, core.Event{Kind: core.EventCheckStarted, At: d.now(), Target: r.target, Candidate: r.members[0].cand, RunID: r.runID, CheckName: check.Name})
}

// stampConsumedImage records, for provenance, the exact immutable image a
// CONSUMER check ran in (CheckResult.Image; history's image column). Build
// nodes are untouched — the executor already set res.Image to the build's
// captured result there, and overwriting it with job.Image ("" for
// builds) would destroy it.
func stampConsumedImage(res *core.CheckResult, job core.CheckJob) {
	if !job.ImageBuild && job.Image != "" {
		res.Image = job.Image
	}
}

// cancelRun aborts every in-flight check of r (Invariant 5 for a move,
// fail-fast for a red verdict, operator cancel): each check's context is
// cancelled (the executor is responsible for killing the underlying
// process group) and its span ends, without waiting for the executor
// goroutines — they report into buffered channels nobody needs to read
// anymore, and each releases its own execution slot on exit regardless.
func (d *Daemon) cancelRun(r *run) {
	for name, inf := range r.inflight {
		inf.cancel()
		obs.EndSpan(inf.span, context.Canceled)
		delete(r.inflight, name)
	}
}
