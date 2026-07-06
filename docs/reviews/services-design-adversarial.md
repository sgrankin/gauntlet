# Adversarial review — docs/plans/services.md (shared services)

Reviewed against DESIGN.md (invariants + ledger), phase1 §9 (Err-vs-verdict,
park semantics), phase5 (lanes/speculation), and ground-truth code:
internal/queue/reconcile.go, internal/executor/container.go,
internal/config/daemon.go, cmd/gauntlet/sweep.go.

Ranked most-severe-first.

---

## FATAL

### F1 — `ensure` at run start blocks the single-threaded reconcile loop for the whole daemon
**§3 (ensure), §9.4 (per-run-union, "leaning").**

The reconcile loop is one goroutine that walks *every* target sequentially
(`ReconcileOnce` → `for _, t := range d.cfg.Targets { d.reconcileTarget }`,
daemon.go:417–425). Checks don't block it: `startCheck` spawns a goroutine
(`go func() { result <- d.exec.RunCheck(...) }`, reconcile.go:606) and
`advanceChecks` polls the result channel *non-blocking* (`select { case res :=
<-r.cur.result: ... default: }`, reconcile.go:519–560). That non-blocking poll
is the only reason a slow check doesn't freeze the daemon.

`ensure` as designed is a **blocking** call: single-flight, create-via-driver,
"poll the ready probe until `ready-timeout`" (default **90s**). §9.4 leans
toward calling it "at run start for the union of all checks' `needs`." Run
start is `startRun`/`refillLane` — which execute *on the reconcile goroutine*.
So a single cold SQL Server (exactly the driver scenario this whole design
exists for) stalls the reconcile loop for up to 90s, blocking **every other
target's reconciliation** — landings, cancels, ref-move detection, park
sweeps, all of it. Under a speculate window (up to 32 runs) all requesting the
same cold key, single-flight funnels them to one 90s creation while the loop
is frozen.

This is the *same* pathology DESIGN.md already flags as a watch item for
`summarize` ("runs synchronously on the reconcile loop … bounds a stall of
*every* target's reconciliation, not just the one being summarized") — but 18×
worse (90s vs a 5s summarize timeout kept deliberately under the poll
interval).

**The design must say:** `ensure` runs *inside the check-execution goroutine*,
never on the reconcile goroutine. Concretely: fold it into the same `go func`
that calls `RunCheck` (or a dedicated ensure-goroutine feeding a ready channel
that `advanceChecks` polls non-blocking, mirroring `checkInFlight`).
Single-flight is then a goroutine-blocking primitive only ever reached from
check goroutines. The doc's "run state machine calls ensure" and "at run start"
language points implementers at exactly the wrong goroutine and must be
rewritten to name the concurrency boundary explicitly, or two implementers will
build it the freezing way.

---

## MAJOR

### M1 — mid-run service death produces RED (park-as-rejected), and §7 does not cover it
**§7 (failure semantics), §3 (probe only at ensure).**

§7 enumerates exactly three infra-becomes-`Err` cases: "can't be created,
doesn't become ready in `ready-timeout`, or fails its probe twice." All three
are **ensure-time**. The design probes *only* at ensure (§3). But ensure
returns an endpoint and the check then runs for minutes/hours in its own
goroutine, holding that endpoint. If the instance dies mid-run (OOM, crash,
another daemon's reaper, host docker restart), the test process gets
connection-refused and exits nonzero → `CheckFailed` → `verdictRejected` →
**park-as-rejected** (reconcile.go:548–549, then OutcomeRejected).

So a service infra failure manufactures a *rejected* verdict on a good
candidate — the strongest possible false negative (rejected reads as "your code
is broken," worse than error). This directly violates the design's own claim
("never a red verdict … must not be parked as rejected because SQL Server was
slow") and the Err-vs-verdict discipline the ledger guards.

**The design should say:** on a red verdict from a check with `needs`, re-probe
the resolved services; if any is now unready/dead, convert the verdict from
`Rejected` to `Err` (OutcomeError, park-as-*error*, retryable) rather than
parking the candidate as rejected. Without a post-check liveness check, "infra
never manufactures a red" is false for the entire duration of every run.

### M2 — key excludes networking mode, but reachability is baked in at instance-create time
**§2 ("key is the hash of the node, full stop") vs §5 (per-executor networking).**

The key derives purely from the service node (image/env/port/probe/name).
§5 gives a *different* network shape per executor: local → publish on
`127.0.0.1:<ephemeral>`; container/docker → attach to `gauntlet-svc-<token>`
net, alias host, **no publish**. The instance's actual reachability is fixed
when it's *created*, by whichever executor created it first — but the key that
decides cache hit/miss ignores the executor entirely. Consequences:

- A cache *hit* can return an endpoint the caller can't reach: instance created
  by a container run (network-attached, unpublished), later reused by a local
  run whose check dials `127.0.0.1` — nothing is published there.
- Adoption across an executor-kind config change (operator switches
  `executor.kind` container→local and restarts) adopts a network-only instance
  whose ready-probe may still pass but which local checks can't reach.

In practice one daemon has one `executor.kind` (config has a single `Executor`
block), so *within a run of one config* the pool is implicitly partitioned by a
daemon-global constant and hits are self-consistent — but the doc never says
that, and it explicitly raises the mixed local/container scenario in the review
prompt as if one instance serves both. **The design must state one of:** (a)
the executor/runtime networking mode is a daemon-global constant, so it's a
key-invariant *and* adoption is invalidated when it changes; or (b) every
instance is created with *all* reachability modes (publish AND network-attach)
so any executor can use any instance. As written, §2's "full stop" and §5's
per-executor divergence contradict.

### M3 — last-used touched only at ensure evicts warm instances right after the run that used them
**§3 (eviction: "Last-used is touched by ensure only").**

Two justifications in §3 are muddled and one is wrong. Claim: a 4h run against
a 2h TTL "is not a problem because the instance was touched when the run's
checks started and TTL enforcement only needs to be approximately right." But
touch-at-start makes last-used *4h stale by run's end* — it's the **refcount**
("never destroys an instance any in-flight run's resolved `needs` still
references"), not the touch, that prevents mid-run eviction. So the
"approximately right" sentence is doing no work and contradicts the refcount
sentence next to it.

The real bug the refcount does *not* fix: the instant a long run concludes,
refcount→0 and last-used is already `run-duration` old. A 4h run leaves an
instance the reaper evicts *seconds later* — even though it was actively serving
1 second ago — because from the reaper's view it's been idle 4h. That **defeats
cross-run reuse for exactly the long suites reuse matters most for**, which is
the design's stated goal. **Fix:** touch last-used at run *end* (or
continuously while refcounted), not just at ensure; and cut the contradictory
"approximately right" justification.

### M4 — service persistence is a beachhead a check isn't; §7 conflates isolation with lifetime
**§7 (gating), §8 (no health/restart).**

§7 argues the `container` driver "keeps them inside the same isolation story as
checks." True for the sandbox; false for *lifetime*. A check runs and dies with
its run. A service ensured by any pushed `for/` ref — **including a red branch
that never lands, or one deliberately crafted to** — leaves an
attacker-chosen `image` running on the builder until `idle-ttl` (default 2h),
and re-touched on every subsequent run so it can persist indefinitely. That is
strictly more than a check can do: a long-lived, self-declared,
arbitrary-image container as a persistent foothold, from anyone who can push a
ref. The allowlist gates *whether* services run at all, but once allowed, every
branch gets persistence. The doc should name this lifetime delta honestly (it's
the real trust change, not the sandbox) and note that `max-instances` +
`idle-ttl` are the *only* bounds on it.

### M5 — spec-only key shares one instance (and one admin account) across repos/targets
**§2 ("Two repos … that declare byte-identical service nodes share an instance; that is a feature").**

DESIGN.md keeps "One daemon, N queues … multiple repos per instance." The key
includes the service *name* but **not repo/target identity**, so two repos with
byte-identical `service "mssql"` nodes share one live SQL Server and one `sa`
account. The harness isolates *databases* per `GAUNTLET_RUN_ID`, but not the
server: repo B's test code (arbitrary, pushable by repo B's authors) runs as
`sa` against a server holding repo A's fixtures — cross-repo read/drop. The
trust boundary is *per-repo* (who can push to A vs B); the key erases it and
§2 frames the collision as a pure feature. At minimum the design must state
that same-key sharing is only safe among targets/repos with a *single* trust
population, and consider folding a repo/remote identity into the key (or
explicitly documenting the shared-`sa` cross-repo exposure as accepted).

---

## MINOR

### m1 — key normalization is underspecified; two implementers diverge
**§2.** "target-independent normalization of the service node" never says
*what* normalization. Env declaration order, whitespace, duplicate keys,
number formatting — raw-KDL-bytes hashing and canonical-struct hashing give
different keys, and it decides whether "byte-identical share" is robust or
brittle (reordering two `env` lines spuriously recycles the instance). Specify
a canonical form (e.g. sort env by key, trim, hash the normalized struct not
the source bytes).

### m2 — keyhash width is inconsistent (`<keyhash>` vs `keyhash8`)
**§3 (names/records use `<keyhash>`) vs §4/adoption prompt (`keyhash8`).**
Full hash vs 8 hex chars (32 bits) is left ambiguous. If record filenames or
network aliases use a truncated `keyhash8`, two distinct specs colliding on the
prefix clobber each other's `<keyhash>.json` record or alias onto one instance.
State the width and that record-matching compares the *full* key from the
snapshot, not the truncated name token.

### m3 — park-as-error on transient cold-start slowness needs a human retry
**§7 ("It parks as error … a retry is cheap").** OutcomeError parks the
(ref, SHA) and phase1 §9.2 shipped *no* auto-retry ("no unbounded retry loops
… backoff/auto-retry is phase 2"). A cold instance that misses `ready-timeout`
by a hair parks the candidate until a human retries or re-pushes. Under a
speculate window, N runs hit the same cold key; single-flight funnels creation
but the *first* creation's ready-timeout gates all of them. "Cheap" understates
the toil unless auto-retry-on-error exists. State who issues the retry.

### m4 — volume/disk lifecycle is absent from the refusal list and will creep back
**§8.** The reaper destroys *containers*; nothing addresses volumes. SQL Server
images declare `VOLUME`, so each instance spawns an anonymous volume; `docker
rm` without `-v` leaks it, and an instance killed between create and record-write
is invisible to the reaper entirely. Over weeks this fills the disk — a classic
operational creep-back §8 should either own (rm with `-v`, prune orphan
`gauntlet-svc-*` volumes) or explicitly refuse *and* document the leak.

### m5 — no service-log capture on ready-probe failure
**§7, §8.** When a service won't become ready, the operator gets "didn't become
ready in ready-timeout" and no `docker logs` of *why*. §8 refuses log capture,
but ready-probe-failure diagnostics are the first thing operators will demand;
name it as a deliberate gap and point at `docker logs <name>` as the manual
recourse, or it creeps back unplanned.

### m6 — adoption-by-name is spoofable on-box (out of threat model, but new surface)
**§3 (adopt by `gauntlet-svc-<token>-<keyhash>`).** Both token (a non-secret
hash of the state-dir path) and keyhash (hash of an in-repo spec) are derivable
from public inputs, so any local process can pre-create a container with the
adoptable name and arbitrary contents; gauntlet adopts it if the ready-probe
passes. Within DESIGN's "own code, not hostile tenants" threat model — but
adoption-by-name is a new *trust-by-name* surface (check-container sweep only
ever *kills*), and the doc should state the assumption explicitly rather than
leave it implied.

---

## QUESTION

### q1 — `max-instances` bounds count, not resources
**§7 ("Memory/CPU limits stay with the driver's runtime defaults in v1").**
Runtime defaults are typically *unlimited*. A single pushed spec (or 8 of them)
with a large image/footprint OOMs the builder; `max-instances 8` doesn't help.
Is that acceptable? At least document that `max-instances` is a fan-out
backstop, *not* a resource bound, and that per-instance mem/cpu is unbounded in
v1.

### q2 — where does the `ready command` execute?
**§1 example (`sqlcmd -S localhost`), §3, §5.** "localhost" implies *inside the
service container* (docker exec), but §3/§5/§6's tiny driver interface never
says. Host? Check container? This is part of the driver contract and the
Endpoint()/probe split; specify it (and note that a host-run probe can't say
`localhost`).

### q3 — refcount coherence across restart
**§3.** The refcount is in-memory and lost on SIGKILL restart (DESIGN: "SIGTERM
gives in-flight checks zero grace"). After boot, adoption + re-derived runs
re-ensure (re-touch), so it heals — *provided* the reaper never runs before
boot adoption and the first reconcile re-ensures in-flight runs. State that
ordering, or a first-tick reaper could evict an instance a just-recovered run
is about to reuse.

---

## Verified sound

- **Pool as pure efficiency state** (JSON hints, not SQLite; "destroy
  everything and the next runs are merely slower"). Consistent with the ledger's
  "SQLite never holds correctness state" and Invariant 4. The live-instance
  listing as truth / records as hints is the right layering.
- **Trial-merged spec keys the instance.** "Tested by its own definition"
  extends to services exactly as to checks; the branch-vs-main coexistence story
  (different keys coexist, old key ages out, nobody migrates a box) is coherent
  and genuinely elegant. No incoherence found in the racing-spec-change case: a
  spec-file merge conflict falls through the existing conflict path, and a
  non-conflicting merge tests the merged spec, which is correct-by-design.
- **Sweep adopt/reap naming split** (`gauntlet-` killed, `gauntlet-svc-`
  adopted). A real, necessary change; correctly flagged as needing to land in
  the *same commit* that first creates an instance. Consistent with the existing
  token-scoped prefix match in cmd/gauntlet/sweep.go (sweepContainerOrphans),
  which already filters `gauntlet-<token>-` — the new `svc` infix slots in
  cleanly and the token-namespacing rationale carries over unchanged.
- **Endpoint contract** (`GAUNTLET_SVC_<NAME>_HOST/PORT`, harness owns per-run
  tenancy via `GAUNTLET_RUN_ID`). Narrow, clean, consistent with the existing
  `GAUNTLET_*` injection in container.go runArgs. Keeping networking-shape
  resolution inside driver `Endpoint()` so it never touches spec/key/harness is
  the right seam.
- **Capability gating as an allowlist, not a boolean**, distinguishing
  `container` (sandboxed) from a future `artifact` driver (native execution).
  Matches the executor-trust model; the "no services block = spec validation
  error" loud-fail is the right call (mirrors config.validate strictness).
- **Ensure-time infra → park-as-error (not rejected)** is consistent with
  phase1 §9.2's OutcomeError treatment (verdictErrored → OutcomeError parks
  distinctly from Rejected). The framing is right *for ensure-time*; M1 is only
  about the mid-run window it omits.
- **Deferring `oci-unpack`, recording it to avoid re-litigation.** Good scope
  discipline; the "vendoring a runtime is how a merge queue wakes up as an
  orchestrator" line is the correct instinct.
- **Secrets-in-repo, scratch-only** is defensible *as stated for a single
  repo*; it only frays at the cross-repo sharing boundary (M5).
