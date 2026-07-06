# Services Phase A — Adversarial Code Review

Reviewer: fresh-context adversarial pass over /tmp/claude/review/services-phaseA.diff
(README, cmd/gauntlet, internal/config, internal/services, internal/queue,
internal/core, internal/executor). `go build ./...`, `go vet`, and
`go test ./internal/... ./cmd/...` all pass (no -race, no dockerlive tag, per
instructions).

Bottom line: the F1/M1/M2/M3 invariants the plan treats as load-bearing are all
honored. One real BUG (container leak + service-key poisoning when an ensure is
canceled mid-ready-poll), two low-severity BUGs, and a handful of NITs.

---

## Findings (most severe first)

### BUG 1 — Ensure canceled mid-ready-poll leaks the container AND poisons the service key until restart
`internal/services/pool.go:2958-2962` (create) — and the same root cause in
`evict`/`doEnsure`/`AnyDead`.

The cleanup path on a not-ready instance destroys it with the *same* context the
ensure ran under:

```go
if err := p.pollReady(ctx, inst, svc.ReadyTimeout); err != nil {
    logs := p.driver.TailLogs(ctx, inst)   // ctx
    p.driver.Destroy(ctx, inst)            // ctx  <-- no-op if ctx is canceled
    return ...
}
```

`containerDriver.Destroy` is `exec.CommandContext(ctx, "rm", "-f", "-v", name).Run()`.
I confirmed by direct test that `exec.CommandContext(canceledCtx, …).Run()` returns
`context canceled` and **never starts the process**. So when `pollReady` returns
because `ctx` was canceled (its own `select { case <-ctx.Done(): return ctx.Err() }`),
the follow-up `Destroy` does nothing — the container it just created keeps running.

This is reachable on the normal merge-queue path: `startCheck`'s `spanCtx` derives
from `checkCtx, cancel := context.WithCancel(r.rootCtx)`; a superseded/aborted
candidate calls `r.cur.cancel()`. If that supersede lands during the ready-poll
(precisely the window that exists *because* services are slow to start — default
60s, example 90s), the half-created instance is orphaned.

Worse than a leak: the container name is `gauntlet-svc-<token>-<key[:12]>`, derived
deterministically from the key, but `create` inserts into `p.instances` only on
success — so the orphan is untracked. The next `EnsureAll` for the same service key
misses in the map, takes the create path again, and `run -d --name <same>` fails
"name already in use". Every subsequent run needing that service parks-as-error for
the rest of the daemon's lifetime. Recovery is only at the next restart (the orphan
has the label but no record was ever written — create fails before doEnsure's record
write — so `Adopt` destroys it).

Fix: detach the cleanup context, e.g. `cctx := context.WithoutCancel(ctx)` (Go 1.21+)
for the `TailLogs`/`Destroy` in `create`, and likewise for the `Destroy` inside
`evict` (reused by `AnyDead` and the doEnsure reuse-probe-failure path). A short
`context.WithTimeout` on the detached ctx keeps shutdown bounded.

### BUG 2 — max-instances is a soft cap under concurrent misses on distinct keys
`internal/services/pool.go:2899-2907` (doEnsure).

```go
p.mu.Lock(); atCap := len(p.instances) >= p.cfg.MaxInstances; p.mu.Unlock()
if atCap { return …error… }
inst, err := p.create(...)          // lock released across the blocking create
p.mu.Lock(); p.instances[key] = inst; ...; p.mu.Unlock()
```

Single-flight only coalesces the *same* key. Two checks ensuring *different* services
concurrently both read `len == MaxInstances-1`, both pass the gate, both create, both
insert → `len == MaxInstances+1`. The plan/design call this a "hard count cap"
(services-impl.md §3.6, README "hard-caps"). The overshoot is bounded by the number of
concurrent distinct-key misses and self-heals via reap, so severity is low — but it is
not the hard cap advertised. If it must be hard, reserve the slot under the same lock
that checks the cap (increment a pending counter before releasing `p.mu`).

### BUG 3 — env-var name collision from distinct service names is silent
`internal/services/pool.go:3185` (envSafeName) + `internal/config/checks.go` validation.

`envSafeName` upcases and maps every non-alphanumeric rune to `_`. Service-name
uniqueness in `CheckSpec.validate()` is exact-string, so `service "my-db"` and
`service "my_db"` are both legal, and both map to `GAUNTLET_SVC_MY_DB_HOST/PORT`. A
check that `needs "my-db" "my_db"` gets `ensured.Env` with two `GAUNTLET_SVC_MY_DB_HOST=`
entries; the executor's env is a last-wins slice, so one service silently shadows the
other and the check can only reach one of them. No validation catches this — it is the
chunk-1 validation gap attack surface 7 anticipated. Fix: in `validate()`, reject two
service names that collide under the env-name transform (or document the constraint and
validate the derived name is unique). Low severity (needs two similarly-named services
in one spec) but silently wrong when hit.

### NIT 1 — stray lastUsed map entry after AnyDead-evict + deferred Release
`internal/services/pool.go` AnyDead/evict vs releaseKeys.

On a mid-run death, `AnyDead` calls `evict(key)` which deletes `instances[key]`,
`refcount[key]`, `lastUsed[key]`. The wrapper's `defer Release(ens)` then runs
`releaseKeys`, which (refcount now absent→0, no underflow — good) unconditionally sets
`lastUsed[key] = now` again, re-adding an entry for a key no longer in `instances`.
`Reap` only ranges `instances`, so it's never read and never cleaned — a small,
slowly-growing map leak keyed by every service that ever died mid-run. Harmless to
correctness. Could guard `lastUsed`/`refcount` writes in `releaseKeys` on
`_, ok := p.instances[key]`, or accept it. (The companion `touchRecordLastUsed` is
safe: it read-modify-writes and returns early when the record file is already gone, so
no record resurrection.)

### NIT 2 — keyhash12 (48-bit) name/alias vs full-key identity
`internal/services/pool.go:2944-2946`. `instances`/records/label key on the full
64-hex key, but the container Name and network Alias truncate to `key[:12]`. Two
distinct full keys colliding on 12 hex → `run -d --name` collision, surfaced as an
ensure error (park-as-error), not silent corruption. Correct and acceptably safe; noted
only because the truncation is a real (astronomically unlikely) collision domain and the
name is built by literal concatenation in `create` rather than a shared helper with
`namePrefix()`.

### NIT 3 — name derivation copy-pasted, not single-source
`gauntlet-svc-<token>-<key[:12]>` is spelled out in `pool.create`, while
`namePrefix()` independently builds `gauntlet-svc-<token>-`, and the dockerlive test and
adopt-test helper each re-spell it. They currently agree; a future rename must touch
several sites. Consider a single `instanceName(key)` helper. Cosmetic.

---

## Verified clean (attack surfaces, with what I found)

1. **F1 pinning** — grep of all call sites: `EnsureAll` (reconcile.go:619), `Release`
   (:624 defer), `AnyDead` (:630) are ONLY inside startCheck's `go func`. `Adopt`
   (main.go:249, before queue.New), `Reap` (main.go:357 reaper ticker), `ArmReaper`
   (daemon.go:488 in ReconcileOnce) are the only other sites. ArmReaper only takes
   `p.mu`, sets a bool, unlocks — trivial, non-blocking, safe on the reconcile
   goroutine. Verified `p.mu` is never held across a blocking driver call: doEnsure,
   ensureOne (single-flight releases mu before doEnsure), evict, Adopt, and Reap all
   drop the lock before Create/Probe*/Destroy/List. No blocking pool call is reachable
   from ReconcileOnce/advanceLane/refillLane/advanceChecks. CLEAN.

2. **M1 conversion** — gated exactly on `res.Err == nil && res.Status == core.CheckFailed`
   (reconcile.go:627); a passing check never calls AnyDead. Only `res.Err` is
   overwritten; Output/LogPath/Duration/Name are the full RunCheck result untouched.
   history/store.go:316 writes `cr.Output` and `cr.LogPath` verbatim regardless of
   Err/status (comment confirms "for every check regardless of status"), so the retained
   red output IS persisted on the converted-to-Err row. Test TestServices_MidRunDeath_M1
   asserts exactly this. CLEAN.

3. **Refcount/Release pairing** — partial ensure failure: EnsureAll releases
   `ensured.keys` before returning and returns `Ensured{}` (zero), and the wrapper only
   installs `defer Release` AFTER the error check, so no double release; `Release(zero)`
   / releaseKeys(nil) is a guarded no-op. Coalesced single-flight: each caller (leader or
   piggybacker) increments refcount once on success → N refs for N callers; failures
   increment nothing. Cancellation mid-ensure: the half-created instance is intended to
   be destroyed (leaked-but-adoptable) — see BUG 1 for why the destroy silently fails.
   CLEAN except BUG 1.

4. **Adoption/sweep** — sweep.go guards `gauntlet-svc-` explicitly and, independently,
   token is `[0-9a-f]{8}` so `gauntlet-<token>-` can never prefix-match a `gauntlet-svc-`
   name (structural disjointness holds). Guard is in the orphan-kill loop, so order vs
   Adopt is irrelevant. `<state>/services` is created with MkdirAll and is NOT passed to
   `sweepAndRecreate` (only trials/ and scratch/ are). CLEAN.

5. **Key/label/name consistency** — Create stamps `dev.gauntlet.service-key = full key`;
   InspectKey reads the same label; Adopt matches full key from the label, never the
   name; record filename is the full key (record.go recordPath). One source of truth for
   *identity* (the full key); names/aliases are the documented 12-hex truncation. See
   NIT 2/3 for the truncation collision domain and copy-paste. CLEAN.

6. **Config/validation seams** — A3 runtime precedence in cmd (main.go:196-203) matches
   the amendment: local ⇒ ModePublish + services.Runtime; container ⇒ ModeNetwork +
   executor.Runtime, with the Apple `container` hard-fail. daemon.go applyDefaults only
   defaults services.Runtime under `Executor.Kind == "local"` and validate() cross-checks
   the container-executor conflict — so a non-defaulted "" under container executor is
   legal, tested. RequiresServices gating is present at BOTH ParseChecks sites
   (reconcile.go:1033 batch, :1387 serial/speculate). Typed-nil gotcha handled: main.go
   assigns `qcfg.Services = pool` only when pool != nil, keeping the interface a genuine
   nil. CLEAN.

7. **Env var mangling** — dash→underscore works; the collision case is BUG 3.

8. **Everything else** — §4.6/§3.7/§2.6 matrix tests assert what their names claim
   (spot-checked single-flight count, reap-skips-refcounted, release-touch M3,
   arm-gating q3, adopt mode-mismatch M2, ensure-failure TailLogs+Destroy, env/networks,
   record round-trip + atomic-replace, gating-loud, needs-free-unaffected, env-injection).
   Destroy uses `rm -f -v` (anonymous volumes removed, m4). Records removed on evict/
   mode-mismatch/adopt-reject. The shared per-daemon network is never removed (stated,
   acceptable — it's one network, reused). README matches the code (runtime-only-under-
   local, Apple hard-fail text, distroless ready-command caveat, count-not-resource cap,
   cross-repo-impossible, hooks-can't-declare). CLEAN.
