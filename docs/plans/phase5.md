# Gauntlet — Phase 5 Implementation Plan: Batching & Speculation

**Status:** planning · **Date:** 2026-07-05 · Generalizes the phase-1 serial
state machine (docs/plans/phase1.md §3, §9) into a per-target *pipeline*. This
is the "Serialize first, speculate later" growth path (DESIGN.md) coming due:
built now, tuned later with the dashboard's queue-depth data.

Serial mode is the default and is **semantically untouched** — every existing
serial test must stay green. Batch and speculate are opt-in per target.

---

## 0. The shape of the change, in one breath

The core generalization is small and uniform:

- Today: `Daemon.runs map[string]*run` — **one** in-flight run per target,
  one candidate, checks stepped one at a time.
- Phase 5: `Daemon.lanes map[string]*lane` — a **list** of in-flight runs per
  target (the pipeline), each run advancing its checks independently. A `run`
  grows from one candidate to a *chain of members* (`[]runMember`).

The three modes are two independent axes over that one structure:

| Mode | Candidates per run | Runs in the lane | CI executions per N candidates |
|---|---|---|---|
| **serial** (default) | 1 | ≤ 1 | N (one at a time) |
| **batch** | up to `max-batch` | ≤ 1 | ⌈N / max-batch⌉ (one suite per batch) |
| **speculate** | 1 | up to `window` | N, but overlapped (pipelined) |

Batch groups candidates *into one run* (one tested tree, one check suite).
Speculation pipelines candidates *across runs* (each its own tree and suite),
chaining each run's base onto the predicted-landed tip of the one before it.
Both build the **same chain of `--no-ff` merge commits** — the only new
mechanic in the whole phase — differing only in how many runs and check suites
sit on top of it.

Everything else (CAS land, per-member slot delete, IsAncestor crash recovery,
park semantics, the one-shot check channel, root spans) is the phase-1
machinery applied per member / per run. No durable in-flight state is added:
a restart re-forms the pipeline from refs, exactly as serial does.

---

## 1. The merge-commit chain (the one genuinely new mechanic)

Both modes rest on building, **before any test runs**, a chain of per-candidate
`--no-ff` merge commits and testing the chain **tip's** tree:

```
target tip ──merge A──▶ M_A ──merge B──▶ M_B ──merge C──▶ M_C   (tested tip)
             (parent[1]=A)  (parent[1]=B)  (parent[1]=C)
```

`M_A = CommitTree(MergeTree(tip, A).Tree, parents=[tip, A])`, then
`M_B = CommitTree(MergeTree(M_A, B).Tree, parents=[M_A, B])`, and so on.
`--first-parent` from `M_C` reads `M_C, M_B, M_A, tip` — **one merge commit per
candidate**, history intact (constraint 1). Landing is a single CAS push of
`target: tip → M_C`; because `M_B` is `parent[0]` of `M_C` (and `M_A` of `M_B`),
pushing the tip *structurally implies* every predecessor's history — you cannot
land C without A and B (constraint 5). Slot deletions stay per-candidate CAS
(constraint 1, Invariant 3).

### 1.1 Existing gitx primitives suffice — verified by spike

Ran against a real bare repo (`git merge-tree --write-tree` / `commit-tree` /
`merge-base --is-ancestor`, git 2.x). Findings:

1. **`commit-tree` persists a merge commit to the object store with no ref
   pointing at it.** The chain lives entirely as loose objects until the land
   push; no `refs/gauntlet/*` scratch namespace is needed.
2. **`merge-tree --write-tree <M_A> <B>` accepts an unpushed commit (`M_A`) as
   the base** and merges correctly — git resolves the commitish from the object
   store regardless of refs. Clean merges return the tree OID; conflicts return
   exit 1 with a tree line + stage lines, **detected identically against a
   chained base** (verified: a candidate conflicting with an earlier chain
   member's change is reported as a conflict against the unpushed `M_A`).
3. **The chain tip's tree is the cumulative tree** (contains every member's
   changes), and `--first-parent` shows exactly one merge per member.
4. **Pushing the tip makes every member and every link an ancestor** — so
   phase-1's `IsAncestor(cand.SHA, tip)` crash-recovery test works unchanged,
   per member.

**Conclusion: no `core.GitRepo` / gitx API change is required.** `MergeTree`,
`CommitTree`, `IsAncestor`, `CASUpdate`, `ExportTree`, `ReadFileFromTree` are
already exactly the surface the chain builder needs. The base argument they
already take is "any commitish"; feeding it an unpushed commit is new only to
the *caller*.

**One caveat to document (not to fix in v1):** chain links are unreferenced
objects. A concurrent `git gc --prune=now` on the daemon's bare repo could
reap them mid-flight. In practice gc never runs unless invoked and the window's
lifetime is seconds; the daemon owns its bare repo. If this ever bites (very
long windows, an ops cron running gc), the mitigation is to write short-lived
`refs/gauntlet/pipeline/<runID>` refs to anchor the chain and delete them on
land/skip — but that adds ref churn and is explicitly **out of scope for v1**.

### 1.2 `buildChainLink` helper

A new helper in `reconcile.go`, used by both refill paths:

```go
// buildChainLink trial-merges cand onto base (base may be the real target tip
// or an unpushed predecessor link), builds the per-candidate --no-ff merge
// commit, and returns it. Errors and conflicts are returned for the caller to
// turn into the mode-appropriate outcome (a conflict aborts a batch or a
// speculation window; it never partially lands). MergeBody is invoked here,
// per candidate, exactly as tryStartTrial does today (constraint 9).
func (d *Daemon) buildChainLink(ctx context.Context, base string, cand core.Candidate, runID string) (link chainLink, trial core.TrialMerge, err error)

type chainLink struct {
	mergeOID string          // the --no-ff merge commit (parent[0]=base, parent[1]=cand.SHA)
	treeOID  string          // trial.TreeOID; checks run against this
	cand     core.Candidate
}
```

---

## 2. State-machine redesign

Per target, per tick, ground truth = post-`Fetch` refs (unchanged). The
per-target driver becomes:

```
reconcileTarget(t, refs):
    targetTip = refs[targetRef]
    cands     = discoverCandidates(t, refs)
    syncBookkeeping(t, cands)                        // §9.1, unchanged
    lane = d.lanes[t.Name]                            // nil ⇒ empty

    concluded = advanceLane(t, targetTip, cands, lane)  // move-checks, check-advance, bubbles, land prefix
    if concluded {                                   // a land/park/skip mutated refs or the lane
        return                                        // defer refill to next tick's fresh Fetch (serial's rule, generalized)
    }
    refillLane(t, targetTip, cands, lane)            // mode-specific; only on a "quiet" tick
```

`concluded` generalizes phase-1's "a run present at tick start claims the whole
tick" rule (reconcile.go's `reconcileTarget` doc): any structural conclusion
this tick (a landing, a park, a suffix invalidation) defers refill to the next
tick, which re-`Fetch`es — so refill never trial-merges against a stale
`targetTip`/`cands` snapshot. Cost: ≤ one idle tick of refill latency per
conclusion, identical to serial's documented cost.

### 2.1 `advanceLane` — walk the pipeline front to back

```
advanceLane(t, targetTip, cands, lane) -> concluded bool:
    // (a) Validity sweep (Invariant 5, generalized — constraint 3).
    for i, r := range lane.runs:
        if runInvalidated(r, i, targetTip, cands):        // see §2.2
            invalidateSuffix(lane, i, reason)             // cancel+Skip run i..end; truncate to lane.runs[:i]
            return true                                   // conclusion: a suffix went away

    // (b) Advance each surviving run's current check (concurrent execution).
    for r := range lane.runs:
        advanceChecks(r)                                  // non-blocking read of r.cur.result; §2.3

    // (c) Bubble: a run that just went red (constraint 2).
    for i, r := range lane.runs:
        if r.verdict == rejected/error:
            finishRun(r, Rejected/Error, park=true)       // the culprit parks (serial semantics)
            invalidateSuffix(lane, i+1, "pipeline bubble") // i+1..end: Skip, NO park, re-queue
            lane.runs = lane.runs[:i]                     // predecessors (0..i-1) survive
            return true

    // (d) Land the contiguous green prefix, FIFO (constraint 5).
    for len(lane.runs) > 0 && lane.runs[0].allGreen():
        landRun(t, lane.runs[0])                          // CAS push + per-member CAS deletes; §2.4
        lane.runs = lane.runs[1:]
        concluded = true
    return concluded
```

Notes:

- **(a) before (b)**: a move must cancel *before* a stale verdict is consumed,
  exactly as serial's `reconcileInFlight` checks moves before reading the
  result channel.
- **(b) concurrent checks**: each run steps *its own* checks sequentially
  (`idx`, one `cur` goroutine — the serial per-run behavior, unchanged), but
  different runs step concurrently across ticks. Batch has one run, so this is
  serial-identical; speculation has up to `window` runs advancing together.
- **(c) bubble**: only the culprit parks. The suffix is *invalidated*
  (OutcomeSkipped, unparked) because its base prediction — "everyone ahead of
  me lands" — is now false. It re-queues and rebuilds next tick on the
  corrected prediction (constraint 4). Predecessors `0..i-1` never depended on
  `i` and keep running toward landing.
- **(d) prefix drain**: only `runs[0]` may land (FIFO). Landing `runs[0]` moves
  the target tip to `runs[0].chainTip`, which is *exactly* `runs[1].baseOID`
  (its predicted base) — so `runs[1]`'s land CAS is valid the instant it too is
  green, with no re-fetch and no rebuild. Draining a whole green prefix in one
  tick is safe because each land uses the runs' own in-memory chained OIDs, not
  the tick's `refs` snapshot; we simply set `concluded` so **refill** (which
  *does* use the snapshot) waits for the next Fetch. (Conservative alternative:
  land one per tick. Rejected — pipeline latency matters and prefix-drain is
  provably CAS-safe.)

### 2.2 `runInvalidated` — the generalized Invariant-5 test

```
runInvalidated(r, laneIndex, targetTip, cands) bool:
    // Any member ref moved or vanished ⇒ this run is invalid.
    for m := range r.members:
        cur, ok := cands[m.cand.Ref]
        if !ok || cur.SHA != m.cand.SHA:
            return true
    // The HEAD run's base is the real target tip; a target move invalidates it.
    // Non-head runs' bases are predicted (a predecessor's chainTip); their
    // validity is transitive — if a predecessor is invalidated, this run is in
    // the truncated suffix anyway, so they don't independently test targetTip.
    if laneIndex == 0 && targetTip != r.baseOID:
        return true
    return false
```

- **Batch** (one multi-member run): if *any* member moves, or the target
  moves, the whole run is invalid → cancel + Skip → the batch **re-forms** next
  tick (constraint 3). A batch is atomic (one tested tree); it cannot partially
  invalidate.
- **Speculate**: if the member of the run at lane index `p` moves, run `p` and
  everything behind it (`p+1..`) go (that member + everything behind it,
  constraint 3); `0..p-1` survive.

### 2.3 `advanceChecks` — one run, one tick

Structurally identical to phase-1 `reconcileInFlight`'s check-advance tail
(reconcile.go:228-257), minus the move/target checks (now hoisted into the
validity sweep, §2.1a) and minus the inline land (now the prefix drain,
§2.1d). It: non-blocking-reads `r.cur.result`; on a result, ends the check
span, appends to the record, emits `EventCheckFinished`; then sets the run's
verdict (`rejected`/`error` short-circuit; `passed`/`skipped` + more → start
next check; + last → mark `allGreen`). It never lands or parks itself — those
are advanceLane's job, so FIFO landing and bubble handling stay centralized.

### 2.4 `landRun` — generalized land (all modes)

```
landRun(t, r):
    // ONE CAS push lands the whole chain (batch: N members; serial/speculate: 1).
    err = CASUpdate(targetRef, r.baseOID, r.chainTip)
    if err: finishRun(r, Skipped, "target moved before land ...")  // Invariant 2, unchanged
            return
    // Per-member slot deletes + per-member terminal event, FIFO (constraints 5, 10).
    for i, m := range r.members:
        delErr = CASUpdate(m.cand.Ref, m.cand.SHA, "")             // Invariant 3, per member
        emit EventLanded with m's OWN RunRecord (BatchID shared; §3.3)
    finalizeRun(r)   // end root span, remove export dir, drop from lane
```

- Serial/speculate: `len(members)==1` → exactly phase-1's `land`, one push one
  delete one event.
- Batch: one push, N deletes, **N `EventLanded` events in member order** — one
  RunRecord per member (§3.3), so hooks fire per candidate in FIFO order
  (constraint 10; the hooks Runner already keys on `EventLanded` + a non-nil
  `Record.MergeSHA`, so N events → N hook runs, each against that member's link
  merge commit's tree).

### 2.5 `refillLane` — mode-specific, quiet-tick-only

```
refillLane(t, targetTip, cands, lane):
    switch t.Mode:
    case serial:
        if len(lane.runs) > 0: return                    // lane busy; exactly serial
        cand, ok := pickNext(t, cands, inFlight=∅)
        if ok: startRun(t, base=targetTip, members=[cand])   // via buildChainLink + recovery/conflict handling

    case batch:
        if len(lane.runs) > 0: return                    // one batch at a time
        members := pickUpTo(t, cands, maxBatch, inFlight=∅)
        if empty: return
        base := targetTip
        for c := range members:
            link, trial := buildChainLink(base, c)       // recovery (IsAncestor) checked on the FIRST member only; see below
            on conflict/err: abortBatch(...)             // §2.6
            base = link.mergeOID
        startRun(t, base=targetTip, chainTip=base, members=links)   // ONE run, ONE check suite over the tip tree

    case speculate:
        base := targetTip
        inFlight := refs already in lane
        if len(lane.runs) > 0: base = lane.runs[last].chainTip      // predicted base = last link
        for len(lane.runs) < window:
            cand, ok := pickNext(t, cands, inFlight)
            if !ok: break                                // queue drained; refill later as candidates arrive
            link, trial := buildChainLink(base, cand)
            on conflict/err: rejectOne(cand, ...); break // this candidate parks; stop extending the window this tick
            r := startRun(t, base=base, chainTip=link.mergeOID, members=[link], predicted=(base != targetTip))
            lane.runs = append(lane.runs, r)
            inFlight[cand.Ref] = true
            base = link.mergeOID                         // next run chains onto this one
```

- `pickNext` / `pickUpTo` generalize `pickHead` (reconcile.go:186): same FIFO
  order (smallest `order`, lexical tie-break), same park-skip, but excluding
  refs already in the lane (`inFlight`) and returning one / up-to-N.
- **IsAncestor recovery** (Invariant 4) runs on the head candidate of a fresh
  refill only (as serial does today): if the next pick is already an ancestor
  of `targetTip`, `recoverLanded` cleans its slot and we retry the pick. For
  batch/speculate, a mid-chain member that's somehow already landed is caught
  the same way once it becomes the head after the prefix ahead of it lands.

### 2.6 Batch red recovery — **recommendation: serial-fallback (v1)**

When a batch run goes red (a check fails on the combined tree), we don't know
*which* member is guilty. Two established strategies (bors precedent):

- **Serial-fallback:** re-queue all members unparked; the *next* refill for
  this target forms them one at a time (serial semantics) until the culprit is
  found and parked, then batching resumes. Simple, one code path (the culprit
  parks via the normal per-run red handling; good members land individually).
- **Bisect:** split the failed set in half, test each half as a sub-batch,
  recurse — culprit in O(log n) rounds, good half lands a round sooner.

**Recommendation: serial-fallback for v1**, because:

1. **Value order (DESIGN.md): clarity/simplicity first.** Bisect needs a
   recursive sub-batch state machine, sub-batch grouping that lives only in
   memory (no ref reflects it), and its own crash-recovery story ("crash
   mid-bisect ⇒ re-form the full set and start over"). Serial-fallback reuses
   the park machinery verbatim.
2. **We are explicitly here to gather data first.** "We will tune with the
   dashboard's queue-depth data." Bisect is a throughput optimization whose
   payoff depends on batch red-rate and size — the exact numbers the dashboard
   will give us. Building it now is tuning ahead of measurement.
3. **The failure is self-healing either way.** A red batch of N parks at most
   N-1 innocents for one extra serial round; a re-push clears any that were
   genuinely fine (they were never parked — only the culprit parks; see below).

**Mechanism.** On batch red, the whole run finishes `Rejected` but we do **not**
park all members (that would freeze innocents). Instead:

- Emit one `EventRejected` naming the batch (BatchID) and the failing check.
- Set a per-target **`batchFallback`** flag (in-memory) that forces the next
  `refill` for this target into serial mode (`max-batch` effectively 1) until a
  landing occurs, then clears. This walks the members one at a time: the guilty
  one parks (normal red park), the innocents land individually. Batching
  resumes automatically on the next landing.

This keeps park semantics honest (only a *proven* red SHA parks) while
recovering the good candidates without a bisector.

Bisect is a documented **growth path** (§9 non-goals): reserve the config knob
`on-batch-red "serial"|"bisect"` (default `serial`); v1 validates it but only
implements `serial`.

---

## 3. Core type & signature changes (actual Go shapes)

All additive or refactor; `queue` still imports only `core` + `obs` + `config`
(Invariant 8 holds).

### 3.1 `internal/queue` — the lane and the grown run

```go
// lane is a target's in-flight pipeline, FIFO. runs[0] is the head (next to
// land). serial and batch hold ≤ 1 run; speculate holds up to Target.Window.
// Reconstructible from refs every tick — no durable state (Invariant 4).
type lane struct {
	runs []*run
}

// Daemon.runs map[string]*run  →  Daemon.lanes map[string]*lane
// (a nil/absent lane is an idle target, exactly as a nil run is today).

// runMember is one candidate within a run and its chain link. len 1 for
// serial/speculate; up to max-batch for batch.
type runMember struct {
	cand     core.Candidate
	mergeOID string           // this member's --no-ff link (parent[1]=cand.SHA verbatim, Invariant 6)
	rec      *core.RunRecord  // per-member terminal record (batch: N records sharing BatchID; §3.3)
}

// run grows: cand→members, mergeOID→chainTip, + batch/predicted metadata.
type run struct {
	target   string
	members  []runMember       // NEW: was a single `cand`
	baseOID  string            // real target tip (serial/batch/head-speculate) OR predicted predecessor chainTip (non-head speculate)
	chainTip string            // NEW: tested merge commit = last member's mergeOID (== members[0].mergeOID for serial)
	predicted bool             // NEW: baseOID is an unpushed predicted commit (speculate, non-head)
	batchID  string            // NEW: "" unless batch; shared across member recs
	runID    string
	dir      string
	checks   []config.Check
	idx      int
	verdict  runVerdict        // NEW: none|green|rejected|error, set by advanceChecks, consumed by advanceLane
	rootCtx  context.Context
	rootSpan trace.Span
	cur      *checkInFlight
}
```

`checkInFlight` is unchanged. The `rec *core.RunRecord` moves onto each
`runMember` (a batch produces N records); serial/speculate have exactly one
member so exactly one record, as today.

### 3.2 `Daemon` field & method touch points

- `runs map[string]*run` → `lanes map[string]*lane`; `New` initializes
  `lanes: make(map[string]*lane)`.
- `reconcileTarget` (reconcile.go:130) rewritten per §2 (dispatch on
  `t.Mode`).
- `reconcileInFlight` splits into `advanceLane` + `advanceChecks` +
  `invalidateSuffix` (§2.1–2.3).
- `tryStartTrial` generalizes into `refillLane` + `startRun` + `buildChainLink`
  (§2.5, §1.2). The recovery / conflict / infra-error handling
  (`rejectPreMerge`, `rejectRun`, `recoverLanded`) is reused verbatim per
  candidate.
- `land` → `landRun` (§2.4). `finishRun` gains no signature change but is
  called per member for terminal events; add `finalizeRun(r)` for the
  once-per-run cleanup (span end, dir removal, lane drop) so a batch doesn't
  end its root span N times.
- `pickHead` → `pickNext` (one, excluding in-flight) + `pickUpTo` (N).
- New in-memory `batchFallback map[string]bool` (§2.6).

### 3.3 `internal/core` — RunRecord & event additions

```go
type RunRecord struct {
	// ...existing fields unchanged...

	// BatchID groups the per-member records of one batch run (empty for
	// serial and speculate). All members of a batch share it; the dashboard
	// and history use it to render "landed together as batch <id>".
	BatchID string

	// Position is this member's 0-based index within its batch (0 for
	// serial/speculate). BatchSize is the batch's member count (1 otherwise).
	Position  int
	BatchSize int

	// Speculated is true iff this run was tested on a *predicted* base
	// (speculation, a non-head window member) rather than the live target
	// tip. Purely informational for the dashboard; the landed commit is the
	// tested commit either way (Invariant 1). Optional — recommend including
	// it so the dashboard can distinguish "landed after a speculative test".
	Speculated bool
}
```

**Decision — per-member RunRecords sharing a BatchID (recommended over a single
shared record).** A batch of N lands N candidates with one CAS push, but
history and the dashboard must stay *per-candidate meaningful* (constraint 10).
So each member gets its own record:

- `Candidate` = that member's candidate.
- `MergeSHA` = that member's **link** merge commit (`runMember.mergeOID`) — a
  real commit whose `--first-parent` row is that candidate's landing, and whose
  tree the per-member hook exports.
- `BaseOID` = that member's link's `parent[0]` (the previous link, or the
  target tip for member 0) — its true base.
- `Checks` = the batch's check results, **duplicated onto every member record.**
  The checks ran once against the chain tip, not per member; attributing them
  to each member is honest in the sense that *the tree containing this member
  was green*, and duplication keeps each history row self-contained (no join
  required to answer "did this land green?"). `BatchID`/`BatchSize`/`Position`
  carry the "tested together" truth for anyone who needs it.
  *(Alternative considered: checks only on the tip member, others reference via
  BatchID. Rejected — forces a join for the commonest history query and makes
  N-1 rows look check-less.)*

No new `EventKind` is needed: batch landings are `EventLanded` (one per member).
`Event` already carries `Record`; consumers that don't know `BatchID` ignore it
(additive, per the "channels ignore unknown fields/kinds" contract).

### 3.4 `internal/queue` snapshot — additive pipeline view (constraint 6)

```go
type TargetSnapshot struct {
	Name      string
	Branch    string
	TargetTip string
	InFlight  *RunSnapshot   // KEPT: now the HEAD run (lane.runs[0]) — back-compat
	Pipeline  []RunSnapshot  // NEW: every in-flight run, head first (nil/empty for idle or serial-idle)
	Waiting   []WaitingEntry
	Parked    []ParkedEntry
}

type RunSnapshot struct {
	Candidate core.Candidate   // KEPT: for a batch, the head member (members[0]) — back-compat
	Members   []core.Candidate // NEW: all members (len 1 for serial/speculate)
	RunID     string
	BaseOID   string
	ChainTip  string           // NEW: was implicitly MergeSHA; keep MergeSHA = ChainTip for compat
	MergeSHA  string           // KEPT (== ChainTip)
	Predicted bool             // NEW (speculation)
	BatchID   string           // NEW
	Done      []core.CheckResult
	Current   *CurrentCheck
	StartedAt time.Time
}
```

`buildTargetSnapshot` fills `InFlight` from `lane.runs[0]` and `Pipeline` from
all `lane.runs`. **UI touch (out of this plan's core, note for the dashboard
chunk):** the dashboard's target view renders `Pipeline` as a stacked list
(head at top), showing each run's members and per-run check progress; old JSON
consumers keep working off `InFlight`. The API/MCP shapes are additive JSON —
add `pipeline` alongside `inFlight`.

---

## 4. Config schema + example

Config stays dumb data (DESIGN.md). Per-target mode + knobs, validated in the
existing `LoadDaemon` style (node-named errors).

### 4.1 `config.Target` additions

```go
type Target struct {
	Name   string `kdl:",arg"`
	Branch string `kdl:"branch"`
	Hooks  []Hook `kdl:"hook,multiple"`

	// Mode selects the queueing discipline. "" defaults to "serial".
	Mode string `kdl:"mode"`               // serial | batch | speculate

	// MaxBatch caps candidates per combined run in batch mode (default 8).
	// Ignored (and rejected if set) unless Mode=="batch".
	MaxBatch int `kdl:"max-batch"`

	// Window is the speculation pipeline depth (fixed; default 4). Ignored
	// (and rejected if set) unless Mode=="speculate".
	Window int `kdl:"window"`

	// OnBatchRed is the batch red-recovery strategy: "serial" (default,
	// v1-implemented) or "bisect" (reserved; validated but not implemented in
	// phase 5). Ignored unless Mode=="batch".
	OnBatchRed string `kdl:"on-batch-red"`
}
```

**Validation (added to `Daemon.validate`):**

- `Mode` ∈ {"", "serial", "batch", "speculate"} — else node-named error.
- `Mode=="batch"`: `MaxBatch` defaults to 8; must be ≥ 1; `Window` must be
  unset. `OnBatchRed` ∈ {"", "serial", "bisect"}; "bisect" accepted by config
  but rejected at daemon construction with "not implemented in phase 5" (so the
  knob is forward-compatible but never silently no-ops).
- `Mode=="speculate"`: `Window` defaults to 4; must be ≥ 1; `MaxBatch` and
  `OnBatchRed` must be unset.
- `Mode` serial/"": `MaxBatch`, `Window`, `OnBatchRed` must all be unset
  (catch config mistakes, matching the strict phase-1 validator).

`MaxBatch==1` / `Window==1` are legal and both degrade to serial behavior (a
one-link chain, a depth-1 pipeline) — useful as test fixtures and safety
valves.

### 4.2 Window sizing — **recommendation: fixed window (v1), governor deferred**

Zuul's governor (start small, grow on green, halve on red) is adaptive state
whose tuning *is the thing we're building the dashboard to inform*. A fixed
window is one integer, trivially reasoned about, and sufficient to start
collecting the queue-depth/red-rate data that would justify a governor.

**Recommendation: fixed `window` for v1.** Reserve the governor as the
documented growth path with these params (validated-but-unimplemented, like
`on-batch-red "bisect"`), so config stays forward-compatible:
`window-start`, `window-max`, `window-halve-on-red` (bool). If the data later
says the window should breathe, the governor slots in behind the same
`refillLane` call with no state-machine change (it only varies the `window`
bound per tick).

### 4.3 Example `gauntlet.kdl`

```kdl
remote "https://github.com/acme/widgets.git"
poll-interval "10s"
committer {
    name "Gauntlet"
    email "gauntlet@ci.acme.example"
}

// Default serial — unchanged from phase 1.
target "main" branch="main"

// Batch: test up to 8 queued candidates as one --no-ff chain; on red, fall
// back to serial until the culprit is found, then resume batching.
target "release" branch="release/v2" {
    mode "batch"
    max-batch 8
    on-batch-red "serial"
}

// Speculate: pipeline 4 candidates, each tested on the predicted landing of
// the ones ahead of it; checks overlap, landings stay strictly FIFO.
target "staging" branch="staging" {
    mode "speculate"
    window 4
}
```

---

## 5. Test strategy (constraint 8)

The testscript harness (`script_test.go`, both TestScriptFake and
TestScriptReal) is the **primary vehicle** for the new semantics; targeted Go
integration tests cover the chain-building/CAS mechanics against real git.
**Every existing serial test and script stays green, unmodified** — serial mode
is byte-for-byte the phase-1 path.

### 5.1 Harness extension: address a specific run in the pipeline

Today's DSL assumes one in-flight run (`currentRunID`, `await-started <check>`).
A pipeline has several concurrent runs, so `await-started` / `release-check`
must name *which* run. Extend the DSL backward-compatibly:

```
await-started [@<selector>] <check>
release-check [@<selector>] <check> <passed|failed|skipped>
```

- `<selector>` = `topic:<topic>` (the run whose head member has that topic) or
  `#<index>` (0-based lane position). **Omitted ⇒ the head run** (`lane.runs[0]`)
  — so all existing scripts, which never pass a selector, keep working
  verbatim.
- Implement by extending `scriptHarness` with `runIDFor(selector) string`
  built over `Daemon.Snapshot().Targets[].Pipeline`; the fake and real harness
  helpers already expose the snapshot.

New assertions:

```
assert-pipeline-depth <target> <n>          // len(lane.runs)
assert-landed-order <target> <topic>...      // EventLanded topics in FIFO order (batch/speculate)
assert-slot-parked-none <target> <user> <topic>  // Skipped, NOT parked (bubble re-queue)
```

`assert-target-is-merge` extends to a chain: add `assert-target-chain <target>
<topic>...` asserting `--first-parent` from the tip is one merge per topic in
order, each merge's `parent[1]` == that candidate's SHA verbatim (Invariant
1/6 for the whole chain — the spike's exact property).

### 5.2 Scenario files (`testdata/script/*.txtar`)

| File | Exercises | Key asserts |
|---|---|---|
| `batch_green_lands_chain.txtar` | 3 candidates, batch, all green | one CI suite runs; `assert-target-chain` (3 merges, first-parent, verbatim parents); 3 `EventLanded`; all 3 slots gone; per-member records share BatchID |
| `batch_red_serial_fallback.txtar` | 3 candidates, batch, tip red; then serial re-forms | batch `EventRejected`; next ticks form single-member runs; the culprit parks, innocents land; batching resumes after a landing |
| `batch_member_repush_reforms.txtar` | member 2 re-pushed mid-check | whole batch Skipped (not parked); re-forms next tick with the new SHA (constraint 3, batch) |
| `speculate_pipeline_fill.txtar` | window 3, 3 candidates, checks overlap | `assert-pipeline-depth 3`; checks for all 3 started before any lands; strict FIFO landing; final chain correct |
| `speculate_bubble.txtar` | window 3, run #1 (middle) fails | #1 parks (Rejected); #2 Skipped-not-parked (bubble); #0 lands; #2 re-queues and rebuilds on corrected base next tick |
| `speculate_member_repush_mid_window.txtar` | window 3, run #1's ref re-pushed | #1 + #2 Skipped (suffix), #0 survives/lands; window refills #1',#2' on new prediction |
| `speculate_head_target_moved.txtar` | direct-push to target during window | head run Skipped (Invariant 5, targetTip≠base); whole window rebuilds on new tip |

Scenarios that are fundamentally about real-git plumbing (a genuine conflict
mid-chain, an actual second process racing the land CAS) run under
`TestScriptReal` only, per the harness's existing carve-out rule.

### 5.3 Targeted Go integration tests (real git, `integrationHarness`)

Chain mechanics and CAS the scripts can't cheaply assert byte-for-byte:

- **`TestChainBuild_TipTreeIsCumulative`** — build a 3-link chain via
  `buildChainLink`; assert the tip tree contains all three members' files and
  `--first-parent` is one merge per member.
- **`TestChainConflictAborts`** — member 2 conflicts with member 1's change;
  assert `buildChainLink` reports conflict against the unpushed link and the
  batch/window aborts without landing anything (Invariant 1: never partial).
- **`TestBatchLand_OnePushNDeletes`** — assert exactly one target CAS push and
  N slot CAS deletes; target tip == chain tip byte-identical (Invariant 1).
- **`TestSpeculateLand_FIFOCAS`** — window 3 all green; assert land order is
  run0,run1,run2 and each land's CAS base == the prior run's chainTip
  (structural FIFO, constraint 5); a forced out-of-order land attempt CAS-fails.
- **`TestBatchCrashRecovery`** — push chain landed, slots not deleted; fresh
  Daemon; assert each member's slot IsAncestor-recovered and cleaned, no
  re-merge (Invariant 4, per member).
- **`TestSpeculateCrashRecovery`** — prefix landed then crash; assert landed
  slots cleaned, remaining window rebuilds on the new tip.
- **`TestBatchMemberRecords_ShareBatchID`** — assert N records, shared BatchID,
  per-member MergeSHA == that member's link, per-member hook fires (via a
  recording hooks Runner).

Everything runs under `go test -race ./...`.

---

## 6. Work breakdown (worktree-parallelizable chunks)

`⚠ CORE` = touches the reconcile core (`reconcile.go`/`daemon.go`/`snapshot.go`);
these must be **serialized** (one worktree at a time, in order) — they share
the state machine and would conflict. Config, DSL, and docs chunks are
file-disjoint and parallel-safe against each other.

| Chunk | Depends on | Files | Acceptance |
|---|---|---|---|
| **P5-A: config** (∥) | — | `internal/config/{daemon,config_test}.go`, `gauntlet.kdl` | `go test ./internal/config/` — parses the 3-mode example; rejects window-on-serial, max-batch-on-speculate, unknown mode, bisect-at-construction; defaults MaxBatch=8/Window=4 |
| **P5-B: core RunRecord + snapshot types** (∥, additive) | — | `internal/core/types.go`, `internal/queue/snapshot.go` (types only) | `go build ./...`; snapshot types compile with `Pipeline`/`Members`/`BatchID`; existing snapshot_test green |
| **P5-C: lane refactor** ⚠ CORE (serialize #1) | P5-B | `reconcile.go`, `daemon.go`, `snapshot.go` | Pure refactor: `runs`→`lanes` with len≤1, `run.members` len==1. **All existing serial tests + scripts green, unchanged.** `go test ./internal/queue/ -race` |
| **P5-D: chain builder + real-git tests** (∥ with C's design, wires after C) | P5-C | `reconcile.go` (`buildChainLink`), new `chain_test.go` | `TestChainBuild_*`, `TestChainConflictAborts` green against real git |
| **P5-E: batch mode** ⚠ CORE (serialize #2) | P5-C, P5-D, P5-A | `reconcile.go`, `daemon.go` | batch scenarios + `TestBatchLand_*`, `TestBatchCrashRecovery`, `TestBatchMemberRecords_*` green; serial still green |
| **P5-F: speculate mode** ⚠ CORE (serialize #3) | P5-C, P5-D, P5-A | `reconcile.go`, `daemon.go` | speculate scenarios + `TestSpeculateLand_FIFOCAS`, `TestSpeculateCrashRecovery` green; serial + batch still green |
| **P5-G: testscript DSL extension** (∥, test-only) | P5-C | `script_test.go` | selector-addressed `await-started`/`release-check`; new asserts; existing scripts green with no selector |
| **P5-H: dashboard/API pipeline view** (∥) | P5-B | `internal/dashboard/*`, MCP/API handlers | renders `Pipeline`; old `inFlight` consumers unaffected |
| **P5-I: docs** (∥) | P5-A | `DESIGN.md` ledger row, `README.md` | mode docs; MergeBody/summary cost caveat (§9-batch); gc caveat (§1.1) |

Serialized core path: **C → E → F** (D wires into E/F; each lands with the full
suite green before the next starts). A, B, G, H, I run in parallel worktrees
around them.

---

## 7. Invariant impact (all 8, per mode)

| # | Invariant | serial | batch | speculate |
|---|---|---|---|---|
| **1** | Land exactly the tested SHA | unchanged | The pushed commit **is** the tested chain tip, byte-identical; every link was built before the one check suite ran, never rebuilt (constraint 2) | Each run's pushed merge commit is the one its checks ran against; if its predicted base didn't hold, the land CAS fails → Skip+rebuild, never a re-merge |
| **2** | CAS everywhere | unchanged | One target CAS push (base→tip); stale → Skip whole batch | Per-run CAS push, chained; stale → Skip that run + suffix |
| **3** | Slot deletion is CAS | unchanged | Per member, N CAS deletes | Per run, one CAS delete |
| **4** | Reconcile idempotent / crash recovery | unchanged | No durable in-flight state; unpushed links are GC-able garbage; post-push crash → per-member IsAncestor recovery cleans slots (§8) | Prefix-landed then crash → landed slots recovered, window rebuilds from live tip |
| **5** | Ref moves detected | unchanged | Any member move OR target move → cancel + Skip → batch re-forms (constraint 3) | Member move at index p → run p + suffix Skipped; predecessors survive (constraint 3). FIFO land order structurally enforced by the CAS chain (constraint 5) |
| **6** | Never rewrite candidate commits | unchanged | Links are **new** merge commits; each candidate is `parent[1]` verbatim; `--first-parent` = one merge per candidate (constraint 1) | Same — each run's link has the candidate verbatim as `parent[1]` |
| **7** | Cache escape hatch | unchanged (clean-build stays per-check) | unchanged | unchanged |
| **8** | Core executor/channel-agnostic | unchanged | Pipeline uses the same interfaces; snapshot & RunRecord additions are additive; `queue` imports only core/obs/config | same |

---

## 8. Crash-recovery walkthrough (no durable in-flight state)

The load-bearing claim: **batch and speculation add no durable state, because
crash recovery for each reduces to phase-1's per-candidate IsAncestor
recovery.** Chain links live only in the object store (and in memory); a
restart re-forms the pipeline from refs.

- **serial** — unchanged (phase-1 §3 "Crash recovery").
- **batch**
  - *Crash before the land push:* nothing landed; the chain's link commits are
    unreferenced loose objects (eventually GC'd). Next tick re-forms the batch
    from refs against the live tip. No corruption (Invariant 2: the push never
    happened).
  - *Crash after the push, before/mid slot deletes:* target tip == chain tip.
    Next tick, for each member ref still present, `IsAncestor(member.SHA, tip)`
    is true → `recoverLanded` CAS-deletes it and emits a recovered
    `EventLanded`. Members recover independently; whichever deletes already
    happened are simply idempotent no-ops (the ref is gone). This is exactly
    serial's between-land-and-delete recovery, run per member — `recoverLanded`
    needs only to be reachable per candidate, which the head-pick + prefix
    logic already provides.
- **speculate**
  - *Crash mid-window (nothing landed):* all predicted links are unreferenced
    garbage; next tick rebuilds the whole window from the live tip. The
    predicted bases are recomputed from scratch — cheap.
  - *Crash after a prefix landed:* target advanced to the last-landed run's
    chainTip. Landed members' slots IsAncestor-recover; the un-landed suffix's
    refs are not ancestors of the new tip, so they rebuild as a fresh window on
    the new tip. FIFO + CAS make any interleaving safe (a duplicate daemon or a
    replayed land CAS-fails cleanly).
  - *Duplicate daemon:* both may build windows; the CAS chain lets exactly one
    land each position, the loser gets `ErrCASStale` and rebuilds — identical
    to serial's duplicate-daemon story, per run.

---

## 9. MergeBody / summary interaction (constraint 9) & non-goals

**MergeBody cost (document, don't fix).** `buildChainLink` invokes
`Config.MergeBody` once per candidate, exactly as `tryStartTrial` does today —
each link's merge message carries *that candidate's* summary (correct for
per-candidate history). Consequences to document:

- **Batch multiplies summary calls per pick:** forming a batch of N makes N
  sequential Claude calls on the reconcile loop *before checks start*, bounding
  a whole-loop stall at up to N × `summarize.timeout` (the existing
  synchronous-summarize stall concern — DESIGN.md watch items — compounded by
  batch size). Acceptable for v1 and **documented in README**; the standing
  fix (async summarize / move it off the reconcile loop) is the same growth
  path already flagged, now with more reason to take it. Recommend operators
  running large `max-batch` either disable summaries or accept the stall.
- **Speculation does not multiply, only pipelines:** one link = one summary
  call per candidate, same total as serial, just issued as the window fills.

**Hooks (constraint 10).** Each landed candidate fires hooks individually in
FIFO order: `landRun` emits N `EventLanded`, each with the member's own
RunRecord (own `MergeSHA` = the member's link). The hooks Runner already
fires once per `EventLanded`-with-`Record`, so this yields N hook runs, one per
candidate, each against that member's link-merge tree — the one-event-per-
landing contract holds unchanged. Hook coalescing across a batch is explicitly
a **hooks-v2 concern, out of scope** (task #10).

**Batch check-spec (a real semantic note).** A batch tests the **chain tip's**
tree, so it reads the tip tree's `.gauntlet.kdl`. If a member modifies the
check spec, the batch tests every member under the tip's (possibly changed)
spec, not each member's own — a departure from "a candidate is tested by its
own definition" (DESIGN.md). **v1 recommendation: use the tip tree's spec
(simplest) and document the caveat.** The stricter option — make a check-spec
modification a *batch boundary* (a candidate that touches the spec starts its
own batch) — is a future refinement; note it, don't build it. Speculation is
unaffected (each run reads its own trial tree's spec, exactly like serial).

**Non-goals (phase 5 does NOT do):**

- **Cross-target batching / speculation** — each target is its own pipeline.
- **Adaptive window governors** beyond the fixed window (Zuul start/grow/halve
  reserved as config knobs, unimplemented — §4.2).
- **Bisect batch recovery** — serial-fallback only; `on-batch-red "bisect"`
  reserved and validated but rejected at construction (§2.6).
- **Priority lanes / reordering** — FIFO only, as phase 1.
- **Combined batch+speculate mode** — one discipline per target.
- **GC-anchoring refs** for in-flight chains (`refs/gauntlet/pipeline/*`) —
  refless v1; documented mitigation only (§1.1).
- **Same-tick refill after a landing** — refill defers one tick to a fresh
  Fetch, preserving the no-stale-snapshot rule (§2); same-tick refill via
  in-memory OIDs is a possible later optimization.
```

---

## 10. Review amendments (authoritative; the design reviewer's rulings)

1. **History must stay statistically honest under batching (schema v4 + batch-aware stats).** Duplicating the batch's check rows onto every member record (§3.3, accepted for self-contained rows) silently corrupts `CheckStats`: a green batch of N counts one suite N times (deflating red-rate); a red batch records N duplicated failures (inflating it). Since queue-tuning data is this phase's stated purpose, that's unacceptable. Amendment: history schema v4 adds `batch_id TEXT NOT NULL DEFAULT ''`, `position INTEGER NOT NULL DEFAULT 0`, `batch_size INTEGER NOT NULL DEFAULT 1` to `runs` (stepwise migration per the established pattern); `CheckStats` counts one suite per distinct `COALESCE(NULLIF(batch_id,''), run_id)`. Dashboard/API may expose BatchID as-is. (New chunk P5-J, parallel-safe, depends on P5-B.)
2. **Batch-red event semantics: per-member `EventSkipped`, no batch-level `EventRejected`.** §2.6's "one EventRejected naming the batch" is wrong in the event vocabulary: Rejected implies a park, and deliberately nothing parks. Amendment: on batch red, emit one `EventSkipped` per member (each with its own record — outcome Skipped, the shared failed checks attached, detail "batch <id> red on check <name>; serializing") and set `batchFallback`. The true `EventRejected` for the culprit arrives from its serial round, keeping park semantics and ghstatus/Slack rendering honest (statuses return to pending on the serial re-trial, exactly as a re-push does).
3. **Spec-change batch boundary is v1, not a future refinement (overrides §9's recommendation).** "A candidate is tested by its own definition" is a KEPT ledger promise; a batch silently testing member 1 under member 3's modified spec is trust-eroding gating semantics. The mechanism is cheap with existing primitives: while chaining member k, `ReadFileFromTree` the spec from the chain tree before and after the link merge — if adding k changed the spec content, k terminates the batch (k is included, tested under its own change; later picks form the next batch). Speculation remains unaffected (per-run own-tree spec).
4. **Window bounds builder concurrency — document it (P5-I).** Each speculative run executes at most one check at a time, so `window` is also the max concurrent check processes/containers on the builder. The config docs must say so (an operator sizing `window` is sizing builder load).
5. **Depth sampler counts lane depth (P5-H).** The cmd sampler's `inFlight` tuple component becomes `len(Pipeline)` (0/1 today), so the queue-depth series — the tuning instrument — reflects pipeline occupancy.
