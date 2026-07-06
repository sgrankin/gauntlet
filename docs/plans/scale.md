# Gauntlet — Scaling Position Paper

**Status:** design thinking, deliberately pre-need · **Date:** 2026-07-06 ·
Nothing here is scheduled. This exists so the scale-out conversation doesn't
restart from zero when load actually arrives, and so the one real
prerequisite (§5) is visible before it becomes urgent.

---

## 0. The decomposition

**The queue cannot scale out, and that is not a limitation.** A merge queue's
product is a total order of landings per target branch — definitionally
serial. The goroutine maintaining it does ref reads and CAS writes;
its cost is indistinguishable from zero. Everything expensive is check
execution. So "how does gauntlet scale" decomposes into:

- the **coordinator** (reconcile loop + refs + verdict authority): never
  needs to scale, and its CAS-over-refs design means it holds almost no
  state a replacement instance couldn't rederive (parks from history,
  everything else from the remote's refs);
- the **builders** (check execution, container runtimes, caches): the only
  thing worth scaling, on three axes below.

What deliberately does NOT exist at any scale: a centralized cache (every
cache — bare clones, module caches, image layers, warm service instances —
is derivable efficiency state, and stale-but-local beats fresh-but-remote
for all of them), and distributed queue state (the remote's refs ARE the
shared state; adding another coordination system would duplicate git,
badly).

## 1. Axis 1 — shard by repo/target (works today)

One daemon per repo. The flock-per-state-dir and token-namespaced container
names already make N daemons on one box safe; N daemons on N boxes is the
same thing with more boxes. No code, no coordination, no shared anything.
This is the answer for "we added three more repos," and it is boring on
purpose.

## 2. Axis 2 — park the builder (Azure deallocation; near-zero code)

Merge-queue load is bursty (work hours). An Azure VM deallocated overnight
retains its disks — every warm cache survives — and bills storage pennies
instead of compute dollars. The shape:

- **Wake:** a parked daemon can't see refs arrive, so the wake signal is
  external and trivial: a timer-driven Azure Function running
  `git ls-remote <remote> 'refs/heads/for/*'` and starting the VM when
  nonempty. Zero gauntlet changes; latency = timer period + VM boot
  (~60-90s), paid once per burst, amortized by every warm cache on disk.
- **Sleep:** wants one small feature — an idle signal. The snapshot already
  knows the answer (no waiting, no in-flight, no hook backlog, across all
  targets); surface it as a `"idleSince"` field on `/api/v1/status` and the
  same Function (or a post-idle hook) deallocates. Landed: `idleSince`
  (RFC3339, omitted while busy) on `GET /api/v1/status`, the MCP `status`
  tool, `gauntlet status`, and a muted line on the dashboard index page —
  queue idleness (`queue.Snapshot.IdleSince`) composed with hook idleness
  (no target's post-land hook running or backlogged) at the
  dashboard/MCP layer, since hooks live outside the queue package.

Caveat named now so nobody trips on it: deallocation mid-run is a crash
(fine — Invariant 4's recovery already handles it; hooks may be
recovery-skipped) — the idle signal exists precisely so the Function never
deallocates a busy builder.

## 3. Axis 3 — remote executors (the real scale-out, when one box saturates)

`core.Executor` is already the seam: a check job is (trial-merge SHA,
command, env) → verdict — a pure function of a commit. The distributed
shape, when a single builder's ceiling is hit:

- Coordinator pushes the trial merge to a scratch ref
  (`refs/gauntlet/trials/<runid>`); a worker fetches it into its own local
  clone (incremental — warm cache again), runs the checks, reports
  (status, output, logs) back over the dashboard's existing HTTP surface
  or a jobs endpoint.
- Correctness never leaves the coordinator: workers hold no refs, make no
  CAS writes, land nothing. A worker is stateless-but-warm — spot/evictable
  by construction, with local caches that make it fast and nothing that
  makes it precious.
- **Useful parallelism is bounded and small:** at most Σ speculate windows
  across targets concurrent suites can ever exist (batch mode reduces the
  need further). This is a "3 builders, maybe 5" design space, not a fleet
  scheduler. Any design that smells like a fleet scheduler is
  over-designed for this problem.
- Services (docs/plans/services.md) compose per-worker: each worker runs
  its own pool with its own local instances — no cross-worker service
  sharing, for the same reason there's no centralized cache.

Trigger for designing this for real: a repo whose speculate window is
genuinely saturating a genuinely large single box. Not before — axis 2
multiplied by axis 1 covers a long way.

## 4. Scale up (the null axis)

Before any of the above: a bigger VM. Checks parallelize internally (test
runners), speculate windows fill more lanes, batch amortizes suites. The
coordinator's cost stays ~zero, so a single box scales as far as its checks
do. Most likely end state for a small org: one healthy box per repo, parked
overnight (axes 1+2), and §3 never built.

## 5. The one real prerequisite: auto-requeue on infra errors

Elastic capacity manufactures infra errors: spot eviction, deallocate-vs-
in-flight races, worker disappearance all surface as `CheckResult.Err` →
`OutcomeError` → parked until a *human* retries. That's tolerable when infra
errors are rare (today) and intolerable when scaling makes them routine.
Auto-requeue-once on error-outcome parks is the connective tissue: it has to
be in place before any axis that makes builders evictable. It is
deliberately narrow (infra errors only, once per `(ref, SHA)`, never red
verdicts) so it doesn't reopen phase 1's "no unbounded retry loops" ruling.
Landed: `auto-retry-errors` (config knob, default **true** — see DESIGN.md's
decision ledger, "Auto-retry once on infra-error parks"), driven through the
same clear-and-emit machinery a human Slack/API/CLI retry already used
(`internal/queue`'s `maybeAutoRetry`/`clearParkAndRetry`). The one real
prerequisite for axes 2 and 3's evictable-builder shape is satisfied.

## 6. What this paper refuses

- No distributed queue state, no leader election, no second coordination
  system beside git's refs.
- No centralized/shared caches of any kind.
- No fleet scheduler; §3's ceiling is a handful of workers.
- No autoscaling on anything but the two signals gauntlet already owns:
  queue depth (sampled, dashboarded) and idleness (snapshot-derivable).
