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
  missing or the body isn't valid JSON; `405` for any method but `POST`.

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
  JSON; `405` for any method but `POST`.

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

## Idle signal

`idleSince` (also on the MCP `status` tool and `gauntlet status`, plus a
muted line on the dashboard index page) exists for external park/wake
automation — e.g. an Azure Function that deallocates a parked-builder VM
once the daemon has been idle long enough, and re-wakes it when refs arrive
(see [design/scaling.md](design/scaling.md)). It's the whole daemon's idleness, not just
the queue's: no waiting candidates and no in-flight runs across every
target, AND no target's post-land hook currently running or backlogged.
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
branch.

## MCP

The daemon also exposes an MCP (Model Context Protocol) server
(`internal/mcp`) at `/mcp`, mounted on the same bind/port as the dashboard
and its JSON API above — there's no separate port to configure. It speaks
the standard Streamable HTTP transport, so any MCP-capable agent or client
can connect directly:

```sh
claude mcp add --transport http gauntlet http://localhost:8080/mcp
```

Nine tools are exposed, mirroring the JSON API above (same lowerCamel field
names, so an agent reading both sees one vocabulary):

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

**Trust model.** Same as the dashboard and its JSON API: no authentication
of its own, so bind it to a trusted interface and put it behind your
proxy/tailnet if agents need to reach it remotely. `retry`, `cancel`, and
`hook_cancel` are the only tools that mutate anything, and each is
non-destructive in the same way its `POST /api/v1/*` counterpart is — see
"Trust model" above.
