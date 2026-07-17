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
| **KEPT** | Conditional execution needs an object store, not a computed diff (decided 2026-07-13) | The affected-only pattern was handed SHAs it couldn't resolve: trial trees are `git archive` exports with no `.git`, so a `git diff base..merge` needed a repo-maintained clone — the watch item bit once batch adopters wanted diff-based skipping and content-keyed test caching. Fix: every check gets `GAUNTLET_GIT_DIR`, the daemon's own bare repo (it already holds base, candidate, AND the trial merge commit — created by `commit-tree` before any push, so it resolves even for runs that never land, including speculate's predicted bases). Local executor exports the absolute host path; container executor auto-mounts it read-only at the FIXED `/gauntlet-git` (a constant like `/workspace` and `/gauntlet`, never host-derived — path-keyed build caches like Go's stay stable across builders; the path is reserved against operator mounts exactly like the other two). Hooks inherit it for free (same executors). The daemon-computes-a-changed-files-list alternative was rejected: strictly less powerful (no `git log`-keyed caching) and it would creep diff policy into the daemon — handing the object store keeps "gauntlet hands you SHAs, the repo owns the logic" intact. Trust, stated honestly: the mounted git dir's `config` carries the remote URL, so a credential embedded in it (vs. a credential helper) is readable by container checks that previously couldn't read it off disk; docs/checks.md says so bluntly. Batch stays the good case: base..merge is exactly the chain the verdict covers. |
| **KEPT** | Candidate-built check images: opaque build command, immutable captured identity (decided 2026-07-17, issue #2) | A trial that changes its Dockerfile proves that change in the same merge transaction. Gauntlet owns exactly five things — schedule the named build once per run, hand it trial identity + `GAUNTLET_IMAGE_RESULT_FILE` (INSTEAD of the check result file: builds have no skipped verdict, the protocols never conflate), validate/canonicalize the captured identity, run every consumer by it, record it — and deliberately never learns Dockerfiles, contexts, build args, or cache keys (BuildKit owns layer caching; duplicate concurrent builds are correct-if-wasteful; a pruned image rebuilds). The mechanism is the scheduler we already had: a build is a synthetic `image:<name>` node (prefix reserved against check names) in the run's dependency graph with an implicit edge onto each consumer — readiness, max-parallel, the execution cap, fail-fast, blocked rows, history rows, and events all treat it as a check, zero second-scheduler code. Identity validation is strict at the seam: exactly one `sha256:<64hex>` local ID or `<repo>@sha256:<64hex>` digest reference; a mutable tag is rejected outright rather than resolved (accept-then-resolve would re-open the exact TOCTOU the feature closes), and an invalid result flips the BUILD node red before storing/eventing — one root cause, consumers block on it, never N consumer failures. Consumers must sit on container-kind profiles (config-owned predicate gate at spec load, like unknown profiles); builds run on ordinary operator profiles and structurally cannot consume a candidate image (config.Image has no image field — no recursive bootstrap). A declared-but-unconsumed image still builds and gates: a Dockerfile-only candidate proves its build. History v10 records the identity on build and consumer rows alike ("explain what ran"). Validation is syntactic only, stated honestly: gauntlet does not probe the runtime for existence/runnability before releasing consumers — a result that names a well-formed but absent image surfaces as the first consumer's infra error (park-as-error, auto-retried once), and probing would add a runtime round-trip per build for a failure mode only a lying build command can produce. Registry-digest results are accepted from day one so horizontal executors need no new spec later. |
| **KEPT** | Named executor profiles, selected by name from the repo spec (decided 2026-07-17, issue #3) | One daemon-global executor forced an all-or-nothing choice; now `executor "name" kind="local"\|"container" {...}` blocks declare profiles beside the (at most one) kind-less legacy/default block, and a check says `executor "name"` — mixing containerized and host-local checks in one candidate. The capability boundary is the whole point: the repo side can only NAME a profile; every host grant (mounts, image, fixed env, `add-host`, `--memory`/`--cpus`) stays operator-owned, typed options only — no `extra-args` escape hatch, ever (it would be arbitrary per-check Docker flags with one level of indirection). Defining a profile IS the allow-list; profiles are guardrails, not a sandbox (a socket mount stays host-authoritative — same trust math as before). Names `local`/`container` rejected: that argument spelling means the default block's KIND, and one word meaning two things across the two forms is how operators get burned. Routing lives in `executor.Mux` on `CheckJob.Executor`; the queue core stays executor-agnostic (Invariant 8) and gates specs with a config-owned known-profile predicate at spec load — an unknown selection parks as a configuration rejection before any command starts, never a red verdict. Fixed profile `env` sits BEFORE the GAUNTLET_*/service env so gauntlet's contract wins collisions, and GAUNTLET_-prefixed names are rejected in config outright. `max-executions` moved to the top level in the same change — it's a host budget spanning every profile, not a property of one block — with the old executor-block spelling still adopted on load (it was briefly the documented location; configs written then must not break). Two deliberate tightenings, disclosed: container-only options (image/mount/cache/add-host/memory/cpus/runtime/workdir) on a kind-local block now reject loudly where they previously parsed-and-were-ignored — silently-inert config is how operators get surprised later — and the services machinery still follows the DEFAULT executor (kind picks endpoint mode, RUNTIME owns the shared network), so a `needs` check on a kind- or runtime-mismatched profile may get unreachable endpoints; both docs say so bluntly — revisit if it bites. |
| **KEPT** | Dependency-aware parallel checks: `after` edges + `max-parallel` + `max-executions` (decided 2026-07-17, issue #1) | The "job is a named command, no DSL" wall stands — `after` (check-level ordering edges) plus a per-candidate `max-parallel` (repo spec, default 1 = byte-identical serial declaration order) is the WHOLE grammar; conditions/matrices/fan-out stay inside repo commands. Three controls deliberately separated: speculation depth (`window`), candidate parallelism (`max-parallel`), and daemon capacity (`executor max-executions`, a plain scalar count over checks + hooks — image builds join it when #2 lands — via one shared core.Slots semaphore; service containers excluded, their own limits apply; default unlimited for compatibility, explicit cap recommended in production since window×max-parallel is unbounded otherwise). Scheduling: ready = every `after` edge finished green (passed/skipped); starts in spec order up to both caps; first red/errored result — consumed in spec order, deterministic — becomes run.culprit, the EXPLICIT root failure (the old last-row-is-culprit inference died with short-circuiting), and the run fails fast: in-flight siblings are cancelled, and every unfinished check is recorded CheckBlocked (a fourth CheckStatus) with BlockedBy naming its failed edges or the culprit — one row per DECLARED check, in SPEC-DECLARATION order (the durable seq/log identity; start/finish order is runtime data, recorded as timestamps, never an identity). Blocked ≠ skipped, on purpose: skipped is a check's own successful nothing-to-do verdict and counts green; blocked means the command never ran. Graph validation (unknown/self/duplicate/cycle edges) runs even at max-parallel 1 so raising it later can't reveal a latent bad graph. CheckResult.Waited (history waited_ms) separates slot starvation from command cost. Fairness: per-tick target-rotation, only under a configured cap — uncapped daemons keep the fixed iteration order and its byte-identical event stream. |
| **KEPT** | GC pins anchor in-flight trial chains (decided 2026-07-16, overturns queue-modes.md's "refless" out-of-scope entry) | The refless stance was sound while only the daemon's own next few ticks ever resolved a chain link — gc.pruneExpire's two-week grace covered objects that lived minutes. `GAUNTLET_GIT_DIR` broke that premise: checks and hooks now resolve the merge commit out of the bare repo for a run's whole lifetime, so reachability became part of the check contract, and an operator's `gc --prune=now` (docs/deploy.md used to forbid it outright, with spurious mid-run failures as the stated blast radius) could reap a live run's objects. Fix: each run pins its chain TIP at `refs/gauntlet/pin/<tip>` right after `commit-tree`, before anything reads through the object — one ref covers the whole chain, since a commit reaches its parents (batch links, member SHAs, the base). Released by `finalizeRun` on every ordinary terminal (reject paths release inline); a LANDING's pin — and an AMBIGUOUS land-push failure's, where the push may have applied server-side — instead outlives the run in `landedPins`, released only once a fetch shows the tip actually REACHABLE from the remote-tracking target ref (IsAncestor, not merely "a fetch succeeded": a lagging replica can serve the pre-push tip while a queued hook still needs the merge; an entry that never anchors rides until the startup sweep). Startup sweeps the whole namespace (crash-stranded pins protect nothing a fresh process needs — Invariant 4). Pins are refs, not objects, so Invariant 6 stands; the namespace sits outside refs/heads/ and refs/remotes/, invisible to Fetch's refspec, `--prune`, and ListRefs, so queue state derivation is untouched. `gc --prune=now` is now safe for every in-flight RUN (proven by a real-git mid-check test) — what remains, stated honestly in deploy.md, is the inherent create-then-reference window between `commit-tree` and the pin (widest during batch chain formation), the same race expire-now pruning has with any concurrent `git commit`; its blast radius is a spurious auto-retried infra park, so cron gets plain `git gc` (grace period covers the window) and `--prune=now` stays a quiesced-repo tool. |
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
| **KILLED** | Break-glass force-land command (decided 2026-07-07) | No `:break-glass:` reaction, no `land --force`, no API endpoint that skips checks — ever. The escape hatch already exists outside the daemon and is better there: push directly to the target with your own git credentials. The queue treats an external tip move as a first-class event (invalidation, re-trial, crash recovery all fire), and git plus the host's audit log attribute the push per-user. An in-daemon bypass would break the invariant the no-auth trust model leans on (docs/api.md: nothing in the API force-pushes, force-lands, or bypasses a check) — anyone who could reach the dashboard port could skip CI, strictly weaker authn than a push. Mechanically it buys nothing either: under batch/speculate a force-land is just a tip move, firing the same invalidation a direct push does. Flake recovery is a separate, already-solved problem: retry (`:recycle:`/API/CLI) plus auto-retry-once. If a deployment ever locks the target branch to the daemon's token, the break-glass belongs in branch protection (admin bypass list), not the queue — an emergency landing must not depend on the queue daemon being healthy, which is exactly when glass gets broken. |
| **KEPT** | GitHub App auth as one refreshable provider; git credentials via ephemeral askpass (issue #6) | A long-running daemon can't live on a job-scoped token or a startup-read string: `ghauth` mints App JWTs (stdlib RS256 — three fixed claims don't justify a JWT dependency) and exchanges them for installation tokens lazily, cached to a 2-minute pre-expiry window, singleflighted, invalidate-and-retry-ONCE on a clear 401 (never on 403 — permission isn't expiry, and retrying arbitrary auth failures forever is forbidden). One provider feeds both consumers: ghstatus requests a token per POST, and gitx authenticates fetch/push through an ephemeral `GIT_ASKPASS` helper — chosen over `-c http.extraHeader` (argv, ps-visible) and over URL-embedded credentials (the ledger already flags those as readable by checks via the mounted `GAUNTLET_GIT_DIR`). The helper is secretless (token rides in the git subprocess env, script scoped to the configured host so an unexpected redirect gets nothing) and removed on success, failure, and cancellation alike. App mode hard-requires an HTTPS remote canonicalizing to the github block's host **and** owner/repo — mismatch is a startup error, never a silent ambient fallback; static-PAT mode keeps today's independent ambient git behavior, unchanged. Private-key rotation = restart; token refresh ≠ restart. Scope: auth only — statuses API unchanged (Checks API migration is separate later work; #7 composes on this provider). |
| **KEPT** | Deterministic per-path mtimes, opt-in and operator-owned (issue #5) | Trial trees are plain exports, so file mtimes are extraction wall time — a guaranteed miss for tools that key caches on path+metadata instead of content. Top-level daemon config `export { mtimes "history" }` turns on a git-restore-mtime pass over every export (checks, candidate image builds, hooks): each tracked file gets the committer time of the last commit that changed it, computed against the exact synthetic merge, so re-exports are metadata-identical. Semantics pinned by test: committer time (future-dated commits verbatim — deterministic beats plausible; a cache may decline reuse until the clock catches up, which beats nondeterministic clamping); a merge owns a path only if it changed vs **all** parents (auto-merge products — including the trial merge itself — get the merge's time, everything else keeps its deeper history time; git emits *no* diff entry for a tree-identical parent, so the walker counts entries against `%P`'s parent count — the up-to-date-candidate trial merge, whose tree equals the candidate's, owns nothing and re-trials restamp identically); renames are changes at the new path; symlinks stamped without following (`lutimes`); directories untouched (git tracks no dir metadata, nothing documented keys on it); the pending set is what the export actually materialized (dir walk, never `ls-tree` — `export-ignore` and gitlinks make the tree listing an over-approximation). The log stream is parsed as NUL-delimited tokens with positional state (NUL is the only byte a path can't contain; the `\x01` header sentinel is only trusted at token start), `--diff-merges=separate` is spelled explicitly (`-m` bends under host `log.diffMerges` config), and when the stream ends on its own the subprocess's exit status is checked — a dying `git log` can truncate output in ways that still parse. One disclosed approximation, matching the standard `git-restore-mtime` tool: "last commit that changed the path" resolves in commit-date order across the whole DAG, so a merge that discarded one side's change can leave the discarded-but-newer commit winning the stamp where `git log -1 -- path` follows the surviving lineage — still fully deterministic per commit (the property caches key on); doing better needs per-path history simplification, which one no-pathspec walk cannot express. Cost: one early-terminating `git log --name-status` subprocess per export — v1 is deliberately **uncached**, with the walk's size on the run's OTel span (`gauntlet.mtimes.*`), so a cache is added if measurement says so, not preemptively. Failure is an `OutcomeError` trial rejection — **no silent wall-clock fallback**, a tree claiming stable metadata must never quietly not have it. Operator config, not repo spec: the repo must not impose a host-side history walk on the daemon. |

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
  time. (CheckBlocked's introduction deliberately added NO event kind:
  blocked rows exist only in the terminal Record's Checks slice — a check
  that never ran emits no started/finished pair — so the emit-site
  contracts were untouched.)
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
- **RESOLVED — trial-tree exports carry no `.git`** (git-archive), so
  affected-only check scripts couldn't `git diff` the exported coordinates
  without maintaining their own object store. It bit (batch-mode adopters
  wanting diff-based test skipping and content-keyed caching), and the
  "mount the bare repo read-only alongside" option won: every check now
  gets `GAUNTLET_GIT_DIR` (see the "Conditional execution needs an object
  store" ledger entry). The clone-instead-of-archive alternative was
  rejected — slower per run, puts a writable `.git` inside the check's
  tree, and a shared-object clone needs the bare repo visible anyway.
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
