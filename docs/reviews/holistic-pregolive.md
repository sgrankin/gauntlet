# Gauntlet Holistic Pre-Go-Live Review

Started against HEAD 2026-07-06. Reviewing the SYSTEM, not a diff.
Severity: BLOCKER / BUG / DRIFT / GAP / NIT.

Working notes below; per-lens verified-clean summaries at the end.

---

## Lens 1 — Cross-feature interaction sweep (in progress)

### auto-retry × cancel — CLEAN (traced)
- `applyCancel`→`cancelInFlight`→`finishRun(OutcomeRejected, cancelDetail, park=true)` (command.go:198). finishRun calls `maybeAutoRetry` only when `park && outcome==OutcomeError` (reconcile.go:1583). Cancel parks as OutcomeRejected, so auto-retry never fires on a cancel. Correct.
- Cancel does NOT touch `d.autoRetried`. So if the operator later retries the cancelled ref and it genuinely errors, the SHA's one-shot budget state is whatever it was. If the SHA had already spent its budget (errored→auto-retried→cancelled), a subsequent error stays parked for a human. Per-SHA budget semantics hold; not a bug.

### auto-retry emit ordering — CLEAN
- Every OutcomeError park site (finishRun, rejectPreMerge, rejectRun, rejectBatch) calls `maybeAutoRetry` AFTER the park + terminal EventError emit (reconcile.go:1584,1701,1724,1154). Auto-retry's EventQueued/EventRetryRequested follow the EventError. Correct.

## DRIFT findings

### DRIFT (lens 3) — DESIGN.md "Batch members share one run_id" watch item is STALE
- DESIGN.md:150-162 watch item ("Batch members share one `run_id`, so history keeps only the last member's row ... Likely fix: mint each member its own RunID") describes a bug that is now FIXED in code. `memberRunID` (reconcile.go:1809-1832) mints pos-0 = bare batchRunID, pos>0 = `<batchRunID>-mN`; every batch member record (finishBatchStart:1076, rejectBatch:1133, finishBatchRed emits m.rec.RunID) carries a distinct RunID while BatchID stays shared. The watch item reads as an open defect but is closed. A new operator/maintainer reading DESIGN.md would believe batch history is broken. FIX: rewrite the watch item to "RESOLVED (memberRunID)" or move to decision ledger.

### batch × post-land hooks — ACCEPTED design, operator-doc GAP
- `landRun` emits N EventLanded for an N-member batch, each carrying that member's own intermediate `MergeSHA` (reconcile.go:1508-1530; finishBatchStart sets member MergeSHA=link.mergeOID, reconcile.go:1081). `hooks.Runner.Emit` enqueues every EventLanded-with-Record → N hook runs per batch, the first N-1 against INTERMEDIATE chain-merge trees that the check suite never ran against in isolation (checks run once on the chain TIP tree). Under the DEFAULT `hooks-policy "queue"`, all N run.
- This is EXPLICITLY documented+accepted in docs/plans/phase5.md §9 ("Each landed candidate fires hooks individually ... N hook runs ... Hook coalescing across a batch is explicitly a hooks-v2 concern, out of scope").
- GAP (lens 3, operator-facing): README's "Hooks"/"Backlog policies"/"Batch" sections do NOT warn an operator that batch mode + a deploy hook runs the deploy N times per batch (N-1 against non-tip, never-in-isolation-tested trees), and that `hooks-policy "coalesce"`/`"cancel"` is the intended mitigation. A production operator running `mode "batch"` with a `hook "deploy"` and the default queue policy will deploy intermediate chain commits. Suggest a sentence in README Backlog-policies tying batch→coalesce.

### GAUNTLET_RUN_ID × batch — CLEAN
- README:1078 claims GAUNTLET_RUN_ID is "stable across every check (and, for a batch, shared by every member)". Checks: job.RunID = r.runID = bare batchID for the whole batch's single suite (reconcile.go:584,591 via startCheck). Consistent with the claim. (Per-member history RunID uses memberRunID, but that's the RunRecord identity, not the env var — no contradiction.)

### Recovered flag × history/dashboard — NIT
- runs table (schema.sql:10-36) has NO `recovered` column (nor `speculated`). RunRecord.Recovered (set in recoverLanded, reconcile.go:1665) and RunRecord.Speculated (startRun:1437) are DROPPED on the history write. The dashboard run page reads from history, so it cannot show a structured "recovered" flag. Mitigation already in place: the persisted `detail` column carries "candidate already ancestor of target; checks not re-run", so a recovered landing still reads sensibly on the run page via its detail text. Load-bearing use of Recovered (suppressing hook auto-run, hooks.go:659) works off the live Event.Record, unaffected. Severity NIT — flag not persisted, but detail text covers the operator-visible need.

## Lens 4 — Operational readiness

### Boot sequence — CLEAN
- Order (main.go run()): LoadDaemon → checkGitVersion → **AcquireLock (flock, S2)** → InstallProvider → gitx.New → sweep trials → sweep scratch → prune logs → stateToken → **services pool New+Adopt** → buildExecutor → **sweepContainerOrphans** → build channels (log, history, dashboard, ghstatus) → reaper goroutine (no-op until ArmReaper) → slack → hooks + sweep hooksDir → queue.New → startDashboard/DepthSampler/LogPruner → d.Run (initial ReconcileOnce arms reaper at end of first pass).
- Flock is taken before ANY sweep — a second daemon on the same -state fails fast (lock.go:56), closing the "second process sweeps the first's in-flight exports" window. Good.
- `pool.Adopt` (adopts warm service containers) runs BEFORE `sweepContainerOrphans`. The check sweep explicitly skips the `gauntlet-svc-` prefix (sweep.go:95) AND filters on `gauntlet-<token>-` — the two namespaces are structurally disjoint, so the check sweep can never reap a just-adopted service. Verified CLEAN.
- Reaper ticker starts before queue.New but Reap() is a no-op until ArmReaper (pool.go:596), which the queue calls at the end of the first full ReconcileOnce (daemon.go:518). Minor imprecision: the first pass only *starts* check goroutines (which then EnsureAll→refcount++ async), it doesn't await them; but the 30s reaper tick vs ms-fast EnsureAll, plus Adopt already having filtered by IdleTTL, means no practical reap-before-refcount race. Not a finding.

## Lens 5 — Trust-boundary coherence

### Capability split — CLEAN, all documented
- Daemon config (admin) gates: executor kind/runtime/image/caches/**mounts** (socket), services **allow**/max-instances/runtime. Repo spec (checks.go) declares: checks (arbitrary commands, by design), services (image/port/env/ready/memory/cpus), needs — but service/needs only RUN if the daemon opted in via `services allow` (RequiresServices gate rejects loudly otherwise, reconcile.go:1053,1408).
- memory/cpus magnitude: repo-controlled, validated FORMAT-only (`999g` passes memoryPattern, checks.go:102). This IS documented: README:999-1007 ("max-instances bounds count, not resources ... memory 2g passed to --memory verbatim") and services.md:387-392. A format-valid-but-impossible request (e.g. "999g") → Create fails → OutcomeError → auto-retry once → park. No undocumented capability leak. CLEAN.
- Mount validation (daemon.go:826-867): absolute paths, no ':', cannot shadow workdir or reserved result-dir (pathAtOrUnder). Socket-mount trust change is the operator's documented explicit choice, not gated (accepted, DESIGN ledger). CLEAN.

### DRIFT (lens 3/5, NIT) — summarize effort "none" omitted from error msg + example
- validSummarizeEfforts accepts "none" (daemon.go:140, the escape hatch for models rejecting output_config.effort like claude-haiku-4-5), but validate()'s rejection message lists only "low, medium, high, xhigh, max" (daemon.go:910) and gauntlet.kdl:110 comment says "one of low|medium|high|xhigh|max". An operator who needs "none" (e.g. running summaries on Haiku) can't discover it from the error or the example config. FIX: add "none" to the daemon.go:910 message and the gauntlet.kdl comment.

## Lens 1 — HEADLINE FINDING

### BUG (lens 1) — speculate: a trial conflict against a PREDICTED base parks the candidate, contradicting the F2 "no false-negative park on a prediction" amendment
- **Where:** `refillSpeculate` (reconcile.go:1290-1298) → `startRun` (predicted=true) → on `!trial.Clean` → `rejectPreMerge(OutcomeConflict)` (reconcile.go:1389) → `d.park` (reconcile.go:1693). A non-head speculation-window candidate that conflicts against its predecessor's UNPUSHED chainTip is PARKED (sticky per ref+SHA).
- **The inconsistency:** phase5.md:831 (F2, the phase-5 adversarial review amendment) established for RED verdicts at lane index >0 that parking on a *prediction* is a "false-negative park: a candidate that would pass cleanly once retested against reality gets stuck Rejected forever" — and changed the red path to Skip+requeue (park=false). The code implements exactly that for reds (advanceLane bubble default branch, reconcile.go:463-466). But a trial CONFLICT against the same kind of predicted base is a prediction in exactly the same sense, and it still PARKS. The F2 amendment scoped itself only to §2.1c (red bubble); nobody reconciled it with §2.5's refill-conflict pseudocode (phase5.md:296 "this candidate parks"), which predates F2.
- **Reachable false park:** window [A,B]; B conflicts with A's predicted chainTip → B parks immediately at refill (tick 1). Later A goes red at index 0 → A parks; target never advances. B is now parked as "conflicts with in-flight a@<sha> (predicted base)" against a merge that never happened — yet B may merge cleanly onto the real (unchanged) target. B stays parked (syncBookkeeping keeps it; SHA unchanged) until a human retry. Auto-retry does NOT rescue it (OutcomeConflict, not OutcomeError). The correct behavior, per F2's own reasoning, is Skip+requeue so B re-forms and, if still conflicting once it reaches index 0 against the real tip, parks for real there.
- **Test status:** `TestSpeculateConflictAgainstPredictedBase_Parks` (speculate_test.go:106) actively enshrines the immediate park and does NOT explore the predecessor-fails-then-B-was-landable case — so the suite locks in the F2-inconsistent behavior rather than catching it.
- **Severity/scope:** correctness/liveness (a landable candidate stuck parked, needs manual retry). Opt-in: speculate mode only (not serial/batch, not the default). Not data corruption. Recommend: extend F2's skip+requeue to the predicted-base conflict path (park=false for predicted conflicts), and update the test.

### mounts × services networks — CLEAN
- runArgs (container.go:345-383) appends Caches (-v), operator Mounts (-v[:ro]), then job.Networks (--network per net), then job.ServiceEnv (-e). A check with BOTH a socket mount and `needs` emits both `-v /var/run/docker.sock:...` and `--network gauntlet-svc-<token>` with no ordering conflict. CLEAN.

### empty-mount preflight × services (M1 re-probe) — CLEAN (minor precedence NIT)
- Order: queue's startCheck goroutine calls `RunCheck` (reconcile.go:639); the container executor's empty-mount diagnosis runs INSIDE RunCheck on the CheckFailed path (container.go:289) and may set res.Err. Back in the queue wrapper, the M1 AnyDead service-death re-probe runs only `if res.Err == nil && res.Status == CheckFailed` (reconcile.go:640-648). So if BOTH the mount is empty AND a service died, the empty-mount Err wins and AnyDead is never consulted. Both paths produce OutcomeError→park-as-error, so the OUTCOME is identical; only the Detail message differs (empty-mount message shown instead of service-death). NIT: diagnostic-precedence only, no correctness impact. The mount is the git-tree export, independent of service health, so a service death cannot manufacture an empty-mount false positive.

### GAP (lens 4) — shared-services lifecycle is invisible in daemon logs (3am-debugging gap)
- The entire `internal/services` package writes NOTHING to stderr/logs on create, evict, reap, adopt-success, or ensure. Grep of internal/services/*.go for Fprint/log/stderr finds only a comment. The pool emits no core.Event either (no Emit in the package). So an operator debugging via `journalctl -u gauntlet` has ZERO record of: when a service was created, when the reaper evicted an idle instance, when adoption re-warmed instances at boot, or when a mid-run service death was detected (only the resulting check EventError park is logged, not the service-level cause).
- By contrast auto-retries surface (EventQueued/EventRetryRequested → log channel, log.go:199,225), container-orphan sweeps log (sweep.go), hook drops/failures log, and adopt FAILURE logs (main.go:250). Services is the one subsystem silent on its own lifecycle.
- Only observability today is the pull-based Snapshot (dashboard/API/MCP) — a reap+recreate between two snapshot reads leaves no trace at all.
- Severity GAP (operational readiness). For a subsystem whose whole value is warm-instance reuse and whose failures are "infra-shaped," the lack of a create/evict/reap log line is a real production-debugging hole. Suggest one stderr line per create/evict/reap/adopt (with key hash + service name), gated behind nothing (steady-state volume is low).

### DRIFT (lens 3/4) — state-dir "safe to delete" table is missing 4 of 6 subdirs + the lock
- deploy-linux.md:365-366's table lists only `repos/` and `trials/`. deploy.md:61 prose says "-state: bare repo clone(s) + trials/ scratch + logs/". But main.go actually creates SIX subdirs under -state: `repos/`, `trials/`, `scratch/`, `logs/`, `hooks/`, `services/` (main.go:155,168,180,193,231,386), plus `gauntlet.lock` (lock.go) and optional `history.db`.
- Missing from operator guidance: `scratch/` and `hooks/` (ephemeral, swept at startup — safe to delete), `services/` (service records = efficiency hints, safe to delete → colder pool), `gauntlet.lock` (must persist / not be deleted under a running daemon; recreated), and `logs/` gets a mention in prose but not the delete-safety table. An operator doing disk cleanup by the table would mistake the state layout. FIX: expand the deploy-linux.md table to all six subdirs + lock + history.db with per-path disposability.

### DRIFT (lens 3) — batch×summarize stall doc overstates cost 4× (describes pre-S6 serial behavior)
- README:413-415 and phase5.md §9:777 say forming a batch of N makes "N sequential summary calls ... bounded by N × timeout". But `precomputeMergeBodies` (summary.go:63) runs the N MergeBody calls CONCURRENTLY, capped at maxConcurrentMergeBodies=4 in flight (summary.go:22). So the actual reconcile-loop stall is ceil(N/4) × timeout — for the default max-batch=8, 2×timeout, not 8×. The S6 phase-6-audit parallelization landed but the docs still describe the old serial cost. Operator-conservative (overstates), but wrong. FIX: update both to "ceil(N/4) × timeout" / "up to 4 concurrent summary calls".

### DRIFT (lens 3) — DESIGN.md "Watch items" has accumulated 3 FIXED-but-not-retired items
A new maintainer reading DESIGN.md's Watch Items would believe these are open defects; all three are closed in code:
1. **"Batch members share one run_id" (DESIGN.md:150-162)** — FIXED by `memberRunID` (reconcile.go:1809). [already detailed above]
2. **"Park-seed resurrection edge" (DESIGN.md:163-172)** — FIXED by the S3 retry_intents suppression. `applyRetry` emits EventRetryRequested → history writes a `retry_intents` row; `LatestTerminalPerRef` LEFT JOINs it and drops the seed when the retry is newer than the terminal row's ended_at (queries.go:399-403,444 `WHERE ... (ri.at IS NULL OR ri.at <= t.ended_at)`). The exact "just-retried ref re-parks on boot" scenario the watch item describes is what this query closes. The watch item predates the fix and still reads as a live edge.
3. **"Event shapes ... EventCheckFinished carries no CheckResult" (DESIGN.md:136-142)** — the specific claim is stale: Event.Check `*CheckResult` exists (types.go:401) and is set on EventCheckFinished (reconcile.go:555, F-a), so channels DO show per-check verdicts mid-run (hooks/dashboard consume it). The watch item's general point (event shapes are fragile) stands, but the stated condition is fixed. NIT within this cluster.
FIX: retire #1 and #2 to the decision ledger as RESOLVED; update #3's wording.

## Lens 2 — Contract parity (my independent spot-checks; full enumeration delegated)

### idleSince composition — CLEAN across surfaces
- Queue-only idleness (snap.IdleSince, excludes pool + hook state) is composed with hook idleness (Running || BacklogDepth>0) IDENTICALLY on the dashboard API (api.go:402-421) and MCP (server.go:403-421). CLI `gauntlet status` reads /api/v1/status so inherits the composed value. Dashboard footer surfaces the same. Slack doesn't surface idleSince (autoscale signal, not needed). Consistent.
- Note: pool/services state is deliberately NOT folded into idleSince (a warm-but-idle refcount-0 instance does not make the daemon "busy") — correct per scale.md §2.

### Recovered/Speculated flags — see Lens-1 NIT above (dropped by history, not on dashboard).

(Full field-by-field parity enumeration across dashboard HTML / API / MCP / CLI / Slack delegated to a sub-audit; results pending.)

---
## PENDING sub-audits (delegated)
- Contract parity (lens 2): full field enumeration across 5 surfaces.
- Docs-vs-code drift (lens 3): README config-ref, API/MCP lists, runbook flags, stale plan claims.
- Test blind spots (lens 6): ensure×preflight compose, config-combo coverage, FindLandingMerge fake drift, batch-member history rows.
