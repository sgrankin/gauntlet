# Worker configuration

See [pull workers](../guides/workers.md) for setup, source transport, services,
and recovery. These are operator settings, separate from repository checks.

## Scheduler listener

| `workers` field | Default | Contract |
|---|---|---|
| `listen` | `127.0.0.1:8081` | Worker protocol listener. |
| `token-env` | `GAUNTLET_WORKER_TOKEN` | Shared bearer token, at least 32 bytes; withheld from candidate environments. |
| `tls-cert`, `tls-key` | None | PEM certificate/key paths, relative to the config file; required together. Non-loopback listeners require TLS. |
| `lease` | `1m` | 3 seconds–1 hour. Workers renew while commands run. |
| `reconnect-grace` | `2m` | At least one lease, at most 1 hour; delays reassignment after scheduler restart. |

Remote profiles use `executor "NAME" kind="remote"`:

| Field | Default | Contract |
|---|---|---|
| `labels` | None | Every required label must be advertised by the worker. |
| `cpus` | `1` | Positive reserved cores; fractional values use millicores. |
| `memory` | `128m` | Positive reserved bytes, with optional `b`, `k`, `m`, or `g` suffix. |
| `supports-images` | `false` | Allow image consumers; matching worker profiles must use containers. |

Remote profiles require a `workers` block. Runtime, image, environment, cache,
and mount settings belong in worker-local profiles. The daemon's default profile
remains local/container for operator-owned hooks and deployment commands.

See [capacity pools](capacity.md) for optional autoscaling providers.

## Worker file

`gauntlet worker -config FILE -state DIRECTORY` loads a worker file:

| Field | Default | Contract |
|---|---|---|
| `scheduler` | Required | HTTP(S) base URL. Non-loopback endpoints require HTTPS. |
| `name` | Hostname | Unique worker identity, at most 128 bytes. |
| `token-env` | `GAUNTLET_WORKER_TOKEN` | Same shared token as scheduler. |
| `labels` | None | Capabilities, operating system, or runner version labels. |
| `cpus`, `memory` | Required | Capacity available to check reservations. |
| `max-executions` | Required | 1–4,096 concurrent jobs. Resource reservations can admit fewer. |
| `urgent-reserve` | `0` | Reserved slots and proportional CPU/memory capacity; smaller than `max-executions`. |
| `shutdown-timeout` | `5m` | Positive, at most 24 hours; drain deadline before stopping remaining jobs. |
| `executor` | Local | Default and named local/container profiles, using the [execution reference](execution.md). Remote profiles are not worker backends. |
| `services` | Disabled | Container service permission and per-pool limits. |

Profile matching includes container capability for image consumers and service
support for checks declaring needs. Workers advertise total reservations and
current usage, including work that is stopping after a stale lease.

## Protocol and retained state

- The worker listener requires bearer authentication on every endpoint.
- Workers claim jobs from the scheduler; resource matching and priority selection
  happen there. Draining workers receive no new jobs.
- The durable backlog permits up to 10,000 active jobs. Producers wait for room.
- Jobs, retry lineage, and current leases persist in `workers.db`. Queue control
  checkpoints preserve the exact prepared commit/run identity needed for adoption.
- Only work adopted by the current scheduler can be newly assigned. Unadopted
  work is retired after the recovery grace and two minutes without a consumer.
- Terminal worker records and unacknowledged worker results expire after 24 hours.
- Source packs include at most 256 required commits and reuse up to 128 worker
  cache roots. Received packs undergo strict Git object validation.
- Result messages are bounded to 16 MiB. Log uploads are bounded to 256 MiB;
  oversized/unavailable logs retain output tails and reports with a diagnostic.
- Raw uploaded attempt logs have SHA-256 checksums. Derived diagnostic logs may
  add decision notes and concatenate retries; they are separate artifacts.
