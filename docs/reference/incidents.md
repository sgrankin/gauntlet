# Incident configuration

## Infrastructure circuit breaker

The breaker is opt-in and per target:

```kdl
circuit-breaker {
    threshold 3
    window "5m"
    backoff "30s"
    max-backoff "10m"
}
```

These are defaults when the block is present. Threshold is 2–100; maximum backoff
is at most one hour. Only executor/daemon infrastructure errors count, across
distinct revisions. Ordinary red checks and model classifications do not open it.
Suspension persists, stops dispatch/publication, and probes one change after
bounded backoff. Recovery preserves manual pause. Target status exposes `circuit`.
See [incident controls](../guides/incidents.md) for emergency command syntax,
permissions, revision binding, and recovery.
