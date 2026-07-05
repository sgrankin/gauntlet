# Gauntlet — Phase 2+3 Implementation Plan

**Scope:** DESIGN.md "Build phases" §2 (executors & channels) and §3 (dashboard + SQLite + OTLP export), combined. DESIGN.md is authoritative; this plan operationalizes it against the **phase-1 code as landed** (the real APIs in `internal/{core,config,gitx,executor,channel,obs,queue}` and `cmd/gauntlet` — those signatures are the contract). Plan drafted by the planning agent (Opus), reviewed and amended by the design reviewer — **§9 amendments are authoritative where they touch earlier sections.**

**Toolchain (verified 2026-07-04):** Go `1.26.4`, git `2.55.0`, module `github.com/sgrankin/gauntlet` (`go 1.26`). Only Apple `container` `1.0.0` is present as a container runtime (no docker, no podman).

**Priority (user-set):** dashboard early; everything locally verifiable or with a fake-based test story + documented manual verification. Chunk ordering in §6 reflects this.

---

## 0. What phases 2+3 are

Phase 1 is a single-lane reconcile loop with a `LogChannel` and a `LocalExecutor`. Phases 2+3 add **output channels and one alternate executor around the unchanged core**, plus a **read-only web dashboard** backed by two data sources: a thread-safe live snapshot the reconcile loop publishes each pass, and a SQLite history the run-record event stream feeds. Concretely:

- **SQLite history** (`internal/history`) — a `core.Channel` writing a run row + per-check rows on every terminal event, plus a periodically-sampled queue-depth series. Read-side query methods feed the dashboard. Absent config ⇒ not constructed; daemon and dashboard run without it.
- **Web dashboard** (`internal/dashboard`) — stdlib `net/http` + `html/template`, read-only, reads the live snapshot (queue state) and SQLite (history). History views degrade to "history disabled" when SQLite is off.
- **GitHub commit-status channel** (`internal/ghstatus`) — one rollup status `gauntlet/<target>` on the candidate SHA via the plain REST statuses API with a PAT from env.
- **Slack channel** (`internal/slack`) — socket mode (outbound websocket): threaded run messages, root edited to ✅/❌, and a `:recycle:` reaction on the root that produces a `core.Command{Kind:"retry"}` — the first real use of `Channel.Commands()`.
- **Container executor** (`internal/executor`, new file) — one generic OCI-CLI wrapper (`docker`/`podman`/Apple `container`) with a per-runtime arg table, persistent named cache volumes, same `core.Executor` contract and env/result-file mechanism.
- **OTLP export** (`internal/obs`, new file) — config-gated: install the real SDK tracer provider + HTTP exporter; the spans phase 1 already emits start exporting. Flush on shutdown.
- **`land` porcelain + docs** — README sections for the git alias / jj equivalent and every new channel/executor; optional tiny `gauntlet land` subcommand.

Three **minimal, interface-shaped core additions** make this possible without the queue learning what Slack or SQLite are: a published state snapshot, a command-drain in the reconcile pass, and park entries that carry their reason (§2). Everything else plugs in behind the existing `core.Channel` / `core.Executor`.

---

## 1. Spike findings (all run on this machine)

### Spike A — SQLite driver: **modernc.org/sqlite** (pure Go). Decided.

| Driver | cgo? | Static binary | API | Notes |
|---|---|---|---|---|
| **`modernc.org/sqlite` v1.53.0** | **no** | **`CGO_ENABLED=0 go build` succeeds** | `database/sql` | SQLite 3.53.2; WAL + `busy_timeout` pragmas via DSN; ~9.6 MB added. Deps: 5 `modernc.org/*` + `golang.org/x/sys`. |
| `zombiezen.com/go/sqlite` v1.4.2 | no | yes (built on modernc) | own single-conn API | No build win over modernc, extra API to learn. |
| `mattn/go-sqlite3` v1.14.47 | **yes** | **breaks the static-binary story** | `database/sql` | Disqualified. |

**Verdict: `modernc.org/sqlite`** — verified end-to-end (open, DDL, insert, query, WAL). Single-writer discipline: DSN `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)` + `db.SetMaxOpenConns(1)`.

### Spike B — Slack library: **use `github.com/slack-go/slack`** (socketmode). Decided.

`v0.27.0`: only non-test runtime dep is `gorilla/websocket v1.5.3`. Actively maintained; handles the socket-mode reconnect/ack protocol + web-API surface (`chat.postMessage`, `chat.update`, reactions). Accepts `slack.OptionAPIURL(...)` so everything routes to an `httptest` server — fully fakeable. Hand-rolling only re-implements the ack/reconnect state machine.

### Spike C — Container runtime: only **Apple `container` 1.0.0**; docker-flag-compatible.

`docker`/`podman` absent; `/opt/homebrew/bin/container` present. Flags mirror docker: `-v/--volume`, `-e/--env`, `-w/--workdir`, `--rm`, `--name`, `--mount`, `--entrypoint`. The system service is **not running** here (`container system status` ⇒ not running) — so the test story is: **container integration tests skip cleanly** when the runtime binary is absent *or* its service is unreachable. Docker-compat collapses the per-runtime arg table to nearly one code path.

### Spike D — OTLP exporter: **`otlptracehttp`**. Decided.

Both http and grpc exporters (`v1.44.0`) build under `CGO_ENABLED=0`; `otlptracehttp` does not link the grpc runtime — smaller binary, proxies-friendly, common default.

---

## 2. Core deltas (minimal, interface-shaped)

Three additions to `internal/queue` + one constant in `internal/core`. All additive or pure-bookkeeping; none touches trial-merge / CAS / land logic (Invariants 1–6 untouched, §7). Queue still imports only `core`+`obs`+`config` (Invariant 8 holds).

### 2.1 Published state snapshot (feeds dashboard live views)

Immutable snapshot under `atomic.Pointer`, published at the end of each reconcile pass. New file `internal/queue/snapshot.go`:

```go
package queue

import (
	"time"
	"github.com/sgrankin/gauntlet/internal/core"
)

// Snapshot is an immutable, point-in-time view published at the end of each
// ReconcileOnce pass. Safe to read from any goroutine: deep-copied from
// reconcile state (only the reconcile goroutine mutates) and never mutated
// after publication.
type Snapshot struct {
	At      time.Time
	Targets []TargetSnapshot
}
type TargetSnapshot struct {
	Name      string
	Branch    string
	TargetTip string         // "" if the target branch doesn't exist yet
	InFlight  *RunSnapshot   // nil when the lane is idle
	Waiting   []WaitingEntry // FIFO order; excludes in-flight and parked refs
	Parked    []ParkedEntry  // refs parked at current SHA, with reason
}
type RunSnapshot struct {
	Candidate core.Candidate
	RunID     string
	BaseOID   string
	MergeSHA  string
	Done      []core.CheckResult // checks finished so far, in order
	Current   *CurrentCheck      // the check running now; nil between checks
	StartedAt time.Time
}
type CurrentCheck struct {
	Name      string
	StartedAt time.Time // elapsed = snapshot.At.Sub(StartedAt)
}
type WaitingEntry struct {
	Candidate core.Candidate
	Seq       int64 // FIFO sequence (queue.order); lower = earlier
}
type ParkedEntry struct {
	Candidate core.Candidate
	Outcome   core.Outcome // why it parked (rejected/conflict/error)
	Reason    string       // RunRecord.Detail at park time
	At        time.Time
}
```

Daemon changes (`daemon.go`):
```go
type Daemon struct {
	// ...existing fields...
	snap atomic.Pointer[Snapshot] // last published; nil until first pass
}
// Snapshot returns the most recently published snapshot, or nil if no pass
// has completed. Safe for concurrent use.
func (d *Daemon) Snapshot() *Snapshot { return d.snap.Load() }
```
At the end of `ReconcileOnce`, after the target loop: `d.snap.Store(d.buildSnapshot(refs))` — deep-copying into the value types on the reconcile goroutine (executor goroutines only send once on the result channel; no race). On Fetch/ListRefs early-error paths, `ReconcileOnce` returns before publishing; the previous snapshot stays (staleness visible via `Snapshot().At`).

### 2.2 Command drain in the reconcile pass (retry; first use of `Channel.Commands()`)

**`ReconcileOnce` drains each channel's `Commands()` non-blockingly at the top of the pass** — no fan-in goroutines, no inbox mutex; command application stays serialized with the pass. *(The original sketch used a `goto`; that was pseudocode — write it idiomatically, e.g. a helper that drains one channel with a labeled break or a `for`/`select`/`default: return` loop. §9.4.)* New file `internal/queue/command.go`:

```go
// drainCommands applies every currently-buffered command, then returns.
// Non-blocking; runs at the top of ReconcileOnce on the reconcile goroutine.
func (d *Daemon) drainCommands(ctx context.Context)

// applyCommand handles one inbound command. Retry clears the park for
// (target, ref) at its current SHA so the next pick re-tests it — the
// sanctioned phase-2 mechanism for DESIGN §9.1 (phase-1 plan). Touches only
// in-memory bookkeeping; refs/CAS/land untouched.
func (d *Daemon) applyCommand(ctx context.Context, cmd core.Command)
```
On `CommandRetry`: if `(cmd.Target, cmd.Ref)` is parked, delete the park entry and emit `EventQueued` with detail "retry: park cleared". `ReconcileOnce` gains one call after `ListRefs`, before the target loop. Add to `internal/core` (new `command.go`):
```go
// Command kinds. Channels produce these; the queue applies them (Invariant 8).
const CommandRetry = "retry"
```
Retry is idempotent. Tests inject via `RecordingChannel.SendCommand` (new test affordance).

### 2.3 Park entries carry their reason (feeds `ParkedEntry`)

Change `done`'s value type only:
```go
// parkEntry records why a (ref, SHA) is parked, for the dashboard snapshot.
type parkEntry struct {
	SHA     string
	Outcome core.Outcome
	Reason  string    // RunRecord.Detail at park time
	At      time.Time
}
// Daemon.done becomes: map[string]map[string]parkEntry
```
Mechanical touch points: `park(...)` gains `(outcome, detail)`; `syncBookkeeping` / `pickHead` compare `entry.SHA`. Semantics identical to phase-1 §9.1.

### 2.4 Run-ID uniqueness (§9.1 amendment — folded into D0)

Run IDs gain a monotonic per-process counter: `<UTC yyyymmddThhmmssZ>-<seq>-<trialTreeOID[:12]>`. C7 demonstrated same-second identical-tree trials mint identical IDs; the container executor also derives container names from run IDs, giving collisions real teeth. The counter resets per process (restart also changes the timestamp; uniqueness holds).

---

## 3. Config schema additions

Config stays dumb data. New nodes all **optional**; presence = required key non-empty. *(Implementer check: confirm kdl-go `*Struct` support; if solid, pointers are acceptable instead of presence-keys — record which was used.)*

```go
type Daemon struct {
	// ...existing: Remote, Poll, CheckSpec, Committer, MergeMsg, Targets...
	History   History   `kdl:"history"`   // Path=="" ⇒ disabled
	Dashboard Dashboard `kdl:"dashboard"` // Bind=="" ⇒ disabled
	GitHub    GitHub    `kdl:"github"`    // Repo=="" ⇒ disabled
	Slack     Slack     `kdl:"slack"`     // Channel=="" ⇒ disabled
	OTLP      OTLP      `kdl:"otlp"`      // Endpoint=="" ⇒ no-op (phase-1 default)
	Executor  Executor  `kdl:"executor"`  // Kind=="" ⇒ "local"
}
type History struct {
	Path        string        `kdl:",arg"`
	SampleEvery time.Duration `kdl:"sample-every,format:units"` // default = Poll
}
type Dashboard struct {
	Bind string `kdl:",arg"` // "localhost:8080"; "" disables
	URL  string `kdl:"url"`  // §9.3: optional public base URL for outbound links
}
type GitHub struct {
	Repo     string `kdl:",arg"`      // "owner/name"
	TokenEnv string `kdl:"token-env"` // default "GITHUB_TOKEN"
	APIURL   string `kdl:"api-url"`   // default "https://api.github.com"
}
type Slack struct {
	Channel     string `kdl:",arg"`          // channel ID
	AppTokenEnv string `kdl:"app-token-env"` // default "SLACK_APP_TOKEN"
	BotTokenEnv string `kdl:"bot-token-env"` // default "SLACK_BOT_TOKEN"
}
type OTLP struct {
	Endpoint string `kdl:",arg"`
	Insecure bool   `kdl:"insecure"`
}
type Executor struct {
	Kind    string  `kdl:",arg"`           // "local" (default) | "container"
	Runtime string  `kdl:"runtime"`        // "docker"|"podman"|"container"; default "container"
	Image   string  `kdl:"image"`          // required when Kind=="container"
	Workdir string  `kdl:"workdir"`        // default "/workspace"
	Caches  []Cache `kdl:"cache,multiple"`
}
type Cache struct {
	Name string `kdl:",arg"`
	Path string `kdl:"path"`
}
```
Validation extends `Daemon.validate` in phase-1 style (node-named errors); defaults as annotated. `Dashboard.URL` defaults to `"http://" + Bind` when unset.

### Complete updated `gauntlet.kdl`
```kdl
remote "https://github.com/acme/widgets.git"
poll-interval "10s"
check-spec ".gauntlet.kdl"
committer {
    name "Gauntlet"
    email "gauntlet@ci.acme.example"
}
merge-message "Merge {{.Topic}} ({{.User}})"

target "main"    branch="main"
target "release" branch="release/v2"

// --- phase 2/3 additions (all optional) ---

history "/var/lib/gauntlet/history.db" {
    sample-every "10s"
}

dashboard "localhost:8080" {
    url "https://gauntlet.internal.example"   // optional; used in outbound links
}

github "acme/widgets" {
    token-env "GITHUB_TOKEN"
    api-url "https://api.github.com"
}

slack "C0123456789" {
    app-token-env "SLACK_APP_TOKEN"
    bot-token-env "SLACK_BOT_TOKEN"
}

otlp "localhost:4318" {
    insecure true
}

executor "container" {
    runtime "container"
    image "ghcr.io/acme/ci:latest"
    workdir "/workspace"
    cache "gocache"    path="/root/.cache/go-build"
    cache "gomodcache" path="/go/pkg/mod"
}
```

---

## 4. Component designs

### 4.1 SQLite history store (`internal/history`)
`Store` implements `core.Channel` (write side) + read methods. Output-only.
```go
func Open(path string) (*Store, error)          // WAL, embedded schema, user_version
func (s *Store) Emit(ctx context.Context, ev core.Event) error // terminal events only (Record!=nil)
func (s *Store) Commands() <-chan core.Command  // never yields
func (s *Store) RecordDepth(at time.Time, target string, waiting, inFlight, parked int) error
func (s *Store) Close() error
// Read side:
func (s *Store) RecentRuns(target string, limit int) ([]RunRow, error)
func (s *Store) Run(runID string) (RunRow, []CheckRow, error)
func (s *Store) CheckStats(target string, since time.Time) ([]CheckStat, error)
func (s *Store) DepthSeries(target string, since time.Time) ([]DepthPoint, error)
```
`Emit`: non-terminal events ignored; terminal ⇒ one tx, `INSERT OR REPLACE` run row + check rows (idempotent on `run_id`; the crash-recovered `EventLanded` with no Record is correctly skipped). Depth sampling is driven from cmd wiring reading `d.Snapshot()` — the queue core stays ignorant of SQLite.

### 4.2 Web dashboard (`internal/dashboard`)
stdlib `net/http` + `html/template`, inline CSS, meta-refresh on live pages. Read-only.
```go
// snapshot returns live queue state (Daemon.Snapshot); store may be nil
// (history views render "history disabled").
func New(snapshot func() *queue.Snapshot, store *history.Store) http.Handler
```
Routes: `GET /` (per-target cards: in-flight + current check + elapsed, waiting count, parked count, recent-runs strip); `GET /t/{target}` (full live queue + parked with Outcome+Reason + recent history); `GET /run/{runID}` (per-check detail); `GET /checks?target=&since=` (red-rate + durations; optional depth series). Bind from config; auth out of scope (documented). Works with `store==nil`.

### 4.3 GitHub commit-status channel (`internal/ghstatus`)
`Channel`; `Commands()` never yields. Constructor takes a package-local params struct (§9.5). Maps events → `POST {api}/repos/{owner}/{repo}/statuses/{candidate_sha}`, one rollup context `gauntlet/<target>`:

| Event | state | description |
|---|---|---|
| `EventTrialClean` | `pending` | "running checks" |
| `EventLanded` | `success` | "landed" |
| `EventRejected` | `failure` | `Record.Detail` (capped ≤140 chars) |
| `EventTrialConflict` | `failure` | "trial merge conflict" |
| `EventError` | `error` | infra detail (capped) |
| `EventSkipped` | *(no post)* | transient; re-posts pending next trial |

`target_url` = `<Dashboard.URL>/run/{runID}` when dashboard configured, else omitted. Short-timeout POSTs; errors logged-and-dropped (Emit never blocks the loop).

### 4.4 Slack channel (`internal/slack`, duplex)
`slack-go/slack` + `socketmode`; app token (socket) + bot token (web API). Constructor takes package-local params (§9.5).
- **Outbound:** `EventTrialClean` ⇒ root `chat.postMessage`; record `runID→rootTS` and `rootTS→(target,ref)` under a mutex. Check events ⇒ threaded replies. Terminal ⇒ `chat.update` root to ✅/❌ + final thread reply, **then delete both map entries for that run (§9.2 — bound the maps; a long-running daemon must not leak an entry per run).** Emit enqueues to an internal buffered channel drained by the channel's own goroutine.
- **Inbound:** socket-mode goroutine handles `reaction_added`; `recycle` on an owned root ts ⇒ `core.Command{Kind: CommandRetry, Target, Ref}` onto buffered `cmds`; daemon drains next tick.
- Lifecycle: `New(params)`; `Run(ctx)` starts socket loop + outbound drainer. Manual doc: app manifest (socket mode; scopes `chat:write`, `reactions:read`, `connections:write`; event `reaction_added`) + two tokens.

### 4.5 Container executor (`internal/executor/container.go`)
One generic `ContainerExecutor` (`core.Executor`) + per-runtime arg table. `RunCheck` builds:
```
<bin> run --rm --name gauntlet-<runID>-<check> \
  -w <workdir> \
  -v <job.Dir>:<workdir>            # trial tree, READ-WRITE (matches LocalExecutor; export is ephemeral) \
  -v <resultDir>:/gauntlet          # writable result dir; GAUNTLET_RESULT_FILE=/gauntlet/result \
  -e GAUNTLET_* (all five) \
  -v <cacheName>:<cachePath> ...    # persistent named cache volumes \
  <image> <job.Command...>
```
Container names derive from run IDs — safe post-§2.4. Cancel: CLI in its own process group (same pattern as `local.go`) + `<bin> kill <name>`. Missing runtime / unreachable service ⇒ `CheckResult.Err` (daemon condition per phase-1 §9.2), not a verdict. Same result-file verdict mapping as local.

### 4.6 OTLP export wiring (`internal/obs/provider.go`)
```go
// InstallProvider installs a real SDK tracer provider (OTLP/HTTP) as the
// global when endpoint is configured; Endpoint=="" installs nothing and
// returns a no-op shutdown. shutdown flushes the batch processor.
func InstallProvider(ctx context.Context, endpoint string, insecure bool) (shutdown func(context.Context) error, err error)
```
cmd calls it before `queue.New`; `defer shutdown(ctx)` flushes on signal exit.

### 4.7 SQLite schema + queries
Embedded via `//go:embed schema.sql`; `PRAGMA user_version` versioning (no framework).
```sql
-- schema.sql (user_version = 1)
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,             -- landed|rejected|conflict|skipped|error
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,          -- unix millis
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,              -- passed|failed|skipped
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);
```
Queries: recent-runs-per-target; run detail (+checks by seq); per-check red-rate/avg/max duration since t; depth series since t.

### 4.8 `cmd/gauntlet` wiring (compose point)
Conditional construction from config: history store (+ depth sampler goroutine on `SampleEvery` reading `d.Snapshot()`), dashboard `http.Server` (graceful `Shutdown`), ghstatus channel, slack channel (+ `go sc.Run(ctx)`), executor selection, `obs.InstallProvider` **before** `queue.New`. All config→params mapping happens here (§9.5).

---

## 5. Test strategy (fakes, no network, no wall-clock sleeps, `-race` clean)

- **Core deltas:** extend the phase-1 harness. Snapshot shape mid-run + parked-with-reason; retry via `RecordingChannel.SendCommand` → park cleared → re-tested at same SHA; park-reason fields; run-ID counter monotonicity (same-tree same-second trials get distinct IDs). All phase-1 tests stay green.
- **History:** real SQLite in `t.TempDir()`; terminal-event writes, four queries, depth round-trip, idempotent re-emit; `CGO_ENABLED=0 go test` passes.
- **Dashboard:** `httptest` against `New(fnSnapshot, store)`; hand-built snapshots; `store==nil` renders "history disabled" without panic.
- **GitHub status:** `httptest.Server`; assert exact method/path/auth/body per event; Skipped posts nothing.
- **Slack:** fake socket-mode server (httptest + websocket upgrade serving `apps.connections.open`, `hello`, injectable `reaction_added`; recorded `chat.postMessage`/`chat.update`) via `slack.OptionAPIURL`. Assert threading, root edit, map cleanup after terminal, retry Command from `:recycle:`. *(Implementer check: confirm `OptionAPIURL` routes `apps.connections.open`.)*
- **Container executor:** arg-table builder unit tests everywhere (pure argv construction); runtime integration tests skip cleanly without a usable runtime, else passed/failed/skipped/cancel.
- **OTLP:** empty endpoint ⇒ no-op; configured ⇒ install + clean flush (no live collector needed).
- **Manual docs (README):** GitHub PAT flow; Slack app manifest + tokens; `container system start` + image; OTLP collector.

**Script-DSL: defer to phase 4.** Phases 2+3 add few daemon-level scenarios; the imperative harness serves. Extract a shared `newHarness(t)` helper now; revisit the txtar DSL when the surface stabilizes.

---

## 6. Work breakdown (chunks; ∥ = file-disjoint parallel-safe)

- **D0 — Core deltas** (blocks D2, D4) — `internal/queue/{snapshot.go, command.go, daemon.go, reconcile.go}`, `internal/core/command.go`, `internal/channel/record.go` (+`SendCommand`), queue tests. Includes the run-ID counter (§2.4). **Highest-risk — touches reconcile; review adversarially.** Accept: `go test ./internal/queue/ -race` — all phase-1 tests green + new snapshot/retry/park-reason/run-ID tests.
- **D-cfg — Config extensions** (blocks D7) **∥ with D0** — `internal/config/{daemon.go, config_test.go}`, updated `gauntlet.kdl`. Accept: parses full example; rejects each malformed optional section with node-named error; kdl-go optional-node behavior recorded.
- **D1 — SQLite history store** **∥** — `internal/history/{store.go, schema.sql, queries.go, store_test.go}`. Dep pre-added. Accept: `CGO_ENABLED=0 go test ./internal/history/`.
- **D2 — Dashboard** (dep D0, D1) — `internal/dashboard/{server.go, templates.go, dashboard_test.go}`. Accept: live views from hand-built snapshot; nil store degrades.
- **D3 — GitHub status channel** **∥** — `internal/ghstatus/{ghstatus.go, ghstatus_test.go}`. Accept: httptest request assertions per event.
- **D4 — Slack channel** (dep D0) **∥** — `internal/slack/{slack.go, slack_test.go}`. Deps pre-added. Accept: fake socket-mode end-to-end incl. retry command + map cleanup.
- **D5 — Container executor** **∥** — `internal/executor/{container.go, container_test.go}`. Accept: arg-builder unit tests; runtime tests skip cleanly.
- **D6 — OTLP wiring** **∥** — `internal/obs/{provider.go, provider_test.go}`. Deps pre-added. Accept: no-op preserved; install + shutdown clean.
- **D7 — cmd wiring** (dep all) — `cmd/gauntlet/main.go`. Accept: build; smoke with history+dashboard: target advances, `/` + `/t/main` render, run row in SQLite, `/run/<id>` shows checks.
- **D8 — `land` porcelain + README docs** **∥** — README sections; optional `cmd/gauntlet/land.go` only if genuinely small.

**Parallelization:** D0 ∥ D-cfg ∥ D1 ∥ D3 ∥ D5 ∥ D6 ∥ D8 (all file-disjoint). D4 after D0 (needs `core.CommandRetry` + `SendCommand`). D2 after D0+D1. D7 last. **Critical path: D0 → D2 → D7.**

---

## 7. Invariant impact (review checklist for the core deltas)

- **1–3, 5–6:** untouched — no delta modifies `MergeTree`/`CommitTree`/`CASUpdate`/land/cancel. Snapshot is read-only; command drain edits only the in-memory park map; park-reason is a value-type change.
- **4:** preserved — snapshot + park map remain reconstructible in-memory state (restart still clears parks). SQLite is explicitly not live source of truth.
- **7:** advanced, not activated — cache volumes arrive with the container executor; `CheckJob.Clean` stays reserved. Retry proves the command channel a future clean-build command reuses.
- **8:** preserved and exercised — queue imports only `core`+`obs`+`config`; all new components sit behind `core.Channel`/`core.Executor`; channels produce `core.Command`, the queue applies `CommandRetry`.

---

## 8. Non-goals

GitHub App / Checks API / Actions-dispatch executor; speculation / batching / parallel checks; dashboard auth; deployments / post-land hooks; Claude merge summaries; SQLite as live source of truth; path-filter globs in config; config hot-reload; retry/backoff frameworks; per-check log storage/streaming; multi-remote; the txtar script-DSL harness (phase 4; thin shared helper now).

---

## 9. Review amendments (authoritative)

Applied by the design reviewer; where these touch earlier sections, §9 wins (sections above already edited to match).

1. **Run-ID uniqueness folded into D0** (§2.4): monotonic per-process counter joins the ID. C7 demonstrated same-second identical-tree collisions; container names derive from run IDs, so collisions would break `--name` too.
2. **Slack channel must bound its maps**: delete `runID→rootTS` and `rootTS→(target,ref)` entries on terminal events. A long-running daemon must not leak per-run state.
3. **`Dashboard.URL`** (optional public base URL) added to config: outbound links (GitHub `target_url`) must not point at the bind address, which is typically localhost.
4. **`drainCommands` sketch was pseudocode** (invalid `goto` placement): implement idiomatically.
5. **Package-local constructor params**: D1/D3/D4/D5/D6 constructors take their own params structs; config→params mapping lives only in cmd (D7). Loosens D-cfg coupling and keeps the config package the sole KDL owner.

## 10. Phase-1 review triage (adversarial review, 2026-07-05)

The phase-1 review found no invariant violations (CAS foundation empirically re-verified against real git). Findings and dispositions:

- **F1 → D0.** The `IsAncestor` recovery path treats any coincidental ancestor as "landed", runs no checks, and emits a terminal `EventLanded` with `Record==nil` — violating the documented terminal-event contract that D1's SQLite writer relies on. Fix: keep the CAS slot-delete (correct — the content is already in the target), but synthesize a proper `RunRecord` (zero checks, `OutcomeLanded`, detail "candidate already ancestor of target; checks not re-run"; run-ID stand-in = candidate SHA). Add a real-git integration test driving this daemon path end-to-end and asserting `Record != nil` (closes O5's coverage gap for this branch; O1 routes through the same emit and is fixed with it).
- **F2 → D0 + D7.** Orphaned trial dirs are never swept. D0: `queue.Config` gains `WorkDir string`; trial exports root there when set (else `os.TempDir()` as today). D7: cmd passes `<state>/trials` and clears it at startup.
- **F3 → D7.** No git-version guard. cmd probes `git --version` at startup and fails loudly below 2.38.
- **F4 → D0.** Trial-merge span is orphaned from the run span (root span starts too late). Restructure: start the run root span before `MergeTree`; set `run.id`/`merge.sha` attributes when known.
- **O2 → D-cfg** (dispatched): validate target *branch* uniqueness, not just name.
- **O3 → ledger note, not fixed:** `extractTar` writes symlink entries verbatim; a candidate tree could plant an escape symlink a *later check* follows. Within the stated own-code threat model; revisit if the threat model ever widens.
- **O4 → D0.** Refs under `for/` naming an unconfigured target are silently ignored. Add `core.EventIgnoredRef`, emitted once per (ref, SHA); LogChannel renders it; other channels ignore unknown kinds.
- Review-quality note: the reviewer independently confirmed the clean areas but missed the run-ID same-second collision C7 had found (§2.4) — future "found clean" lists get weighed accordingly.
