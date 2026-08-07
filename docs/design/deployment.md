# Deployment: desired-state dispatch over deploy refs

**Status:** proposed · **Date:** 2026-08-07

Gauntlet's answer to deployment today is the post-land hook — fire commands
on the land event, coalesce a backlog, hand off to a real CD system when
needs grow. That answer is correct for *reactions to a landing* and wrong
for *deployment as an ongoing concern*: a hook is edge-triggered and keyed
to the merge that just landed, so there is no surface anywhere in the
daemon to say "environment `prod`, app `app1`, revision `X`, now" — no
older revision, no re-deploy, no promotion between environments, no
per-environment view. This doc designs that surface.

The requirements it serves:

- **Multiple environments** (`prod`, `dev`), each fed from its own source
  branch (`main`, `release/*`) — deployment decoupled from landing.
- **Parallel deploys** across apps within an environment (`prod`/`app1`
  alongside `prod`/`app2`), serialized per app.
- **Arbitrary revisions** — deploy an older SHA, re-deploy the current one,
  promote what `dev` runs to `prod`.
- **Operator UI** — an overview of every environment × app, and full
  per-deploy logs, on the existing dashboard.

The scope boundary from the decision ledger ("Deployments as post-land
hooks") survives, restated more precisely at the end: gauntlet gains a
deploy **dispatcher** — schedule a named command against a revision for an
environment, record the outcome — and still never grows a CD
**controller**. It never learns whether an environment is healthy, never
decides to roll back, never shapes traffic. Those stay in the repo's deploy
scripts or the CD system they invoke.

## The model: desired state in refs, level-triggered

The merge queue's own discipline applies unchanged: durable ground truth
lives in refs on the remote, and the daemon is a reconcile loop that
re-derives everything else. Two ref families per (environment, app) pair:

- **Desired** — `refs/heads/deploy/<env>/<app>`, an ordinary branch,
  human- or daemon-moved. Whatever commit it points at is what should be
  running. Deploying is pushing this ref; rolling back is pushing it to an
  older SHA; promotion is pushing it to the SHA another environment runs.
- **Observed** — `refs/gauntlet/deployed/<env>/<app>`, daemon-owned,
  CAS-moved only after a deploy command succeeds. The last revision whose
  deploy is known to have exited green.

Desired refs are ordinary branches for the same reason candidate refs are
(ledger: "Refs are ordinary branches"): humans push them with stock git or
jj, they're visible in every UI, and hosted-remote branch protection
applies — of which more below. Observed refs sit in the daemon's own
namespace for the same reason GC pins and trial refs do: machine state,
never hand-edited, invisible to the branch list, proven pushable on GitHub
by the issue #7 spike.

Reconciliation per (env, app) lane, every poll tick:

```
desired != observed, lane idle  →  start a deploy of desired
deploy exits green (or skipped) →  CAS observed: old → desired
deploy exits red                →  park the lane at (desired SHA)
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

## Sources: track and manual environments

An environment names a **source** branch and one of two advance modes:

- **`track`** — the daemon itself CAS-moves every desired ref in the
  environment to the source tip whenever the source moves. Landing on
  `main` → `dev`'s desired refs advance → lanes reconcile → deploys run.
  The land event is never consumed directly: the deploy subsystem watches
  the branch, not the queue, so a human push to `main` (the break-glass
  path) deploys exactly like a landing does.
- **`manual`** — the daemon never moves desired refs; humans (or porcelain,
  or an external system) push them. The source is advisory: the dashboard
  shows drift between desired and the source tip, and porcelain defaults
  to it.

`dev` tracks `main`; `prod` is manual, sourced from `release` — both
branches being ordinary merge-queue targets landed via `for/release/...`
as usual. A tracked environment's rollback story is honest and blunt: the
tracker will re-advance the ref on the next source move, because a tracked
environment *means* "runs the tip." Pinning during an incident is a config
edit (`track` → `manual`) — deliberately a restart-shaped operation, not an
API toggle whose in-memory state a restart silently forgets.

**Approval falls out for free.** Who may deploy to `prod` is exactly who
may push `deploy/prod/*`, and that is the remote's branch-protection
problem — host-enforced, per-user audited, working even when the daemon is
down. This is the break-glass argument from the ledger applied in reverse:
gauntlet adds no approval model, no deploy ACL, no privileged API, because
the remote already has a better one.

## What runs: deploy nodes, from the deployed revision's own tree

A deploy is a named command — the "job is a named command, no DSL" wall
stands. The repo spec (`.gauntlet.kdl`) grows top-level `deploy` nodes,
siblings of `check`:

```kdl
deploy "app1" {
    command "./scripts/deploy" "app1"
    executor "deployer"          // optional named profile, as for checks
}
deploy "app2" {
    command "./scripts/deploy" "app2"
}
```

The spec is read from the tree of the revision **being deployed** — the
deploy-side twin of "a candidate is tested by its own definition." This is
load-bearing for older revisions: rolling `prod/app1` back to last week's
SHA runs last week's deploy script against last week's tree, not today's
script against a tree it never knew. The command runs against an export of
that tree (same `ExportTree` machinery, same isolated-workspace policy if
configured) with the check contract plus deploy coordinates:

| Variable | Value |
| --- | --- |
| `GAUNTLET_DEPLOY_ENV` | environment name (`prod`) |
| `GAUNTLET_DEPLOY_APP` | app name (`app1`) |
| `GAUNTLET_DEPLOY_SHA` | the revision being deployed (== desired) |
| `GAUNTLET_DEPLOYED_SHA` | the observed ref's SHA before this deploy; empty on a lane's first-ever deploy |
| `GAUNTLET_GIT_DIR` | the daemon's bare repo, read-only, resolving both SHAs |
| `GAUNTLET_RESULT_FILE` | the existing skipped-verdict protocol |

`GAUNTLET_DEPLOYED_SHA` + `GAUNTLET_GIT_DIR` make affected-only deploys a
repo-side one-liner — `git diff --quiet $GAUNTLET_DEPLOYED_SHA..$GAUNTLET_DEPLOY_SHA -- app1/`
→ report `skipped` — which is what makes a ten-app monorepo environment
cheap: a landing that touched only `app1` skips nine lanes in seconds.
**A skip advances the observed ref** exactly like a pass: it is the
command's own verdict that this revision is already effectively deployed
for this app. Same path-filter-unsoundness acceptance, same repo-owns-the-
logic stance as conditional checks.

Environments and their app lists are **daemon config** — deployment
capability is operator-owned, like executor profiles and hooks:

```kdl
deploy {
    environment "dev" {
        source "main"
        track
        app "app1"; app "app2"
    }
    environment "prod" {
        source "release"
        app "app1"; app "app2"
        max-parallel 2           // lanes started concurrently; default unbounded
        on-desired-move "finish" // or "cancel": kill the in-flight deploy
    }
}
```

An app named in config but missing a `deploy` node in the revision's spec
is a spec rejection (a `SpecRejectReason`-style park, never a red
verdict) — the fail-closed stance from receipts: a lane must never
silently believe it deployed when no command existed to run.

Credentials: deploy commands are candidate-code-class (landed, gated, but
still repo-authored), so the issue-13 secret-stripping applies unchanged —
they never see the daemon's own operator secrets. Deploy credentials
arrive the way check credentials do: fixed `env` on an operator-owned
executor profile, or workload identity on the builder host (ledger:
"Workload identity lives on the builder host"). Nothing new to design.

## Concurrency, parks, retries

A lane — one (env, app) pair — is the unit of serialization: never two
deploys of the same lane in flight, so an app's environment sees a strict
sequence of deploy attempts. Across lanes everything is concurrent, capped
by the environment's `max-parallel` and by the daemon-global
`max-executions` slots that checks, builds, and hooks already share — a
deploy occupies one slot, so a deploy burst and a check burst negotiate
over the same honest host budget. If starvation in either direction shows
up in `waited_ms`, a separate deploy budget is the knob to add then, not
now.

- **Red parks the lane** at that desired SHA — no retry loop, exactly like
  a red candidate. Cleared by: a new desired SHA (any push, including
  re-pointing at the same revision's ancestor for rollback), or an
  explicit retry (`POST /api/v1/deploy/retry {env, app}`, MCP `deploy_retry`,
  dashboard). Infra-shaped failures (`OutcomeError`: executor unreachable,
  export failure) get the standing auto-retry-once budget per (lane,
  desired SHA), through the same machinery.
- **Desired moves mid-deploy**: `finish` (default) lets the in-flight
  command complete, records it, then reconciles again — one more tick, one
  more deploy. `cancel` kills it immediately (the `hooks-policy "cancel"`
  semantic), records the cancellation, reconciles. Either way the observed
  ref only ever moves to a SHA whose deploy actually finished green.
- **Crash mid-deploy**: the lane re-derives `desired != observed` on boot
  and re-runs. Stated bluntly, as a contract: **deploy commands must
  tolerate re-execution** — a crash, a cancel-then-retry, or an ambiguous
  failure all re-run the same deploy. This is the same idempotence gauntlet
  demands of every reconcile step (Invariant 4) pushed across the
  executor boundary, and it is not optional. A command that cannot re-run
  safely must make itself idempotent (deployment tooling worth invoking
  already is).
- **GC**: the lane pins its desired SHA (`refs/gauntlet/pin/<sha>`, the
  existing namespace) for the duration of a deploy, since the command
  resolves it through `GAUNTLET_GIT_DIR`; the desired branch itself keeps
  it reachable on the remote. Observed refs are fetched alongside so
  deployed SHAs stay resolvable locally for the diff pattern above.

## Surfaces: UI, history, events, API, porcelain

**Dashboard.** Two pages, same auto-refresh morph as the queue pages:

- `/deploys` — the overview grid, one row per (env, app) lane: desired vs
  observed SHA (each linked to the commit; a drift is visually loud),
  state (`in sync` / `deploying` / `parked` / `waiting`), the running or
  last deploy's duration, and a link to it. Environments group the rows;
  a tracked environment also shows its source tip so "landed but not yet
  picked up" is visible.
- `/deploy/{id}` — the detail page, structurally the run page: env, app,
  desired and previous SHAs, timing (`waited_ms` split from command time),
  the tail-capped output inline, and the "full log" link.

**Logs and history** reuse the check plumbing wholesale: full combined
output at `<state>/logs/deploy-<id>/<app>.log.zst` under the existing
`log-retention` sweep; a `deploys` history table (id, env, app, sha,
prev_sha, outcome, detail, started/ended, waited_ms, output tail) feeding
the pages above. History stays efficiency-only: lose the db and the
overview rebuilds from refs; only old detail pages are gone. Deploy spans
(`gauntlet.deploy` root, child per command) and the terminal-summary
metrics join the existing OTel shape, with env/app/outcome as attributes —
lane names are config-bounded, so cardinality is safe where run IDs were
not.

**Events**: `EventDeployStarted` / `EventDeployFinished` (carrying the
result, per the emit-site contract — extend the contract tests first;
event shapes are the standing soft underbelly). Slack posts a root message
per deploy with the failure tail on red. No Slack reaction commands in
v1 — a reaction anchors to a run's (target, ref) and a lane is neither;
retry lives on the API/dashboard/MCP until a lane-anchored message
metadata scheme is worth designing.

**API/MCP**: `GET /api/v1/deploys` (the grid), `GET /api/v1/deploy/{id}`,
`POST /api/v1/deploy/retry`, `POST /api/v1/deploy/cancel` (env+app
addressed, the hook-cancel precedent: lanes are not `core.Command`
material since they have no candidate ref), and MCP mirrors. The no-auth
trust model holds: nothing here moves a desired ref — the API can retry
and cancel, never deploy; deployment authority is push authority.

**CLI**, thin as ever, one worder like `land`:

```sh
gauntlet deploy -env prod -app app1              # desired := source tip
gauntlet deploy -env prod -app app1 -rev <sha>   # arbitrary revision
gauntlet promote -from dev -to prod              # desired := dev's observed, per shared app
```

All three are porcelain over `git push` (CAS, `--force-with-lease`-style)
of `deploy/<env>/<app>` — usable without the binary, subject to branch
protection, attributed to the pusher.

## Deliberately not built

- **Health checks, rollback automation, progressive delivery, traffic
  shaping.** The dispatcher/controller line. A deploy command that drives
  Argo CD, waits on a rollout, or runs smoke tests owns that logic — and
  its exit code is the only thing gauntlet reads.
- **Environment drift detection.** The observed ref records the last green
  *deploy command*, not the environment's live state. If the environment
  changed underneath, gauntlet cannot know and does not pretend to.
- **Deploy pipelines/stages** (deploy A, then B, then verify). Ordering
  across apps is either the repo script's job or a real CD system's; an
  `after` grammar for lanes is the pipeline-DSL slope and is declined.
- **A deploy approval model.** Branch protection on `deploy/*` is
  strictly better: host-enforced, audited, daemon-independent.
- **Windows/freezes/calendars.** A freeze is `manual` mode plus not
  pushing; a deploy window belongs in the deploy script or the humans.

## Proposed ledger amendment

> **AMENDED: Deployments as post-land hooks** — ~~the hook is the whole
> deployment story~~ → hooks remain for land-reactions (notify, cache
> warm); *deployment* moves to desired-state dispatch: per-(env, app)
> lanes reconciling `deploy/<env>/<app>` (human/tracked desired) against
> `refs/gauntlet/deployed/<env>/<app>` (daemon-observed), running repo-
> declared `deploy` commands from the deployed revision's own tree. The
> hard boundary survives, sharpened: gauntlet is a deploy **dispatcher**
> (schedule, execute, record, display), never a CD **controller** (no
> health, no rollback decisions, no traffic) — those live in the deploy
> command or the CD system it hands off to. Level-triggered by
> construction: rollback, promotion, and re-deploy are ref pushes;
> approval is branch protection; crash recovery is rescan (Invariant 4).

## Open questions

- **Tag sources.** `release` as a branch is assumed; environments sourced
  from tags (`v*`) would suit release-train shops but complicate the
  track semantics (tags don't move; the *newest matching* tag does).
- **Batch landings on a tracked source** produce N tip moves in quick
  succession; the tracker naturally coalesces to the last one seen per
  tick, but a `cancel`-policy environment could churn. Likely fine;
  measure before adding debounce.
- **Slack lane threading** — a per-lane thread (root per lane, replies per
  deploy) would read better than a message per deploy for chatty tracked
  environments; needs the lane-anchored metadata scheme noted above.
- **`promote` and per-app skew** — promoting an environment whose apps
  observe *different* SHAs (mid-rollout) copies skew to the target env.
  Per-app promote is the primitive; whether env-level promote should
  refuse on skew is an operator-experience call.
