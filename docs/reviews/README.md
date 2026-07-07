# Reviews — archived point-in-time artifacts

Dated adversarial/holistic review snapshots, kept as the record of what
was found and how each finding was dispositioned. Nothing here is a
living document; every ship-blocking finding was fixed before its feature
landed (the final gate was `holistic-pregolive.md`, 2026-07-06), and even
the acknowledged nits were later closed (the phase-B `hits`-map prune and
btrack S2's `ignored_refs` retention both shipped — see
`internal/services/pool.go` and `internal/history.PruneIgnoredRefs`).

Known accepted leftovers, all deliberate scope choices rather than open
bugs: `dashpolish-` / `execmounts-adversarial.md`'s parity notes and the
docker-socket trust decision, explicitly out of scope for their diffs.
