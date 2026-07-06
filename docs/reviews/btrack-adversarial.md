# B-track adversarial review — a80a04c284dd..43f742f83fe2

Scope: durable hook owed/skipped markers (S1-C), operator retry-intent
persistence (S3), global ignored-refs capture (S7c), hook live-state + new
EventKinds (S5), batch/checks JSON surfaces, flock single-instance guard (S2),
executor scratch relocation + container-orphan sweep (S16), batch-summary
parallelization (S6), shutdown wg. `go build ./...` and `go vet ./...` clean.

Verdict: **one blocker (cross-daemon container kill), two should-fix, three
nits.** The crash-window durability core (S1-C/S3) is sound — I traced the
happens-before edges and they hold.

---

## BLOCKER

### B1. `sweepContainerOrphans` kills a *live sibling daemon's* containers — the flock does not protect against this
`cmd/gauntlet/sweep.go:44-63`, `cmd/gauntlet/main.go:202-223`, `internal/executor/container.go:317`

The startup orphan sweep runs `<runtime> ps --filter name=gauntlet- --format
{{.Names}}` and `<runtime> rm -f <name>` on every match. Container names are
`gauntlet-<runID>-<check>` (`containerName`, container.go:317) — the
`gauntlet-` prefix is **host-global**, identical for every gauntlet process on
the box.

The flock (S2) is taken on `<stateDir>/gauntlet.lock` — **per state dir**. Two
gauntlet daemons on one host with different `-state` dirs each acquire their
own distinct lock and both start successfully (that per-state-dir keying is
itself the signal that multi-daemon-per-host is a supported topology).
Daemon B's startup sweep then `ps`-matches and `rm -f`s daemon A's **live,
in-flight** check containers.

The code's own safety claim is wrong:
```
// only safe now that AcquireLock (S2) above guarantees no live
// sibling daemon's own in-flight containers could be mistaken for orphans.
```
AcquireLock guarantees no sibling *on the same state dir*. It says nothing
about a sibling on a different state dir, whose containers share the global
`gauntlet-` namespace. `sweepAndRecreate` on trials/scratch/hooks (all rooted
*under* the locked state dir) genuinely is lock-protected — the container sweep
is the one that escapes the lock's scope, because it keys on a host-global
name, not a path under state.

Failure scenario: multi-repo host, daemon A running a check, daemon B restarts
(systemd) → A's check container is `rm -f`'d out from under it → A's check
errors/fails spuriously, silently. This is precisely the "sweep destroys a live
daemon's work" hazard S2 was added to prevent, reintroduced through a namespace
the lock doesn't cover.

Fix: namespace container names with a per-daemon token (e.g. a short hash of
the absolute state dir, or a random instance id minted at startup) and filter
the sweep on `name=gauntlet-<token>-` / a per-daemon label, so a daemon only
ever reaps its *own* orphans. Note main.go:210-216 already flags that the sweep
was never run against a real runtime (only a fake script) — so the output-shape
assumption is also unverified, but that's secondary to the scoping bug.

---

## SHOULD-FIX

### S1. hook_runs FK-safety silently depends on channel registration order; violations are swallowed
`cmd/gauntlet/main.go:228-316`, `internal/queue/daemon.go:427-431`, `internal/history/store.go:439-465`

`hook_runs.run_id REFERENCES runs(run_id)` and FKs **are** enforced
(`foreign_keys(on)` in the DSN, store.go:56 — verified, not dead metadata). The
owed/skipped rows are therefore only insertable *after* the `runs` row exists.

That ordering holds today purely because of construction order in main.go:
`chans = [log, history, dashboard, ghstatus, slack, hr]` — history at index 1,
the hooks runner `hr` last. On the `EventLanded` fan-out, `d.emit` iterates
`d.chans` in order, so `history.Emit` commits the runs row (synchronous sqlite
write) *before* `hr.Emit` enqueues the landing; the hooks goroutine then dequeues
(channel receive → happens-after the send → after the write) and later emits
EventHookStarted/Skipped. Solid happens-before — **currently correct**.

But nothing at the emit site enforces or documents it. Reorder chans so `hr`
precedes `history` and `writeHookStarted`/`writeHookSkipped` FK-violate; the
error is discarded (`_ = ch.Emit` in daemon.go:429 and the hooks notify closure
main.go:296), so the durable owed/skipped marker is **silently dropped** —
defeating S1-C in exactly the crash-discoverability case it exists for, with no
log line. Recommend: document the "history must be registered before the hooks
runner" constraint where chans is built, and/or stop swallowing history Emit
errors (at least log them) so an FK violation is visible.

### S2. `ignored_refs` grows unbounded — no retention, unlike every other high-churn table
`internal/history/store.go:423-430`, `internal/history/queries.go:39-40`, `internal/queue/reconcile.go:85-115`

`ignored_refs` is append-only (`INSERT OR REPLACE` on PK `(at, target, ref)`)
and nothing ever prunes it. `queue_depth` — the other high-frequency series —
has `PruneDepth` (store.go:477); runs/checks are deliberately retained but are
bounded by real merge activity. `ignored_refs` has neither a bound nor a sweep.

The in-memory dedup in `checkIgnoredRefs` (reconcile.go:98, `d.ignoredRefs[ref]
== sha`) prevents per-tick spam — good, that kills the worst case — but a
chronically misconfigured ref that is re-pushed (one row per distinct SHA;
think CI force-pushing a branch under a misnamed target) plus one row per daemon
restart (the in-memory map resets, so every still-present ignored ref re-emits
once) accumulates monotonically forever. Recommend adding `ignored_refs` to the
retention sweep (a `DELETE FROM ignored_refs WHERE at < ?` alongside PruneDepth)
or capping it.

---

## NITS

### N1. Seed-park retry suppression tie-breaks the wrong way at millisecond granularity
`internal/history/queries.go:405-418`

`LatestTerminalPerRef`'s join is `WHERE t.rn = 1 AND (ri.at IS NULL OR ri.at <=
t.ended_at)` — the park is *kept* (ref re-parks) when `ri.at == t.ended_at`.
Both timestamps are `.UnixMilli()`-truncated. For the terminal being retried
*away from*, a retry landing in the same millisecond as the rejection (an
automated immediate-retry, or a fixed/coarse test clock) satisfies `<=` → park
kept → the operator's retry is silently discarded on restart, the exact S3 bug
in a narrow window. It can't be fixed by flipping to `<`: the retried run's own
*newer* terminal at `ended_at == ri.at` must stay `<=` to re-park with the new
reason. The tie is genuinely unresolvable by timestamp compare alone and would
need a monotonic sequence / run-identity disambiguator. Low severity (a manual
retry normally happens long after the rejection, so `ri.at >> ended_at`), and
the seedparks/retryintent tests only pass because the clock advances between
reject and retry — the equal-`at` boundary is untested.

### N2. `precomputeMergeBodies` SHA-keyed map collides on two refs at the same SHA
`internal/queue/summary.go:63-78`

Results are keyed by `cand.SHA`. A batch containing two distinct refs pointing
at the same commit writes `results[SHA]` twice; both chain links then read the
one surviving body. Harmless when `MergeBody`'s output depends only on
`base..SHA` (identical for both), but if it incorporates `cand.Ref`/`Topic`, one
ref's summary is applied to the other's merge commit. Cosmetic; worth a one-line
note in the doc that SHA-keying assumes body ≈ f(base..SHA).

### N3. Slack posts one standalone message per hook start *and* per hook finish
`internal/slack/slack.go:772-793`

`postHookStarted`/`postHookSkipped` each post a fresh top-level (non-threaded)
channel message. A landing with N hooks now yields ~2N standalone messages
(start + finish per hook). Noise, not a correctness bug — but potentially
chatty for multi-hook targets. (Not a blocking concern: `slack.Emit` is a
non-blocking outbox send that drops on overflow, so it never stalls the hooks
goroutine.)

---

## Explicitly verified correct (checked, no issue found)

- **Owed-row crash window (S1-C core):** EventHookStarted → `history.Emit`
  (synchronous sqlite write, `notifyChans` index 1) commits **before**
  `r.exec.RunCheck` (hooks.go:719-740). `slack.Emit` is a non-blocking outbox
  send (slack.go:326-333) and `dashboard.Emit` is a no-op, so history is the
  *only* synchronous write on the path — durability-before-subprocess holds
  without coupling hook latency to Slack/network. The notify ctx is
  `context.WithoutCancel`, so a shutdown mid-landing still records the owed row.
- **Recovery-skip FK:** `recoverLanded` emits EventLanded with a full Record
  (reconcile.go:1592-1604), so history writes the runs row before `hr` processes
  the landing → EventHookSkipped's hook_runs FK is satisfied.
- **Struct conversions** `hooks.LiveState → dashboard.LiveHook /
  gauntletmcp.LiveHook` are direct Go type conversions (dashboard.go:95,112) —
  compile-time-checked; a diverging field fails the build, it cannot silently
  drop. Self-pinning, no test needed (hunt #8 concern doesn't apply).
- **New EventKinds vs all consumers:** log (explicit cases + `unknown(%d)`
  default), slack (explicit cases + documented RetryRequested fall-through),
  ghstatus (`default:` no-post, `EventKind(999)` test proves the default is
  safe), dashboard.Channel (`return nil` no-op), history (switch), record
  (appends all). No panic or error path on the new kinds.
- **flock lifetime:** `*Lock` holds the `*os.File`, kept reachable by `defer
  lock.Close()` (main.go:133) → GC won't finalize it. Lock file is
  `<stateDir>/gauntlet.lock`, directly in stateDir, never inside a swept subdir.
  AcquireLock (main.go:129) precedes every sweep (168/180/222/313), closing the
  acquire-vs-sweep race. EWOULDBLOCK handled specifically; other errno →
  generic error.
- **Summary parallelization:** results map guarded by `mu` on write, read only
  after `wg.Wait()`; each `sem <- {}` is matched 1:1 by a goroutine that
  defer-releases (even on panic); no deadlock under ctx cancel since `mergeBody`
  honors cmd's timeout.
- **Retry-intent timing:** applyRetry emits EventRetryRequested synchronously
  after clearing the in-memory park (command.go); a crash in the tiny window
  only *loses* the retry (operator re-retries), no persistent divergence — the
  one exception being a swallowed history write error, which is best-effort per
  the "Emit never fails the loop" contract.
- **Shutdown wg:** `wg.Wait()` (main.go:396) joins slack/hooks/serve/sampler/
  pruner before the deferred `store.Close()`; no new deadlock (Channel.Emit
  non-blocking by contract; slack outbox non-blocking; history bounded by
  busy_timeout=5s).
- **Test honesty:** retryintent_test drives `SendCommand(CommandRetry)` →
  applyRetry → simulated restart (fresh Daemon on same store) → ReconcileOnce,
  asserting the ref is not re-parked — real entry points, not canned data.
