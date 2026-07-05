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
| **KEPT** | SQLite for history only | Run records, verdicts, timings — feeds the dashboard and red-rate analysis. Never the source of truth for anything live; refs are. |
| **KEPT** | Persistent warm builder as the *primary* executor model | The fly.io builder insight: a dedicated box (local or cloud) with persistent caches on fast storage (GOCACHE, GOMODCACHE, NuGet, docker images) beats hermetic-ephemeral on speed by a lot. Threat model is our own code, not hostile tenants. Containers (docker / podman / Apple `container`) are wrappers with named cache volumes, not isolation theater. Host docker socket mounted in for testcontainers workloads. |
| **KEPT** | Executor as plugin interface | "Run this suite against this tree, return verdict + logs." Impls: local command (v1), container-on-builder, GitHub Actions dispatch-and-await (reuse existing workflow defs at work). What "green" means is the executor's contract; the core never knows. |
| **KEPT** | Channels as the duplex plugin abstraction | Events out (queued / testing / verdict), commands in (retry, cancel, clean-build, status). Slack (socket mode — outbound websocket, no ingress; threading; reaction commands like `:recycle:` = retry), GitHub commit status (PAT, v1) → Checks API (App, later), web dashboard, CLI, stdout — all siblings of one interface. Commands defined by the core; channels transport. |
| **KEPT** | Templated merge commit | Subject `Merge <topic> (<author>)` — the `--first-parent` view should carry information. Trailers for machines: `Gauntlet-Ref:`, `Gauntlet-Run:`, CI URL. Optional Claude-generated summary in the body. Template in per-repo config. |
| **KEPT** | Workload identity lives on the builder host | Azure managed identity / cloud-native federation is a property of where the executor runs, not of the queue. The daemon injects job metadata, not credentials. Daemon-side secrets (Slack, GitHub, Anthropic) from its own store. |
| **KEPT** | Deployments as post-land hooks | A hook stage triggered by the land event, same executor machinery. Keeps the queue core pure; avoids growing a CD system in v1. |
| **KEPT** | KDL for config | Queues / channels / executors nest naturally as KDL nodes; reads better than TOML for trees of typed things. Risk: thin Go ecosystem. Mitigation: all parsing isolated in one `config` package unmarshaling to plain structs; syntax is swappable. *(Spike: `sblinch/kdl-go` fitness, KDL 2.0 status.)* |
| **KEPT** | One daemon, N queues | Multiple target branches and multiple repos per instance, config-driven. Cheap now, painful to retrofit. |
| **KILLED** | A job/pipeline DSL | GHA-yaml is a programming language in a data-format costume. A **job is a named command**; structure (matrix, setup, ordering) belongs in the repo's own scripts (shell/make/just). A queue runs multiple named checks (`lint`, `test`, …), each a command; verdict = all green. Buys per-check history and per-check red pings with zero DSL. |
| **KEPT** | Job spec lives in the repo, read from the trial tree | CI definition versions with the code; a candidate that changes its checks is tested by its own definition. Daemon config keeps only operations: remotes, credentials, channels, builders. |
| **KEPT** | OTel-shaped observability from day one | A run is a trace: root span per run, children for trial-merge, each check, the land. Core emits structured run records (stable run ID; per-check name/verdict/duration) through the OTel API with a no-op provider from phase 1; OTLP exporter is config, phase 3. SQLite stays as the *queryable* local history (dashboard, red-rate) — OTel is export, not storage. |
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

## Open spikes

- `sblinch/kdl-go` fitness; KDL 2.0 support status.
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
