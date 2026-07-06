# Gauntlet — Shared Services Design

**Status:** design, pre-implementation · **Date:** 2026-07-06 · Revised same
day after adversarial review (docs/reviews/services-design-adversarial.md);
§11 records what the review changed. Nothing here is implemented. The driver
was a real repo whose test suite needs SQL Server: per-test container spin-up
is too slow, per-run spin-up (GitHub-Actions-style `services:`) still pays
startup on every run, and operator-provisioned box services couple system
configuration to what branches can test. Design intent, stated by the
operator directly: make the merge-queue CI process *fast* — aggressive
caching and reuse are goals, not accidents.

---

## 0. The shape of the idea, in one breath

**A service instance is a cache entry, not a supervised unit.**

```
ensure(key, spec) → endpoint
```

The key is derived from the service's declaration; anything holding the same
key shares the same live instance — across concurrent runs *and* across runs
over time. Eviction is idle-TTL. A dead or wedged instance is not a health
incident, it's a cache miss: evict, recreate on next request. There are no
restart policies, no upgrade orchestration, no monitoring loop — that absence
is the design. If a service deserves managed uptime, it deserves an owner
that is not the merge queue.

This is the same mental move gauntlet already makes everywhere else: the ref
is the queue slot, the SHA is the tested unit, parks derive from history —
identity comes from content, and durable state is either derivable or purely
an efficiency. The service pool holds no correctness state at all: destroy
every instance and every pool record, and the next runs are merely slower.

## 1. Where services are declared: the repo, not the daemon

Service declarations live in the repo's check-spec file (`check-spec
".gauntlet.kdl"`), alongside the checks that need them:

```kdl
service "mssql" {
    image "ghcr.io/sgrankin/mssql-fts:2022-cu14"
    port 1433
    env "ACCEPT_EULA" "Y"
    env "MSSQL_SA_PASSWORD" "Gauntlet-scratch-pw1"
    ready command "/opt/mssql-tools/bin/sqlcmd" "-S" "localhost" "-U" "sa" "-P" "Gauntlet-scratch-pw1" "-Q" "SELECT 1"
    ready-timeout "90s"
    idle-ttl "2h"
}

check "test" {
    command "go" "test" "./..."
    needs "mssql"
}
```

Because the check spec is read from the **trial-merged tree**, service
declarations get the property that made in-repo check specs valuable in the
first place: a branch that bumps the image tag, adds an env var, or changes
the ready probe is *tested with its own declaration*. The changed spec hashes
to a different key (§2), so the branch's runs get a fresh instance while
main's concurrent runs keep hitting the warm old one. Both coexist in the
pool; when the bump lands, the old key stops being requested and ages out.
Nobody migrates a box.

`needs` is per-check, not per-target: a lint check shouldn't block on (or
keep warm) a database it never touches. A check with no `needs` has no
service dependencies and is wholly unaffected by this design.

**Secrets in the spec, stated honestly:** the SA password above is in the
repo. That is acceptable for what these are — throwaway scratch services
whose entire dataset is generated test fixtures, reachable only from the
builder (§5). A service that needs a *real* secret (one that protects
anything) is by definition not a scratch service and belongs outside this
design. The docs will say exactly that. There is deliberately no env
interpolation from the daemon's environment — that would reintroduce the
daemon-side coupling this design exists to remove.

## 2. Key derivation

```
key = hash(repo remote URL, canonical form of the service node)
```

**The repo is the sharing boundary.** The push-trust boundary is per-repo
(who can push `for/` refs to A is not who can push to B), and every service
instance has exactly one all-powerful account (the `sa` in §1) that the
harness's per-run databases do not and cannot partition. Sharing an instance
across repos would let repo B's pushed test code read and drop repo A's
fixtures as `sa` — so the remote URL participates in the key, full stop.
Targets *within* one repo (`main`, `dev`) share instances: same trust
population, same spec, same cache entry. Cross-repo dedup is the price, and
it is the right price. (Review finding M5; §11.)

**Canonical form, specified** (review m1): the key hashes the *parsed
struct*, never raw KDL bytes — fields in fixed declaration order, `env`
pairs sorted by name, strings length-prefixed, durations normalized to
nanoseconds, absent optional fields hashed as explicit zero values. So
reordering `env` lines or reflowing whitespace does not recycle an instance,
and a future gauntlet that changes a *default* (e.g. default `idle-ttl`)
recycles instances whose specs relied on it — accepted, and correct: the
effective spec changed.

Everything in the node participates: image, env, port, probe, timeouts,
TTL, and the service *name* (so `service "mssql-a"` / `"mssql-b"` with
identical bodies stay distinct when an author wants two instances on
purpose). Deliberate consequences:

- **Tags are keyed as written, not resolved.** `mssql-fts:2022-cu14` keys on
  that string; gauntlet never resolves tags to digests. If the tag is
  repointed upstream, an existing warm instance keeps running the old bytes
  until it ages out — the same semantics as every other image cache in CI.
  Repos that want exact reproducibility use digests; repos that want a
  forced recycle bump a throwaway env var (`env "RECYCLE" "2"`). No knob
  needed: the keying *is* the knob.
- **Key material vs name material:** records and adoption matching use the
  **full** key hash (stored in the pool record and as a container label).
  Instance *names* and network aliases embed only a 12-hex-char truncation
  (`gauntlet-svc-<token>-<keyhash12>`) — names are for humans and prefix
  listing; a truncation collision is resolved by comparing the full key from
  the label at adopt time, never by trusting the name (review m2, m6).

## 3. Lifecycle

The pool is a per-daemon object (the flock guarantees one daemon per state
dir; the state-dir token namespaces instance names on the host, exactly as it
already does for check containers).

### ensure — and which goroutine pays for it

**`ensure` is a blocking call and must never run on the reconcile
goroutine.** The reconcile loop is one goroutine serving every target;
checks only avoid freezing it because `startCheck` runs `RunCheck` in its
own goroutine and `advanceChecks` polls the result channel non-blockingly.
`ensure` — single-flight, driver create, ready-probe poll up to
`ready-timeout` (90s in §1's example) — called from `startRun`/`refillLane`
would stall every target's reconciliation on one cold SQL Server: the
documented `summarize` stall pathology, an order of magnitude worse. (Review
F1 — the one FATAL; §11.)

So: **ensure executes inside the check-execution goroutine**, as a prefix of
the same `go func` that calls `RunCheck` — resolve the check's `needs`
(ensure each, single-flight making overlaps cheap), inject the endpoint env,
then run the check; failures flow back through the existing non-blocking
result channel as `CheckResult.Err`. The reconcile loop never waits on a
service. This also settles the old per-run-vs-per-check open question:
per-check, lazily, in the goroutine that already exists — a cold service
delays only the checks that need it, and `needs`-free checks (lint) proceed
unaffected. No prewarm machinery in v1; single-flight already collapses the
speculate-window thundering herd to one creation.

The ensure algorithm, per key:

1. Registry hit + instance alive + ready probe passes → return endpoint.
2. Miss → single-flight: create via the driver (§6), poll the ready probe
   until `ready-timeout`, write the pool record, return endpoint.
3. Probe failure on a supposedly-live instance → evict (destroy +
   deregister) and fall through to creation, once; a second failure is an
   infra error (§7).

### Adoption at boot, not reaping

Instances are *meant* to survive daemon restarts — reuse across restarts is
half the value. At boot the pool lists live instances by name prefix
(`gauntlet-svc-<token>-`), matches each against pool records **by the full
key in the instance's label** (names are never trusted; §2), re-checks the
recorded reachability mode (§5), and adopts what matches and probes ready;
everything else — unmatchable, unready, mode-mismatched, or beyond TTL — is
destroyed. This forces a sharp naming split from day one:

- `gauntlet-<token>-…` — check containers. Boot sweep **kills** orphans
  (never legitimate across a restart).
- `gauntlet-svc-<token>-…` — service instances. Boot sweep **adopts** them.

The existing orphan sweep must learn this distinction *in the same commit*
that first creates a service instance, or a restart eats the warm pool.

**Reaper/boot ordering** (review q3): the in-memory refcount is lost on
restart, so the reaper is not armed until the first full reconcile pass has
completed — by then, recovered in-flight work has re-ensured (and so
refcounted) everything it still uses. A first-tick reaper racing boot
adoption is thereby impossible by construction, not by luck.

### Pool records

JSON files under `<state>/services/<full-keyhash>.json` (spec snapshot, full
key, endpoint, reachability mode, driver handle, last-used). They are
efficiency state only, in the strict DESIGN.md sense: the boot reconcile
treats the live-instance listing as truth and the records as hints; a
deleted record with a live instance means the instance can't be matched and
is destroyed (slower, never wrong). SQLite is deliberately not involved.

### Eviction

A reaper piggybacked on an existing periodic tick (as ignored-refs pruning
piggybacks on the depth sampler) destroys instances whose idle time exceeds
their `idle-ttl`, where **idle starts when the last in-flight reference
releases**: the pool refcounts resolved `needs` per run (in-memory only),
and **last-used is touched on release, not just on ensure** — a 4-hour run
against a 2h-TTL instance ends with last-used = *now*, so the instance the
long suite just proved it needs stays warm for the next one. (Review M3: the
draft touched at ensure only, which evicted exactly the instances reuse
matters most for, seconds after their longest runs ended.) The reaper never
destroys a refcounted instance regardless of clock. Destroy always removes
the instance's anonymous volumes with it (`rm -v` semantics — review m4);
named-volume leakage can't occur because specs cannot declare volumes.

## 4. What checks see: the endpoint contract

For each resolved `needs`, the executor injects:

```
GAUNTLET_SVC_<NAME>_HOST
GAUNTLET_SVC_<NAME>_PORT
```

(name upcased, non-alphanumerics → `_`). Combined with the already-landed
`GAUNTLET_RUN_ID`, the tenancy contract is explicit and narrow:

- **Gauntlet guarantees:** an instance matching your declaration exists, is
  ready, and is reachable at that endpoint for the duration of your run.
- **The harness owns:** per-test/per-run tenancy *within* the instance —
  `CREATE DATABASE testdb_${GAUNTLET_RUN_ID}_${n}`, cleanup, and concurrency
  safety. This is exactly the division the operator's work project already
  implements against testcontainers-with-reuse, unchanged.

## 5. Networking (the honest hard part)

The service must be reachable from wherever checks run, which differs by
executor and runtime:

- **Local executor:** publish the service port on `127.0.0.1:<ephemeral>`;
  endpoint is `127.0.0.1:<assigned>`.
- **Container executor, docker/podman:** attach service and check containers
  to a shared named network (`gauntlet-svc-<token>`); endpoint host is the
  service's network alias (its keyhash12), port is the declared container
  port. No host publishing; instances are unreachable from off-box by
  construction.
- **Container executor, Apple `container`:** no docker-style user networks.
  Containers get vmnet IPs and can reach the host; the likely shape is
  host-published `127.0.0.1` + checks dialing the vmnet gateway IP — but
  this is **unverified** and is the top livetest item (§9). Fallback: the
  service's vmnet IP as the endpoint host directly.

**Reachability mode is a pool-global invariant, not key material** (review
M2 — the draft's "key is the hash of the node, full stop" contradicted this
section). A daemon has exactly one `executor` block, so every instance it
creates shares one reachability mode; within one daemon's lifetime, hits are
self-consistent by construction. The mode is recorded in the pool record,
and **boot adoption rejects an instance whose recorded mode differs from the
current config** — an operator who switches `executor "container"` to
`"local"` and restarts gets a cold (correct) pool, not a warm instance whose
endpoint nothing can reach. The key stays pure spec identity; reachability
is enforced where it can actually vary, at adoption.

Per-(executor, runtime) resolution otherwise lives entirely inside the
driver's `Endpoint()` — checks only ever see host+port env vars, so a
networking-shape change never touches the spec, the key, or the harness.

**Ports: who allocates, who cares** (operator question, 2026-07-06). The
spec's `port 1433` is the *container-side* listening port and nothing else:
the service always listens on its natural port and never learns anything
about host mapping — `GAUNTLET_SVC_*` env vars go to *checks*, never into
the service's own environment. Conflict avoidance therefore needs no
allocator in gauntlet at all:

- **Network mode** (container executor, docker/podman): no host ports
  exist. Every instance is its own alias on the shared network, all
  listening on their own 1433s; N SQL Servers coexist with zero
  coordination.
- **Publish mode** (local executor; Apple pending §9): instances are
  created with `-p 127.0.0.1:0:1433` — port **0**, so the *kernel* assigns
  a free ephemeral port, which the driver reads back (`docker port`) and
  records in the pool record. Two instances get two kernel-assigned ports;
  a daemon restart adopts instances with their already-assigned ports
  intact. Gauntlet never picks a number, so gauntlet can never pick a
  duplicate.

`GAUNTLET_SVC_<NAME>_PORT` always carries the port *as dialable from the
check* — the container port in network mode, the kernel-assigned host port
in publish mode. Multi-port services (e.g. a broker exposing two listeners)
are out of scope for v1; the recorded evolution, when a real service needs
it, is repeatable named ports (`port "broker" 9092`) surfacing as
`GAUNTLET_SVC_<NAME>_PORT_<PORTNAME>`, with the bare `_PORT` form reserved
for the single-port common case.

## 6. Drivers

One interface, deliberately tiny — create / probe-alive / probe-ready /
destroy (including its volumes) / endpoint / list — mirroring how
`runtimeSpec` already isolates CLI differences.

**Where probes run** (review q2): `ready command` executes *inside* the
service instance (`docker exec` or equivalent) — which is why §1's example
says `localhost` — because the artifact driver (v2) runs it as a host
process against its own endpoint instead, and the spec must not care.
Omitting `ready command` gets the default probe: a TCP dial of the endpoint
by the daemon. Probe-alive (used at adoption and post-check, §7) is
existence + running state, not the ready command.

**On ready-probe failure**, the driver captures the instance's last ~50 log
lines into the daemon log before destroying it (review m5) — "didn't become
ready in 90s" with no `docker logs` is the first support question this
feature would otherwise generate. This is failure-path diagnostics only;
routine service-log capture stays refused (§8).

1. **`container` (v1, the only one initially built):** `docker run -d` (or
   podman/Apple equivalents) with the token+keyhash12 name, a label carrying
   the full key, restart policy `no`. Reuses the existing runtimeSpec
   plumbing and the operator's existing custom images (the mssql+FTS image
   works as-is — images are just the packaging).
2. **`artifact` (v2, when a concrete need lands):** Nomad's good idea
   without Nomad: `url` + `checksum` → fetch into a content-addressed cache
   under `<state>/artifacts/`, unpack, run the declared command as a process
   group with a pidfile in the pool record. For plain-binary services on
   boxes without (or not wanting) a container runtime.
3. **`oci-unpack` (explicitly deferred, maybe forever):** the fly.io move —
   unpack an image's rootfs and run it natively. Attractive for exactly one
   case (a container-packaged service on a runtime-less box) and expensive
   everywhere (userland rootfs semantics, uid mapping, host-lib drift).
   Recorded so it isn't re-litigated from scratch; not scheduled. Worth
   reading Nomad's driver code for ideas before ever building; vendoring a
   runtime is how a merge queue wakes up as an orchestrator.

A note for Linux deployments, so nobody de-containerizes for the wrong
reason: at steady state a *reused* container is a native process with
namespaces around it — the overhead reuse eliminates (spin-up, pulls) is the
overhead that mattered. The artifact driver exists for boxes where a
container runtime is absent, not because containers are slow on Linux.

## 7. Trust and failure semantics

**Capability gating.** The repo spec declares *intent*; the daemon config
gates *capability*:

```kdl
services {
    allow "container"          // drivers permitted on this box; default: none
    max-instances 8
}
```

With no `services` block, `service`/`needs` nodes in a check spec are a
**spec validation error** (loud, like a malformed check — an author must not
believe a service was provided when it silently wasn't).

**The real trust change is lifetime, not sandbox** (review M4). A check
container dies with its run; a service instance ensured by *any* pushed
`for/` ref — including a red branch that never lands, or one crafted for the
purpose — is an author-chosen image that **persists on the builder** until
idle-TTL, and can be kept warm indefinitely by continued pushes. The
`container` driver keeps it inside the same sandbox as checks, but
persistence is strictly more capability than checks grant today, and
`max-instances` + `idle-ttl` are the *only* bounds on it. This is accepted
within DESIGN.md's threat model (your own developers, not hostile tenants) —
the allowlist exists so operators who don't want that class of capability on
a given box never grant it, and a future `artifact` driver (native
execution, no sandbox) stays a separately-granted capability. Likewise
accepted and stated: adoption-by-name+label at boot trusts on-box processes
not to have minted forgeries (review m6) — same threat model, new surface,
named here so it's a decision and not an accident.

**Failure semantics** extend the executor's existing vocabulary, now
covering the *whole run window*, not just ensure-time (review M1 — the
draft's gap):

- **Ensure-time** failure (can't create, not ready in `ready-timeout`,
  probe fails twice) → `CheckResult.Err` → OutcomeError. Park-as-error,
  never rejected: a candidate must not read as "your code is broken" because
  SQL Server was slow to boot. Retry is the existing *human* retry
  (Slack :recycle:, API, CLI) — phase-1 §9.2 deliberately shipped no
  auto-retry, and this design doesn't reopen that; if cold-start
  ready-timeout parks prove toilsome in practice, auto-retry-once for
  service-ensure errors is a recorded phase-B candidate, not v1 (review m3).
- **Mid-run death** — the window the draft missed: ensure returns, the check
  runs for minutes holding the endpoint, the instance dies (OOM, host
  restart), the tests fail with connection-refused, and a *good candidate*
  would park as **rejected**, the strongest false negative there is. So: on
  a **failed** check whose spec had `needs`, the run re-probes (probe-alive)
  the resolved instances before the verdict is finalized; if any is dead,
  the failure converts to `CheckResult.Err` (park-as-error, instance
  evicted), and the check's captured output still lands in the log for the
  skeptical. A passing check never re-probes — nothing to convert.

**Resource honesty** (review q1): `max-instances` bounds *count*, nothing
else. Per-instance memory/CPU is whatever the runtime defaults to —
typically unlimited — so one heavyweight spec can still hurt the builder;
that is documented, not solved, in v1. If it bites, per-service `memory`/
`cpus` passthrough flags are the obvious phase-B shape. Phase B landed
exactly that: optional `memory`/`cpus` fields passed to the runtime verbatim,
joining the cache key like every other spec field.

## 8. What this design refuses to do

- No restart policies, no health monitoring between ensures (probe-alive at
  ensure, adoption, and red-check conversion only), no upgrade orchestration
  (new spec = new key = new instance).
- No per-test tenancy (the harness owns it; §4).
- No daemon-config-declared services (one source of truth, the repo; the
  operator objection that killed the alternative is recorded in §1).
- No image building (`image` is a reference; building the mssql+FTS image
  stays wherever it happens today).
- No spec-declared volumes, and destroy always removes an instance's
  anonymous volumes (§3) — the disk-leak creep-back is closed by
  construction rather than managed (review m4).
- No routine service-log capture — failure-path tail only (§6); operators
  wanting live logs use `docker logs gauntlet-svc-…` by hand (review m5).
- No env interpolation in specs (§1).
- No cross-repo instance sharing (§2) and no cross-state-dir sharing (token
  isolation; two daemons on one box = two pools, same as check containers).

## 9. Open questions / livetest items (blocking implementation, not design)

1. **Apple `container` networking** (§5): can a check container reach a
   host-published 127.0.0.1 port via the vmnet gateway? Livetest before the
   driver's Apple path is written; the docker/podman path is not blocked on
   it.
2. **Adoption probe cost** at boot with N warm instances (serial probes
   could slow a restart; probably fine, measure).
3. **Red-check re-probe cost** (§7): probe-alive per needed service on every
   failed check — cheap (a CLI inspect), but confirm it doesn't extend the
   red path noticeably under batch fallback storms.

## 10. Phasing sketch (not a plan yet)

- **Phase A:** spec parsing + key derivation + pool with container driver
  (docker/podman only) + endpoint env + sweep adopt/reap split + red→Err
  conversion + docs. The operator's mssql repo is the acceptance test.
- **Phase B:** Apple `container` networking path (after livetest), dashboard
  surface (pool contents, last-used, per-key hit counts — the tuning
  instrument, as queue-depth was for speculation), and the recorded
  candidates: auto-retry-once on service-ensure error, per-service resource
  flags.
- **Phase C (unscheduled):** artifact driver, if and when a concrete
  runtime-less box exists.

## 11. What the adversarial review changed (2026-07-06)

Findings archived at docs/reviews/services-design-adversarial.md. Accepted
wholesale; the material changes:

- **F1:** ensure pinned to the check-execution goroutine; reconcile loop
  never blocks on a service. Also resolved the per-run-vs-per-check open
  question (per-check, lazily).
- **M1:** mid-run service death no longer manufactures a rejected park —
  failed checks with `needs` re-probe and convert red→Err on a dead service.
- **M2:** reachability mode = pool-global invariant recorded per instance,
  enforced at adoption; key stays pure spec identity.
- **M3:** last-used touched on release; idle clock starts when the last
  reference drops. The draft's eviction story evicted exactly the instances
  long suites had just proven they need.
- **M4/m6:** the trust section now names lifetime-persistence as the real
  capability change and states the adoption threat-model assumption.
- **M5:** repo remote URL folded into the key; the sharing boundary is the
  repo, matching the push-trust boundary. Cross-repo dedup forfeited
  knowingly.
- **m1/m2:** canonical-struct hashing specified; full-key labels for
  matching, 12-hex names for humans.
- **m3/m4/m5, q1/q2/q3:** human-retry named, volume cleanup by construction,
  failure-path log tail, max-instances honesty, probe execution location,
  reaper armed only after first reconcile pass.
