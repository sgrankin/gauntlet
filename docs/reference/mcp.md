# MCP

## MCP

The daemon also exposes an MCP (Model Context Protocol) server
(`internal/mcp`) at `/mcp`, mounted on the same bind/port as the dashboard
and [HTTP API](http-api.md) — there's no separate port to configure. It speaks
the standard Streamable HTTP transport, so any MCP-capable agent or client
can connect directly:

```sh
claude mcp add --transport http gauntlet http://localhost:8080/mcp
```

Fourteen tools are exposed, mirroring the [HTTP API](http-api.md) (same lowerCamel
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
`POST /api/v1/*` counterpart is — see [security](../operations/security.md). In particular
**no tool here can deploy**: `deploy_retry` re-runs the graph for the
revision an environment is *already* pointed at, and changing that revision
is a push of the environment's desired ref.
