# Deployment: desired-state dispatch over deploy refs

**Status:** shipped (D1–D4, 2026-08-09), rev 2 (environment lanes + node
graph) · **Date:** 2026-08-08

Gauntlet's answer to deployment today is the post-land hook — fire commands
on the land event, coalesce a backlog, hand off to a real CD system when
needs grow. That answer is correct for *reactions to a landing* and wrong
for *deployment as an ongoing concern*: a hook is edge-triggered and keyed
to the merge that just landed, so there is no surface anywhere in the
daemon to say "environment `prod`, revision `X`, now" — no older revision,
no re-deploy, no promotion between environments, no per-environment view.
This doc designs that surface.

The requirements it serves:

- **Multiple environments** (`dev`, `prod`, `prod2`), deployment decoupled
  from landing, each environment fed from its own source.
- **A deploy is a graph, not a command.** Within an environment: first a
  migration job migrates the environment's databases, *then* the apps
  deploy in parallel. Across environments: `dev` deploys first; `prod` and
  `prod2` follow, in parallel with each other.
- **Arbitrary revisions** — deploy an older SHA, re-deploy the current one,
  promote what `dev` runs to `prod`.
- **Operator UI** — an overview of every environment, and full per-node
  logs, on the existing dashboard.

The scope boundary from the decision ledger ("Deployments as post-land
hooks") survives, restated more precisely at the end: gauntlet gains a
deploy **dispatcher** — schedule named commands against a revision for an
environment, record the outcomes — and still never grows a CD
**controller**. It never learns whether an environment is healthy, never
decides to roll back, never shapes traffic. Those stay in the repo's deploy
scripts or the CD system they invoke.

## The model: desired state in refs, level-triggered

The merge queue's own discipline applies unchanged: durable ground truth
lives in refs on the remote, and the daemon is a reconcile loop that
re-derives everything else. Two refs per environment:

- **Desired** — `refs/heads/deploy/<env>`, an ordinary branch, human- or
  daemon-moved. Whatever commit it points at is what the environment
  should be running. Deploying is pushing this ref; rolling back is
  pushing it to an older SHA; promotion is pushing it to the SHA another
  environment runs.
- **Observed** — `refs/gauntlet/deployed/<env>`, daemon-owned, CAS-moved
  only after the environment's *entire* deploy graph finishes green. The
  last revision known to be fully deployed.

**The environment is the unit, deliberately.** An earlier revision of this
design gave every (env, app) pair its own desired ref, for independent
per-app rollback — and the migration requirement killed it. A migration
job that moves every database in the environment couples every app in that
environment to one schema, and therefore to one revision: "roll back
`app1` alone" under a shared migrated schema is not a smaller operation,
it is a lie the ref model would have happily recorded. One desired ref per
environment makes the coupled thing the addressable thing. (Per-app
*work avoidance* survives intact — see the skip protocol below; per-app
*versions* do not, and should not.)

Desired refs are ordinary branches for the same reason candidate refs are
(ledger: "Refs are ordinary branches"): humans push them with stock git or
jj, they're visible in every UI, and hosted-remote branch protection
applies — of which more below. Observed refs sit in the daemon's own
namespace for the same reason GC pins and trial refs do: machine state,
never hand-edited, invisible to the branch list, proven pushable on GitHub
by the issue #7 spike.

Reconciliation per environment lane, every poll tick:

```
desired != observed, lane idle  →  run the deploy graph at desired
graph finishes all-green        →  CAS observed: old → desired
any node red                    →  park the lane at (desired SHA)
desired moves while parked      →  un-park, reconcile fresh
```

Level-triggering is what buys every requirement at once. There is no
deploy *queue* and no backlog policy vocabulary: intermediate desired
states that came and went between ticks simply never existed as far as the
lane is concerned — the structural version of what `hooks-policy
"coalesce"` approximates by dropping queue entries. A crash loses nothing:
desired and observed are both on the remote, so a restarted daemon
re-derives every lane's state from one `ls-remote`, exactly like queue
state (Invariant 4). "Deploy an older revision" is not a feature; it's a
ref push.

## Sources: branches, and other environments

An environment names a **source** — where its revisions come from — and
one of two advance modes. A source is either a branch or another
environment:

- `source "main"` — a branch of the watched remote.
- `source env="dev"` — another environment's **observed** ref: the
  revisions this environment may run are exactly the revisions that
  environment has *finished deploying green*.

Modes:

- **`track`** — the daemon itself CAS-moves the environment's desired ref
  to the source tip whenever the source moves. Landing on `main` → `dev`'s
  desired advances → `dev` deploys. `dev`'s graph goes green → `dev`'s
  observed advances → `prod` and `prod2` (both tracking `env="dev"`)
  advance → both deploy, in parallel with each other.
- **`manual`** — the daemon never moves the desired ref; humans (or
  porcelain, or an external system) push it. The source is advisory: the
  dashboard shows drift against it, and `gauntlet promote` defaults to it.

This is the whole cross-environment story. **The environment DAG is
data flow, not control flow**: "dev first, then prod and prod2 in
parallel" is not a scheduler ordering three jobs — it is `prod` and
`prod2` each declaring that their input is `dev`'s output. There is no
cross-environment `after` grammar, no environment pipeline object, no
run-spanning state machine — a chain of sources *is* the pipeline, each
link independently level-triggered, each resumable from refs alone after
a crash. A cycle in `source env=` references is a config error at load.

`dev` tracks `main`; `prod` and `prod2` track `env="dev"` — confirmed
(2026-08-09 review) as the intended topology: promotion is automatic,
every revision `dev` deploys green rolls to both prods, and incident
pinning is the config edit below. Sources are **branches and environments
only**, also confirmed: tag sources (track the newest `v*`) were
considered and declined — tags don't move, the *newest matching* one
does, and that resolution rule is exactly the kind of computed config
this codebase keeps out; a release train lands onto a `release` branch
and sources that. A tracked environment's
rollback story is honest and blunt: the tracker will re-advance the ref on
the next source move, because a tracked environment *means* "runs the
source." Pinning during an incident is a config edit (`track` → `manual`)
— deliberately a restart-shaped operation, not an API toggle whose
in-memory state a restart silently forgets.

**Approval falls out for free.** Who may deploy to `prod` is exactly who
may push `deploy/prod`, and that is the remote's branch-protection
problem — host-enforced, per-user audited, working even when the daemon is
down. This is the break-glass argument from the ledger applied in reverse:
gauntlet adds no approval model, no deploy ACL, no privileged API, because
the remote already has a better one. (A *tracked* environment's desired
ref is daemon-pushed, so its gate is upstream: whatever protects the
source. The manual mode is where branch protection earns its keep.)

## What runs: a node graph, from the deployed revision's own tree

A deploy node is a named command — the "job is a named command, no DSL"
wall stands, and so does its one sanctioned crack: `after` edges plus
`max-parallel`, the exact grammar checks already have (ledger, issue #1:
"`after` edges + `max-parallel` is the WHOLE grammar"). The repo spec
(`.gauntlet.kdl`) grows top-level `deploy` nodes, siblings of `check`:

```kdl
deploy "migrate" {
    command "./scripts/migrate"
}
deploy "app1" {
    command "./scripts/deploy" "app1"
    after "migrate"
    executor "deployer"          // optional named profile, as for checks
}
deploy "app2" {
    command "./scripts/deploy" "app2"
    after "migrate"
}
```

One environment deploy = one run of this graph, scheduled by the **same
run-graph scheduler checks use** — readiness (`after` edges green),
spec-order starts under `max-parallel` and the daemon-global slot cap,
fail-fast with an explicit culprit, `blocked` rows for nodes whose edges
failed, per-node `waited_ms`. Image builds already joined that scheduler
as synthetic nodes with zero second-scheduler code (issue #2); deploy
nodes are the third tenant, not a new machine. Graph validation (unknown/
self/duplicate/cycle edges) runs at spec load, exactly as for checks.

The spec is read from the tree of the revision **being deployed** — the
deploy-side twin of "a candidate is tested by its own definition." This is
load-bearing for older revisions: rolling `prod` back to last week's SHA
runs last week's graph (its migration step, its apps, its edges) against
last week's tree, not today's graph against a tree it never knew. Each
node runs against an export of that tree (same `ExportTree` machinery,
same isolated-workspace policy if configured) with the check contract plus
deploy coordinates:

| Variable | Value |
| --- | --- |
| `GAUNTLET_DEPLOY_ENV` | environment name (`prod`) |
| `GAUNTLET_DEPLOY_NODE` | node name (`app1`) |
| `GAUNTLET_DEPLOY_SHA` | the revision being deployed (== desired) |
| `GAUNTLET_DEPLOYED_SHA` | the observed ref's SHA before this deploy; empty on an environment's first-ever deploy |
| `GAUNTLET_GIT_DIR` | the daemon's bare repo, read-only, resolving both SHAs |
| `GAUNTLET_RESULT_FILE` | the existing skipped-verdict protocol |

One graph serves every environment — `GAUNTLET_DEPLOY_ENV` is how
`./scripts/migrate` knows which environment's databases to migrate. An
environment that deploys a *subset* of nodes (say `prod2` runs fewer apps)
lists them: `nodes "migrate" "app1"` in its config block selects a
subgraph, and selection is transitively closed over `after` edges — naming
a node pulls in its ancestors, because running `app1` without the
`migrate` it declares it needs is exactly the kind of silent lie spec
rejection exists to prevent. No `nodes` list means the whole graph.

`GAUNTLET_DEPLOYED_SHA` + `GAUNTLET_GIT_DIR` make affected-only deploys a
repo-side one-liner — `git diff --quiet $GAUNTLET_DEPLOYED_SHA..$GAUNTLET_DEPLOY_SHA -- app1/`
→ report `skipped` — which is what keeps a ten-app environment cheap: a
revision that touched only `app1` skips the other nine nodes in seconds.
**Skipped counts green**, exactly as in check graphs: it is the node's own
verdict that this revision needs nothing from it, it satisfies `after`
edges, and an all-green-or-skipped graph advances the observed ref. Same
path-filter-unsoundness acceptance, same repo-owns-the-logic stance as
conditional checks.

Environments are **daemon config** — deployment capability is
operator-owned, like executor profiles and hooks:

```kdl
deploy {
    environment "dev" {
        source "main"
        track
    }
    environment "prod" {
        source env="dev"
        track
        max-parallel 4           // node-level, within this env's graph
        on-desired-move "finish" // or "cancel": kill the in-flight graph
    }
    environment "prod2" {
        source env="dev"
        track
        nodes "migrate" "app1"   // subgraph; closure over `after` enforced
    }
}
```

A `nodes` entry naming no `deploy` node in the revision's spec is a spec
rejection (a `SpecRejectReason`-style park, never a red verdict) — the
fail-closed stance from receipts: an environment must never silently
believe it deployed when no command existed to run.

Credentials: deploy commands are candidate-code-class (landed, gated, but
still repo-authored), so the issue-13 secret-stripping applies unchanged —
they never see the daemon's own operator secrets. Deploy credentials
arrive the way check credentials do: fixed `env` on an operator-owned
executor profile, or workload identity on the builder host (ledger:
"Workload identity lives on the builder host"). Nothing new to design.

## Concurrency, parks, retries

An environment lane is the unit of serialization: never two graph runs for
the same environment in flight, so an environment sees a strict sequence
of deploy attempts. Across environments everything is concurrent —
`prod` and `prod2` run their graphs side by side — and within a graph,
nodes parallelize under the environment's `max-parallel`. Every node
occupies one daemon-global `max-executions` slot, the same budget checks,
builds, and hooks share: a deploy burst and a check burst negotiate over
the same honest host capacity. If starvation in either direction shows up
in `waited_ms`, a separate deploy budget is the knob to add then, not now.

- **A red node fails the graph fast** — in-flight siblings are cancelled,
  unstarted nodes record `blocked` naming their failed edges, the red node
  is the culprit, and the lane **parks** at that desired SHA. The observed
  ref does not move: a half-deployed environment is recorded as exactly
  that — parked, with per-node rows saying which half. No retry loop,
  exactly like a red candidate. Cleared by: a new desired SHA (any push,
  including a rollback push), or an explicit retry
  (`POST /api/v1/deploy/retry {env}`, MCP `deploy_retry`, dashboard).
  Infra-shaped failures (`OutcomeError`: executor unreachable, export
  failure) get the standing auto-retry-once budget per (env, desired SHA),
  through the same machinery.
- **Retry re-runs the whole graph**, not the red suffix. Green nodes from
  the failed attempt run again — and either self-skip (the diff protocol
  answers "already effectively deployed" as cheaply on retry as on a fresh
  revision) or re-execute idempotently. Resuming a graph mid-way from
  recorded per-node results would make history rows *correctness* state,
  which they are forbidden to be (ledger: SQLite never holds correctness);
  the refs say desired ≠ observed, so the graph runs. Migration jobs make
  this concrete: every serious migration tool already tracks applied
  migrations and no-ops on re-run. One cost caveat, confirmed real by the
  adopting team (2026-08-09): re-runs are *safe* but not always *cheap* —
  a long backfill that half-applied before a failure re-runs from
  wherever the tool's own ledger says it stopped, and the diff-based skip
  protocol cannot help a same-revision retry (desired and observed
  haven't moved, the diff is unchanged). The resume mechanism for
  expensive steps is therefore the migration tool's applied-ledger, by
  design: split long backfills into separately-recorded steps in the
  tool, and gauntlet's whole-graph retry stays a cheap re-invoke. This is
  the dispatcher/controller line again — step-level resume state belongs
  to the tool that owns the steps, never to gauntlet's history rows.
- **Desired moves mid-graph**: `finish` (default) lets the in-flight graph
  complete, records it, then reconciles again — one more tick, one more
  run. `cancel` kills it immediately (the `hooks-policy "cancel"`
  semantic), records the cancellation, reconciles. Either way the observed
  ref only ever moves to a SHA whose graph actually finished green.
- **Crash mid-graph**: the lane re-derives `desired != observed` on boot
  and re-runs. Stated bluntly, as a contract: **deploy commands must
  tolerate re-execution** — a crash, a cancel-then-retry, or an ambiguous
  failure all re-run the graph. This is the same idempotence gauntlet
  demands of every reconcile step (Invariant 4) pushed across the executor
  boundary, and it is not optional.
- **GC**: the lane pins its desired SHA (`refs/gauntlet/pin/<sha>`, the
  existing namespace) for the duration of a run, since nodes resolve it
  through `GAUNTLET_GIT_DIR`; the desired branch itself keeps it reachable
  on the remote. Observed refs are fetched alongside so deployed SHAs stay
  resolvable locally for the diff pattern above.

## Surfaces: UI, history, events, API, porcelain

**Dashboard.** Two pages, same auto-refresh morph as the queue pages
(a mockup accompanies this doc):

- `/deploys` — the overview. A source-chain strip up top (`main → dev →
  prod, prod2`) so the promotion topology is legible at a glance, then one
  card per environment: desired vs observed SHA (each linked; drift is
  visually loud), state (`in sync` / `deploying` / `parked` / `waiting`),
  the source it follows and its mode, per-node status chips for the
  current or last run, and recent-deploy chips linking to details.
- `/deploy/{id}` — the detail page, structurally the run page: env,
  desired and previous SHAs, trigger (tracked advance vs manual push, with
  pusher where knowable), timing, then the node list in spec order — chip,
  name, its `after` edges, duration, `waited_ms`, the tail-capped output
  inline behind the same expandable rows a run's checks use, command echo,
  and the "full log" link. Blocked rows name their failed edges; the
  culprit is visually distinct.

**Logs and history** reuse the check plumbing wholesale: full combined
output at `<state>/logs/deploy-<id>/<node>.log.zst` under the existing
`log-retention` sweep; `deploys` + `deploy_nodes` history tables (run id,
env, sha, prev_sha, outcome, culprit; per-node name, outcome, blocked_by,
started/ended, waited_ms, output tail) feeding the pages above. History
stays efficiency-only: lose the db and the overview rebuilds from refs;
only old detail pages are gone. Deploy spans (`gauntlet.deploy` root,
child per node) and terminal-summary metrics join the existing OTel shape,
with env/node/outcome as attributes — both config- or spec-bounded, so
cardinality is safe where run IDs were not.

**Events**: `EventDeployStarted` / `EventDeployNodeFinished` /
`EventDeployFinished` (terminal, carrying the record — per the emit-site
contract; extend the contract tests first, event shapes are the standing
soft underbelly). Slack posts a root message per graph run, threads node
failures under it with the failing tail — decided (2026-08-09) over a
per-environment thread: it mirrors how candidate runs post today, and a
lane-anchored thread scheme can be designed later if tracked-env chatter
proves noisy. No Slack reaction commands in v1 — a reaction anchors to a
run's (target, ref) and an environment lane is neither; retry lives on
the API/dashboard/MCP until that lane-anchored metadata scheme exists.

**API/MCP**: `GET /api/v1/deploys` (the overview), `GET /api/v1/deploy/{id}`,
`POST /api/v1/deploy/retry`, `POST /api/v1/deploy/cancel` (env-addressed,
the hook-cancel precedent: lanes are not `core.Command` material since
they have no candidate ref), and MCP mirrors. The no-auth trust model
holds: nothing here moves a desired ref — the API can retry and cancel,
never deploy; deployment authority is push authority.

**CLI**, thin as ever, one worder like `land`:

```sh
gauntlet deploy -env prod                  # desired := source tip (dev's observed)
gauntlet deploy -env prod -rev <sha>       # arbitrary revision
gauntlet promote -to prod                  # same as the first form, reads better
```

All of it is porcelain over a CAS `git push` of `deploy/<env>` — usable
without the binary, subject to branch protection, attributed to the
pusher.

## Deliberately not built

- **Health checks, rollback automation, progressive delivery, traffic
  shaping.** The dispatcher/controller line. A deploy node that drives
  Argo CD, waits on a rollout, or runs smoke tests owns that logic — and
  its exit code is the only thing gauntlet reads.
- **Cross-environment control flow.** No `after` between environments, no
  pipeline object, no "promote when green" rule engine beyond `track` on
  an `env=` source. Source chaining already expresses the environment DAG
  as independently-recoverable data flow; a cross-env scheduler would
  reintroduce exactly the durable multi-step workflow state this codebase
  killed Temporal to avoid.
- **Partial-graph resume on retry.** Correctness state stays in refs;
  per-node results stay history. The skip protocol makes re-runs cheap;
  that is the mechanism, not an optimization of it.
- **Environment drift detection.** The observed ref records the last green
  *graph run*, not the environment's live state. If the environment
  changed underneath, gauntlet cannot know and does not pretend to.
- **A deploy approval model.** Branch protection on `deploy/*` is strictly
  better: host-enforced, audited, daemon-independent.
- **Windows/freezes/calendars.** A freeze is `manual` mode plus not
  pushing; a deploy window belongs in the deploy script or the humans.

## Proposed ledger amendment

*Ratified into DESIGN.md (D4) — the ledger's "Deployments as post-land
hooks" row now carries this text, condensed.*

> **AMENDED: Deployments as post-land hooks** — ~~the hook is the whole
> deployment story~~ → hooks remain for land-reactions (notify, cache
> warm); *deployment* moves to desired-state dispatch: per-environment
> lanes reconciling `deploy/<env>` (human/tracked desired) against
> `refs/gauntlet/deployed/<env>` (daemon-observed, advanced only on an
> all-green graph run), executing repo-declared `deploy` node graphs —
> `after` + `max-parallel`, the check grammar, the same scheduler — from
> the deployed revision's own tree. Cross-environment ordering is source
> chaining (`source env="dev"`: an environment's input is another's
> observed output), never a cross-env scheduler. The hard boundary
> survives, sharpened: gauntlet is a deploy **dispatcher** (schedule,
> execute, record, display), never a CD **controller** (no health, no
> rollback decisions, no traffic) — those live in the deploy command or
> the CD system it hands off to. Level-triggered by construction:
> rollback, promotion, and re-deploy are ref pushes; approval is branch
> protection; crash recovery is rescan (Invariant 4).

## Implementation plan

Four phases, each independently landable through the merge queue, each
inert until the next is ready: a daemon config with no `deploy` block
stays **byte-identical to today** at every phase — the standing pattern
for every optional subsystem. The deploy machinery lives in a new
`internal/deploy` package, a sibling of `internal/hooks` (operator-driven
runner consuming `core`/`gitx`/`executor` interfaces), never inside
`internal/queue` — the queue core stays a merge queue (Invariant 8's
spirit: the deploy subsystem sees the same interfaces channels and
executors do).

**Phase D1 — config and ref plumbing, nothing executes.**
`internal/config`: `deploy` nodes in the repo spec (`checks.go` — name,
command, `after`, `executor`, with the existing graph validation and
known-profile gates) and the daemon `deploy { environment ... }` block
(`daemon.go` — source branch/`env=`, `track`, `nodes`, `max-parallel`,
`on-desired-move`; cycle detection over `env=` sources at load).
`internal/gitx`: fetch coverage for `refs/gauntlet/deployed/*` alongside
the existing refspec, CAS helpers reused as-is. `internal/deploy`: the
**tracker** only — derive lane state from refs each tick, advance tracked
desired refs (branch and `env=` sources), Snapshot fields. Tests: config
rejection tables; a deploy testscript harness skeleton (fake-git and
real-git Setups over one Cmds set, the `script_test.go` pattern) with
scenarios asserting only ref movement. Smallest phase; proves the ref
model end-to-end against a real remote before any command runs.

**Phase D2 — graph execution.** The risky seam, and the extract-vs-
parallel decision spike (2026-08-09) ran ahead of implementation and
came back **parallel**: a minimal scheduler inside `internal/deploy`
(~180 lines mirroring `advanceChecks`'s four steps: spec-order starts
under two caps, drain-then-cull, spec-first culprit, blocked-row
synthesis), NOT an extraction. Three findings decided it, recorded so
the next reader doesn't re-litigate: (1) the "byte-identical event
streams" guard the extraction plan leaned on is **currently unpinned** —
the queue suite's `assert-event` checks kind existence, not order or
content, so a provable extraction first requires a golden-event-stream
pinning sub-project that doesn't exist; (2) only ~85 of the scheduler's
lines are graph logic — the rest is queue tenancy (image/receipt
validation mutating results mid-drain, batch attribution, trial-ref
gates), and the worst coupling is an *ordering* contract an interface
can't enforce; (3) the two tenants want different admission modes — the
queue's non-blocking `TryAcquire`/`readyAt` bookkeeping exists only
because the reconcile loop must never block, while a deploy lane is a
goroutine (the hooks precedent) that simply blocks on `Acquire`, so a
shared component carries both modes: more code than either caller has
today. One piece IS shared now: `core.NodeGreen`, the
skipped-counts-green predicate the queue scheduler restates three times.
Divergence guard: the six `parallel_test.go` behaviors are ported into
`internal/deploy`'s scheduler tests under the same names. Future
unification path, in order, if a third tenant appears: build the event-
stream pin first, then a step-function `Advance` mode, then extraction —
never extraction first. Then the lane runner: export
of the desired revision (reuse `ExportTree` + workspace policy + mtimes),
`GAUNTLET_DEPLOY_*` env, result-file skip protocol, candidate-code
secret-stripping, GC pins, observed-ref CAS advance on all-green, parks +
retry + auto-retry-once, `on-desired-move` finish/cancel, drain
integration (a draining daemon finishes in-flight graphs like it
finishes hook backlogs — no new admission). Events
(`EventDeployStarted`/`NodeFinished`/`Finished`) land **contract-tests
first** — event shapes are the standing soft underbelly. Scenario tests:
migrate→apps green path, culprit+blocked, skip-advances-observed,
mid-graph desired move under both policies, crash-restart re-run,
cancel.

**Phase D3 — record and surfaces.** `internal/history`: schema vNN with
`deploys` + `deploy_nodes` tables (efficiency-only, per the ledger); log
files under `<state>/logs/deploy-<id>/<node>.log.zst` inside the existing
retention sweep. Dashboard: `/deploys` and `/deploy/{id}` per the
mockup, nav entry, morph refresh. API/MCP: `GET /api/v1/deploys`,
`GET /api/v1/deploy/{id}`, `POST /api/v1/deploy/{retry,cancel}` + MCP
mirrors (env-addressed, the hook-cancel out-of-band precedent). Slack
root-per-run posting; `ghstatus` deliberately ignores deploy events
(the same CD-boundary stance it takes on hooks). OTel: `gauntlet.deploy`
span tree + node terminal metrics (env/node/outcome attributes only —
config/spec-bounded cardinality). The idle signal composes deploy
activity exactly as it composes hooks (scaling.md's scale-to-zero
depends on it).

**Phase D4 — porcelain and docs.** `gauntlet deploy` / `gauntlet promote`
(the `land.go` pattern: thin CAS-push porcelain, no daemon round-trip);
docs/config.md and docs/checks.md reference sections; README; the
branch-protection recipe for `deploy/*` in docs/setup.md; and the
DESIGN.md ledger amendment above, ratified as part of the final landing.

Watch items going in: the scheduler extraction (D2) is where the plan
bends — decide extract-vs-parallel within the first spike, not after;
observed-ref fetch must not disturb `Fetch`'s refspec-derived queue-state
invariants (pins survived this, trial refs survived this, same care);
and drain/idle integration is easy to forget and cheap to test early.

## Follow-ups recorded during implementation

Things this design describes that the shipped code does *not* do, recorded
here rather than quietly forgotten. None of them is load-bearing; each is a
deliberate stop, and the reference docs describe the shipped behavior.

- **No trigger row, and no `after` edges, on `/deploy/{id}`.** The detail
  page above sketches "trigger (tracked advance vs manual push, with pusher
  where knowable)" and per-node `after` edges. `core.DeployRecord` carries
  neither — a run records env, both SHAs, outcome, culprit, detail, timing,
  and one `CheckResult` per declared node — so the page renders neither. A
  blocked node's *failed* edges do appear (`BlockedBy` carries them, which
  is the case that actually needs them). Adding a trigger means the tracker
  recording *why* a desired ref moved, which today it does not know:
  desired-ref movement is observed from refs, not from an event with a
  pusher attached.
- **`GET /api/v1/deploy/{id}` is history-only.** The HTML page falls back to
  the live snapshot for a run still in flight (so the overview's "deploying"
  link never 404s); the JSON route deliberately does not, on the grounds
  that a JSON client polling a known run ID is better served by
  `GET /api/v1/deploys`' `running` block than by a half-populated record.
  With no `history` store, the JSON route is a flat 503 and the deploy
  detail is dashboard-only.
- **Slack lane threading is still open.** As decided, Slack posts a root
  message per graph run and threads node failures under it. A
  lane-anchored thread ("one thread per environment, forever") was left for
  later, and so were reaction commands on deploy posts — a reaction anchors
  to a run's (target, ref), and an environment lane is neither, so retry
  lives on the API/dashboard/MCP until that lane-anchored metadata scheme
  exists.
- **`gauntlet validate -config` doesn't cross-check deploy nodes.**
  `SpecRejectReason` — the shared gate the validate command and the queue
  both call — covers checks, images, services, and receipts, not deploy
  nodes. A deploy node naming an executor profile the daemon doesn't define
  is caught at deploy time, as a spec-rejection park, not at validate time.
  The deploy graph's own structure (names, commands, edges, cycles) *is*
  validated, since that is plain spec parsing.
- **The CLI can't resolve an environment's configured source.** The
  porcelain sketch above shows `gauntlet deploy -env prod` meaning "desired
  := source tip". The shipped command is a pure git client with no daemon
  round-trip, so it cannot read daemon config, and requires `-rev` or
  `-from-env` (the error says why and names both). `gauntlet promote` takes
  `-from` and `-to` rather than defaulting `-from` to the configured source,
  for the same reason. Restoring the sketch means either a daemon
  round-trip (giving up "works while the daemon is down") or teaching the
  CLI to read the operator's `gauntlet.kdl` — neither obviously worth it.
- **`gauntlet deploy` fetches when the revision isn't local.** `git push
  <sha>:<ref>` resolves its source locally, so the porcelain fetches the
  resolved ref first when the object is missing — always, for a promotion,
  since a normal clone never fetches `refs/gauntlet/deployed/*`. That is one
  more remote round trip than the "just a push" sketch implies.

## Open questions

Three earlier questions were resolved in the 2026-08-09 review and folded
into the sections above: tag sources (declined — branches and
environments only), promotion topology (auto-track confirmed for both
prods), and Slack shape (root per graph run). Still open:

- **Batch landings on a tracked source** produce N tip moves in quick
  succession; the tracker naturally coalesces to the last one seen per
  tick, but a `cancel`-policy environment could churn. Likely fine;
  measure before adding debounce.
- **Node-level cancel** — `deploy/cancel` kills the environment's whole
  in-flight graph; whether a single node is worth addressing (the batch
  member-cancel precedent says maybe) can wait for a real need.
- **Fan-in sources** — an environment sourcing *two* environments ("prod3
  follows whichever of prod/prod2 is behind") has no obvious ref
  semantics; declined until someone actually wants it, and probably then
  too.
