# Gauntlet — a merge queue

**Status:** design settled, phase 1 planning · **Date:** 2026-07-04

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
  tip-as-it-will-be is the point. Zuul-style speculative pipelining is the
  designed-in growth path, built only if measured queue depth hurts.
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
| **KEPT** | Executor as plugin interface | "Run this suite against this tree, return verdict + logs." Impls: local command (v1), container-on-builder, GitHub Actions dispatch-and-await (reuse existing workflow defs at work). What "green" means is the executor's contract; the core never knows. |
| **KEPT** | Channels as the duplex plugin abstraction | Events out (queued / testing / verdict), commands in (retry, cancel, clean-build, status). Slack (socket mode — outbound websocket, no ingress; threading; reaction commands like `:recycle:` = retry), GitHub commit status (PAT, v1) → Checks API (App, later), web dashboard, CLI, stdout — all siblings of one interface. Commands defined by the core; channels transport. |
| **KEPT** | Templated merge commit | Subject `Merge <topic> (<author>)` — the `--first-parent` view should carry information. Trailers for machines: `Gauntlet-Ref:`, `Gauntlet-Run:`, CI URL. Optional Claude-generated summary in the body. Template in per-repo config. |
| **KEPT** | Workload identity lives on the builder host | Azure managed identity / cloud-native federation is a property of where the executor runs, not of the queue. The daemon injects job metadata, not credentials. Daemon-side secrets (Slack, GitHub, Anthropic) from its own store. |
| **KEPT** | Deployments as post-land hooks | A hook stage triggered by the land event, same executor machinery. Keeps the queue core pure; avoids growing a CD system in v1. The hook is a hard scope boundary: when deployment needs grow (health checks, rollback, progressive delivery), the hook *hands off* to a real CD system (Argo CD on k8s, terraform pipelines, whatever the environment runs) — gauntlet never grows one. |
| **KILLED** | Config that computes (EDN, or any lisp-shaped config) | Config is dumb data, forever. If config ever needs conditionals/loops/abstraction, the "jobs are commands, no DSL" wall is breached — the fix is moving logic back into repo scripts, not upgrading the config language. (Also binds CUE, if it wins: plain-data mode only.) |
| **KEPT** | KDL for both config files (CUE and TOML rejected) | Head-to-head spike (docs/plans/phase1.md §7): CUE wins maturity and error messages; KDL decisively wins legibility of the repo-side check spec — the adoption surface every team writes — and one language/one dep beats a split. kdl-go's staleness accepted with mitigations: Go-side validation pass, all parsing isolated in one `config` package unmarshaling to plain structs (swap stays cheap), vendor/fork as last resort. If CUE ever returns: plain-data mode only. |
| **KEPT** | One daemon, N queues | Multiple target branches and multiple repos per instance, config-driven. Cheap now, painful to retrofit. |
| **KILLED** | A job/pipeline DSL | GHA-yaml is a programming language in a data-format costume. A **job is a named command**; structure (matrix, setup, ordering) belongs in the repo's own scripts (shell/make/just). A queue runs multiple named checks (`lint`, `test`, …), each a command; verdict = all green. Buys per-check history and per-check red pings with zero DSL. |
| **KEPT** | Job spec lives in the repo, read from the trial tree | CI definition versions with the code; a candidate that changes its checks is tested by its own definition. Daemon config keeps only operations: remotes, credentials, channels, builders. |
| **KEPT** | Conditional execution is the check script's job, not config's | Monorepo "only web changed" skips: caching first (warm GOCACHE makes affected-only testing *sound* and free for Go), script-level skips second (the executor exports `GAUNTLET_BASE_SHA` / `GAUNTLET_MERGE_SHA` / `GAUNTLET_CANDIDATE_SHA` / `GAUNTLET_REF`, so the condition is repo-owned code; the repo accepts path-filter unsoundness — semantic cross-project breaks — explicitly, per check). Checks can report `skipped` (distinct from `passed`, via a result file gauntlet provides — not exit-code conventions) so history doesn't lie. Path globs in gauntlet config: never. Queue-level batching/speculation is the later answer to slow full suites. |
| **KEPT** | OTel-shaped observability from day one | A run is a trace: root span per run, children for trial-merge, each check, the land. Core emits structured run records (stable run ID; per-check name/verdict/duration) through the OTel API with a no-op provider from phase 1; OTLP exporter is config, phase 3. SQLite stays as the *queryable* local history (dashboard, red-rate) — OTel is export, not storage. |
| **KEPT** | Go-team testing style | Test at the API layer (`ReconcileOnce`, `LoadDaemon` — not internals). **Fakes, not mocks**: test doubles are real implementations with affordances (gated executor, recording channel), and the git layer is exercised against real bare repos, never a stubbed interface. Deterministic stepping via injectable ticks, no wall-clock sleeps. Growth layer: rsc/script-style scenario tests (a tiny command DSL over txtar — `push-candidate` / `tick` / `release-check` / `assert-target`) once the daemon surface stabilizes; the "write the DSL that makes good testing easy" move belongs in tests, exactly where it's banned from config. *(Library decided 2026-07-05 by head-to-head spike: `go-internal/testscript` — actively maintained, per-scenario state via Setup/Values, hermetic env; rsc.io/script is orphaned. Port pattern: one Cmds set, two Setups — fake-git and real-git harnesses run the same scenario files.)* |
| **KEPT** | Full per-check log files (decided 2026-07-05, supersedes the 64KiB-only stance) | The executor tees each check's combined output to `<state>/logs/<runID>/<check>.log` (CheckJob.LogPath, assigned by the queue; empty ⇒ no file). The in-band `CheckResult.Output` stays tail-capped at 64KiB — it's the fast inline view (notifications, run page, history row); the file is the complete record (dashboard "full log" link, API/MCP path). Serving is containment-checked under the log root; retention prunes by age (default 30d). `Event` additionally carries the finished `*CheckResult` on check-finished events so channels can show per-check verdicts mid-run. |
| **KILLED** | Persistent staging branch | A second head you reconcile forever; pure contention with fast committers. (Inherited verdict from the original design exploration.) |

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
  (phase-2/3 review, ship-blocker), and `EventCheckFinished` carries no
  `CheckResult`, so channels can't show per-check verdicts mid-run. The
  emit-site contract ("terminal events carry a Record"; "run-scoped events
  carry the run ID") is now partially test-enforced; when events next grow,
  extend those contract tests first.
- **`core.Command` carries no SHA** — a delayed retry clears whatever park
  currently exists at the ref. Benign today (parks are keyed to the current
  SHA and a re-push already clears them); matters if commands ever queue for
  long or gain more destructive kinds. `core.CommandCancel` (manual operator
  cancellation) is now that more-destructive kind, and inherits this exact
  gap unchanged: same by-ref, no-SHA addressing, same benign consequence.
- **Batch members share one `run_id`, so history keeps only the last
  member's row.** The queue reuses one RunID verbatim across every member of
  a batch (it doubles as BatchID), but `runs.run_id` is the history table's
  PRIMARY KEY and writes are `INSERT OR REPLACE` — each member's terminal
  event clobbers the previous member's row (fresh-context review, 2026-07,
  confirmed empirically: 3 member events sharing one run_id leave exactly 1
  row). `history.Store`'s own tests fixture distinct per-member RunIDs
  sharing a BatchID — the shape the schema assumes; the queue never
  produces it. Effects: per-member batch history/dashboard rows are
  silently lost, and boot-time park seeding (`LatestTerminalPerRef`) only
  ever sees a past batch's last-emitted member — benign for correctness (a
  missing row just means "no seed") but it defeats the seeding benefit for
  most batch members. Likely fix: mint each member its own RunID and keep
  BatchID as the grouping key.
- **Park-seed resurrection edge** (`queue.Config.SeedParks`, Feature 2): retry
  a parked ref, then restart the daemon before any new verdict lands for it
  — the retry cleared the in-memory park, but history's latest row for that
  (ref, SHA) is still the old red verdict, so `SeedParks` re-parks it on
  boot. Rare (needs a restart racing right after a retry, before the next
  reconcile pass even runs) and self-healing (another retry clears it again,
  exactly as it did the first time) — no correctness impact either way
  (Invariant 4 still holds; this only ever costs one extra doomed-retest
  cycle avoided or one extra retry needed), but worth knowing about if an
  operator reports a just-retried ref looking parked again after a restart.
- **`extractTar` writes symlink entries verbatim** — a candidate tree can
  plant a symlink escaping the export dir that a later check follows. Within
  the own-code threat model; revisit if the threat model widens.
- **Trial-tree exports carry no `.git`** (git-archive), so affected-only check
  scripts can't `git diff` the exported coordinates without their own object
  store. Options if this bites: export via clone instead, or mount the bare
  repo read-only alongside.

First live run (crashtest demo, 2026-07-05) surfaced three more:

- **Red pings need the failing output.** A rejection's detail says `check "test"
  failed`; the actually-useful line (`airbag: deploy at 148ms, want <= 25ms`)
  lives only in the RunRecord's tail-capped Output, which history deliberately
  doesn't store. Channels should include the failing check's output tail in
  terminal notifications.
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
