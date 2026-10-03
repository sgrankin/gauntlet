# HTTP API

The dashboard listener serves JSON under `/api/v1`, full logs under `/run`,
and [MCP](mcp.md) under `/mcp`. Keep operator routes behind
[trusted ingress](../operations/security.md). JSON errors use `{"error":"..."}`;
response fields generally use lowerCamel names. Control request fields use the
capitalization shown below.

## Read endpoints

| Endpoint | Parameters | Result |
|---|---|---|
| `GET /api/v1/status` | None | All target snapshots, waiting/parked/in-flight work, lifecycle, pause/circuit state, and idle signal. |
| `GET /api/v1/runs` | Required `target`; `limit` defaults to 20 | Recent runs, newest first. |
| `GET /api/v1/run/{id}` | Run ID | Check/hook results and log links; check output is available through logs or MCP `run`. |
| `GET /api/v1/batch/{id}` | Batch ID | Member runs and positions. |
| `GET /api/v1/checks` | Required `target`; optional `since` duration | Check statistics and queue-depth series. |
| `GET /api/v1/services` | None | Live pool instances, leases, reuse, and capacity. |
| `GET /api/v1/deploys` | Optional `env` | Desired/observed revisions, active graphs, and recent deployments. |
| `GET /api/v1/deploy/{id}` | Run ID | Finished deployment graph, node results, and output. |
| `GET /api/v1/failures` | Optional `check` | 20 recent model observations and actual retry/pass counters. |
| `GET /api/v1/policy-decisions` | None | Bounded queue policy audit, hashes, requirements, and errors. |
| `GET /run/{id}/log/{name}` | Run/check or hook name | Full uncapped log, containment-checked beneath the log root. |

Missing resources return 404; missing/disabled stores or not-yet-published
snapshots return 503. Statistics count a shared batch suite once. Policy audit
entries describe completed evaluations, not hypothetical command previews.

```sh
curl -s 'http://localhost:8080/api/v1/runs?target=main&limit=5' | jq .
```

## Queue and lifecycle actions

| Endpoint | JSON body | Effect |
|---|---|---|
| `POST /api/v1/retry` | `{"target":"main","ref":"REF"}` | Requeue a parked ref at its current revision. |
| `POST /api/v1/cancel` | Same | Cancel and park the named ref; batch siblings requeue. |
| `POST /api/v1/hooks/cancel` | `{"target":"main"}` | Cancel the target's current hook execution. |
| `POST /api/v1/deploy/retry` | `{"env":"prod"}` | Clear a deployment park and rerun its graph. |
| `POST /api/v1/deploy/cancel` | `{"env":"prod"}` | Cancel the active graph; the lane may reconcile again. |
| `POST /api/v1/drain` | Optional `{"deadline":"RFC3339"}` | Stop new admission, finish admitted work/hooks, then exit. |
| `POST /api/v1/control` | See below | Queue an operator control request. |

Invalid bodies return 400; disabled controls return 503; full command buffers
return 429; unsupported methods return 405. Queue acknowledgements are not
proof that a command has applied. Inspect status for the resulting state/error.

```sh
curl -s -X POST http://localhost:8080/api/v1/retry \
  -H 'Content-Type: application/json' \
  -d '{"target":"main","ref":"refs/heads/for/main/alice/fix"}'
```

## Control requests

- `Kind`: `pause`, `resume`, `urgent`, `merge-paused`, or `merge-anyway`.
- `Target`: configured target; `*` is permitted only for pause/resume.
- `Actor`: audit label. It does not authenticate the caller.
- `Reason`: required operator explanation.
- `Revisions`: ordered `{ref, sha, version}` list from live status for urgency
  and emergency requests; stale selections require a new request.
- `OverridePause`: independent permission to run one merge while the target
  remains paused. Required for `merge-paused`, which runs all validation.

`merge-anyway` requires operator enablement and waives validation; provenance,
source authorization, signing, policy, and CAS remain mandatory. Inspect target
`controlError`, `pause`, and `circuit` after a 202 acknowledgement. See
[incident controls](../guides/incidents.md) for safety and GitHub command behavior.

## Idle and drain

`idleSince` is present when every target and post-land hook has been idle. Pending
checks, queues, and hook work suppress it. The lifecycle reports `running`,
`draining`, or `drained`, plus in-flight counts and drain timing. There is no
undrain operation; restart resumes admission. Deployment lanes have their own
state and should be inspected separately when automating host shutdown.

## Trust model

The standalone API represents one trusted ingress principal. JSON cannot supply
`Principal`; embedded handlers may use `WithPrincipalResolver` for an identity
established by a trusted authenticator. Cross-origin browser controls are rejected.
These measures do not replace ingress authentication. See
[policy identity](policy.md#identity-and-diagnostics).
