# API, CLI, and MCP

## JSON API

The dashboard (`internal/dashboard`) exposes a small JSON API under
`/api/v1`, mounted on the same handler/bind as the HTML pages. It exists
for agents, scripts, and the MCP server below that want machine-readable
queue status and a way to trigger a retry without a browser. Every response
is `Content-Type: application/json`, with stable lowerCamel field names;
errors are always `{"error": "..."}`.

- **`GET /api/v1/status`** — every target's live queue state: name, branch,
  tip SHA, the in-flight run (ref/sha/runID/currentCheck/startedAt/
  checksDone, or `null` if idle), the waiting queue (ref/sha/seq, FIFO
  order), and parked refs (ref/sha/outcome/reason/at). `503
  {"error":"no snapshot yet"}` before the first reconcile pass completes.
  Also carries a top-level `"idleSince"` (RFC3339 instant, omitted while
  busy) once the WHOLE daemon — every target's queue and post-land hooks —
  has been idle: see "Idle signal" below.

  ```sh
  curl -s http://localhost:8080/api/v1/status | jq .
  ```

- **`GET /api/v1/runs?target=<name>&limit=<n>`** — a target's recent runs
  from history, newest first (`limit` defaults to 20). `target` is
  required (`400` if missing). `503 {"error":"history disabled"}` if no
  `history` store is configured.

  ```sh
  curl -s 'http://localhost:8080/api/v1/runs?target=main&limit=5' | jq .
  ```

- **`GET /api/v1/run/{id}`** — one run's full detail, including its
  per-check results, plus a `hooks` array (its post-land hook results, same
  shape as `checks` — always present, empty when the run had no hooks).
  Each check/hook carries `logPath` (the full log file's path
  on disk, or `""` if none was written) and, only when the dashboard is
  configured to actually serve it, `logUrl` (a relative link to `GET
  /run/{id}/log/{name}` — omitted from the JSON entirely otherwise).
  `404 {"error":"not found"}` for an unknown run ID; `503
  {"error":"history disabled"}` if no `history` store is configured.

  ```sh
  curl -s http://localhost:8080/api/v1/run/<run-id> | jq .
  ```

- **`GET /api/v1/batch/{id}`** — every member run of one batch: run ID,
  target, position, candidate user/topic/SHA, outcome, detail, timing.
  `404 {"error":"not found"}` for an unknown or empty batch ID; `503
  {"error":"history disabled"}` if no `history` store is configured.

  ```sh
  curl -s http://localhost:8080/api/v1/batch/<batch-id> | jq .
  ```

- **`GET /api/v1/checks?target=<name>&since=<duration>`** — per-check
  red-rate/duration stats plus the queue-depth series for one target, the
  same data the dashboard's `/checks` page renders as a table and SVG
  chart, as JSON. `target` is required (`400` if missing); `since`
  defaults to the same window the HTML page uses. `503
  {"error":"history disabled"}` if no `history` store is configured.

  ```sh
  curl -s 'http://localhost:8080/api/v1/checks?target=main' | jq .
  ```

- **`GET /api/v1/services`** — the shared-services pool: every live
  instance (service name, image, key, mode, host/port, created/last-used,
  refcount, cumulative hit count) plus the pool's `max-instances` and
  pending-create count. `503 {"error":"services disabled"}` when no
  `services` block is configured.

  ```sh
  curl -s http://localhost:8080/api/v1/services | jq .
  ```

- **`POST /api/v1/retry`** — re-queues a parked ref at its current SHA,
  same effect as re-pushing it or reacting `:recycle:` in Slack (see
  README's ["Retry semantics"](../README.md#landing-changes)). Body:
  `{"target": "main", "ref": "refs/heads/for/main/alice/my-feature"}`.
  `202 {"status":"queued"}` on success; `400` if `target` or `ref` is
  missing or the body isn't valid JSON; `503` when queue controls are disabled;
  `429` when the command buffer is full; `405` for any method but `POST`.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/retry \
    -H 'content-type: application/json' \
    -d '{"target":"main","ref":"refs/heads/for/main/alice/my-feature"}'
  ```

- **`POST /api/v1/cancel`** — stops whatever is currently happening to a
  candidate and parks it at its current SHA (see README's ["Operator
  cancellation"](../README.md#landing-changes)), same effect as reacting
  `:x:` in Slack. Body: `{"target": "main", "ref":
  "refs/heads/for/main/alice/my-feature"}`. `202 {"status":"queued"}` on
  success; `400` if `target` or `ref` is missing or the body isn't valid
  JSON; `503` when queue controls are disabled; `429` when the command buffer
  is full; `405` for any method but `POST`.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/cancel \
    -H 'content-type: application/json' \
    -d '{"target":"main","ref":"refs/heads/for/main/alice/my-feature"}'
  ```

- **`POST /api/v1/hooks/cancel`** — cancels a target's currently-running
  post-land hook execution, if any (see [config.md's
  "Hooks"](config.md#hooks)). Body: `{"target": "main"}`. `202
  {"status":"cancelled"}` if a running landing was found and signalled,
  `202 {"status":"no-op"}` if nothing was running for that target (not an
  error); `400` if `target` is missing or the body isn't valid JSON; `503
  {"error":"hooks disabled"}` if no target configures any hooks; `405` for
  any method but `POST`.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/hooks/cancel \
    -H 'content-type: application/json' \
    -d '{"target":"main"}'
  ```

- **`GET /api/v1/deploys`** — the deployment overview (see [config.md's
  "Deployment"](config.md#deployment)): one entry per environment lane with
  its `mode` (`track`/`manual`), `source` (a branch name, or `env=dev`),
  `sourceTip`, `desired`, `observed`, the `inSync`/`drift`/`pending` flags,
  a folded `state` word (`parked` → `deploying` → `pending` → `in sync` →
  `never deployed` → `waiting`, in that priority), `lastAdvance`,
  `lastError`, and — when present — `running` (runID/deploySHA/
  deployedSHA/startedAt/nodes), `parked` (sha/runID/outcome/detail/at), and
  `lastResult` (runID/outcome/culprit/detail/timing). With `history`
  configured it also carries `recent`, the daemon-wide list of finished
  deploy runs (omitted entirely, not just empty, when history is off).
  `503 {"error":"deploy not configured"}` when the daemon has no `deploy`
  block; `503 {"error":"no snapshot yet"}` before the first reconcile pass
  publishes.

  ```sh
  curl -s http://localhost:8080/api/v1/deploys | jq .
  ```

- **`GET /api/v1/deploy/{id}`** — one finished deploy graph run by run ID:
  env, deploySHA/deployedSHA, outcome, culprit, detail, timing, and a
  `nodes` array in the same shape as a run's `checks` (name, status,
  duration, output, `logPath`/`logUrl`, resource usage). **History-backed
  only**: a run still in flight has no record yet, so a JSON client should
  poll `GET /api/v1/deploys`' `running` block instead (the HTML page at
  `/deploy/{id}` does fall back to the live snapshot, so the overview's
  "deploying" link never 404s — the JSON route deliberately doesn't).
  `404 {"error":"not found"}` for an unknown ID; `503
  {"error":"history disabled"}` if no `history` store is configured; `405`
  for any method but `GET`.

  ```sh
  curl -s http://localhost:8080/api/v1/deploy/<deploy-run-id> | jq .
  ```

- **`POST /api/v1/deploy/retry`** — clears an environment's park so the next
  reconcile pass re-runs its **whole** deploy graph. Body: `{"env":
  "prod"}`. `202 {"status":"retried"}` if a park was actually cleared, `202
  {"status":"no-op"}` if the lane wasn't parked (not an error); `400` if
  `env` is missing or the body isn't valid JSON; `503
  {"error":"deploy not configured"}`; `405` for any method but `POST`.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/deploy/retry \
    -H 'content-type: application/json' -d '{"env":"prod"}'
  ```

- **`POST /api/v1/deploy/cancel`** — interrupts an environment's in-flight
  deploy graph run. Body: `{"env": "prod"}`. `202 {"status":"cancelled"}` if
  a run was signalled, `202 {"status":"no-op"}` if nothing was running (not
  an error); same `400`/`503`/`405` semantics as retry. The cancelled run
  does *not* park the lane, so the next pass reconciles fresh: this
  interrupts one attempt, it does not stop deploying.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/deploy/cancel \
    -H 'content-type: application/json' -d '{"env":"prod"}'
  ```

  **Neither route can deploy anything.** Nothing in the API moves an
  environment's desired ref — deployment authority is push authority (see
  ["Trust model"](#trust-model) and
  [setup.md](setup.md#deploy-refs-and-branch-protection)). Deploying is
  `gauntlet deploy`, or any `git push` of `refs/heads/deploy/<env>`.

- **`POST /api/v1/drain`** — begins a graceful shutdown drain (see
  [config.md's `shutdown`](config.md)): stop admitting new candidates, let
  the in-flight set finish, then the daemon exits. Body is optional; an
  empty body drains with no deadline, or `{"deadline": "<RFC3339>"}` forces
  the immediate kill at that instant. `202 {"status":"draining"}`;
  idempotent (a repeat never resumes admission and only ever shortens the
  deadline); `400` if the deadline isn't RFC3339 or the body isn't valid
  JSON; `503 {"error":"drain unavailable"}` if no drain surface was wired;
  `405` for any method but `POST`. Poll `GET /api/v1/status`'s `lifecycle`
  field (`running` → `draining` → `drained`) to follow it; `activeRuns`/
  `activeChecks` show what's still in flight.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/drain \
    -H 'content-type: application/json' -d '{}'
  ```

## CLI

**`gauntlet status`**, **`gauntlet retry`**, **`gauntlet cancel`**,
**`gauntlet hooks-cancel`**, and **`gauntlet drain`** are thin CLI wrappers
over the same API (client-side porcelain, like `gauntlet land`):

```sh
gauntlet status -url http://localhost:8080                  # compact per-target summary
gauntlet status -url http://localhost:8080 -target main     # one target only
gauntlet status -url http://localhost:8080 -json            # raw API response

gauntlet retry -url http://localhost:8080 -target main -ref refs/heads/for/main/alice/my-feature
gauntlet cancel -url http://localhost:8080 -target main -ref refs/heads/for/main/alice/my-feature
gauntlet hooks-cancel -url http://localhost:8080 -target main

gauntlet drain -url http://localhost:8080                   # begin a graceful drain, return
gauntlet drain -url http://localhost:8080 -wait             # block until lifecycle=drained
gauntlet drain -url http://localhost:8080 -deadline 30m     # force the kill 30m out if unfinished
```

`gauntlet drain` fails with a clear error if there is no reachable admin
endpoint (a daemon with no `dashboard` bind drains by signal only — a first
SIGTERM), rather than pretending a drain began.

### `gauntlet deploy` / `gauntlet promote`

These two are **git porcelain, not API clients** — the only CLI verbs in
this document that never talk to the daemon. Deploying an environment *is*
pushing `refs/heads/deploy/<env>`, so they resolve a revision on the remote
and compare-and-swap that ref, nothing more. They work against a remote
whose daemon is down, they are subject to branch protection, and the push is
attributed to whoever ran them (see
[setup.md](setup.md#deploy-refs-and-branch-protection)).

```sh
gauntlet deploy -env prod -rev main            # deploy main's tip, as the remote has it
gauntlet deploy -env prod -rev v1.4.2          # a tag (annotated tags deploy the commit)
gauntlet deploy -env prod -rev 9f1c…           # a full SHA, taken verbatim
gauntlet deploy -env prod -from-env dev        # promote what dev finished deploying
gauntlet promote -from dev -to prod            # exactly the line above, spelled better
```

- **`-env` is required**, and so is **exactly one of `-rev` / `-from-env`**.
  The design sketch had a bare `gauntlet deploy -env prod` meaning "deploy
  the configured source tip"; the shipped command cannot do that, and says
  so: with no daemon round-trip there is nothing to read an environment's
  `source` *from*. Naming the revision (or the environment to promote) is
  the price of a client that needs no daemon.
- **`-rev`** is resolved on the **remote**, not in your local clone: a
  branch or tag name goes through `git ls-remote`, so `-rev main` deploys
  the main the remote has even from a stale checkout. A name matching more
  than one ref (a branch *and* a tag called `rel`) is an error naming both,
  never a guess; a full 40/64-hex SHA is taken verbatim with no round trip.
- **`-from-env dev`** reads `refs/gauntlet/deployed/dev` — what `dev` has
  actually *finished deploying green*, not what it was last asked to run. An
  environment that has never completed a deploy has no such ref, and the
  error says exactly that.
- The push is `--force-with-lease` against the value just read, so two
  people deploying different revisions at once cannot silently lose one:
  the loser is told the ref moved and re-runs. A revision your clone doesn't
  have yet is fetched first (which every promotion needs — a normal clone
  never fetches `refs/gauntlet/deployed/*`).
- `-remote` defaults to `origin`. Re-pushing the value the ref already holds
  is reported as a no-op rather than a deploy: the daemon is
  level-triggered, so re-running an already-deployed revision is
  `POST /api/v1/deploy/retry` (or MCP `deploy_retry`, or the dashboard
  button), not a ref push.

There is no `gauntlet deploy-retry` / `deploy-cancel` client: the two
mutating deploy routes are reachable from the dashboard, the MCP tools, or
`curl` (above).

## Idle signal

`idleSince` (also on the MCP `status` tool and `gauntlet status`, plus a
muted line on the dashboard index page) exists for external park/wake
automation — e.g. an Azure Function that deallocates a parked-builder VM
once the daemon has been idle long enough, and re-wakes it when refs arrive
(see [design/scaling.md](design/scaling.md)). It's the whole daemon's idleness, not just
the queue's: no waiting candidates and no in-flight runs across every
target, AND no target's post-land hook currently running or backlogged, AND
no deploy lane either running a graph *or* about to start one on the next
pass (both halves count — a scale-to-zero timer that deallocated the builder
between them would kill the deploy it was about to admit).
Absent (not `null` or `""`) whenever the daemon is busy right now — there's
no "was idle a moment ago" value, only "idle since T" or nothing.

## Trust model

Same as the dashboard itself: the API has no authentication of its own, so
bind it to a trusted interface and put it behind your proxy/tailnet if you
need one. `retry` is non-destructive — it only re-queues an already-parked
ref for another trial-merge-and-check pass; it never touches the target
branch, force-lands anything, or bypasses a check. `cancel`/`hooks-cancel`
are the same kind of non-destructive operational control — they park a ref
or interrupt a hook command, never delete anything or touch the target
branch. The two deploy routes are bounded the same way, and one step
further: `deploy/retry` and `deploy/cancel` act only on the revision an
environment is *already* pointed at, and **no route in this API can move a
desired ref**. Deployment authority is push authority — branch protection on
`deploy/*` is the approval model, host-enforced, audited, and working while
the daemon is down.

## MCP

The daemon also exposes an MCP (Model Context Protocol) server
(`internal/mcp`) at `/mcp`, mounted on the same bind/port as the dashboard
and its JSON API above — there's no separate port to configure. It speaks
the standard Streamable HTTP transport, so any MCP-capable agent or client
can connect directly:

```sh
claude mcp add --transport http gauntlet http://localhost:8080/mcp
```

Fourteen tools are exposed, mirroring the JSON API above (same lowerCamel
field names, so an agent reading both sees one vocabulary):

- **`status`** (`target` optional) — every target's live queue state, or
  just one target's if `target` is given. Same shape as `GET /api/v1/status`.
- **`runs`** (`target` required, `limit` optional, default 20) — a target's
  recent runs from history, newest first. Errors with `"history disabled"`
  if no `history` store is configured.
- **`run`** (`run_id` required) — one run's full detail, including every
  check's captured output — the JSON API's `GET /api/v1/run/{id}` omits
  output (it's meant for a human on the dashboard's run page); this tool is
  where an agent debugging a red run gets it. Each check also carries
  `logPath` and, when the dashboard is configured to serve it, `logUrl`,
  same as the JSON API's `GET /api/v1/run/{id}`.
- **`retry`** (`target` and `ref` required) — re-queues a parked ref at its
  current SHA, the same effect as `POST /api/v1/retry` or a Slack
  `:recycle:` reaction. Returns `{"status": "queued"}` on success, or an
  error if retry isn't wired up or the retry queue is full.
- **`cancel`** (`target` and `ref` required) — stops whatever is currently
  happening to a candidate and parks it, the same effect as
  `POST /api/v1/cancel` or a Slack `:x:` reaction. Returns
  `{"status": "queued"}` on success, or an error if cancel isn't wired up or
  the cancel queue is full.
- **`hook_cancel`** (`target` required) — cancels a target's currently
  running post-land hook execution, the same effect as
  `POST /api/v1/hooks/cancel`. Returns `{"status": "cancelled"}` or
  `{"status": "no-op"}` (nothing was running — not an error), or an error if
  hook cancellation isn't wired up.
- **`batch`** (`batch_id` required) — every member run of one batch (run ID,
  ref, position, outcome, SHA). Same shape as `GET /api/v1/batch/{id}`.
- **`checks`** (`target` required, `since` optional) — per-check
  red-rate/duration stats plus the queue-depth series, the dashboard's
  `/checks` page as data. Same shape as `GET /api/v1/checks`.
- **`services`** (no arguments) — the shared-services pool: every live warm
  instance with image, endpoint, age, last-used, refcount, and cumulative
  hit count. Same shape as `GET /api/v1/services`; errors with
  `"services disabled"` when no `services` block is configured.
- **`drain`** (`deadline` optional) — begins a graceful shutdown drain, the
  same effect as `POST /api/v1/drain`.
- **`deploys`** (`env` optional) — the deployment overview: every
  environment lane's desired/observed revisions, the source it follows, its
  state word, the nodes running right now, and recent finished deploys.
  Same shape as `GET /api/v1/deploys`; `env` narrows both the lanes *and*
  the recent list. Errors with `"deploy not configured"` when the daemon has
  no `deploy` block, `"no snapshot yet"` before the first pass.
- **`deploy`** (`run_id` required) — one deploy graph run's full detail,
  every node's status, duration, and captured output — where an agent
  debugging a parked environment looks. Same shape as
  `GET /api/v1/deploy/{id}`, and history-backed the same way: a run still in
  flight has no record, so use `deploys`' `running` block for that.
- **`deploy_retry`** (`env` required) — clears an environment's park so the
  next pass re-runs its whole graph, same as `POST /api/v1/deploy/retry`.
  Returns `{"status": "retried"}` or `{"status": "no-op"}` (the lane wasn't
  parked — not an error).
- **`deploy_cancel`** (`env` required) — interrupts an environment's
  in-flight graph run, same as `POST /api/v1/deploy/cancel`. Returns
  `{"status": "cancelled"}` or `{"status": "no-op"}`. The cancelled run
  doesn't park the lane, so the next pass reconciles fresh.

**Trust model.** Same as the dashboard and its JSON API: no authentication
of its own, so bind it to a trusted interface and put it behind your
proxy/tailnet if agents need to reach it remotely. `retry`, `cancel`,
`hook_cancel`, `drain`, `deploy_retry`, and `deploy_cancel` are the only
tools that mutate anything, and each is non-destructive in the same way its
`POST /api/v1/*` counterpart is — see "Trust model" above. In particular
**no tool here can deploy**: `deploy_retry` re-runs the graph for the
revision an environment is *already* pointed at, and changing that revision
is a push of the environment's desired ref.

## Requesting a GitHub PR landing

`gauntlet land-pr -config gauntlet.kdl -pr 123` posts a permission-checked
`@gauntlet merge` comment, which requests only that PR and is rejected if
it has an open prerequisite. Optional mutually exclusive `-stack`,
`-whole-stack`, `-ready`, `-prefix N`, and `-cancel` flags request the stack
through that PR, the entire stack, ready prefix, bottom N unlanded PRs, or
cancellation. `-stack` posts `@gauntlet merge stack`. Enable `github pull-requests`
in the daemon config first. The command acknowledges posting; landing
remains asynchronous.

Imported reviews have virtual queue slots named
`refs/heads/for/<target>/github/pr-<10-digit-number>` or
`refs/heads/for/<target>/gerrit/change-<10-digit-number>`. They appear in
normal queue snapshots, history, and events, but are not remote branches.
Use PR comments to withdraw GitHub requests, or change Gerrit readiness;
do not push or delete these reserved slot names. See
[review integration](design/reviews.md) for details.
