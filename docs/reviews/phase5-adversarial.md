# Phase-5 adversarial review — complete

Hand-traced the lane state machine, chain/CAS mechanics, batch-member records, park seeding, and cross-mode config. Empirically confirmed F1's mechanism and F3 (scratch tests, since removed). Baseline `go test ./internal/queue/` green (10.9s). All findings latent.

**Severity summary: 2 high (F1, F3), 1 medium (F2), 1 low (F4), plus 3 observations.**

---

## Findings (ranked by severity)

### F1 (HIGH) — Slack batch summary: a dropped member terminal event → nil-deref panic (daemon crash) or permanent map leak

`internal/slack/slack.go:381-412` (`postBatchTerminal`) buffers per-member records into `batchRecs[BatchID]` indexed by `Position`, flushing only when `Position == BatchSize-1` arrives. But `Emit` (`slack.go:163-170`) silently *drops* events when the 256-slot outbox is full, and the drainer runs on its own goroutine — so within the N back-to-back per-member emits (`landRun`/`finishBatchRed`/`rejectBatch`/`cancelBatchMember` each emit N in a row), the drainer can free a slot mid-burst, dropping a **middle** member while a later one still enqueues.

When the last member then arrives, `summarizeBatch` (`slack.go:507-525`) runs over `recs` with a `nil` hole: `head := recs[0]` or `for _, r := range recs { r.Position }` dereferences nil. No `recover()` exists in `drainOutbox` (verified) → panics the drainer goroutine → **crashes the daemon process**.

If instead the **last** member is the one dropped, the flush never fires: `batchRecs[BatchID]` plus the `runRoot`/`roots` entries **leak forever** — violating the §9.2 bound the code's own doc comment promises ("never leaks an entry per finished batch"). Contract broken: a dropped event is supposed to "never do anything worse than lose a diagnostic line."

- **Precondition:** outbox saturation (256 backlog) with a running-but-slow drainer — uncommon, but consequence is a process crash.
- **Fix:** make `summarizeBatch` tolerate `nil` holes; flush/forget with a completeness fallback (arrival count + time bound) so a drop degrades to a partial summary, never a panic or leak.

### F2 (MEDIUM) — A speculative non-head run parks red before its predecessor lands; a later predecessor failure can't reset it (false-negative park)

`advanceLane`'s bubble step (`reconcile.go:406-418`) parks the first red run at **any** lane index. Trace window `[A, B]`:

1. Tick N: A still mid-check (verdict none), B resolves red — but B was tested on A's **predicted** merge, so B's red can be caused by A's changes.
2. Loop skips A (index 0, not red), hits B (index 1, red), parks it (`finishRun(..., park=true)`), truncates lane to `[A]`. **B has left the lane.**
3. Later tick: A goes red, or A's land CAS goes stale, or targetTip moves and A is invalidated (`reconcile.go:340`). `invalidateSuffix` fires — but B is already gone, so **nothing resets it**.

B stays parked `Rejected` though it was never proven red against the **real** target, only against `target+A`. This is a gap in precisely the machinery meant to handle predecessor failure: §2.1c resets the suffix still **in** the lane, not a successor that already parked out of it. Self-heals only on B's re-push or an operator retry. `testdata/script/speculate_bubble.txtar` only covers the predecessor-lands case, so this is untested.

- **Fix:** only **park** a red culprit at lane index 0 (its base is the swept-valid real tip); a red run at index > 0 should Skip-unparked like the suffix and be re-tested when it reaches head (the Zuul reset-on-predecessor-reset behavior).

### F3 (HIGH) — Snapshot `Waiting` double-counts in-flight batch/speculate members, corrupting the queue-depth tuning instrument

`buildTargetSnapshot` (`snapshot.go:140-149`) excludes only the single head-run head member (`ts.InFlight.Candidate.Ref`) from `Waiting`, not all in-flight members. **Empirically confirmed:** a filled speculate window of 3 reports `Pipeline=3` AND `Waiting=2` (bob, carol) — the non-head runs appear in both lists. Same for a batch of N (members 1..N-1 leak into Waiting).

The cmd depth sampler records `len(ts.Waiting)` (`cmd/gauntlet/dashboard.go:151`) as the "waiting" series — the exact instrument §10 amendment 5 says the phase exists to gather — so it's inflated by `window-1` / `batchSize-1` every busy tick, and the dashboard shows phantom queued entries.

- **Fix:** collect every `lane.runs[*].members[*].cand.Ref` into a set and exclude all of them from `waitingRefs`.

### F4 (LOW) — deploy.md `git gc` guidance is incomplete and contradicts plan §1.1

`docs/deploy.md:324-330` tells operators an "occasional `git gc` … (safe while the daemon runs; git locks correctly)." Locking protects repo integrity but is **not** why in-flight chain links survive — they survive only because default `gc.pruneExpire` is `2.weeks.ago`, so recently-written loose objects reachable only from daemon **memory** aren't pruned. Plan §1.1 explicitly warns `git gc --prune=now` "could reap them mid-flight." A `--prune=now` (a common "clean up now" incantation, or a repo with `gc.pruneExpire=now`) can reap an unpushed link a window is about to land onto or chain further, causing spurious land failures / infra-error parks.

- **Verdict on the doc claim:** the "safe while the daemon runs" conclusion is right for **default** gc but for the wrong stated reason (locking, not the grace period), and dangerously silent on `--prune=now`.
- **Fix:** state that plain `git gc` is safe **because of the loose-object prune grace period**, and explicitly warn against `git gc --prune=now` / `gc.pruneExpire=now` while the daemon is live.

---

## Observations (lower confidence / minor)

- **O1** — Seed can overwrite a same-tick operator cancel. `drainCommands` runs before `reconcileTarget`→`seedParksOnce`. On a target's first tick, a `cancelWaiting` park (`command.go:225-251`) is written into `d.done`, then `seedParksOnce` (`reconcile.go:207-227`) unconditionally overwrites `m[seed.Ref]` if the same ref has a red seed at the same SHA — replacing `cancelDetail` with the historical reason. Both are red parks, so cosmetic, but the "cancelled by operator" provenance is lost.
- **O2** — Recovered batch-member landings lose batch identity. `recoverLanded` (`reconcile.go:1470-1490`) synthesizes a fresh non-batch `runID` and empty `BatchID`, so a crash between the batch push and the Nth slot-delete records the recovered members as **serial** landings (no `batch_id`). Correct for slot cleanup/CAS and Slack stays quiet, but the batch grouping isn't reconstructed. A one-line §8 doc note would close it.
- **O3** — Spec-change boundary is content-delta, not touch-based. `specChanged` (`reconcile.go:692-703`) compares spec content between links; a member that deletes and a later member re-adds an identical spec within one batch won't fire a boundary between them. Matches the documented "content, not blob OID" intent — confirming it's deliberate.

---

## Checked clean

- **CAS/FIFO land mechanics** — one target push + N slot deletes (`landRun` `reconcile.go:1338-1388`); prefix-drain chains each land's CAS base to the prior run's `chainTip`; out-of-order land CAS-fails. `TestSpeculateLand_FIFOCAS` honestly asserts the full old/new CAS chain. Concurrent direct-push during a drain → clean `ErrCASStale` Skip, no corruption (hand-traced).
- **Validity sweep / suffix truncation** — sweep runs before verdict-consume (`reconcile.go:384-389`); `invalidateSuffix(i+1)` + `runs[:i]` keeps predecessors and drops culprit+suffix with no double-finish (hand-traced).
- **§7 invariant spot-checks (verified against code, not comments):** Inv 2 — `landRun` does exactly one `CASUpdate(targetRef, baseOID, chainTip)`. Inv 3 — per-member `CASUpdate(m.cand.Ref, m.cand.SHA, "")`, N deletes. Inv 6 — `buildChainLink` (`reconcile.go:657`) commits `parents=[base, cand.SHA]`, so `parent[1]==cand.SHA` verbatim. All three hold.
- **Batch red → CheckStats honesty (amendment 1)** — `CheckStats` (`queries.go:265-311`) counts one suite per `COALESCE(NULLIF(batch_id,''), run_id)` via a `MIN(run_id)` representative CTE; duplicated member check-rows counted once.
- **Suffixed member run IDs** — `memberRunID` (`reconcile.go:1633-1638`) gives pos>0 a `-mN` suffix so `INSERT OR REPLACE` doesn't collapse the batch's N history rows; `LatestTerminalPerRef` partitions by `candidate_ref` (distinct per member), so seeding is unaffected.
- **Park seeding fidelity** — red-family filter (`isRedOutcome`, `reconcile.go:217`) + `syncBookkeeping`'s SHA-currency drop means a SKIPPED batch member never seeds a park; `parseOutcome` mapping matches `outcomeString`'s vocabulary.
- **Cancel same-tick-as-refill** — cancels mutate no git ref, so re-picking siblings against the same tick's cands is safe (`command.go:162-174`); cancelling a middle speculate run bubbles only its suffix.
- **`advanceChecks` nil-`cur` guard** (`reconcile.go:474-477`) — correct for a green non-head run waiting behind a slower predecessor.
- **Test honesty** — no `t.Skip`, sleeps, or TODOs in the new suites (batch/speculate/cancel/chain/seedparks); `TestSpeculateLand_FIFOCAS` and `TestBatchLand_OnePushNDeletes` assert structural properties, not tautologies.

---

**Ship-blockers: F1 (only finding that can crash the daemon) and F3 (silently corrupts the metric this phase exists to gather). F2 is the interesting semantic gap the current tests route around.** Review done; nothing remains.
