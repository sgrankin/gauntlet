# History, dashboard, and telemetry

```kdl
history "/var/lib/gauntlet/history.db" {
    sample-every "10s"
    depth-retention "336h"
}
dashboard "localhost:8080" {
    url "https://gauntlet.internal.example"
}
otlp "localhost:4318" {
    insecure true
}
```

## History

| Field | Default | Contract |
|---|---|---|
| `history` path | Disabled | SQLite run/check/hook history and queue-depth samples. |
| `sample-every` | Queue `poll-interval` | Positive sampling duration. |
| `depth-retention` | `"336h"` | Positive retention for depth samples; other history rows are not automatically pruned. |

History feeds diagnostics and restart park seeding. Remote provenance remains
landing truth. See [state and retention](../operations/storage.md).

## Dashboard

| Field | Default | Contract |
|---|---|---|
| `dashboard` bind | Disabled | Starts HTML, JSON API, MCP, log serving, and configured webhook route. |
| `url` | `http://<bind>` | Public base URL for outbound links; set explicitly behind ingress. |

There is no built-in authentication. Use [trusted ingress](../operations/security.md).
Full logs are served under `/run/{id}/log/{name}`; pruned/missing logs return 404.

The browser's Theme selector offers System, Light, and Dark · amber. Light uses
the Classic Mac layout; dark uses an amber terminal palette. Explicit selection
persists in the browser. Without JavaScript, browser preference still applies.

## OTLP

`otlp` enables HTTP trace and metric exporters. Its argument is the collector
endpoint; `insecure true` uses plaintext HTTP. Without an endpoint, instruments
have no export destination. OTLP and local SQLite history are independent.

| Instrument | Units / dimensions |
|---|---|
| `gauntlet.node.duration` | Milliseconds; target, node name, kind, outcome. |
| `gauntlet.node.peak_rss` | Bytes; same dimensions, when measured. |
| `gauntlet.node.user_cpu`, `gauntlet.node.sys_cpu` | Milliseconds; same dimensions, when measured. |
| `gauntlet.queue.depth` | Per target and waiting/in-flight/parked state. |
| `gauntlet.slots.in_use` | Configured host capacity occupancy; absent without a cap. |
| `gauntlet.runs.in_flight` | Daemon-wide active runs. |

Metrics use bounded dimensions. Run IDs, refs, and SHAs belong in spans and
history, not metric attributes. Container executors do not measure every process
resource statistic. See [telemetry verification](../guides/telemetry.md).
