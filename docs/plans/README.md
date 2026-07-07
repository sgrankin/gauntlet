# Plans — historical records

Every plan in this directory has been executed (the one exception:
scale.md's scale-out axes are deliberately unbuilt — see its entry below);
these are frozen build records, not roadmaps. **Do not renumber or restructure their
sections**: code comments throughout `internal/` and `cmd/` cite them by
path and section (`docs/plans/phase5.md §10` style), as do two runtime
error messages, so the section numbering is load-bearing.

- [phase1.md](phase1.md) — the single-lane phase-1 daemon: ref-derived
  queue, `merge-tree` trial merges, CAS land/delete, local checks, KDL
  config. §8 (invariant→mechanism map) and §9 (review amendments) are the
  most-cited design rationale.
- [phase23.md](phase23.md) — phases 2+3: GitHub status + Slack channels,
  container executor, SQLite history, dashboard, OTLP export.
- [phase5.md](phase5.md) — batching and speculation as per-target queue
  modes. Documents the reserved-but-rejected growth knobs
  (`on-batch-red "bisect"`, the adaptive window governor).
- [scale.md](scale.md) — scaling position paper. The one plan with
  deliberately unbuilt content: its prerequisites (`idleSince`,
  `auto-retry-errors`) shipped; the scale-out axes (builder parking,
  remote executors) are intentionally future work.
- [services.md](services.md) / [services-impl.md](services-impl.md) —
  shared-services design and its three-chunk execution plan (phases A and
  B shipped). Unbuilt-by-design: the `artifact`/`oci-unpack` drivers and
  Apple `container` networking (§9).

Adversarial-review artifacts for these plans live in
[../reviews/](../reviews/README.md).
