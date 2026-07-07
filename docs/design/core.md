# The queue core

This is the mechanism-level record of the daemon's queue core — the
per-target reconcile loop in `internal/queue` and the domain vocabulary in
`internal/core` it drives. It assumes the top-level model, the decision
ledger, and the eight invariants from [DESIGN.md](../../DESIGN.md); this
document adds the detail those cover only at altitude.

The core sees three interfaces and nothing concrete: `core.GitRepo`,
`core.Executor`, `core.Channel` (`internal/core/interfaces.go`). The `queue`
package imports only `core`, `obs`, and `config` — which is the whole
mechanism behind the "executor- and channel-agnostic core" invariant.
Everything below is a property of that package, not of any git backend,
executor, or channel.

## Candidate ref grammar

A queue slot is a git branch named `refs/heads/for/<target>/<rest>`. The ref
name is durable identity; the SHA it points at is merely what gets tested
this tick and changes on every re-push. `parseCandidateRef`
(`internal/queue/reconcile.go`) splits `<rest>`:

- Two or more slash-separated segments: the first is `user`, the remainder
  (slashes allowed) is `topic`. `for/main/alice/feat/foo` → target `main`,
  user `alice`, topic `feat/foo`.
- A single segment: `user` is empty (solo setups), `topic` is that segment.
- Anything else — wrong prefix, empty target, no topic, an empty user or
  topic segment — does not parse and is ignored.

Target names may not contain `/` (the config layer enforces this); a
target's underlying *branch* may. A well-formed ref whose target segment
names no configured target is a common misconfiguration (a typo, or a target
retired from config while stale refs linger). Rather than silently dropping
it, the core emits `EventIgnoredRef` once per `(ref, SHA)` — deduped in
`d.ignoredRefs`, which is pruned of vanished refs every tick so it cannot
grow without bound. Malformed refs (those that don't parse at all) are
dropped with no event.

## Run identity

A run ID is `<UTC yyyymmddThhmmssZ>-<seq>-<treeOID[:12]>` (`newRunID`). Three
parts, three jobs:

- The **UTC timestamp** gives uniqueness across restarts with no persisted
  state: the same merge re-tested after a restart mints a new ID because the
  clock moved.
- The **monotonic per-process counter** (`runIDCounter`, package-level, not
  per-`Daemon`) closes the same-second gap: two trials sharing an identical
  trial tree started within one UTC second — a re-push restoring
  previously-tested content, or two daemon instances racing one candidate —
  would otherwise mint identical IDs. The counter is package-level because
  the uniqueness it protects is process-wide: two `Daemon`s in one process
  must not collide either. It matters concretely because the container
  executor derives container names from run IDs.
- The **trial tree OID** content-addresses the ID to exactly what the checks
  test, and stays human-correlatable (`git log --format='%H %T'` ties each
  merge commit to its tree).

The ID is minted from the *trial tree* OID, not the merge commit OID,
deliberately. The merge commit's message must carry a `Gauntlet-Run` trailer
holding the run ID, and a commit OID hashes its own message — so an ID
containing a prefix of the merge OID would be a genuine circular dependency.
The tree OID is known before any commit exists. Commit-to-run correlation is
the trailer's job; run-to-commit is `RunRecord.MergeSHA`'s.

The ID is minted once, at the moment the trial is confirmed clean and before
`EventTrialClean` is emitted, then reused verbatim for every check and every
event of the run. Channels join a run's events by run ID (Slack threading,
GitHub `target_url`), so emitting any run-scoped event without the ID would
break that join for the run's whole life.

**Stability across batch members.** A batch's members share one *check
suite* on the chain tip and therefore one mid-run event stream (the
`EventCheckStarted`/`EventCheckFinished` pair per check is keyed on the
shared `run.runID`). But each member needs a *distinct* terminal
`RunRecord.RunID`, because history's `runs` table is keyed on `run_id` with
`INSERT OR REPLACE` — N members sharing one ID would collapse to one row.
`memberRunID` resolves this: position 0 keeps the bare run ID; position `p>0`
gets `<runID>-m<p>`. `BatchID` stays the bare, shared run ID for every member
— it is the grouping key.

**Stand-in IDs.** Outcomes decided before a clean trial exists (a conflict, a
pre-merge infra error, an `IsAncestor` failure, a crash-recovered landing,
a cancel of a never-started ref) have no trial tree to name the run after, so
they derive the ID from the candidate SHA instead — still minted through
`newRunID` and its counter, so a stand-in can never collide with a real ID.

## The reconcile pass

`ReconcileOnce` is one full, non-blocking sweep over every target. It is
single-threaded and never overlaps itself; the only other goroutines are
per-check executor runs, which report back solely by sending once on a
one-shot buffered channel that the pass reads non-blockingly. There are no
locks. Every test can therefore control exactly when a pass happens and when
each verdict lands.

Ground truth each tick is the post-`Fetch` `ListRefs` snapshot — nothing
durable. In-memory state (`order`, `done`, `lanes`, and friends) is all
reconstructible from refs; losing it to a crash costs at most some re-tests,
never correctness. The pass, in order: `Fetch`; `ListRefs`; seed each
target's park list from history exactly once (below); drain inbound commands;
flag ignored refs; advance each target's state machine; arm the services
reaper once a full sweep has completed; publish a `Snapshot`. On a `Fetch` or
`ListRefs` error the pass returns early, before seeding, draining, or
publishing — the previously published snapshot stays current, its staleness
visible via `Snapshot().At`.

Seeding runs *before* draining commands, not after. A first-tick operator
cancel and a first-tick history seed can name the same ref; whichever writes
`done[target][ref]` last wins, and only the cancel carries the "cancelled by
operator" provenance. Seeding first means a same-tick cancel always wins.

**One lane claims the whole tick.** A target whose lane holds any run at the
start of the tick advances that lane and, if anything *structural* concluded
(a landing, a park, a suffix invalidation), returns without refilling.
Refilling in the same tick would trial-merge against `targetTip`/`cands`
snapshotted at the top of the function — stale the instant a landing mutated
the target and slot refs — and re-test the candidate that just landed.
Deferring the next pick to the following tick's fresh `Fetch`/`ListRefs`
avoids the staleness for at most one idle tick of latency. (Speculate's
window is the one mode that tops up on a quiet tick where runs are still
mid-check; serial and batch hold at most one run and never refill a busy
lane.)

FIFO order is `Daemon.order`: each ref gets a monotonically increasing
sequence number the first time it's seen, tie-broken lexically by ref name.
The head is the smallest-sequence candidate whose current SHA is not parked.

## Trial merge, check spec, and the merge commit

A clean trial merge (`buildChainLink`) produces a tree OID; the check spec is
read straight out of *that trial tree* (`ReadFileFromTree` on
`cfg.CheckSpec`, default `.gauntlet.kdl`), never from a checkout and never
from the target. This is the mechanism behind "a candidate that changes its
checks is tested by its own definition" — the spec versions with the code.
A missing or unparseable spec is `OutcomeRejected` and parks (the author must
fix it; the daemon must not spin).

The tested merge commit is built once, with `CommitTree(tree, [base,
candidateSHA], msg, committer)` — a `--no-ff` two-parent merge whose second
parent is the candidate SHA verbatim. This is the *only* object gauntlet ever
creates; candidate commits are never rewritten. The exact commit that was
tested is the exact commit that lands (see [Crash recovery](#crash-recovery)
for why re-merging is never needed).

The message (`buildMergeMessage`) is a Go `text/template` subject, an
optional blank-line-separated body, then `Gauntlet-Ref` and `Gauntlet-Run`
trailers. The default subject degrades to `Merge <topic>` when user is empty
(rather than a bare `Merge <topic> ()`); a config-supplied template is
rendered exactly as written, since the operator owns it. The body is
`Config.MergeBody`'s return (a Claude-written summary), best-effort by
contract: never retried, an error or empty return is not a failure, and a
landing never blocks on it. Bounding that call with a timeout is the caller's
job — a hung `MergeBody` with no deadline would wedge the whole reconcile
loop.

The exported trial tree carries no `.git` and is created under `Config.WorkDir`
(the OS temp dir when unset) and removed on every terminal transition.
Per-check log files under `Config.LogDir` are the exception: they outlive
their run by design and are never swept by the reconcile loop. The check
environment contract (the `GAUNTLET_*` vars, the result-file protocol that
distinguishes passed/failed/skipped) lives in [docs/checks.md](../checks.md);
knob semantics live in [docs/config.md](../config.md).

## Park semantics

`Daemon.done` is a park list: `target → ref → parkEntry{SHA, Outcome,
Reason, At, RunID}`. A park means "this exact `(ref, SHA)` received a
terminal verdict; do not re-test it." A ref parks on the red family only —
`OutcomeRejected` (a check failed, or a bad check spec), `OutcomeConflict`
(the trial merge itself conflicted), and `OutcomeError` (a daemon-side
failure: executor unreachable, export failure, a service dying mid-run). It
never parks on `OutcomeLanded` or `OutcomeSkipped`.

A park is sticky per `(ref, SHA)`. It clears only when:

- the ref's SHA changes (a re-push) or the ref vanishes — `syncBookkeeping`
  drops the entry;
- a `CommandRetry` clears it explicitly;
- an automatic retry clears it (below);
- the process restarts (parks are in-memory; restart clears them as a side
  effect of holding no durable state).

Crucially, landing some *other* candidate never clears a park. Otherwise one
sleeping author's red branch would re-run and re-ping on every unrelated
landing.

**Why `OutcomeError` parks rather than retries.** An exec-start failure
(`Command[0]` missing or not executable) is a *verdict*, `CheckFailed`, not
`CheckResult.Err` — it is the author's spec bug and must park, not retry
forever. `Err` is reserved for daemon-caused conditions (context
cancellation, executor I/O failure). `OutcomeError` parks the `(ref, SHA)`
like a rejection but reports a distinct event so operators can tell infra
from red. The baseline rule is no unbounded retry loops.

**Auto-retry once on infra-error parks.** Narrowly: an `OutcomeError` park —
and only that, never a red verdict, never a conflict — is automatically
cleared and re-queued exactly once per `(ref, SHA)` when `Config.AutoRetryErrors`
is set (`maybeAutoRetry`, `internal/queue/autoretry.go`). It runs through the
*same* clear-and-emit machinery a human retry drives (`clearParkAndRetry`),
so Slack threading, history's stale-park suppression, and the dashboard all
treat it identically to a human retry — only the event `Detail` tells them
apart. The once-per-SHA budget (`d.autoRetried`) is in-memory, pruned in
lockstep with `done`: a restart re-grants one already-spent retry per
still-parked ref (bounded by restarts, never a loop), and a fresh SHA on the
same ref always gets a fresh budget. This is the connective tissue that makes
builders safely evictable — infra errors that were rare become routine at
scale, and a single automatic retry absorbs them. See DESIGN.md's decision
ledger row "Auto-retry once on infra-error parks."

**Park seeding from history at boot.** `Config.SeedParks`, if set, is
consulted once per target per `Daemon` lifetime (`seedParksOnce`) to pre-seed
`done` from each ref's most recent verdict in history — so a restarted daemon
skips one doomed re-test per still-parked ref. This is the one place history
feeds back into the live queue, and it can never affect correctness: seeds
are filtered to the red family, and then the very next step (`syncBookkeeping`'s
existing SHA-currency check) drops any seed whose ref has since vanished or
moved — exactly as it drops a live park on a re-push. Every seed is
re-validated against the ref's *current* SHA before it is ever trusted. A
stale or missing database costs at most an avoidable re-test; it can never
manufacture a landing or suppress a real one. A seeded ref is not announced
as freshly `EventQueued`, since it was never actually queued just now.

## Command model

Channels produce `core.Command{Kind, Target, Ref}`; the queue applies it. The
entire application surface is `applyCommand` (`internal/queue/command.go`),
drained non-blockingly at the top of each pass — no fan-in goroutine, no
inbox mutex, so command application stays serialized with the rest of the
pass. Unrecognized kinds are ignored, symmetric with channels ignoring event
kinds they don't recognize.

- **`CommandRetry`** clears the park for `(Target, Ref)` at its current SHA,
  if one exists, and emits `EventQueued` plus `EventRetryRequested` so the
  next pick re-tests it. Idempotent: retrying an unparked ref is a silent
  no-op.
- **`CommandCancel`** stops whatever is happening to `(Target, Ref)` and
  parks it at its current SHA exactly like a red verdict (`Detail` "cancelled
  by operator"). A member of an in-flight run: that run is cancelled with the
  same invalidation machinery a ref move uses; serial/speculate park the
  member, batch parks only the named member and re-queues its siblings. A
  merely-waiting ref: parked directly (cancel-before-start). Unknown or
  already-parked: a no-op.

Both commands are addressed by ref with **no SHA**. A delayed command clears
or parks whatever exists at the ref *now*, not the SHA the operator was
looking at. This is benign today — parks are keyed to the current SHA and a
re-push already clears them — but it is a known sharp edge if commands ever
queue for long or gain more destructive kinds. `EventRetryRequested` is
persisted precisely so a crash between a retry and the retried run's own
terminal event can't let boot-time seeding re-read the stale pre-retry
verdict and silently re-park.

**Hook-cancel is out-of-band, not a `core.Command`.** A post-land hook stage
has no candidate ref to name, so it never fit this ref-addressed model. It is
a direct closure wired straight into the dashboard API and MCP, bypassing
`drainCommands`/`applyCommand` entirely. See DESIGN.md's ledger row
"Hook-cancel is out-of-band."

## Event model

`core.EventKind` spans queue lifecycle (`EventQueued`, `EventTrialClean`,
`EventTrialConflict`, `EventCheckStarted`, `EventCheckFinished`,
`EventLanded`, `EventRejected`, `EventSkipped`, `EventError`), plus additive
kinds (`EventIgnoredRef`, the hook events, `EventRetryRequested`). Channels
**must ignore kinds they don't recognize** rather than erroring — new kinds
are always additive.

Emit-site contracts, all partially test-enforced:

- **Terminal events carry a non-nil `Record`.** `EventLanded`,
  `EventRejected`, `EventTrialConflict`, `EventSkipped`, `EventError` each
  carry the finished `*RunRecord`. This holds even on paths where no run
  object ever existed — a crash-recovered landing synthesizes a complete
  record (zero checks, `OutcomeLanded`, `Recovered: true`) rather than
  emitting `EventLanded` with a nil record, because history's SQLite writer
  joins on it.
- **Run-scoped events carry the run ID.** Any event a channel joins by run
  ID (`EventTrialClean` onward) carries it — which is why the ID is minted
  before `EventTrialClean`, not after.
- **`EventCheckFinished` carries the finished `CheckResult`** in `Event.Check`,
  so channels can render a per-check verdict mid-run without waiting for the
  terminal record. `Event.Check` is nil on every other kind; consumers must
  nil-check before dereferencing.

Event shapes are the historically fragile spot: the ID-missing and
`CheckResult`-missing gaps above were each caught late. When events next
grow, extend the contract tests first. See DESIGN.md's "Event shapes are the
soft underbelly" watch item.

## Snapshot and the idle signal

At the end of each successful pass, `buildSnapshot` publishes an immutable
`Snapshot` under an `atomic.Pointer` (`Daemon.Snapshot()`). It is deep-copied
out of reconcile state on the reconcile goroutine — the only goroutine that
mutates that state — so any goroutine may read it without locks. It feeds the
dashboard's live views and history's depth sampler, keeping both ignorant of
the queue's internals.

`Snapshot.IdleSince` is the queue-idleness signal that lets external
automation park a builder (an Azure VM deallocated overnight retains its warm
caches). It is the instant the **queue** most recently became idle — no
waiting candidates and no in-flight pipeline runs across *any* target — held
steady across however many idle ticks follow, and the zero time while busy
(`queueIdle`). Parked candidates don't count: they're dormant, not being
worked on. Any non-idle tick zeroes the tracked instant, so the next idle
stretch gets a fresh one rather than a stale one.

This is **queue** idleness only. A daemon is fully idle only when its
post-land hooks are also quiet, and hooks live outside the `queue` package by
design (the agnostic-core invariant). So the composition happens one layer
up, where both a `queue.Snapshot` and a hook-state snapshot are in hand:
`internal/dashboard/api.go` and `internal/mcp/server.go` each return
`snap.IdleSince` unless the queue is busy *or* some target's hook is running
or backlogged, in which case they return zero. The composed signal surfaces
on `GET /api/v1/status` (`idleSince`, RFC3339, omitted while busy), the MCP
`status` tool, `gauntlet status`, and a muted dashboard footer line — the
absent-while-busy contract is uniform across all four. An external timer
function polls this to deallocate an idle builder, and never a busy one.

## Crash recovery

The daemon holds no durable in-flight state; recovery is rescan-and-heal, and
every mutation is compare-and-swap.

- **Reconcile is idempotent.** In-flight state is `(slot, tested SHA, run
  ID)` and nothing else durable. Restart rescans refs and reattaches or
  reruns; a rerun trial merge is cheap.
- **CAS everywhere, including slot deletion.** The target push is
  `CASUpdate(targetRef, base, chainTip)`; a stale result (a human push, a
  second daemon, a replay) yields `ErrCASStale` → `OutcomeSkipped` → rebuild
  next tick, never corruption. A *failed target push must Skip, never park*,
  even on a non-stale error — a real push can fail ambiguously after taking
  effect server-side, and parking would freeze the slot and block the
  recovery path that exists to heal exactly that ambiguity. Slot deletion is
  equally CAS: `CASUpdate(candRef, testedSHA, "")`; if the author re-pushed
  between land and delete, the delete goes stale and the slot survives at its
  new SHA and re-queues — while the landed commit still holds exactly the
  tested SHA.
- **Ref-moved-mid-test detection.** Every tick re-reads ground truth before
  consuming any check verdict (`runInvalidated`, the validity sweep runs
  before the result read). A member's ref moving or vanishing, or the head
  run's real target tip moving out from under its base, cancels the run
  (context cancel; the executor kills the process group) and re-queues at the
  new SHA — the stale verdict is discarded.
- **Already-landed recovery.** Before starting a trial, the head pick is
  checked with `IsAncestor(cand.SHA, targetTip)`. If the candidate is already
  an ancestor, an earlier run landed it before a crash interrupted slot
  cleanup: the slot is CAS-deleted, no merge or check re-runs, and
  `recoverLanded` synthesizes the terminal record. `MergeSHA` is a
  best-effort `FindLandingMerge` lookup (walking the target's first-parent
  chain for the merge whose second parent is `cand.SHA` exactly) — enrichment
  only; a `""` or failed lookup never aborts recovery, since the candidate is
  already known landed. A duplicate daemon resolves the same way: one push
  wins, the loser gets `ErrCASStale` and rebuilds.

## Observability

A run is a trace. The root span (`obs.StartRun`) starts *before* the trial
merge — so `trial-merge` parents under it rather than orphaning — with
`run.id` and `merge.sha` backfilled onto the same span as each is minted.
Child spans are `trial-merge`, one per `check`, and `land`; goroutine children
parent through `run.rootCtx`. The root ends on the terminal transition with
status from the head member's `RunRecord`, which is the source of truth `obs`
maps to attributes. With no provider installed the tracer is a genuine no-op;
installing the OTLP exporter starts exporting the same spans unchanged.

## Deliberately not built

- **No durable queue state / workflow engine.** The reconcile loop over
  refs-as-ground-truth is the design; a second coordination system would
  duplicate git's refs. SQLite holds history and efficiency state only, never
  correctness state — the one feedback path (park seeding) is re-validated
  against live refs and so cannot affect correctness.
- **No path-filter globs in config.** Conditional execution is the check
  script's job, via the exported environment ([docs/checks.md](../checks.md)).
- **No SHA in commands.** By-ref, no-SHA addressing is accepted with the
  known-benign consequence documented above.
- **`CheckJob.Clean`** (the clean-build cache escape hatch) is reserved and
  always false — architected for, not triggerable, in the core loop itself.
