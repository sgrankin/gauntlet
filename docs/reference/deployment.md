# Deployment configuration

## Deployment

Deployment reconciles desired environment refs independently of landings.
Use [post-land hooks](automation.md) for one-off reactions, or this subsystem
for tracked/manual environments and promotion. The [deployment model](../architecture/deployment.md)
explains desired and observed state.

```kdl
deploy {
    environment "dev" {
        source "main"                // a branch of the watched remote
        track                        // the daemon owns this lane's desired ref
    }
    environment "prod" {
        source env="dev"             // what dev has finished deploying green
        track
        max-parallel 4               // node-level, within this env's graph
        on-desired-move "finish"     // or "cancel": kill the in-flight graph
    }
    environment "prod2" {
        source env="dev"
        nodes "migrate" "app1"       // subgraph; closure over `after` enforced
        // no `track`: manual — only a human (or `gauntlet deploy`) moves it
    }
}
```

- **`source`** is required, and takes exactly one of two forms.
  `source "main"` names a **branch** of the watched remote, resolved as
  `refs/heads/main`; `source env="dev"` names another **environment**, and
  resolves to *that environment's observed ref* — the revisions this
  environment may run are exactly the ones `dev` has finished deploying
  green. That second form is the whole promotion story: the environment DAG
  is data flow (one lane's input is another's output), not a cross-
  environment scheduler. A source that doesn't resolve yet — a branch that
  doesn't exist, an environment that has never deployed — makes the lane a
  no-op, never an error: a config ahead of reality is an ordinary state.
- **`track`** is a bare presence node, not a boolean: writing `track true`
  is a loud parse error, not a second spelling. Present ⇒ the daemon
  CAS-advances this lane's desired ref to the source tip whenever the source
  moves. Absent ⇒ **manual**: the daemon never writes the desired ref at
  all, and a human push (or `gauntlet deploy`, see [HTTP API](cli.md#cli)) is
  the only thing that moves it.
  A tracked lane follows its source **verbatim, including backwards** — there
  is deliberately no ancestry gate, because refusing to follow a force-reset
  source is exactly wrong during the incident that caused it. Pinning an
  environment is therefore the config edit `track` → manual (a
  restart-shaped operation, not an API toggle a restart would forget).
- **`nodes`** selects a subgraph of the revision's deploy nodes; omitted
  means the whole graph. Selection is **transitively closed over `after`**:
  naming `app1` pulls in the `migrate` it declares it needs, because running
  a node without its declared prerequisite is precisely the silent lie spec
  rejection exists to prevent. The resulting graph always runs in
  *declaration* order, never selection order, so a subgraph schedules
  identically to the way it would as part of the whole graph. Whether a
  named node exists is a property of the revision being deployed, so an
  unknown name is a per-revision spec rejection (below), not a config error.
- **`max-parallel`** bounds concurrency *within* this environment's graph
  (default `1` — serial, like a check spec's own default; legal range 1–64).
  It is not a host bound: every deploy node also takes one slot from the
  daemon-wide `max-executions` budget that checks, image builds, and hooks
  draw from, so a deploy burst and a check burst negotiate over the same
  honest capacity.
- **`on-desired-move`** decides what happens to an in-flight graph run when
  the desired ref moves under it. `"finish"` (the default) lets it complete
  against the revision it started on, records it, and reconciles again on
  the next tick — one more tick, one more run; a graph that has already
  started migrating a database is not something to kill by default.
  `"cancel"` kills it immediately, records the cancellation, and starts the
  newer revision. Either way the observed ref only ever moves to a revision
  whose graph actually finished green.

Environments are independent: `prod` and `prod2` deploy side by side, and one
environment's failure never stops another's lane. Within one environment
there is never more than one graph run in flight — an environment sees a
strict sequence of deploy attempts.

**Reconciliation** runs on the same poll tick as the queue. Each pass reads
every ref once and processes each lane against that single snapshot, so a
chain (`main` → `dev` → `prod`) propagates one link per tick. That is the
level-triggered contract, not a limitation: there is no deploy queue and no
backlog policy vocabulary, because intermediate desired states that came and
went between ticks simply never existed as far as the lane is concerned. A
crash costs nothing but latency — both refs are on the remote, so a restarted
daemon re-derives every lane from them.

**Parks, retries, and cancellation.**

- A **red node** fails the graph fast (in-flight siblings cancelled,
  unstarted nodes recorded as blocked naming their failed edges, the red node
  named as culprit) and **parks** the lane at that desired revision. The
  observed ref does not move: a half-deployed environment is recorded as
  exactly that. No retry loop, exactly like a red candidate.
- An **infra-shaped failure** (`OutcomeError`: the export failed, the
  executor never returned a verdict, the observed-ref CAS was lost to another
  writer) parks the same way, but gets the standing **auto-retry-once**
  budget per (environment, desired SHA) — the same top-level
  `auto-retry-errors` knob (default true) the queue uses, with the same
  semantics: error parks only, never a red verdict, exactly once per desired
  revision, fresh budget on a new one, and never while draining.
- A **spec rejection** — the revision has no readable/valid check spec, its
  `nodes` selection names something it doesn't declare, a node selects an
  executor profile this daemon doesn't define, or it declares no deploy nodes
  at all — parks with a `spec reject: ...` detail and **never** a red
  verdict: no command ran, so no node has a verdict to report. An environment
  must never silently believe it deployed when no command existed to run.
- A park is cleared by **any new desired SHA** (including a rollback push to
  an older one) or by an **explicit retry** — `POST /api/v1/deploy/retry`,
  the MCP `deploy_retry` tool, or the dashboard button (see
  [HTTP API](http-api.md)). Retry re-runs the **whole graph**, never the red suffix:
  green nodes run again and either self-skip through the diff protocol
  ([check reference](deploy-nodes.md#deploy-nodes)) or re-execute idempotently. Resuming
  from recorded per-node results would make history rows correctness state,
  which they are forbidden to be. An operator retry deliberately does *not*
  refresh the auto-retry-once budget — that budget exists to absorb infra
  flakes without a human, and a human is exactly what just arrived.
- **Cancelling** (`POST /api/v1/deploy/cancel`, MCP `deploy_cancel`)
  interrupts one attempt: the run records an externally-concluded outcome and
  does *not* park, so the next pass reconciles fresh — with desired
  unchanged, it deploys the same revision again. Cancelling is not a way to
  stop deploying; a freeze is manual mode plus not pushing.

**Deploy commands must tolerate re-execution.** A crash mid-graph, a
cancel-then-retry, and an ambiguous failure all re-run the graph. This is the
idempotence gauntlet already demands of every reconcile step, pushed across
the executor boundary, and it is not optional.

**Drain and idle.** A graceful shutdown (`shutdown "drain"`, a first SIGTERM,
or `POST /api/v1/drain`) closes deploy admission too: no new graph run
starts, and in-flight ones are allowed to finish — the daemon waits for them
after the queue stops landing, rather than killing a run mid-migration. A
second signal (or the drain deadline) forces the kill, which is the
crash-equivalent path deploy commands must tolerate anyway. Deploy activity
also composes into the daemon-wide idle signal exactly as hooks do: a lane
with a graph running, *or* one whose next pass would start one, keeps the
daemon non-idle, so a scale-to-zero timer can't deallocate the builder
between those two states (see [api.md's "Idle signal"](http-api.md#idle-and-drain)).

**Validation** happens at load, like everything else here, and only when the
block declares at least one environment (a missing block, or a bare
`deploy {}`, is simply disabled):

| Rule | Error |
| --- | --- |
| name non-empty, unique, a single valid ref path component | `deploy: environment "prod/eu": name must not contain '/'` · `deploy: environment "pr od": name is not a valid ref name (no * ? [ \ ~ ^ : .. @{ or whitespace)` · `deploy: environment "prod": duplicate` |
| exactly one `source` form | `deploy: environment "prod": source is required` · `deploy: environment "prod": source and source env= are mutually exclusive — pick one` |
| a branch source is a branch, not a ref | `deploy: environment "dev": source "refs/heads/main" must be a branch name, not a ref (drop the "refs/" prefix)` |
| an `env=` source names a declared environment (forward references are fine) | `deploy: environment "prod": source env "devv": no such environment declared` |
| no cycle in `env=` sources, self-reference included (the error names the environment the cycle was *closed* at, in declaration order) | `deploy: environment "prod": source env "prod": dependency cycle` |
| `max-parallel` in 1–64 (an omitted or `0` value takes the default `1`, so only a negative or oversized one can fail) | `deploy: environment "prod": max-parallel must be between 1 and 64, got 128` |
| `on-desired-move` is `finish` or `cancel` | `deploy: environment "prod": on-desired-move must be "finish" or "cancel", got "abort"` |
| `nodes` entries non-empty and unique | `deploy: environment "prod": nodes "app1": duplicate` |

Every deploy node's full log is written to
`<state>/logs/<deploy run ID>/<n>-<node>.log.zst` — the same per-run
directory shape a check's logs use, so `log-retention`'s sweep covers deploy
logs with no extra configuration. With `history` configured, finished runs
land in the `deploys`/`deploy_nodes` tables feeding the dashboard's
`/deploys` and `/deploy/{id}` pages; history stays efficiency-only, as
always — lose the database and the overview rebuilds from refs.
