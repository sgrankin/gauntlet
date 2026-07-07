# Gauntlet — a merge queue

**Status:** feature-complete through phase 5, plus shared services (phase
A) and auto-retry-once on infra-error parks — serial/batch/speculate
modes, local+container executors, a shared-services pool, dashboard/API/MCP,
Slack duplex with reaction commands, GitHub statuses, post-land hooks,
Claude merge summaries, full log capture, auto-retry, and park persistence
are all shipped; post-completion consistency audit done · **Date:** 2026-07-06

A merge queue for teams that merge often and want their branch history intact.
Your branch runs the gauntlet: push it to a magic ref, the daemon trial-merges it
against the live target tip, runs the suite, and lands it — commits preserved,
one `--no-ff` merge per landing. Red pings you; fix and re-push the same ref.

Open source, git-native (jj-friendly), built for a world where much of the code
is agent-written and *how the branch got there* is data worth keeping.

---

## The model

- **Queue slot = ref name; SHA = what gets tested.** A candidate is a branch
  pushed to `for/<target>/<user>/<topic>`. The name is the durable identity:
  resubmit is re-pushing the same name, cancellation is deleting it,
  attribution is parsed from it. Portable across git and jj clients.
- **Ephemeral trial merge.** The queue owns no branch, only a process. Each
  candidate is merged in-memory onto the *current* target tip
  (`git merge-tree --write-tree`, bare repo, no worktree); green promotes that
  exact merge commit, red is discarded.
- **Serialize first, speculate later.** One lane, FIFO. Two changes each green
  against the current tip can still break together; testing against
  tip-as-it-will-be is the point. *(The growth path was built 2026-07-05 —
  per-target `mode "serial"|"batch"|"speculate"`; serial remains the default.)*
- **Preserve history: merge `--no-ff`, candidate as-is.** Never rebase, never
  squash — rewriting SHAs/messages destroys the record of how the work
  happened. `log --first-parent` reads as the ledger of landings; full branch
  history hangs off each merge commit.

## Decision ledger

| Verdict | Position | Why |
|---|---|---|
| **KEPT** | Ref name as queue unit | The identity both git and jj share. Durable slot, free attribution, resubmit = re-push. jj change-ids were rejected earlier: they only work for jj users. |
| **KEPT** | Refs are ordinary branches (`refs/heads/for/...`) | Custom namespaces (`refs/for/*`) are hostile on hosted remotes — hidden in UI, don't trigger CI, sometimes blocked. Branches work everywhere. *(Spike: verify GitHub behavior for custom namespaces anyway.)* |
| **KEPT** | Go daemon on git plumbing, bare repo | The daemon's whole VCS surface is `ls-remote`, `fetch`, `merge-tree --write-tree`, `commit-tree`, CAS `push` — porcelain-free, no working copy. Requires git ≥ 2.38. |
| **KILLED** | jj as the daemon's VCS backend | jj's value is managing *evolving human work* (working copies, conflicts-as-data, rewrites). The daemon has no working copy and rewrites nothing; jj can't operate bare and adds a binary dep. jj stays first-class **client-side** (`land` alias) and for developing gauntlet itself. |
| **KILLED** | Temporal / durable-workflow engine | The daemon is a **reconcile loop over durable ground truth** (Kubernetes-controller style): desired state re-read from refs, in-flight state is one small fact per run, every action CAS'd and idempotent. Crash recovery = rescan + reattach-or-rerun. Temporal's cluster + Postgres buys nothing at this scale and kills the single-static-binary story. |
| **KEPT** | SQLite for history only | Run records, verdicts, timings — feeds the dashboard and red-rate analysis. Never the source of truth for anything live; refs are. The rule, sharpened: SQLite never holds *correctness* state — only *efficiency* state may ever be derived from it. Boot-time park seeding (`queue.Config.SeedParks`, fed by `internal/history`'s `LatestTerminalPerRef`) is the one place history feeds back into the live queue: a restarted daemon pre-seeds `done` from each ref's latest red verdict so it skips one doomed re-test per still-parked ref. A stale or missing db just costs that re-test back — every seed is re-validated against the ref's CURRENT SHA (the same `syncBookkeeping` check that drops a live park on a re-push) before it's ever trusted, so this can never manufacture a landing or suppress a real one; worst case is a wasted trial-merge-and-check cycle, never a correctness gap. |
| **KEPT** | Persistent warm builder as the *primary* executor model | The fly.io builder insight: a dedicated box (local or cloud) with persistent caches on fast storage (GOCACHE, GOMODCACHE, NuGet, docker images) beats hermetic-ephemeral on speed by a lot. Threat model is our own code, not hostile tenants. Containers (docker / podman / Apple `container`) are wrappers with named cache volumes, not isolation theater. Host docker socket mounted in for testcontainers workloads. |
| **KEPT** | Generic container mounts, not a dedicated docker-socket flag | `executor`'s `mount` (host path + in-container path + optional `readonly`), config-shaped exactly like `cache`: one primitive that happens to cover the docker-socket/testcontainers case, rather than a `docker-socket true`-style special-case knob that only ever does one thing. Covers whatever else a repo's checks need visible from the host filesystem, for free. The trust change a socket mount makes is real and is the operator's own explicit, documented choice, not something this feature quietly enables: mounting `/var/run/docker.sock` hands every check — i.e. anyone who can push a `for/` ref — full control of the host docker daemon, root-equivalent on most setups; docs/setup.md's "Container executor" guide says so bluntly, including that `readonly` does not narrow the socket's own API surface. |
| **KEPT** | Executor as plugin interface | "Run this suite against this tree, return verdict + logs." Impls: local command (v1), container-on-builder, GitHub Actions dispatch-and-await (reuse existing workflow defs at work). What "green" means is the executor's contract; the core never knows. |
| **KEPT** | Channels as the duplex plugin abstraction | Events out (queued / testing / verdict), commands in (retry, cancel, clean-build, status). Slack (socket mode — outbound websocket, no ingress; threading; reaction commands like `:recycle:` = retry), GitHub commit status (PAT, v1) → Checks API (App, later), web dashboard, CLI, stdout — all siblings of one interface. Commands defined by the core; channels transport. |
| **KEPT** | Templated merge commit | Subject `Merge <topic> (<author>)` — the `--first-parent` view should carry information. Trailers for machines: `Gauntlet-Ref:`, `Gauntlet-Run:`, CI URL. Optional Claude-generated summary in the body. Template in per-repo config. |
| **KEPT** | Workload identity lives on the builder host | Azure managed identity / cloud-native federation is a property of where the executor runs, not of the queue. The daemon injects job metadata, not credentials. Daemon-side secrets (Slack, GitHub, Anthropic) from its own store. |
| **KEPT** | Deployments as post-land hooks | A hook stage triggered by the land event, same executor machinery. Keeps the queue core pure; avoids growing a CD system in v1. The hook is a hard scope boundary: when deployment needs grow (health checks, rollback, progressive delivery), the hook *hands off* to a real CD system (Argo CD on k8s, terraform pipelines, whatever the environment runs) — gauntlet never grows one. The commit-status channel (`internal/ghstatus`) deliberately ignores `EventHookFinished` for the same reason: the status describes the *landing*, and a hook failure repainting an already-green landing red would blur that CD hand-off boundary — a failed hook is Slack's and the log channel's job to surface, not ghstatus's. |
| **KILLED** | Config that computes (EDN, or any lisp-shaped config) | Config is dumb data, forever. If config ever needs conditionals/loops/abstraction, the "jobs are commands, no DSL" wall is breached — the fix is moving logic back into repo scripts, not upgrading the config language. (Also binds CUE, if it wins: plain-data mode only.) |
| **KEPT** | KDL for both config files (CUE and TOML rejected) | Head-to-head spike: CUE wins maturity and error messages; KDL decisively wins legibility of the repo-side check spec — the adoption surface every team writes — and one language/one dep beats a split. kdl-go's staleness accepted with mitigations: Go-side validation pass, all parsing isolated in one `config` package unmarshaling to plain structs (swap stays cheap), vendor/fork as last resort. If CUE ever returns: plain-data mode only. |
| **KEPT** | One daemon, N queues | Multiple target branches and multiple repos per instance, config-driven. Cheap now, painful to retrofit. |
| **KILLED** | A job/pipeline DSL | GHA-yaml is a programming language in a data-format costume. A **job is a named command**; structure (matrix, setup, ordering) belongs in the repo's own scripts (shell/make/just). A queue runs multiple named checks (`lint`, `test`, …), each a command; verdict = all green. Buys per-check history and per-check red pings with zero DSL. |
| **KEPT** | Job spec lives in the repo, read from the trial tree | CI definition versions with the code; a candidate that changes its checks is tested by its own definition. Daemon config keeps only operations: remotes, credentials, channels, builders. |
| **KEPT** | Conditional execution is the check script's job, not config's | Monorepo "only web changed" skips: caching first (warm GOCACHE makes affected-only testing *sound* and free for Go), script-level skips second (the executor exports `GAUNTLET_BASE_SHA` / `GAUNTLET_MERGE_SHA` / `GAUNTLET_CANDIDATE_SHA` / `GAUNTLET_REF`, so the condition is repo-owned code; the repo accepts path-filter unsoundness — semantic cross-project breaks — explicitly, per check). Checks can report `skipped` (distinct from `passed`, via a result file gauntlet provides — not exit-code conventions) so history doesn't lie. Path globs in gauntlet config: never. Queue-level batching/speculation is the later answer to slow full suites. |
| **KEPT** | OTel-shaped observability from day one | A run is a trace: root span per run, children for trial-merge, each check, the land. Core emits structured run records (stable run ID; per-check name/verdict/duration) through the OTel API with a no-op provider from phase 1; OTLP exporter is config, phase 3. SQLite stays as the *queryable* local history (dashboard, red-rate) — OTel is export, not storage. |
| **KEPT** | Go-team testing style | Test at the API layer (`ReconcileOnce`, `LoadDaemon` — not internals). **Fakes, not mocks**: test doubles are real implementations with affordances (gated executor, recording channel), and the git layer is exercised against real bare repos, never a stubbed interface. Deterministic stepping via injectable ticks, no wall-clock sleeps. Growth layer: rsc/script-style scenario tests (a tiny command DSL over txtar — `push-candidate` / `tick` / `release-check` / `assert-target`) once the daemon surface stabilizes; the "write the DSL that makes good testing easy" move belongs in tests, exactly where it's banned from config. *(Library decided 2026-07-05 by head-to-head spike: `go-internal/testscript` — actively maintained, per-scenario state via Setup/Values, hermetic env; rsc.io/script is orphaned. Port pattern: one Cmds set, two Setups — fake-git and real-git harnesses run the same scenario files.)* |
| **KEPT** | Full per-check log files (decided 2026-07-05, supersedes the 64KiB-only stance) | The executor tees each check's combined output to `<state>/logs/<runID>/<check>.log` (CheckJob.LogPath, assigned by the queue; empty ⇒ no file). The in-band `CheckResult.Output` stays tail-capped at 64KiB — it's the fast inline view (notifications, run page, history row); the file is the complete record (dashboard "full log" link, API/MCP path). Serving is containment-checked under the log root; retention prunes by age (default 30d). `Event` additionally carries the finished `*CheckResult` on check-finished events so channels can show per-check verdicts mid-run. |
| **KEPT** | Batching and speculation as per-target modes (phase 5) | `batch`: up to max-batch candidates chained into per-candidate `--no-ff` merges, ONE suite on the chain tip, one CAS push lands all (`--first-parent` unchanged: one merge per candidate); red ⇒ per-member skip + serial fallback until the culprit parks; spec-changing members terminate their batch ("tested by its own definition" holds). `speculate`: window of pipelined runs, each on the predicted tip; red ⇒ bubble (suffix re-queues); FIFO landings structurally CAS-enforced. Both tunable with the dashboard's queue-depth data; governor/bisect knobs reserved. docs/design/queue-modes.md is the record. |
| **KILLED** | Persistent staging branch | A second head you reconcile forever; pure contention with fast committers. (Inherited verdict from the original design exploration.) |
| **KEPT** | Speculate has no spec-change boundary | Correct by design, not a missing feature: each speculate window member is an independent run reading its own check spec from its own trial tree, so "tested by its own definition" already holds per-member with no extra bookkeeping. Only `batch` chains multiple candidates through ONE shared suite on the chain tip — that sharing is exactly why *batch* needs a spec-changing-member boundary (see "Batching and speculation as per-target modes" above); speculate was never structurally exposed to the hazard the boundary guards against. |
| **KEPT** | Hook-cancel is out-of-band, not a `core.Command` | A direct closure (`func(string) bool`) wired straight into the dashboard API and MCP, bypassing `drainCommands`/`applyCommand` entirely — deliberate, since a hook stage has no candidate ref to name and so never fit the ref-addressed command model checks/landings use. Slack intentionally has no hook-cancel surface at all: a reaction command is anchored to a run's root message's (target, ref) metadata, and a hook stage has no ref to anchor one to (see docs/config.md's "Hooks" and README's "Operator cancellation"). |
| **KEPT** | Merge-summary text lives only in the git commit message | `summarize`'s `MergeBody` output is inserted straight into the landed merge commit's body; no operator surface — dashboard, JSON API, MCP, Slack — echoes it back separately. An operator reads it the same way as any other commit message, `git show <mergeSHA>`. Consistent across all four surfaces, not an asymmetry. A `RunRecord` echo of the summary text is a possible future enhancement, not a gap. |
| **KEPT** | SIGTERM gives in-flight checks/hooks zero grace | Shutdown sends the process group an immediate SIGKILL — no drain window — unlike the dashboard's own 5s `srv.Shutdown`. Correctness-safe by Invariant 4 (reconcile is idempotent; a killed run just costs one re-test on restart) but behaviorally a crash, not a graceful stop. Operators should expect crash-equivalent shutdown for whatever check or hook was mid-run at SIGTERM, never a drain sequence. |
| **KEPT** | History grows unboundedly by design | `runs`/`checks`/`hooks` rows, and their tail-capped `output` column, are an audit-quality record: never pruned, no `VACUUM`. Only the queue-depth sample series has a retention knob (`history`'s `depth-retention`, default 14 days — see docs/config.md). This is accepted growth, not an oversight; `output` (up to 64KiB/check, stored verbatim) is the bulk column, so a future retention knob — if the `.db` file's unbounded high-water mark ever bites — should target it first, not the run/check rows themselves. |
| **KEPT** | CLI is a thin HTTP client, deliberately | `status`/`retry`/`cancel`/`hooks-cancel`/`land`/`version` only — no `runs`/`run`/`batch`/`checks`/log subcommands. Richer browsing is the dashboard/API/MCP's job (docs/api.md); the CLI exists for the handful of write/porcelain actions worth a bare command, not as a second read surface that has to stay in sync with the other three. |
| **KEPT** | Dashboard auto-refresh via fetch + DOM morph, not `<meta http-equiv="refresh">` | A live page (`/`, `/t/{target}`) polls its own URL every 5s and morphs the fetched body onto the live DOM with vendored idiomorph (id-based diffing), instead of a full reload — so an operator's scroll position and any in-progress text selection survive a refresh tick, and there's no navigation flash, instead of the page blowing away all of that and flashing blank every 5s. (`<details>` isn't an example of preserved state here: idiomorph syncs attributes from the fetched HTML, so a viewer-toggled `open` attribute is stripped right back off on the next morph regardless — see base.html's own comment, and note the pages that morph today have no `<details>` at all; the run page's captured-check-output `<details>` lives only on `/run/{id}`, which never sets `Refresh`.) `<noscript>` carries the old bare meta-refresh as the no-JS fallback; if idiomorph itself fails to load, the poller falls back to `location.reload()` so the page still keeps refreshing either way. History/static pages (`/run/{id}`, `/batch/{id}`, `/checks`) never set `Refresh` and get none of this — they don't auto-refresh at all. |
| **KEPT** | Auto-retry once on infra-error parks (phase-B amendment) | Narrowly amends phase 1's §9.2 "no unbounded retry loops" ruling: an `OutcomeError` park (executor unreachable, service-ensure failure, a service dying mid-run — never a red verdict, never a trial conflict) is automatically cleared and re-queued exactly once per `(ref, SHA)`, through the exact same clear-and-emit machinery an operator's Slack `:recycle:`/API/CLI retry already drives (`internal/queue`'s `maybeAutoRetry`/`clearParkAndRetry`) — Slack threading, history's retry-intent stale-park suppression, and the dashboard all treat it identically to a human retry, with only the event `Detail` telling the two apart. The once-per-SHA budget is in-memory only (bounded by daemon restarts, never an unbounded loop); a second `OutcomeError` for the same SHA stays parked for a human, and a new SHA on the same ref always gets a fresh budget. Config knob `auto-retry-errors`, default **true**; set `false` to fully restore the phase-1 behavior. Motivated by two phase-B pressures that manufacture infra-shaped parks a single retry absorbs: cold-service ready-timeouts (docs/design/services.md "Failure semantics") and, ahead of evictable builders, the ephemeral-worker prerequisite (docs/design/scaling.md). |

## Invariants

The review checklist. Every plan and every implementation gets graded against these.

1. **Land exactly the tested SHA.** The merge commit that was tested is the
   commit that lands — byte-identical, not "re-merge and hope."
2. **CAS everywhere.** Every push to the target is compare-and-swap with the
   expected old OID. A direct human push, a second daemon instance, or a
   replayed step must fail cleanly and trigger re-trial, never corrupt.
3. **Slot deletion is CAS too.** Delete the candidate ref only with the
   expected old OID (the tested SHA). If the author re-pushed mid-test, the
   delete fails and the slot naturally re-queues.
4. **Reconcile is idempotent.** Any step may be repeated after a crash with no
   ill effect. In-flight state is (slot, tested SHA, executor run-id); recovery
   is rescan refs → reattach by run-id or rerun (trial merges are cheap).
5. **Ref moves mid-test are detected**, the running suite is aborted (or its
   verdict discarded), and the slot re-queues at the new SHA.
6. **Never rewrite candidate commits.** No rebase, no squash, no message
   mutation. The only new object gauntlet creates is the merge commit.
7. **Cache escape hatch exists.** A clean-build command (config + channel
   command) for suspected cache poisoning on the warm builder.
8. **The queue core is executor- and channel-agnostic.** It sees interfaces;
   adding Slack or Actions touches no core logic.

## Architecture (phase-1 shape)

```
             ┌───────────────────────────── daemon ─────────────────────────────┐
 git remote  │  intake            reconcile loop              executor (iface)  │
 for/* refs ─┼─ ls-remote ─→ queue state ─→ trial merge ──→ run suite on tree ──┼─→ verdict
             │      ↑          (in memory,     (merge-tree,                     │
             │      └── poll    re-derived)     commit-tree)                    │
             │                        │                                         │
             │                        ├─ green → CAS push target · CAS delete slot
             │                        └─ red   → keep target · notify author    │
             │                   channels (iface): events out / commands in     │
             └──────────────────────────────────────────────────────────────────┘
```

## Build phases

1. **Core loop, local-only.** Watch `for/*` refs, trial-merge, run the named
   checks defined by a KDL file in the trial tree, CAS land, stdout channel.
   Structured run records (run ID; per-check name/verdict/duration) in the
   event model via the OTel API (no-op provider). End-to-end usable against any
   git remote; tested entirely with local bare repos. No SQLite, no container,
   no network services.
2. **Executors & channels.** Container wrapper (docker/podman/Apple
   `container`) with persistent cache volumes; GitHub Actions
   dispatch-and-await; Slack (socket mode, threads, reaction commands); GitHub
   commit status.
3. **Dashboard + SQLite history + OTLP export.** Read-only web UI; queue
   state, run history, red-rate (per check). Bind localhost/tailnet; auth is
   your proxy's job. OTLP span exporter as a config option — same run records,
   exported instead of stored.
4. **Porcelain & polish.** `land` one-worder for git and jj; post-land hooks
   (deployments); Claude merge summaries; speculation if queue-depth data
   demands it.

## Watch items

- **Event shapes are the soft underbelly.** Two review cycles found the same
  family: `EventTrialClean` shipped without the `RunID` its consumers join on
  (phase-2/3 review, ship-blocker), and `EventCheckFinished` originally
  carried no `CheckResult`, so channels couldn't show per-check verdicts
  mid-run — **resolved**: `Event.Check *CheckResult` (`internal/core/types.go`)
  is now set on every `EventCheckFinished`, and hooks/dashboard consume it.
  The emit-site contract ("terminal events carry a Record"; "run-scoped
  events carry the run ID") is now partially test-enforced; when events next
  grow, extend those contract tests first — event shapes have broken twice
  already and are still the part of the design most likely to break a third
  time.
- **`core.Command` carries no SHA** — a delayed retry clears whatever park
  currently exists at the ref. Benign today (parks are keyed to the current
  SHA and a re-push already clears them); matters if commands ever queue for
  long or gain more destructive kinds. `core.CommandCancel` (manual operator
  cancellation) is now that more-destructive kind, and inherits this exact
  gap unchanged: same by-ref, no-SHA addressing, same benign consequence.
- **RESOLVED — batch members shared one `run_id`, so history kept only the
  last member's row.** The queue used to reuse one RunID verbatim across
  every member of a batch (it doubles as BatchID), and `runs.run_id`'s
  `INSERT OR REPLACE` PRIMARY KEY meant each member's terminal event
  clobbered the previous member's row (fresh-context review, 2026-07,
  confirmed empirically: 3 member events sharing one run_id left exactly 1
  row). Fixed by `memberRunID` (`internal/queue/reconcile.go`): position 0
  keeps the bare `batchRunID`, position >0 gets `<batchRunID>-mN` — every
  member now carries a distinct RunID while BatchID stays the shared
  grouping key, so per-member history/dashboard rows and boot-time park
  seeding (`LatestTerminalPerRef`) both see every member, not just the last.
- **RESOLVED — park-seed resurrection edge** (`queue.Config.SeedParks`,
  Feature 2): retrying a parked ref, then restarting the daemon before any
  new verdict landed for it, used to re-park the ref on boot — the retry
  cleared the in-memory park, but history's latest row for that (ref, SHA)
  was still the old red verdict, and `SeedParks` trusted it. Fixed by the S3
  `retry_intents` suppression: `applyRetry` now emits `EventRetryRequested`,
  history writes a `retry_intents` row, and `LatestTerminalPerRef` LEFT JOINs
  it, dropping the seed whenever the retry is newer than the terminal row's
  `ended_at` (`internal/history/queries.go`) — exactly the scenario above.
- **`extractTar` writes symlink entries verbatim** — a candidate tree can
  plant a symlink escaping the export dir that a later check follows. Within
  the own-code threat model; revisit if the threat model widens.
- **Trial-tree exports carry no `.git`** (git-archive), so affected-only check
  scripts can't `git diff` the exported coordinates without their own object
  store. Options if this bites: export via clone instead, or mount the bare
  repo read-only alongside.
- **`TestScriptReal` occasionally hung forever under `-race`** (macOS,
  ~1-in-8 runs under load, never observed without `-race`, pre-phase-5
  environmental issue): goroutine dumps on timeout showed the parent stuck
  in `syscall.forkExec` → `readlen`, blocked reading the exec-status pipe of
  a `git` child that never reached exec — testscript unconditionally runs
  every scenario's real-git-spawning subtest in parallel, and forking a
  child out of a heavily-threaded, TSan-instrumented process can wedge the
  child pre-exec on a copied-in lock. Mitigated by serializing
  `TestScriptReal`'s scenarios only when built with `-race`
  (`internal/queue/race_test.go`, `serialScriptT` in `script_test.go`);
  non-race builds are unaffected. If CI on Linux ever shows this hang,
  re-open — the theory is macOS-specific fork/TSan behavior, not portable.

First live run (crashtest demo, 2026-07-05) surfaced three more:

- **Red pings need the failing output.** A rejection's detail says `check "test"
  failed`; the actually-useful line (`airbag: deploy at 148ms, want <= 25ms`)
  lives in the RunRecord's tail-capped Output. History *does* store it
  (schema v2's `checks.output` column) — the gap is that terminal channel
  notifications (Slack, GitHub status, the log channel) still don't include
  it. Channels should include the failing check's output tail in terminal
  notifications; the data is already there, only the surfacing is missing.
- **kdl-go rejects single-line child blocks** (`check "vet" { command ... }` on
  one line) and reports the error at "line 0". Adopters will write the
  single-line form; docs must show multi-line only, and the parse-error paths
  should be made friendlier (or the parser quirk fixed/reported upstream).
- **Apple `container` has no named volumes** — cache "volumes" work as
  host-path bind mounts (an absolute path in the cache name slot). Config
  semantics should acknowledge both forms explicitly per runtime.
- **`summarize`'s Messages API call runs synchronously on the reconcile
  loop**, before checks start, once per clean trial — its `timeout`
  (default 5s, down from 10s) bounds a stall of *every* target's
  reconciliation, not just the one being summarized. Fine while it's a
  single small call with a tight timeout; revisit (async summarize, or
  move it off the reconcile loop) if it ever grows slower or less optional.

## Open spikes

- Config language head-to-head: `sblinch/kdl-go` (fitness, KDL 2.0 status) vs
  `cuelang.org/go` (ergonomics in plain-data mode, error-message quality).
- GitHub behavior for pushes to non-`refs/heads` custom namespaces (confirm the
  branches-not-refs decision).
- `git merge-tree --write-tree` conflict-reporting details across git versions
  (minimum version pin).
- Slack socket-mode rate limits / reconnect behavior (phase 2).

## Origins

Distilled from an earlier design exploration for a work integration branch
(ref-slot model, ephemeral trial merge, detection-before-gating) and research
notes on the merge-queue landscape (bors/Tide lineage for PR-as-unit gating;
Zuul for the speculative growth path; spindle for executor isolation patterns;
Gerrit rejected for commit-as-review-atom). Gauntlet generalizes that design
into an open-source tool.
