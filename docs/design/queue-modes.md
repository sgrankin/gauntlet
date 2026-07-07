# Per-target queue modes: serial, batch, speculate

Every target reconciles through the same per-target state machine, but each
picks its own *queueing discipline* via `mode`. Serial is the baseline: one
candidate tested and landed at a time. Batch and speculate are two independent
optimizations layered onto one shared structure — the **lane**. This doc covers
the lane model, the merge-commit chain both modes rest on, and the semantics
(landing, red-recovery, cancellation, crash-recovery) that differ per mode.

Operator knobs (`mode`, `max-batch`, `window`, `on-batch-red`, defaults and
bounds) are documented in [config.md](../config.md#queue-modes) — this doc
explains the mechanism behind them, not their surface. The one-line summary
lives in the [decision ledger](../../DESIGN.md#decision-ledger) row "Batching
and speculation as per-target modes".

## The lane model

The generalization is small and uniform. Instead of one in-flight run per
target, each target owns a **lane** — a FIFO list of in-flight runs:

```go
type lane struct {
    runs []*run   // runs[0] is the head: the next run eligible to land
}
```

Each target's lane lives in `Daemon.lanes map[string]*lane`. A
nil/absent lane, or one with an empty `runs` slice, is an idle target. The
three modes are two orthogonal axes over this one structure:

| Mode | Candidates per run | Runs in the lane | CI suites per N candidates |
|---|---|---|---|
| **serial** | 1 | ≤ 1 | N (one at a time) |
| **batch** | up to `max-batch` | ≤ 1 | ⌈N / max-batch⌉ (one suite per batch) |
| **speculate** | 1 | up to `window` | N, overlapped (pipelined) |

Batch groups candidates *into one run* — one tested tree, one check suite.
Speculate pipelines candidates *across runs* — each its own tree and suite,
chained onto the predicted landing of the one ahead of it. A `run` therefore
holds a *list* of members rather than a single candidate:

```go
type runMember struct {
    cand     core.Candidate
    mergeOID string          // this member's own --no-ff link commit
    rec      *core.RunRecord // per-member terminal record
}

type run struct {
    members   []runMember // len 1 for serial/speculate; up to max-batch for batch
    baseOID   string      // real target tip, OR (non-head speculate) a predicted predecessor chainTip
    chainTip  string      // the tested merge commit == members[len-1].mergeOID
    predicted bool        // baseOID is an unpushed prediction (non-head speculate only)
    batchID   string      // "" unless batch; shared across the run's member records
    verdict   runVerdict  // none | green | rejected | errored
    // ...checks, idx, cur (the currently-running check), spans...
}
```

No durable in-flight state is added. A lane is reconstructible from refs every
tick, exactly as the single serial run was — the only unrederivable field is
`cur` (a running check), whose loss costs at most a re-run, never correctness.

## The merge-commit chain

Both batch and speculate rest on one genuinely new mechanic: building, *before
any check runs*, a chain of per-candidate `--no-ff` merge commits and testing
the chain **tip's** tree.

```
target tip ──merge A──▶ M_A ──merge B──▶ M_B ──merge C──▶ M_C   (tested tip)
             parent[1]=A    parent[1]=B    parent[1]=C
```

Each link is `M = CommitTree(MergeTree(base, cand).Tree, parents=[base, cand])`,
with the next link's `base` set to the previous link's merge commit. `buildChainLink`
(in `reconcile.go`) does exactly this for one candidate: trial-merge, and on a
clean trial build the merge commit. A conflict is signalled as data (`trial.Clean
== false`, zero link, nil error), not an error — the caller decides what a
conflict means for its mode.

Two properties make this work:

- **`--first-parent` from the tip reads one merge per candidate**, history
  intact — each candidate appears verbatim as its link's `parent[1]`, candidate
  commits are never rewritten.
- **Landing is a single CAS push** `target: baseOID → chainTip`. Because each
  link is `parent[0]` of the next, pushing the tip *structurally implies* every
  predecessor's history: you cannot land C without also landing A and B. This is
  what makes FIFO landing order a structural guarantee rather than a policy.

`base` need not be a real ref. `MergeTree`/`CommitTree` resolve any commit-ish
from the object store regardless of refs, so a link can be built onto an
unpushed predecessor link — which is exactly what speculation does. Chain links
live only as loose objects in the daemon's bare repo until the land push; there
is no scratch ref namespace anchoring them (see [Deliberately not built](#deliberately-not-built)).

## The per-target tick

`reconcileTarget` snapshots ground truth once at the top — `targetTip` and the
tick's `cands` — then either advances the lane or refills it:

1. If the lane holds any run, run `advanceLane`. If it **concludes** something
   structural (a landing, a park, or a suffix invalidation), return: refill is
   deferred to the next tick's fresh `Fetch`/`ListRefs`. A landing mutates the
   target and slot refs out from under the snapshot, so immediately reusing that
   stale snapshot to start a new trial would trial-merge against outdated ground
   truth. The cost of deferring is at most one idle tick per conclusion.
2. Otherwise (lane empty, or a "quiet" tick where every run is still
   mid-check), run `refillLane`.

`advanceLane` walks the lane front to back for one tick:

- **(a) Validity sweep** — `runInvalidated` flags any run whose member ref
  moved/vanished, or (head run only) whose `baseOID` no longer equals the live
  target tip. A non-head speculate run's base is a *prediction*, so its validity
  is transitive: if a predecessor invalidates, the suffix behind it is truncated
  anyway. The first invalidation truncates the lane and concludes the tick.
- **(b) Advance checks** — each surviving run steps its own current check once
  (non-blocking). Different runs advance concurrently across ticks; within a run,
  checks are sequential.
- **(c) Bubble** — a run that just went red (see each mode's red-recovery
  section below).
- **(d) Prefix land** — while `runs[0]` is green, land it and pop it. A whole
  green prefix can drain in one tick because each land uses the runs' own
  in-memory chained OIDs, not the snapshot; only refill is deferred.

`landRun` is uniform across modes: one CAS push lands the whole chain, then a
per-member slot delete (CAS `cand.SHA → ""`) plus one terminal event per member,
in FIFO member order. For serial/speculate (`len(members)==1`) this is exactly
one push, one delete, one event.

## Batch

`refillBatch` picks up to `max-batch` queued candidates FIFO and chains them via
`startBatchRun`. `IsAncestor` crash-recovery is checked on the head pick only
(as serial does); a mid-chain member that somehow already landed is caught once
it becomes a future refill's head. The whole batch is one run at lane index 0,
one check suite over the chain tip's tree.

### Chain formation and boundaries

`startBatchRun` chains each picked candidate, advancing the base to each new
link. Chaining stops — *without* failing the whole batch — at the first member
that either conflicts against the chain built so far or hits an infra error
building its link. That member parks via the normal per-candidate path
(park-and-stop, not skip-and-continue: it preserves FIFO, since members after
the parked one are simply left for the next refill). If the *very first*
candidate fails this way, no batch forms at all — byte-for-byte serial's own
reject path.

A member is also a **batch boundary** if its link *changes the check-spec
content*. `specChanged` reads `cfg.CheckSpec` from the chain tree before and
after the member's link merge; if it differs, that member terminates the batch
*after* itself (it is included, tested under its own change; later picks start
the next batch). This exists because a batch tests the chain **tip's** tree, and
therefore the tip tree's check spec. Without the boundary, member 1 could be
tested under member 3's modified spec — silently violating "a candidate is
tested by its own definition". The boundary confines a spec change to a batch it
heads. Speculate is structurally immune (see [why speculate has no boundary](#why-speculate-has-no-spec-change-boundary)).

### Landing: one push, per-member events

`landRun` pushes the tip once (`baseOID → chainTip`), landing all N candidates
atomically, then deletes each member's slot and emits N `EventLanded` in member
order — each carrying that member's own `RunRecord` (its own `MergeSHA` = its
link commit). The hooks runner fires once per `EventLanded`-with-record, so N
landings yield N hook runs, one per candidate, each against that member's own
link-merge tree.

### Member run identity

Each batch member gets its own `RunRecord`, and their `RunID`s must be distinct.
`memberRunID` gives position 0 (the head) the bare batch run ID verbatim, and
every later member a `-mN` suffix. The reason is concrete: history's `runs` table
is keyed on `run_id` with INSERT-OR-REPLACE, so N members sharing one `run_id`
silently clobbered the first N-1 rows with the last — gutting the batch-members
history for both green and red batches. The head keeps the bare ID because the
mid-run check events (one suite, shared across the batch, keyed on `run.runID`)
and the Slack root's tracking key both need a single, real member identity.
`BatchID` stays the bare (unsuffixed) run ID for *every* member — it is the join
key history and Slack use to reassemble "landed together as batch X".

Because the shared check suite's results are duplicated onto every member record
(so each history row is self-contained), history also records
`batch_id`/`position`/`batch_size` columns and counts check statistics per
*distinct batch* (`COALESCE(NULLIF(batch_id,''), run_id)`), not per member.
Otherwise a green batch of N would count its one suite N times, deflating the
measured red-rate that batch tuning depends on.

### Red-recovery: serial fallback

When a batch goes red, we don't know which member is guilty, so **nothing
parks** — parking all members would freeze innocents; parking one would be a
false accusation. `finishBatchRed` instead emits one `EventSkipped` per member
(each with the shared failing checks attached, detail "batch X red on check Y;
serializing") and sets an in-memory `batchFallback[target]` flag. While that
flag is set, `refillLane` routes to `refillSerialOne` — forming members one at a
time — until a landing clears the flag. The guilty member parks via its normal
single-culprit red path in *its own* serial round (its genuine `EventRejected`
comes from there, keeping park semantics honest: only a proven-red SHA ever
parks); the innocents land individually. Batching resumes automatically on the
next landing (`landRun` deletes the flag).

The fallback path deliberately is *not* "a size-1 batch": routing through
`refillSerialOne` (not a one-candidate `startBatchRun`) means a red there takes
the normal single-culprit park, not batch-red's no-park skip. This is why a
batch that happens to form with exactly one member is treated as serial for
red-handling — it degrades to serial behavior byte-for-byte.

### Cancelling a batch member

An operator cancel that names one member of an in-flight batch parks **only that
member** (`OutcomeRejected`, "cancelled by operator"); every sibling Skips
unparked and re-queues, re-batching together on the next refill. Unlike a
genuine batch-red verdict, there is no ambiguity here for a serial fallback to
resolve, so no fallback is triggered. The re-batch happens in the *same*
reconcile pass that drained the cancel: a cancel touches only in-memory
bookkeeping (no git ref moves), so there is no stale-snapshot hazard to defer a
tick for.

## Speculate

`refillSpeculate` tops up the window whenever it has room — including quiet ticks
when the lane already holds runs (the one mode whose refill runs on a non-empty
lane). The first run of an empty lane bases on the live target tip (the head,
`predicted=false`); each subsequent run bases on the previous run's `chainTip` —
an unpushed prediction (`predicted=true`). `pickNext` excludes every ref already
chained into the lane, so one window never holds the same candidate twice.
`IsAncestor` recovery applies only to the head pick of a wholly-empty lane.

### FIFO landings, structurally CAS-enforced

Only `runs[0]` may land. Landing it moves the target tip to `runs[0].chainTip`,
which is *exactly* `runs[1].baseOID` — so `runs[1]`'s land CAS is valid the
instant it too goes green, with no re-fetch and no rebuild. Out-of-order landing
is structurally impossible: a run whose predecessor hasn't landed has a `baseOID`
that isn't the current target tip, so its CAS fails. The green-prefix drain in
`advanceLane` can therefore land several runs in one tick, safely, off their
in-memory chained OIDs.

### Red bubble: only index 0 parks

A red verdict parks the candidate **only at lane index 0**, where the base is the
real target tip and the red is proven against reality. A red at index `i > 0` was
tested against a *predicted* base (a predecessor's own unpushed chainTip) — that
predecessor might itself be at fault, or might never land — so parking there
would be a false-negative park: a candidate that would pass cleanly once retested
against reality, stuck rejected until an operator noticed. Instead, index `i > 0`
Skips unparked ("red on predicted base (behind <head>@<sha>); re-queuing for
retest") and re-forms at the front of a future window. If it is *still* red once
it genuinely reaches index 0 against the real tip, it parks for real then. A
check-time error at `i > 0` is folded into the same no-park branch as a red.

Whichever run bubbles, everything behind it in the lane is invalidated
(`invalidateSuffix`, unparked): its prediction "everyone ahead of me lands" is
now false, so it re-queues and rebuilds next tick on the corrected prediction.
Predecessors never depended on it and keep running toward landing.

### Conflict against a predicted base is a skip, not a park

The same reasoning applies one step earlier, at chain-build time. A candidate's
trial merge can conflict against the predicted base it is chained onto. At the
head (`predicted=false`, real target tip) that conflict parks, exactly as
serial/batch. At a non-head position (`predicted=true`) it does **not** park:
`skipPreMergePredicted` Skips it unparked ("conflicts with in-flight
<topic>@<sha> (predicted base)") and re-queues it. A candidate must never park on
a prediction — only a conflict against the *real* tip is a proven conflict. An
infra error building a link against a predicted base is treated identically (it
says nothing about the candidate against the real tip either). So a window
`[A, B]` where B conflicts with A's prediction, and A then reds at head and never
lands, will re-test B against the real, unmoved tip and land it — rather than
leaving B stuck forever against a base that never came to exist.

### Target-moved and member-repush mid-window

- **Target moved** (a direct push to the target during the window): the head
  run's `baseOID` no longer equals the live tip, so `runInvalidated` flags it;
  the whole window Skips and rebuilds on the new tip next tick.
- **A member re-pushed** at index `p`: that member's ref moved, so run `p` and
  everything behind it Skip; predecessors `0..p-1` survive and keep going. The
  window refills the emptied slots on the corrected prediction.

### Why speculate has no spec-change boundary

Each speculate run is an independent run that reads its own check spec from its
*own* trial tree — so "a candidate is tested by its own definition" already holds
per-member, with no extra bookkeeping. Only *batch* chains multiple candidates
through one shared suite on the chain tip, which is the exact sharing the
[batch boundary](#chain-formation-and-boundaries) guards against. Speculate was
never structurally exposed to the hazard. (See the ledger row "Speculate has no
spec-change boundary".)

### Window as a builder-concurrency bound

Each speculative run executes at most one check at a time, so `window` is also
the maximum number of concurrent check processes/containers the target drives
against the executor. Sizing `window` is sizing builder load, not just queue
depth — see [config.md](../config.md#queue-modes).

## Cancellation semantics per mode

An operator cancel of an in-flight ref cancels the run and then, per mode:

- **serial / speculate** (`len(members)==1`): the run's sole member parks at its
  current SHA (`OutcomeRejected`, "cancelled by operator"), via the normal park
  path.
- **batch** (`len(members)>1`): only the named member parks; every sibling Skips
  unparked and re-batches (see [above](#cancelling-a-batch-member)).

Either way, any run *behind* the cancelled one in a speculation window is
invalidated and re-queued. A cancel of a ref that is merely *waiting* (queued,
not in-flight) parks it directly. Because a cancel never moves a git ref, the
lane's freed room is refilled in the same reconcile pass.

## Crash recovery adds no durable state

Batch and speculate reduce to serial's per-candidate `IsAncestor` recovery;
chain links live only in the object store and in memory.

- **Batch, crash before the land push**: nothing landed; the links are
  unreferenced loose objects. Next tick re-forms the batch from refs against the
  live tip.
- **Batch, crash after the push, mid slot-deletes**: the target tip equals the
  chain tip. For each member ref still present, `IsAncestor(member.SHA, tip)`
  holds → `recoverLanded` deletes the slot and emits a recovered `EventLanded`.
  Members recover independently. Their synthesized records carry zero-valued
  `BatchID`/`Position`/`BatchSize`, so a crash in this window surfaces as N
  serial-shaped landings, not one batch summary — correct for recovery, but the
  batch grouping is not reconstructed.
- **Speculate, crash mid-window**: all predicted links are garbage; the whole
  window rebuilds from the live tip.
- **Speculate, crash after a prefix landed**: landed slots `IsAncestor`-recover;
  the un-landed suffix rebuilds as a fresh window on the new tip.
- **Duplicate daemon** (either mode): both may build; the CAS chain lets exactly
  one land each position, and the loser rebuilds — identical to serial's
  duplicate-daemon story, per run.

## Snapshot and pipeline view

`TargetSnapshot.InFlight` keeps pointing at the head run (`runs[0]`) for
back-compatible consumers; `Pipeline []RunSnapshot` (head first) exposes the full
lane. `RunSnapshot` gains `Members`, `ChainTip`, `Predicted`, and `BatchID` — all
additive JSON alongside the existing `inFlight`. The queue-depth sampler counts
`len(Pipeline)`, so the depth series (the tuning instrument) reflects pipeline
occupancy rather than a 0/1 busy flag.

## Merge-body cost

`buildChainLink` invokes `Config.MergeBody` (the Claude-written commit-body
summary) once per candidate, before checks start — each link's message carries
that candidate's own summary. Batch multiplies this per pick: forming a batch of
N makes N summary calls. To keep the reconcile loop's stall bounded they are
precomputed concurrently, capped at 4 in flight, so the stall is roughly
⌈N/4⌉ × timeout rather than N × timeout. Speculate does not multiply — one call
per candidate, the same total as serial, just issued as the window fills.

## Deliberately not built

These are reserved config surface, validated but rejected at daemon construction,
so config stays forward-compatible without ever silently no-opping:

- **Bisect batch red-recovery** (`on-batch-red "bisect"`): splitting a failed set
  and recursing to find the culprit in fewer rounds. Only serial fallback is
  implemented; bisect needs a recursive sub-batch state machine and its own
  crash-recovery story, and its payoff depends on batch red-rate and size — the
  exact numbers the dashboard's queue-depth data is being collected to inform.
  Building it now is tuning ahead of measurement.
- **Adaptive window governor** (`window-start`, `window-max`,
  `window-halve-on-red`): a window that starts small, grows on green, and halves
  on red. Only the fixed `window` is implemented. The governor would slot in
  behind the same `refillLane` call with no state-machine change (it only varies
  the per-tick window bound), so deferring it costs nothing structurally.

Also out of scope by design: cross-target batching or speculation (each target is
its own pipeline), combined batch+speculate on one target (one discipline each),
priority lanes or reordering (FIFO only), GC-anchoring refs for in-flight chains
(refless — links are short-lived loose objects the daemon owns), and same-tick
refill after a landing (deferred one tick to preserve the no-stale-snapshot
rule).
