# Shared services

The shared-services pool lets a check declare a dependency on a long-lived
side process — a database, a broker, anything a test suite needs running —
and have gauntlet cache one live instance across every run that shares the
same declaration. It exists for one reason: a merge queue that respins a
per-run SQL Server on every candidate pays that startup cost forever, and a
box-provisioned service couples system configuration to what branches are
allowed to test. Caching a warm instance and sharing it removes both.

Operator- and author-facing reference — the declaration grammar, the daemon
capability block, and sizing guidance — lives in
[checks.md's "Shared services"](../checks.md#shared-services) and
[config.md](../config.md). This document is the design record: the model, the
invariants, and the rationale behind them.

## The model: a cache entry, not a supervised unit

A service instance is a cache entry keyed by a hash of its declaration.
Anything holding the same key shares the same live instance — across
concurrent runs and across runs over time. There are no restart policies, no
health monitoring between requests, no upgrade orchestration. That absence is
deliberate. A dead or wedged instance is not a health incident; it is a cache
miss — evict it, recreate it on the next request.

The pool holds no correctness state. Destroy every instance and every on-disk
record and the next runs are merely slower, never wrong. This is the same
move gauntlet makes everywhere: identity comes from content, and durable
state is either derivable or purely an efficiency. If a service deserves
managed uptime, it deserves an owner that is not the merge queue.

Two sources of authority meet here. The **repo declares intent** — service
nodes live in the check spec (`.gauntlet.kdl`), read from the trial-merged
tree, so a branch that bumps an image tag or changes a ready probe is tested
against its own declaration. The **daemon config gates capability** — a
`services` block with an `allow` list is what permits any service to run on a
given box at all. With no such block, a spec that declares a `service` or a
check `needs` is rejected loudly at parse time, exactly like a malformed
check: an author must never believe a dependency was provided when it
silently was not.

## Instance keying

```
key = SHA-256(remote URL, canonical encoding of the service node)
```

The key hashes the **parsed, defaulted struct**, never the raw KDL bytes:
reordering `env` lines or reflowing whitespace never recycles an instance.
The encoding is fixed and total — every field length-prefixed or
count-prefixed so no two distinct field sequences can collide on the same
byte stream, `env` pairs sorted by name, durations as nanoseconds, defaults
folded in at their effective value before hashing. See
`internal/config/servicekey.go` for the exact byte layout.

Every field participates, including the service *name* — so `service
"mssql-a"` and `"mssql-b"` with identical bodies stay distinct when an author
wants two instances on purpose. Consequences that follow directly:

- **Tags are keyed as written, not resolved.** `mssql-fts:2022-cu14` keys on
  that literal string; gauntlet never resolves tags to digests. If the tag is
  repointed upstream, a warm instance keeps running the old bytes until it
  ages out — the same semantics as every other image cache in CI. Repos
  wanting exact reproducibility use digests; repos wanting a forced recycle
  bump a throwaway env var. The keying is the knob.
- **A branch's changed spec hashes to a new key.** The branch's runs get a
  fresh instance while other targets' concurrent runs keep hitting the warm
  old one. Both coexist; when the change lands, the old key stops being
  requested and ages out. Nobody migrates a box.

**The remote URL is the sharing boundary.** Targets within one repo (`main`,
`dev`) share instances — same push-trust population, same spec, same cache
entry. Cross-repo sharing is forfeited deliberately: every instance has one
all-powerful account (the `sa` password sits in the repo spec, acceptable
only because these are throwaway scratch services), and the per-run tenancy
the harness builds on top cannot partition it. Sharing across repos would let
one repo's pushed test code read or drop another's fixtures as that superuser.
So the remote joins the key, full stop. Cross-repo dedup is the price, and it
is the right price.

**Full key versus name.** Records and adoption matching use the **full**
64-hex key, stored both in the on-disk record and as a container label
(`dev.gauntlet.service-key`). Instance names and network aliases embed only a
12-hex truncation (`gauntlet-svc-<token>-<keyhash12>`) — names are for humans
and for prefix listing. A truncation collision is resolved by comparing the
full key read back from the label at adoption time, never by trusting the
name.

**Adding a field to the canonical encoding recycles the pool once.** Because
the key is baked into each instance's label and record at creation, a new
binary that extends the encoding computes a different key for every spec, old
or new. Existing instances are not destroyed — adoption still finds their
label and record agreeing — but nothing ever asks for their old key again, so
they age out via idle-TTL while every check gets a fresh instance. The net
effect is a one-time, slower-but-correct full recycle after such an upgrade,
never a wrong answer.

## Lifecycle: ensure, release, reap

The pool is a per-daemon object (the state-dir flock guarantees one daemon
per state directory; the state-dir token namespaces instance names on the
host, exactly as it does for check containers).

**`EnsureAll` runs on the check-execution goroutine, never the reconcile
loop.** This is the load-bearing structural rule. The reconcile loop is one
goroutine serving every target; it stays responsive only because checks run
in their own goroutines and their results are polled non-blockingly. `ensure`
is a blocking call — single-flight create plus a ready-poll of up to the
spec's ready-timeout — so calling it from the reconcile loop would stall
*every* target's reconciliation on one cold SQL Server. Instead it runs as a
prefix of the same goroutine that executes the check: resolve the check's
`needs`, inject the endpoint env, run the check, release on the way out.
A cold service delays only the checks that need it; `needs`-free checks and
hooks proceed untouched. The reconcile loop and the result-polling path are
behaviorally unchanged, because an ensure failure flows back as an ordinary
`CheckResult.Err` and the existing error-verdict machinery does the rest.

The ensure algorithm, per key:

1. Registry hit, instance alive, ready probe passes → return the endpoint
   (and count a warm hit).
2. Miss → single-flight create via the driver, poll the ready probe until the
   ready-timeout, write the record, return the endpoint.
3. A supposedly-live instance that fails its probe is evicted and falls
   through to creation, once; a second failure is an infra error.

**Single-flight** is a homegrown per-key map guarded by the pool mutex — N
concurrent resolutions of one key collapse to a single create, which is what
tames the thundering herd when many candidates speculate at once. Every
caller, leader or piggybacker, gets its own refcount increment; the refcount
is not touched inside the shared create, so a coalesced hit still counts as N
references for N callers.

**Refcounts and the idle clock.** Each resolved `needs` increments the
instance's refcount; `Release` decrements it. Crucially, **last-used is
touched on release, not on ensure.** A four-hour run against a two-hour-TTL
instance ends with last-used = now, so the instance a long suite just proved
it needs stays warm for the next run. Touching only at ensure would evict
exactly the instances reuse matters most for, seconds after their longest
runs ended.

**The reaper** piggybacks on a dedicated periodic tick and destroys instances
whose idle time (now minus last-used) exceeds their idle-TTL *and* whose
refcount is zero — a referenced instance is never reaped regardless of the
clock. Destroy always removes the instance's anonymous volumes with it; specs
cannot declare named volumes, so nothing else can leak. The reaper is not
armed until the queue's first full reconcile pass completes: the in-memory
refcount is lost across a restart, so arming later guarantees that any
recovered in-flight work has already re-ensured (and refcounted) everything
it still uses. A first-tick reaper racing boot adoption is impossible by
construction, not by luck.

**Eviction order.** Eviction removes the key from the in-memory registry
(instance, refcount, last-used, created-at), then destroys the container and
its record on a detached cleanup context. It is triggered from three places:
a failed liveness or ready probe during ensure, a mid-run death re-probe, and
adoption rejecting an unmatchable instance.

**The hit counter** records how often a key resolved by warm reuse rather
than a create — one increment per alive-and-ready reuse, not per `EnsureAll`
call, since single-flight piggybackers never reach the reuse path. It is the
"is reuse actually happening" signal for the dashboard/API/MCP tuning
surface: a key with refcount zero and zero hits has never once saved a cold
create. The counter survives an evict-and-recreate cycle (the key is spec
identity, so the cumulative count stays meaningful) but is dropped when the
reaper retires a key for good — otherwise every abandoned key, including
every pre-upgrade key after an encoding change, would hold an entry for the
daemon's lifetime.

## Boot adoption

Instances are meant to survive daemon restarts — reuse across restarts is
half the value. At boot the pool lists live instances by name prefix, matches
each against a pool record **by the full key in the instance's container
label** (names are never trusted), re-checks the recorded reachability mode
against current config, and probes ready. What matches and probes ready is
adopted; everything else — unmatchable, unready, mode-mismatched, or beyond
its idle-TTL — is destroyed.

This forces a sharp naming split. Check containers are `gauntlet-<token>-…`
and the boot orphan sweep **kills** them (a check container is never
legitimate across a restart). Service instances are `gauntlet-svc-<token>-…`
and the sweep **adopts** them. The check-orphan sweep carries an explicit
guard skipping the `gauntlet-svc-` prefix, so a future naming change cannot
silently regress the adopt path into a kill.

Adoption trusts on-box process names and labels not to have been forged by
something already running as the daemon's own user. This is stated plainly as
a decision, not left as an accident: it sits inside the same threat model as
the rest of the system — your own developers, not hostile tenants — where the
capability that matters is *lifetime*, not sandbox escape. A service instance
ensured by any pushed `for/` ref, including a red branch that never lands,
persists on the builder until its idle-TTL and can be kept warm indefinitely
by continued pushes. `max-instances` and `idle-TTL` are the only bounds on
it. The `allow` list exists precisely so an operator who does not want that
class of capability on a given box never grants it.

## Reachability mode

A daemon has exactly one executor block, so every instance it creates shares
one reachability shape. **Reachability mode is therefore a pool-global
invariant, not key material.** The key stays pure spec identity; reachability
is enforced where it can actually vary — at boot adoption, which rejects an
instance whose recorded mode differs from the current config. An operator who
switches `executor "container"` to `"local"` and restarts gets a cold,
correct pool, not a warm instance whose endpoint nothing can reach.

Two modes exist:

- **Network mode** (container executor, docker/podman): service and check
  containers attach to one shared user-defined network per daemon
  (`gauntlet-svc-<token>`); each service is an alias (its keyhash12) on that
  network. The endpoint host is the alias, the port is the container's own
  declared port. Nothing is host-published, so instances are unreachable from
  off-box by construction. N services coexist as N aliases, all listening on
  their natural ports, with zero coordination.
- **Publish mode** (local executor): the instance is created with `-p
  127.0.0.1:0:<port>` — port zero, so the *kernel* assigns a free ephemeral
  port, which the driver reads back and records. Gauntlet never picks a
  number, so it can never pick a duplicate; a restart adopts instances with
  their already-assigned ports intact.

Per-mode resolution lives entirely inside the driver's endpoint logic. Checks
only ever see host and port env vars, so a networking-shape change never
touches the spec, the key, or the harness.

**Apple's `container` runtime is unsupported for services.** It has no
docker-style user networks, and no verified equivalent for reaching a service
from a sibling check container, so the pool refuses to construct with that
runtime and cmd wiring hard-fails at boot. The same sentence is quoted in
both error strings. The **runtime selection rule** at cmd wiring: when the
executor kind is `container`, the executor's own runtime wins and must be
docker or podman; when it is `local` (which has no runtime of its own), the
`services` block's `runtime` field supplies it (default docker). A
`services.runtime` that conflicts with the executor's runtime is a config
validation error.

## Readiness probing

A service declares an optional `ready-command`. When present, it executes
**inside the instance** (via `exec`), which is why a probe command addresses
`localhost` — the daemon holds no direct route onto the container network, and
a future host-process driver would run the same command against its own
endpoint, so the spec must not care where the probe runs.

When `ready-command` is absent, a default probe applies, and its shape
depends on the mode. In publish mode it is a genuine host-side TCP dial of
the resolved `127.0.0.1:<port>`. In network mode the daemon is not on the
service network and literally cannot dial the alias, so the default probe
instead execs into the instance and checks `/proc/net/tcp[6]` for a LISTEN
entry on the declared port — equivalent in spirit to "alive and listening,"
chosen over reusing the liveness probe so that a running-but-not-yet-listening
service still parks as not-ready. **Caveat:** because the network-mode default
probe runs a command inside the container, a distroless or shell-less image
needs an explicit `ready-command` that names some binary the image does
carry.

The ready-timeout bounds how long the pool waits for readiness; it is
measured against real wall-clock time, deliberately not the injectable test
clock that drives idle/reap bookkeeping. On a ready-probe failure the driver
captures the instance's last ~50 log lines into the daemon log before
destroying it — "didn't become ready in time" with no captured output is the
first support question this feature would otherwise generate. That log tail is
failure-path diagnostics only; routine service-log capture is refused
(operators wanting live logs run `docker logs` by hand).

## Failure semantics

Service failures extend the executor's existing outcome vocabulary and cover
the whole run window, never just ensure-time. **A service problem is always an
infra error (`OutcomeError`, park-as-error), never a red verdict.** A
candidate must not read as "your code is broken" because SQL Server was slow
to boot or died mid-suite.

- **Ensure-time failure** — cannot create, not ready within the ready-timeout,
  probe fails twice, or `max-instances` reached on a miss — returns as
  `CheckResult.Err` and parks the run as error. Retry is the existing human
  retry path; there is also an opt-in auto-retry-once for infra-error parks
  (see [DESIGN.md's decision ledger](../../DESIGN.md), "Auto-retry once on
  infra-error parks"), which treats a service-ensure park like any other
  `OutcomeError`.
- **Mid-run death** — ensure returns, the check runs for minutes holding the
  endpoint, and the instance dies (OOM, host restart). The tests fail with
  connection-refused, and a *good candidate* would otherwise park as
  **rejected**, the strongest false negative there is. So on a failed check
  whose spec had `needs`, the run re-probes the resolved instances' liveness
  before finalizing the verdict; if any is dead, the failure is rewritten to
  an infra error and the dead instance evicted. The check's captured output is
  preserved for the skeptical. A *passing* check never re-probes — there is
  nothing to convert — and the liveness re-probe checks existence-and-running,
  not readiness, so a genuine red whose service is fine is never inverted into
  a spurious error.

**Canceled-context cleanup.** Teardown after a failed or canceled ensure runs
on a context detached from the caller's cancellation (still time-bounded). If
the caller's context is exactly what just got canceled — a superseded run
canceling mid-poll — a command started on that already-canceled context would
never launch the subprocess, so the destroy would silently no-op and leak a
half-created container under its deterministic name. That leak would then wedge
the key: every future ensure fails "name already in use" until the next
restart's adoption cleans it up. Detaching the cleanup context is what keeps a
canceled ensure from wedging its key.

## The endpoint contract

For each resolved `needs`, the executor injects two environment variables:

```
GAUNTLET_SVC_<NAME>_HOST
GAUNTLET_SVC_<NAME>_PORT
```

`<NAME>` is the service name upcased with every non-alphanumeric rune replaced
by `_`. Two distinct service names that mangle to the same variable (e.g.
`my-db` and `my_db`) are rejected at spec validation, because the executor's
env is last-wins and one service would otherwise silently shadow the other's
endpoint. The `_PORT` value always carries the port *as dialable from the
check* — the container port in network mode, the kernel-assigned host port in
publish mode.

The tenancy contract is narrow. Gauntlet guarantees an instance matching the
declaration exists, is ready, and is reachable at that endpoint for the run's
duration. The harness owns tenancy *within* the instance — per-run databases
keyed off `GAUNTLET_RUN_ID`, cleanup, concurrency safety. That division is
exactly what a testcontainers-with-reuse setup already implements, unchanged.

## State on disk

Each instance has a JSON record under `<state>/services/<full-keyhash>.json`
holding the spec snapshot, full key, endpoint, reachability mode, container
ID, and timestamps. Records are efficiency hints, not truth: boot treats the
live-instance listing as authoritative and the records as hints toward
matching it — a live instance with no matchable record is destroyed (slower,
never wrong), a record with no live instance is ignored. Writes are atomic
(temp-then-rename), always a full rewrite so a crash mid-write can never leave
a mixed-field record. A malformed record is skipped at boot, never fatal.
There is no SQLite here.

## Deliberately not built

- **Alternative drivers.** The container driver is the only one built. Two
  others were designed and left unbuilt: an **artifact driver** (a `url` +
  `checksum` fetched into a content-addressed cache and run as a process group,
  for boxes without a container runtime) and an **oci-unpack driver** (unpack
  an image's rootfs and run it natively). The rationale for not rushing them:
  at steady state a *reused* container is a native process with namespaces
  around it — the overhead reuse eliminates (spin-up, image pulls) is the
  overhead that mattered, so de-containerizing is not the win on Linux. The
  artifact driver exists for genuinely runtime-less boxes, not for speed;
  oci-unpack is attractive for exactly one case and expensive everywhere
  (rootfs semantics, uid mapping, host-lib drift) and is recorded only so it
  is not re-litigated from scratch.
- **Services from hooks.** A hook's job never carries `needs` — the grammar
  surface does not exist — so hooks cannot declare service dependencies. No
  code forbids it because there is nothing to forbid.
- **Multi-port services.** A service exposes one port. The recorded evolution,
  when a real service needs two listeners, is repeatable named ports
  (`port "broker" 9092`) surfacing as `GAUNTLET_SVC_<NAME>_PORT_<PORTNAME>`,
  with the bare `_PORT` form reserved for the single-port common case.
- **Per-instance resource enforcement beyond passthrough.** `memory` and
  `cpus` are passed to the runtime verbatim (and join the cache key like every
  other field); when unset, no flag is emitted and the runtime's own default
  (typically unlimited) applies. `max-instances` bounds instance *count*, not
  memory or CPU, so one heavyweight spec can still hurt the builder. That is
  documented, not solved.
- **No env interpolation, no image building, no spec-declared volumes, no
  daemon-config-declared services.** One source of truth for what a service
  is — the repo — and no daemon-side coupling reintroduced through a back door.
