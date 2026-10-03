# Telemetry

## OTLP export

1. Point `otlp` at any OTLP/HTTP collector endpoint (e.g. a local
   `otel-collector` on `localhost:4318`; `insecure true` if it's plain
   HTTP).
2. Push a candidate. Spans should appear in whatever backend the collector
   forwards to: one root span per run, with children for the trial merge,
   each check, and the land. This is export only — SQLite `history` (if
   configured) is a separate, always-local store; OTLP doesn't feed it and
   isn't fed by it.
