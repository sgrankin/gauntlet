# Phase B adversarial review — findings

Reviewer: fresh-eyes adversarial. Repo: /Users/sgrankin/Code/x/gauntlet.
Diff: /tmp/claude/review/phaseB.diff (~2.6k lines).

## Verdict
No BLOCKERs, no correctness BUGs found. Two NITs (one small in-principle
memory leak, one doc-wording imprecision). `go build ./...`, `go vet`, and
`go test` on all six touched packages PASS.

---

## Findings

### NIT-1 — `p.hits` is the sole pool map never pruned on evict (slow leak)
`internal/services/pool.go` — `evict` (~486) deletes `instances`,
`refcount`, `lastUsed`, `createdAt` for the key but deliberately NOT `hits`
(documented ~475). The doc justifies keeping the count across an
evict+RECREATE cycle (spec still in use), which is sound. But it does not
address the permanent-orphan case: once a key is reaped for idle-ttl AND the
spec never returns (a config edit, a deleted branch, or — per servicekey.go's
own doc — the one-time full-pool recycle THIS upgrade forces by extending the
key encoding), `hits[key]` persists forever. Every other per-key map is
pruned; `hits` is the lone exception, exactly the "NIT-1 lastUsed leak shape"
the pool already guards against elsewhere (releaseKeys, ~430).

Impact: memory only, invisible in output (Snapshot ranges `p.instances`, so
orphaned `hits` entries never render). One `int` per distinct-reused
historical key. Bounded by daemon uptime between restarts — the same bound
the auto-retry map and `done` rely on — so genuinely low severity, but worth
a deliberate call rather than an accident.

Fix options: (a) accept + document the bound alongside the existing
"deliberately NOT cleared" comment; or (b) prune `hits[key]` in `Reap` after
a successful destroy (loses the cumulative-across-idle-recreate benefit the
comment wants). (a) is probably the right call given the tiny footprint.

### NIT-2 — `Hits` counts warm-resolution events, not "EnsureAll calls" (doc)
`internal/services/pool.go` doEnsure (~257) increments `hits[key]` once per
reuse-path doEnsure. Single-flight piggybackers (ensureOne ~208) never reach
doEnsure — they `<-call.done` then only bump refcount — so N concurrent
callers coalescing onto one warm instance record **1** hit, not N. This is
the OPPOSITE of the double-count risk one might fear (verified: no
double-count), but it means the field doc ("how many EnsureAll calls resolved
this key by reuse", InstanceStatus.Hits ~641, and the API/MCP "cumulative
reuse count") slightly overstates it — it's warm-resolution events, an
undercount vs. actual reuse callers whenever coalescing happens. Harmless for
the "is reuse happening?" signal (non-zero still means yes). Reword the doc,
or leave it — defensible as a warm-resolution proxy.

---

## Verified clean, per surface

### A. Auto-retry-once
- **Loop safety (the phase-1 §9.2 ban): CLEAN.** `syncBookkeeping`
  (reconcile.go ~200) prunes `autoRetried` with the IDENTICAL condition
  (`!ok || c.SHA != sha`) on the SAME `cands` map in the SAME loop as `done`
  (~191). The two can never diverge: whenever `autoRetried[ref]` is pruned,
  `done[ref]` is too, and vice-versa. So a re-park at the same SHA keeps BOTH
  entries (budget stays spent → second error stays parked); a SHA move or
  vanished ref drops both (fresh budget, correct). `cands` is the full git
  ref set (`discoverCandidates`), independent of park state, so un-parking
  (delete from `done`) never trips a prune. Even a hypothetical wrong prune
  couldn't loop: a parked ref never spontaneously re-tests.
- **No same-tick self-prune:** `syncBookkeeping` runs at the TOP of
  `reconcileTarget`; the park+`maybeAutoRetry` record happens LATER same tick
  (in `advanceLane`→finish paths). The just-written entry is seen for pruning
  only next tick, when the ref is still at the same SHA. No staleness window.
- **rejectBatch mid-loop mutation: CLEAN.** Loop iterates `links` (a slice);
  `clearParkAndRetry` mutates `done[target]` (a map) — no iterator hazard.
  Per-member refs are distinct, so each member clears only its own park.
- **finishRun batch OutcomeError: CLEAN.** Parks all members first, then a
  second loop emits+maybeAutoRetry per member; each clears its own distinct
  ref. Order is park → EventError → EventQueued/EventRetryRequested (correct
  arrival order).
- **Services ensure-failure budget: CLEAN.** ensure-fail parks OutcomeError →
  auto-retry → re-ensure → fail → park → budget spent → stays parked. Exactly
  two ensure attempts, then human. Budget holds.
- **Cancel: CLEAN.** Cancel parks `OutcomeRejected`+cancelDetail
  (command.go ~205, ~229); `maybeAutoRetry` only fires on `OutcomeError`.
  Reconcile is single-goroutine, so a cancel command and an error-finish
  serialize — either order ends in a Rejected park (cancelWaiting re-parks if
  auto-retry already un-queued it); no retry escapes a cancel.
- **All four park sites wired:** finishRun, rejectBatch, rejectPreMerge,
  rejectRun each call `maybeAutoRetry` after their emit.
- **`*bool` config: CLEAN.** `main.go:480` `*cfg.AutoRetryErrors` is the only
  deref; `cfg` comes only from `LoadDaemon` (main.go:108) which always calls
  `applyDefaults` (daemon.go:497), defaulting nil→true. No other Daemon-load
  path. Tests cover explicit false/true round-trip and the default.

### B. Pool observability
- **Snapshot: CLEAN.** Holds `p.mu` for map ranges + sort only — no I/O, no
  driver/blocking calls. Returns a freshly-`make`d `[]InstanceStatus` of
  scalar copies; no slice/map aliases pool internals. `key[:12]` is panic-safe
  (ServiceKey is always 64-hex). Deterministic (service,key) sort.
- **hits increment placement: CLEAN** — reuse path only, no double-count (see
  NIT-2).
- **Template escaping: CLEAN.** Image/endpoint/keyHash are pushed-branch-
  derived (spec image IS attacker-controlled) but render through html/template
  `{{.Field}}` auto-escaping. The one `template.HTML` field (LastUsed via
  formatTime) interpolates only a daemon-generated RFC3339 timestamp, no spec
  data (server.go:1010).
- **Degradation consistency: CLEAN.** Dashboard omits section (nil snapshot);
  API 503 `{"error":"services disabled"}`; MCP error result "services
  disabled"; CLI omits section. Each matches its surface's existing
  convention. `gauntlet status` fetch is best-effort: `httpGetBody` errors on
  status≥300, so a 503 → err → `svc` nil → section omitted; malformed JSON →
  nil → omitted. Cannot fail the command.
- **createdAt lifecycle: CLEAN.** Set on create (~289) and adopt (~571),
  deleted on evict (~491). No leak (unlike `hits`).

### C. memory/cpus flags
- **servicekey encoding: CLEAN.** Memory then CPUs appended after IdleTTL
  (servicekey.go ~77), matching the doc comment; length-prefixed via
  writeString. servicekey_test variance cases (memory=4g, cpus=2 vs base) DO
  catch an omitted field (base now carries 2g/1.5, so an ignored field would
  collide the keys and fail the test).
- **Validation regexes: CLEAN.** `memoryPattern (?i)^[0-9]+[bkmg]?$` and
  `cpusPattern ^[0-9]+(\.[0-9]+)?$` both fully anchored. Rejects "lots",
  "2 gigs", "-1", "2gb" (two-letter unit), ".5"; accepts "2g"/"2G"/"512m"/
  bytes and "1.5"/"0.5"/"2". Only checked when non-empty. Matches docker/
  podman's actual (single-letter-suffix, no-decimal-memory) grammar closely
  enough for a plausibility gate.
- **createArgs purity/placement: CLEAN.** Pure function; --memory/--cpus
  emitted after mode flags, before env and image (image stays last). Omitted
  entirely when empty. Unit-tested without a live runtime.

### D. Cross-feature integration
- MCP: 9 tools registered (status/runs/run/retry/cancel/hook_cancel/batch/
  checks/services), README says "Nine" and documents the three new ones —
  coherent.
- README sections (retry semantics, config reference, memory/cpus, services
  visibility) live in distinct regions; no leftover collision from the three
  concurrent edits.
- New tests assert real behavior (event Detail strings, mergeTreeCalls deltas,
  refcount/hits values, key-change detection, 503 vs empty-pool), not
  tautologies.
