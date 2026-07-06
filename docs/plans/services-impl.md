# Services — Phase A Implementation Plan

**Status:** implementation plan, pre-code · **Date:** 2026-07-06 · Planned by
Opus against docs/plans/services.md; §Amendments (authoritative, override the
body) added by Fable plan review the same day.

Authoritative design: `docs/plans/services.md` (all §). Adversarial review:
`docs/reviews/services-design-adversarial.md`. Invariants + ledger:
`DESIGN.md`. This plan chunks Phase A (services.md §10) for three
implementers with zero file overlap, pins every cross-chunk contract, and
encodes the design's hard rules (F1 ensure-goroutine pinning, M1 red→Err,
M2 mode-at-adoption, M3 release-touch) into the contract doc-comments.

---

## §0. The one idea every implementer must hold

A service instance is a **cache entry keyed by `hash(remote, canonical
spec)`**, resolved lazily **inside the check-execution goroutine**, never on
the reconcile loop. The pool holds no correctness state: delete every
instance and record and the next runs are merely slower. Three hard rules,
violated by the obvious implementation, are the whole point of Phase A:

1. **F1 — ensure never runs on the reconcile goroutine.** It is a blocking
   (create + up-to-90s ready-poll) call folded into the same `go func` that
   calls `RunCheck`.
2. **M1 — a dead service never manufactures a red verdict.** A failed check
   with `needs` re-probes its services *before its result leaves the
   goroutine*; if any is dead the result is rewritten to `Err` (→
   `OutcomeError`, park-as-error).
3. **M3 — last-used is touched on release, not ensure.** A 4h run against a
   2h-TTL instance ends with last-used = now.

The single most important structural decision (§4.3): **both F1 and M1 are
handled entirely inside the check goroutine.** The reconcile goroutine and
`advanceChecks` are behaviorally *unchanged* — because
`Err`→`verdictErrored`→`OutcomeError` already exists (reconcile.go:546). No
blocking service call ever touches the reconcile loop, including the mid-run
re-probe.

---

## §1. Architecture and package layout

```
internal/config/     (chunk 1) — Service/needs KDL parsing, spec validation, canonical key
internal/services/   (chunk 2, NEW) — Pool (policy) + Driver iface + containerDriver + records
internal/queue/      (chunk 3) — ensure/release/re-probe folded into startCheck; gating; Config.Services
internal/core/       (chunk 3) — CheckJob.ServiceEnv + CheckJob.Networks
internal/executor/   (chunk 3) — local.go + container.go consume the two new CheckJob fields
cmd/gauntlet/        (chunk 3) — pool construction, boot adopt, reaper goroutine, sweep exclusion
README.md            (chunk 3) — services section
```

**Import graph (acyclic, verified):** `services → config, core`; `queue →
services, config, core, obs`; `cmd/gauntlet → queue, services, config`. No
cycle. `internal/services` does **not** import `internal/executor` (§3
driver decision) or `internal/queue`.

**Driver reuse decision (pinned):** the services container driver does
**not** reuse `internal/executor`'s `runtimeSpec`/`probeRuntime`. Rationale
in one sentence: the two share only the binary name and a reachability
probe, while the driver's entire argv surface (`run -d`, `network create`,
`inspect`, `exec`, `rm -v`, `logs`, `port`) is disjoint from the check
executor's `run --rm`, so coupling them would export executor internals for
a one-line benefit and add a package dependency that buys nothing.
`internal/executor/container.go` is **untouched by chunk 2**; chunk 3
modifies it only to consume the two new `CheckJob` fields.

---

## §2. CHUNK 1 — spec, config, gating, canonical key

**Owns (exclusive write):**
- `internal/config/checks.go` — add `Service`, `EnvVar`, `Check.Needs`;
  spec-local validation; `RequiresServices()` helper.
- `internal/config/daemon.go` — add the `Services` daemon block + its
  defaults + validation.
- `internal/config/servicekey.go` — **NEW** — canonical key derivation.
- `internal/config/checks_test.go`, `internal/config/config_test.go`,
  `internal/config/servicekey_test.go` (**NEW**).

**Read-only:** `docs/plans/services.md §1,§2,§7`, `DESIGN.md`.

### 2.1 Repo-side spec types (checks.go)

```go
// Service is one shared service a check may declare a dependency on
// (docs/plans/services.md §1). Read from the trial-merged tree, so a branch
// that changes a service's image/env/probe is tested with its own
// declaration. EVERY field participates in the cache key (§2, ServiceKey):
// two specs differing in any field are distinct cache entries by design.
type Service struct {
	Name         string        `kdl:",arg"`
	Image        string        `kdl:"image"`
	Port         int           `kdl:"port"`
	Env          []EnvVar      `kdl:"env,multiple"`
	ReadyCommand []string      `kdl:"ready-command,child"` // optional; absent ⇒ default TCP-dial probe
	ReadyTimeout time.Duration `kdl:"ready-timeout,format:units"` // default defaultReadyTimeout
	IdleTTL      time.Duration `kdl:"idle-ttl,format:units"`      // default defaultIdleTTL
}

// EnvVar is one `env "NAME" "VALUE"` pair set inside the service container.
type EnvVar struct {
	Name  string `kdl:",arg"`
	Value string `kdl:",arg"`
}

// Check gains:
type Check struct {
	Name    string   `kdl:",arg"`
	Command []string `kdl:"command,child"`
	Needs   []string `kdl:"needs"` // service names this check requires; each must match a declared Service
}

// CheckSpec gains:
type CheckSpec struct {
	Checks   []Check   `kdl:"check,multiple"`
	Services []Service `kdl:"service,multiple"`
}
```

**KDL grammar note (deviates deliberately from services.md §1's
illustrative snippet):** pin the concrete grammar as `ready-command "prog"
"arg"...` (child-node argv like `command`), `needs "mssql" "redis"`
(repeated args on one node), `env "K" "V"` (two positional args, repeated
node). Multi-line child blocks only (DESIGN watch item: kdl-go rejects
single-line child blocks, reports "line 0").

**FIRST TASK for chunk 1 (kdl-go risk spike, blocks everything
downstream):** confirm kdl-go maps (a) `env "K" "V"` into `[]EnvVar` with
two `,arg` fields by declaration order, and (b) `ready-command "a" "b"` into
`[]string`. If either fails, fall back to `env "K" value="V"` and record the
chosen grammar in §Amendments. Everything else is independent of which wins
— hashing is over the parsed struct.

### 2.2 Spec-local validation (checks.go `validate()`)

Extend `CheckSpec.validate()`: each `Service.Name` non-empty + unique;
`Image` non-empty; `Port` in `[1,65535]`; `ReadyTimeout>0`, `IdleTTL>0`
**after defaults applied**; every `Check.Needs` entry must name a declared
`Service` (undeclared ⇒ error, rides the existing `ParseChecks` error path →
`OutcomeRejected`); duplicate needs within a check ⇒ error. `ParseChecks`
calls `applyServiceDefaults()` **before** `validate()` and before any caller
hashes.

### 2.3 Daemon capability block (daemon.go)

```go
// Services gates whether repo-declared services may run on this box
// (docs/plans/services.md §7). Absent ⇒ Allow nil ⇒ a spec declaring
// service/needs is REJECTED at run time (loud, like a malformed check).
// Presence is signalled by len(Allow) > 0.
type Services struct {
	Allow        []string `kdl:"allow"`         // driver kinds permitted; phase A: only "container"
	MaxInstances int      `kdl:"max-instances"` // default defaultMaxInstances; hard count cap
}
// on Daemon:
Services Services `kdl:"services"`
```

`applyDefaults`: when `len(Allow)>0` and `MaxInstances==0`, set
`defaultMaxInstances` (8). `validate`: every `Allow` entry must be
`"container"` (reject `"artifact"`/`"oci-unpack"` as "reserved for a future
release", mirroring on-batch-red `"bisect"`); `MaxInstances>=1`. Gating
against the executor (Apple deferral) lives in cmd (§4.5), not here.

### 2.4 Defaults (daemon.go constants)

```go
defaultReadyTimeout  = 60 * time.Second
defaultIdleTTL       = 30 * time.Minute
defaultMaxInstances  = 8
```

`applyServiceDefaults(svc *Service)` fills `ReadyTimeout`/`IdleTTL` when
zero, **before hashing** — this resolves the apparent §2 tension ("absent
fields hashed as zero" vs "changing a default recycles"): defaulted fields
hash at their *effective* value (so a future default change recycles); only
genuinely defaultless optionals (`ReadyCommand`, empty `Env`) hash as zero.

### 2.5 Canonical key (servicekey.go) — pinned contract

```go
// ServiceKey returns the full cache key for svc under repo remote: the hex
// SHA-256 over a canonical, target-independent encoding of (remote, svc).
// The key hashes the PARSED, DEFAULTED struct — never raw KDL bytes
// (review m1) — so reordering env lines or reflowing whitespace never
// recycles an instance. Encoding, fixed and total (§2):
//   remote, Name, Image: length-prefixed
//   Port: 8-byte big-endian
//   Env: sorted by Name, each (Name,Value) length-prefixed
//   ReadyCommand: element count + each element length-prefixed
//   ReadyTimeout, IdleTTL: int64 nanoseconds, 8-byte big-endian
// Returns the full 64-hex digest. Truncation to keyhash12 (=key[:12]) is the
// POOL's job for names/aliases; records and adoption match the FULL key
// (review m2/m6).
func ServiceKey(remote string, svc Service) string
```

`applyServiceDefaults` must run before `ServiceKey` (chunk 2 calls
`config.ServiceKey` on already-defaulted values received from the queue,
which got them from `ParseChecks`).

### 2.6 Chunk-1 test matrix (unit only, pure functions)

| Test | Asserts |
|---|---|
| key: env reorder | identical when two `env` lines swap order |
| key: whitespace | identical across reflowed KDL |
| key: any-field-change | image/port/env-value/ready-command/timeout/ttl each flip the key |
| key: remote-change | different remote ⇒ different key (M5 boundary) |
| key: default-applied | omitting idle-ttl hashes == writing the default explicitly |
| parse | `service`/`needs`/`env`/`ready-command` nodes populate structs (the §2.1 spike) |
| validate | undeclared need, dup service, dup need, empty image, port 0, non-positive ttl each error |
| gating helper | `RequiresServices()` true iff `len(Services)>0 || any check has needs` |
| daemon | `services { allow "container" }` parses; `allow "artifact"` rejected; max-instances default/validate |

---

## §3. CHUNK 2 — internal/services (pool + container driver)

**Owns (exclusive write):**
- `pool.go` — `Pool`, `Config`, `Ensured`, `EnsureAll`/`Release`/`AnyDead`/
  `Adopt`/`ArmReaper`/`Reap`, single-flight, refcount, max-instances.
- `driver.go` — `Driver` interface, `Instance`, `InstanceSpec`.
- `driver_container.go` — the docker/podman `containerDriver`.
- `record.go` — JSON record schema + atomic read/write/list.
- `pool_test.go`, `record_test.go` — unit tests with a **fake Driver** (no
  docker).
- `driver_container_test.go` — `//go:build dockerlive`, skipped without a
  runtime.

**Read-only:** `internal/config` (chunk 1: `Service`, `ServiceKey`),
`docs/plans/services.md §2,§3,§5,§6`. **Depends on chunk 1.**

### 3.1 Reachability mode + pool Config

```go
type Mode int
const (
	ModeNetwork Mode = iota // container executor: shared user network, alias host, no publish
	ModePublish             // local executor: -p 127.0.0.1:0:<port>, kernel-assigned host port
)

type Config struct {
	Remote       string           // the daemon's remote URL — key material (§2, M5)
	Token        string           // state-dir token; namespaces names/network (== cmd's stateToken)
	Mode         Mode             // derived from executor kind (cmd wiring, §4.5)
	Runtime      string           // "docker" | "podman"; "container" (Apple) REJECTED in phase A
	StateDir     string           // <state>/services lives here (records)
	MaxInstances int
	Now          func() time.Time // injectable clock (tests)
}

func New(cfg Config) (*Pool, error) // rejects Runtime=="container" with the phase-A deferral error
```

**Mode is a pool-global invariant, not key material (M2).** One daemon, one
executor block, one mode. Recorded per instance; adoption rejects a
mode-mismatched record (§3.6).

### 3.2 Queue-facing surface (`Ensured` + methods queue calls)

```go
// Ensured is EnsureAll's output: env+networks to reach the services, plus an
// opaque handle for Release/AnyDead. keys is unexported (queue holds it
// opaquely and hands it back).
type Ensured struct {
	Env      []string // ["GAUNTLET_SVC_MSSQL_HOST=…","GAUNTLET_SVC_MSSQL_PORT=…"]
	Networks []string // ["gauntlet-svc-<token>"] in ModeNetwork; nil in ModePublish
	keys     []string
}

// EnsureAll resolves every name in needs to a ready instance and returns the
// env+networks to reach them. BLOCKING (create + ready-poll up to each spec's
// ReadyTimeout) and SINGLE-FLIGHT per key. MUST be called only from a
// check-execution goroutine, NEVER the reconcile goroutine (review F1) — its
// doc comment says exactly this. Each resolved need increments that
// instance's refcount. Errors (create failed, not ready in ReadyTimeout,
// probe failed twice, max-instances exceeded) are returned for the caller to
// map to CheckResult.Err (park-as-error, §7). On partial failure it releases
// whatever it already ensured.
func (p *Pool) EnsureAll(ctx context.Context, svcs []config.Service, needs []string) (Ensured, error)

// Release drops one reference per key and TOUCHES last-used = now on each
// (review M3 — idle clock starts when the LAST reference drops). Never
// destroys; the reaper does. Idempotent-safe.
func (p *Pool) Release(e Ensured)

// AnyDead probe-alives every instance in e and reports whether any is dead
// (review M1). BLOCKING; called only from the check goroutine, only on a
// FAILED check. A dead instance is evicted here so the next run re-creates it.
func (p *Pool) AnyDead(ctx context.Context, e Ensured) bool
```

### 3.3 Boot + reaper surface (cmd calls these)

```go
// Adopt lists live gauntlet-svc-<token>-* instances and, matching each
// against records BY FULL KEY IN THE INSTANCE LABEL (names never trusted,
// §2/m6), re-checks recorded Mode == cfg.Mode (M2) and probes ready; adopts
// matches, DESTROYS everything else (unmatchable, unready, mode-mismatched,
// beyond IdleTTL). Called once at boot, before any check goroutine or reaper.
func (p *Pool) Adopt(ctx context.Context) error

// ArmReaper marks the reaper live. The in-memory refcount is lost on restart,
// so the reaper is a no-op until the queue's FIRST full reconcile pass has
// re-ensured (refcounted) everything still in flight (review q3). Idempotent.
func (p *Pool) ArmReaper()

// Reap destroys every instance whose (now - last-used) exceeds its IdleTTL AND
// whose refcount is 0. No-op until ArmReaper. BLOCKING destroys — runs on its
// OWN goroutine (cmd's reaper ticker), never the reconcile loop.
func (p *Pool) Reap(ctx context.Context)
```

The queue's consuming interface (chunk 3, §4.4) is the subset `{EnsureAll,
Release, AnyDead, ArmReaper}`. `*services.Pool` satisfies it structurally.

### 3.4 Driver interface + the container driver

```go
type InstanceSpec struct {
	Key   string         // full key (label)
	Spec  config.Service // defaulted
	Name  string         // gauntlet-svc-<token>-<keyhash12>
	Alias string         // keyhash12 (network alias, ModeNetwork)
	Mode  Mode
	Net   string         // gauntlet-svc-<token> (ModeNetwork)
}
type Instance struct {
	Name        string
	Key         string
	ContainerID string
	Mode        Mode
	Host, Port  string // resolved endpoint (Port read back via `docker port` in ModePublish)
}

// Driver is the tiny CLI shim (services.md §6). One impl in phase A
// (containerDriver); the interface exists for the v2 artifact driver and for
// the Pool's fake-driver unit tests.
type Driver interface {
	Create(ctx, is InstanceSpec) (Instance, error)   // run -d; ModeNetwork: net create (idempotent) + --network + --network-alias; ModePublish: -p 127.0.0.1:0:<port> then read back
	ProbeAlive(ctx, in Instance) (bool, error)        // existence + running (inspect); NOT ready command
	ProbeReady(ctx, in Instance) error                // ready-command via `exec` INSIDE the instance, or TCP-dial endpoint when absent (§6 q2)
	Destroy(ctx, in Instance) error                   // rm -f -v — ALWAYS removes anonymous volumes (review m4)
	Endpoint(in Instance) (host, port string)         // per-mode; checks only ever see this
	List(ctx) ([]string, error)                        // live gauntlet-svc-<token>-* names (adoption)
	TailLogs(ctx, in Instance) string                  // last ~50 lines, failure diagnostics only (review m5)
}
```

`containerDriver` holds `{bin /*docker|podman*/, token, net}`, reuses the
*listing pattern* from sweep.go (`ps -a --format {{.Names}}`, prefix-filter
in Go) but not that file's code. Network creation: `<bin> network create
gauntlet-svc-<token>` on first `Create` in `ModeNetwork`, "already exists" =
success (idempotent). **On ready-probe failure the pool calls `TailLogs` →
daemon log, then `Destroy` (review m5).**

### 3.5 Record schema (record.go) — pinned JSON

`<state>/services/<full-keyhash>.json`, one file per instance, atomic write
(temp+rename):

```json
{
  "key": "<64-hex>",
  "name": "gauntlet-svc-<token>-<keyhash12>",
  "repo": "<remote url>",
  "mode": "network",
  "spec": { "...config.Service, defaulted..." },
  "containerID": "…",
  "endpoint": {"host": "…", "port": "1433"},
  "network": "gauntlet-svc-<token>",
  "createdAt": "RFC3339",
  "lastUsed": "RFC3339"
}
```

Records are **efficiency hints, not truth** (Invariant 4): boot treats the
live-instance listing as truth; a live instance with no matchable record is
destroyed (slower, never wrong). `lastUsed` rewritten on `Release` (M3) via
atomic replace. **No SQLite.**

### 3.6 Single-flight, refcount, max-instances (pinned choices)

- **Single-flight:** homegrown per-key `map[string]*inflight` guarded by
  `p.mu` — **not** `golang.org/x/sync/singleflight` (keeps the direct-dep
  list unchanged, one-dep discipline; ~25 lines).
- **Refcount:** `map[string]int` under `p.mu`; `EnsureAll` ++ per key,
  `Release` -- and set `lastUsed`. `Reap` skips refcount>0 regardless of
  clock.
- **max-instances:** on a *miss* that would push live count ≥
  `MaxInstances`, `EnsureAll` errors (→ park-as-error). Reuse never counts
  against the cap. Bounds count only, not memory/CPU (review q1 —
  documented, not solved).

### 3.7 Chunk-2 test matrix (unit with fake Driver; real docker only under build tag)

| Test | Harness | Asserts |
|---|---|---|
| single-flight | fake Driver counting Create | N concurrent EnsureAll on one key ⇒ 1 Create |
| refcount + reap | fake + injected clock | reaper skips refcounted; destroys idle>ttl at refcount 0 |
| release-touch (M3) | fake + clock | ensure t=0, release t=4h, ttl=2h ⇒ NOT reaped |
| arm gating (q3) | fake | Reap before ArmReaper is a no-op even past ttl |
| adopt match | fake listing + records | full-key match adopts; unmatchable/unready destroyed |
| adopt mode (M2) | fake | record mode ≠ cfg.Mode ⇒ destroyed, not adopted |
| max-instances | fake | miss at cap ⇒ error; reuse at cap ⇒ ok |
| ensure failure | fake not-ready | error after ReadyTimeout; TailLogs+Destroy called |
| env/networks | fake | Env = GAUNTLET_SVC_<NAME>_HOST/PORT; Networks per mode |
| record round-trip | real FS temp | write→list→read; atomic replace on touch |
| **container driver** | `//go:build dockerlive` | create/probe/exec/destroy against real docker, else skip |

---

## §4. CHUNK 3 — integration (queue, core, executor, cmd, README)

**Owns (exclusive write):** `internal/core/types.go`;
`internal/executor/local.go`, `internal/executor/container.go`;
`internal/queue/daemon.go`, `internal/queue/reconcile.go`;
`internal/queue/services_test.go` (**NEW**) + additions to
`internal/queue/daemon_test.go`; `cmd/gauntlet/main.go`,
`cmd/gauntlet/sweep.go`; `README.md`.

**Read-only:** `internal/config` (chunk 1), `internal/services` (chunk 2).
**Depends on both.**

### 4.1 core.CheckJob (types.go) — pinned

```go
// ServiceEnv is extra environment (GAUNTLET_SVC_<NAME>_HOST/PORT) for this
// check's resolved needs, appended after the built-in GAUNTLET_* vars by every
// executor. nil for checks with no needs and for hooks. (services.md §4)
ServiceEnv []string

// Networks are container networks the check must join to reach its services
// (ModeNetwork). The container executor adds one --network per entry; the
// local executor ignores it (ModePublish reaches 127.0.0.1). nil for no-needs
// checks, hooks, and publish mode.
Networks []string
```

### 4.2 Executors consume them

- `local.go` (after the six built-ins at 66-73): `cmd.Env = append(cmd.Env,
  job.ServiceEnv...)`. Ignores `Networks`.
- `container.go` `runArgs` (after the six `-e` at 337-343, before `Image`):
  one `--network <n>` per `job.Networks`, and `-e <kv>` per
  `job.ServiceEnv`. Keep `runArgs` pure/exec-free (its test contract).

### 4.3 The ensure/release/re-probe wrapper (reconcile.go `startCheck`) — load-bearing change

`run` gains two build-time-immutable fields (set once in
`startRun`/`startBatchRun`, read-only thereafter — **no cross-goroutine
mutation, no race**): `services []config.Service // == spec.Services`
(per-check needs already live in `r.checks[r.idx].Needs`).

`startCheck` (reconcile.go:604-608) replaces the bare `go func` with a
service-aware wrapper. **All blocking service work — ensure, and the M1
re-probe — happens here, in the check goroutine.** `advanceChecks` is
unchanged: `res.Err != nil` already maps to `verdictErrored`→`OutcomeError`
(reconcile.go:546, runRejectOutcome:369).

```go
result := make(chan core.CheckResult, 1)
needs := r.checks[r.idx].Needs
svcs := r.services
go func() {
	if len(needs) == 0 || d.cfg.Services == nil {
		result <- d.exec.RunCheck(spanCtx, job) // unchanged; hooks & needs-free checks
		return
	}
	ens, err := d.cfg.Services.EnsureAll(spanCtx, svcs, needs) // BLOCKING, off reconcile loop (F1)
	if err != nil {
		result <- core.CheckResult{Name: check.Name, Err: fmt.Errorf("service ensure: %w", err)}
		return // → verdictErrored → OutcomeError, park-as-error (§7)
	}
	defer d.cfg.Services.Release(ens) // refcount--, touch last-used (M3)
	job.ServiceEnv, job.Networks = ens.Env, ens.Networks
	res := d.exec.RunCheck(spanCtx, job)
	if res.Err == nil && res.Status == core.CheckFailed { // M1: only a real red re-probes
		if d.cfg.Services.AnyDead(spanCtx, ens) {
			res.Err = fmt.Errorf("service died mid-run (park-as-error); check output retained above")
			// res.Output/LogPath preserved for the skeptical (§7)
		}
	}
	result <- res
}()
```

A passing check never re-probes (§7). `AnyDead` and `EnsureAll` are the only
new blocking calls, both in this goroutine.

### 4.4 Config.Services interface + gating + arm-reaper (daemon.go, reconcile.go)

```go
// in queue.Config:
Services ServicePool // nil ⇒ services disabled (hooks & needs-free checks unaffected)

// ServicePool is the subset of *services.Pool the queue consumes. Its blocking
// methods (EnsureAll/AnyDead) are called ONLY from check goroutines (review
// F1) — never ReconcileOnce. Safe for concurrent use.
type ServicePool interface {
	EnsureAll(ctx context.Context, svcs []config.Service, needs []string) (services.Ensured, error)
	Release(services.Ensured)
	AnyDead(ctx context.Context, e services.Ensured) bool
	ArmReaper()
}
```

**Gating (design §7, "loud like a malformed check"):** in
`startRun`/`startBatchRun`, right after `config.ParseChecks`
(reconcile.go:1347 and :1002), if `spec.RequiresServices() &&
d.cfg.Services == nil`, reject via the existing `rejectRun`/`rejectBatch`
path with `OutcomeRejected` and detail `"check spec declares services but
this daemon has no services block"`.

**Arm-reaper (q3):** `Daemon` gets `reaperArmed bool`; at the end of the
first `ReconcileOnce` that completes a full target sweep, if `!reaperArmed
&& d.cfg.Services != nil` call `d.cfg.Services.ArmReaper(); d.reaperArmed =
true` (analogous to the existing `seeded` once flag at daemon.go:257).

### 4.5 cmd wiring (main.go, sweep.go)

Insert pool construction after `stateToken` (main.go:202), before
`queue.New` (:388):

1. **Mode + phase-A gate:** if `len(cfg.Services.Allow) > 0`:
   - `mode := ModePublish` when `cfg.Executor.Kind == "local"`, else
     `ModeNetwork`.
   - **Reject Apple:** `ModeNetwork` and effective runtime `"container"` ⇒
     `return fmt.Errorf("services require docker or podman in phase A;
     Apple container networking is deferred (docs/plans/services.md §9)")`
     — the §9-q1 deferral made a hard fail.
   - `pool := services.New(services.Config{Remote: cfg.Remote, Token:
     token, Mode: mode, Runtime: runtime, StateDir:
     filepath.Join(*statePath,"services"), MaxInstances:
     cfg.Services.MaxInstances, Now: time.Now})`.
   - `mkdir <state>/services` (durable, **never swept** — like `logsDir`).
   - `pool.Adopt(ctx)` — best-effort (log and continue, like
     `sweepContainerOrphans`).
   - Thread `Services: pool` into `queue.Config`.
   - Start the reaper goroutine under the existing `wg` (dedicated 30s
     ticker, no config knob in phase A) calling `pool.Reap` each tick until
     ctx done.
   - `len(Allow)==0` ⇒ `queue.Config.Services` stays nil (byte-identical
     current behavior).

2. **Sweep exclusion (sweep.go, same commit — design §3 mandate):** the
   check-orphan sweep filters `gauntlet-<token>-`; a service name
   `gauntlet-svc-<token>-…` is structurally disjoint (after `gauntlet-`
   comes `svc-`, never the 8-hex token). Add an explicit `if
   strings.HasPrefix(name, "gauntlet-svc-") { continue }` guard + comment so
   a future naming change can't silently regress the adopt/reap split into a
   kill.

### 4.6 Chunk-3 test matrix (extend the queue fake-executor harness; fakes not mocks)

Add a **fake `ServicePool`** to the queue test setup (real in-memory struct
with affordances: scriptable `EnsureAll` error, scriptable `AnyDead`,
recorded `Release` calls — gated-executor style, not a mock). Drive timing
with the existing `release()`-driven check harness.

| Test | Asserts |
|---|---|
| ensure-time failure | fake EnsureAll errors ⇒ run parks OutcomeError, not Rejected (§7, m3) |
| mid-run death (M1) | gated executor CheckFailed + fake AnyDead=true ⇒ result Err ⇒ OutcomeError; captured output still in record |
| passing check + needs | AnyDead never consulted; ServiceEnv reached the executor (recording executor) |
| env injection | GAUNTLET_SVC_MSSQL_HOST/PORT present in the job the recording executor saw |
| release always | Release called on pass, red, and ensure-failure-after-partial |
| gating | spec with needs + Config.Services==nil ⇒ OutcomeRejected, loud detail |
| needs-free unaffected | a no-needs check runs byte-identically (no pool calls) |
| hooks unaffected | a hook's CheckJob has nil ServiceEnv/Networks (hooks.go:700 builds without them) |
| executor unit | container.runArgs emits --network/-e for Networks/ServiceEnv; local appends env, ignores networks |

---

## §5. Hooks in v1 (pinned: no)

A hook's `CheckJob` simply never carries `needs` (hooks.go:700 builds it
without a `Needs` field; hooks have no `needs` grammar). Post-land hooks
declaring services is scope creep. No code is needed to *forbid* it — the
surface doesn't exist. README states hooks cannot declare services in v1.

---

## §6. Integration order + who verifies what

Agents never run `jj`/`git` and never `-race`; the orchestrator does all VCS
and all `-race`.

1. **Chunk 1 lands first** (config types + key; §2.1 kdl-go spike gates the
   grammar). Orchestrator: `go test ./internal/config/ -race`.
2. **Chunk 2 lands second** (depends on 1). Orchestrator: `go test
   ./internal/services/ -race` (fake-driver units; `dockerlive` off).
3. **Chunk 3 lands third** (depends on 1+2): core → executor → queue → cmd →
   README. Orchestrator: `go build ./...`, `go vet ./...`, `go test ./...
   -race`, then the §7 livetest.

Chunks 1 and 2 can be *written* concurrently (zero file overlap; chunk 2
codes against §2.5's pinned contract); they merge in dependency order.
Chunk 3 is written concurrently against §3.2/§3.3 and merges last. After
chunk 3, launch a fresh-context code-review agent (DESIGN process rule)
before the livetest.

---

## §7. Acceptance livetest (orchestrator-run, this mac, colima docker)

Prereqs: `colima start`, arm64-friendly image (`postgres:16-alpine`).
Config adds `services { allow "container"; max-instances 4 }` and
`executor "container" { runtime "docker"; image "…arm64 check image with
nc/pg_isready…" }`.

1. **Crashtest branch** adds to `.gauntlet.kdl`:
   ```kdl
   service "pg" {
       image "postgres:16-alpine"
       port 5432
       env "POSTGRES_PASSWORD" "scratch"
       ready-command "pg_isready" "-h" "localhost"
       ready-timeout "60s"
       idle-ttl "2m"
   }
   check "svc" { command "sh" "-c" "nc -z $GAUNTLET_SVC_PG_HOST $GAUNTLET_SVC_PG_PORT" ; needs "pg" }
   ```
   Push to `for/main/<user>/svc-livetest`.
2. **Observe ensure/create/ready** in the daemon log: network create,
   `run -d`, ready-poll passing, check green over the shared network (the
   `nc -z` proves the check container reached the service alias).
   `docker ps` shows `gauntlet-svc-<token>-<keyhash12>` on
   `gauntlet-svc-<token>`.
3. **REUSE:** push a second candidate. Assert a registry hit, **no** second
   `run -d`, same container ID, warm.
4. **Adoption across restart:** SIGTERM the daemon (crash-equivalent),
   confirm the service survives, restart; assert `Adopt` matches by
   full-key label, probes ready, adopts (no re-create), a new run reuses it.
5. **Reaper eviction:** with `idle-ttl "2m"`, let the ref go idle, wait past
   2m + one reaper tick; assert `docker ps` no longer lists the instance and
   its anonymous volume is gone (`docker volume ls` clean — review m4).
6. **red→Err (M1):** while a `needs "pg"` check runs, `docker kill` the
   service; assert the run parks as **error**, not rejected, and the check's
   captured output is in the run record.

---

## §8. Risk register (top 5, each with its tripwire)

1. **kdl-go can't map `env "K" "V"` / `ready-command` (chunk 1).** Tripwire:
   §2.1 spike test fails day one. Mitigation: fall back to `value=` property
   form; record in §Amendments. Known kdl-go staleness risk (DESIGN "line 0"
   quirk); gated first so it can't surprise chunks 2/3.
2. **An ensure or re-probe lands on the reconcile goroutine (F1
   regression).** Tripwire: one cold service (60s ready) freezes *other*
   targets' reconciliation; or review flags a blocking pool call outside
   `startCheck`'s `go func`. Guard: doc comments say "check goroutine only";
   the only call sites are the §4.3 wrapper. Reviewer checks no pool method
   is called from `ReconcileOnce`/`advanceLane`/`refillLane`.
3. **Reaper races boot adoption and evicts a just-recovered instance (q3).**
   Tripwire: restart-under-load livetest (step 4) shows an adopted instance
   destroyed before its recovered run re-ensures. Guard: `Reap` no-ops until
   `ArmReaper` (only after the first full reconcile pass); fake-clock unit
   test asserts Reap-before-arm is a no-op even past TTL.
4. **Container-executor check can't resolve the service alias (network
   wiring).** Tripwire: livetest step 2's `nc -z` fails on name resolution.
   Likely cause: check container not joined to `gauntlet-svc-<token>`, or
   service created without `--network-alias <keyhash12>`. Guard: `runArgs`
   emits `--network` per `job.Networks`; driver `Create` sets
   `--network-alias`. Verify with `docker network inspect`.
5. **Mid-run death check inverts a legitimate red into a spurious error (M1
   over-fires).** Tripwire: a genuinely-failing check whose service is fine
   gets parked as error, hiding the real red. Guard: `AnyDead` re-probes
   probe-*alive* (existence+running), not readiness, and only on
   `CheckFailed` with `res.Err == nil`; unit-test both directions
   (dead⇒convert, alive⇒stay-red).

Secondary watch: **max-instances is a count cap only** (review q1) — a
heavyweight spec can still OOM the builder; documented in README, not
solved. **Cross-repo dedup forfeited** (M5, remote in key) — documented.
**Adoption trusts on-box process names** (m6) — stated in README's trust
note.

---

## §Amendments

Fable plan review, 2026-07-06. These override the body.

**A1 — ProbeReady needs the spec; `Instance` carries it.** §3.4's
`ProbeReady(ctx, in Instance)` cannot execute `ready-command` — the command
lives in the spec, and `Instance` doesn't carry one. Pin: `Instance` gains
`Spec config.Service` (the defaulted spec), populated by `Create` and by
adoption (from the record's spec snapshot). `ProbeReady`'s signature is
unchanged; it reads `in.Spec.ReadyCommand`. This also gives `Reap` per-
instance `IdleTTL` without a registry lookup.

**A2 — Integration is strictly serial: 1 → 2 → 3, no stubs.** The body's
"written concurrently against pinned contracts" invites stub drift (the
phase-6 B-track's canned-stub test failures) for minutes of wall-clock
savings: chunk 2 cannot compile without chunk 1's types, chunk 3 not
without chunk 2's package. Each chunk's agent starts when the previous
chunk has landed on main. The pinned contracts remain binding — they are
what each later agent codes against without renegotiation, not a
parallelism mechanism.

**A3 — Services runtime when the executor is `local`.** §4.5 derives the
pool's runtime from the executor block, but `executor "local"` has no
runtime — and local-executor-plus-containerized-services is a first-class
deployment shape (likely the operator's production Linux box). Pin: the
daemon `services` block gains `runtime "docker"` (optional, default
`"docker"`, validated `docker|podman` in phase A). When `executor.Kind ==
"container"`, the executor's runtime wins (it must be docker/podman — the
Apple hard-fail in §4.5 stands) and a conflicting `services.runtime` is a
config validation error; when `local`, `services.runtime` is used as given.
Chunk 1 owns the config field; chunk 3 owns the cmd wiring.

**A4 — EnsureAll's needs→service resolution is defensive.** A `needs` name
with no matching entry in `svcs` returns an error (mapped to
`CheckResult.Err`), even though chunk 1's spec validation makes it
unreachable — the pool must not panic on a contract violation by a future
caller. One unit test in chunk 2's matrix.

**A5 — Livetest step 6 needs a long-running red.** A `nc -z` check is too
fast to `docker kill` a service mid-run. The M1 livetest uses a dedicated
branch whose check is `sh -c 'sleep 30; exit 1'` with `needs "pg"` — kill
the service during the sleep; the exit-1 red then converts to Err via the
AnyDead re-probe. (The unit test drives the same path with the gated
executor; the livetest exists to see it against real docker.)
