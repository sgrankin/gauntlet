# Adversarial review: executor bind mounts (`exec-mounts.diff`)

Scope: config `mount` node → `config.Mount` → `executor.Mount` → `-v host:path[:ro]`
in `runArgs`. Reviewed all non-`.playwright-mcp` hunks. `go build`, `go vet`,
`go test ./internal/config ./internal/executor ./cmd/gauntlet` all pass.
Edge-case claims below were verified with a scratch `validate()` test (since removed).

Overall: solid change. No BLOCKERs. One real (low-severity) BUG in the
reserved-path guard; the rest are NITs, most of which match pre-existing Cache
behavior (the stated consistency bar).

---

## BUG — reserved-path guard is exact-string `==`, bypassed by equivalent paths

`internal/config/daemon.go:718` and `:721`

```go
if m.Path == d.Executor.Workdir { ... "collides with executor workdir" }
if m.Path == "/gauntlet" { ... "collides with the reserved result-dir mount" }
```

The guard's own comment (daemon.go:714-717, and the `Mount` doc, and DESIGN.md's
ledger row) promises the trial tree and result dir "must never be silently
shadowed by an operator mount." The comparison is raw string equality, so any
non-canonical spelling of the same container path sails through. Verified NOT
rejected:

| mount `path=` | Workdir | result |
|---|---|---|
| `/workspace/`   | `/workspace` | accepted (should collide) |
| `//workspace`   | `/workspace` | accepted |
| `/workspace/.`  | `/workspace` | accepted |
| `/gauntlet/`    | (result dir) | accepted |

Also symmetric: an operator who writes `workdir "/workspace/"` gets
`Workdir == "/workspace/"`, and a mount at the canonical `/workspace` then
bypasses the guard while the trial tree is bind-mounted at `/workspace/`.

Failure scenario: docker/podman clean the destination path, so at runtime these
resolve to the *same* container mount point as the trial tree / result dir.
Depending on runtime, that's either a loud "Duplicate mount point" start failure
(merely a worse error than the config-time one this guard is supposed to give) or
— if a runtime lets the last `-v` win — a *silent* shadow: the check runs against
operator-mounted content instead of the actual trial tree, i.e. exactly the
correctness hole the guard claims to prevent. The docker-socket motivating case
doesn't trip this, so it's low-probability, but the guard is materially weaker
than its comment asserts.

Fix: canonicalize before comparing.

```go
mp := filepath.Clean(m.Path)
if mp == filepath.Clean(d.Executor.Workdir) { ... }
if mp == containerResultDir /* "/gauntlet", cleaned */ { ... }
```

(`filepath` is already imported by this change.)

---

## NIT — subpath mounts under the trial tree / result dir are unguarded

`internal/config/daemon.go:701-724`

`mount path="/workspace/src"` and `mount path="/gauntlet/sub"` are both accepted
(verified). Nested binds are legal in docker and won't produce a duplicate-mount
error, so this is a genuinely *silent* partial shadow of the trial tree — an
operator can overlay host content onto a subdirectory of the check's own source.
Arguably legitimate (operator's explicit choice) and out of scope for a
"don't collide with the exact reserved path" guard, but if the intent is
"protect the trial tree," a prefix check (`strings.HasPrefix` on cleaned,
slash-terminated paths) would be the complete form. At minimum worth a sentence
in the `Mount` doc / README that nested mounts under `workdir` are the operator's
responsibility.

## NIT — cache-path vs mount-path collision not validated

`internal/config/daemon.go:701-724`

A `cache` and a `mount` targeting the same container path (verified: cache
`/cache` + mount `/cache` both accepted) emit two `-v` with the same destination
→ runtime "Duplicate mount point", not a config error. Matches the existing bar:
Cache validation (`:693-700`) doesn't check cache-vs-cache dup paths either, and
this fails loudly at run time. Noting for completeness; cheap to fold into the
same guard loop if you want config-time rejection.

## NIT — duplicate mount `path`s not validated

`internal/config/daemon.go:701-724`

Two mounts at the same container path (verified accepted) → same duplicate-mount
runtime failure. Same reasoning/severity as above.

## NIT — `:` in Host/Path silently corrupts the `-v` argv

`internal/executor/container.go:342` builds `m.Host + ":" + m.Path`. A `:` in
either (verified: host `/a:b` accepted by validation) makes docker misparse the
`-v` value with no error from gauntlet. Matches Cache behavior exactly (Cache
also concatenates `Name+":"+Path` with no `:` guard), and absolute Linux paths
essentially never contain `:`, so consistent and low-risk. Only flag it if you
decide to raise the bar for both Cache and Mount together.

## NIT — mounts on a `local` executor are validated then silently dropped

`cmd/gauntlet/executor.go:28-56` maps `Mounts` only in the `container` branch;
the `local` branch ignores them. `validate()` still runs the full mount checks
for `kind "local"` (verified accepted). So an operator who puts `mount` under a
local executor gets no error and no effect. Identical to how Caches behave on
`local`, so consistent — but a foot-gun for both. Optional: reject `mount`/`cache`
on non-container executors.

## NIT — hardcoded `"/gauntlet"` string duplicates `executor.containerResultDir`

`internal/config/daemon.go:721` literals `/gauntlet`; the source of truth is
`internal/executor/container.go:24` `containerResultDir = "/gauntlet"`. The
config package deliberately doesn't import executor (documented on `Params`), so
the duplication is intentional, but a change to `containerResultDir` would
silently desync the guard. A `// keep in sync with executor.containerResultDir`
comment (there's a partial one at :716) or a shared const package would harden it.
Low priority.

## NIT — `TestLoadDaemon_Example` can't catch an accidental parse of the commented mount

`internal/config/config_test.go:100-121` asserts Kind/Runtime/Image/Workdir/
Caches but never asserts `Executor.Mounts` is empty. The `gauntlet.kdl` example
gained a commented `// mount "/var/run/docker.sock" ...`. `//` is a valid KDL
line comment in this lib (`sblinch/kdl-go`; the existing `// summarize {` block
already relies on it and the test passes), so it can't parse — but if it ever did
(e.g. de-commented by mistake), that mount at `/var/run/docker.sock` collides
with neither workdir nor `/gauntlet`, so validation passes and this test stays
green. Adding `if len(d.Executor.Mounts) != 0 { … }` would close the loop cheaply.

---

## Verified clean

- **KDL parsing of `readonly=true`.** `Mount.ReadOnly` has `kdl:"readonly"`;
  `TestLoadDaemon_ExecutorMounts` sets `readonly=true` and asserts `ReadOnly:true`
  via `reflect.DeepEqual` against the full struct. Default is `false`, so a
  misspelled tag would yield `false != true` and fail the test — the test is
  meaningful, not just re-asserting the default. Confirmed it runs and passes.
  `Host` uses `kdl:",arg"`, identical to `Cache.Name` (daemon.go:240) — the
  "shaped exactly like cache" claim holds.
- **Negative parse/validation tests.** missing-path, relative-host, relative-
  container, workdir-collision, result-dir-collision all present and assert the
  right substrings (`executor` / `workdir` / `result-dir`); all pass.
- **Workdir defaulting order (the flagged bypass).** `applyDefaults()` runs
  before `validate()` (LoadDaemon, :415-416) and defaults `Workdir` to
  `/workspace` for `kind "container"` (:487-489). So by the time the collision
  check runs, `Workdir` is populated — the "empty Workdir bypasses the check but
  the executor later defaults it" bypass does NOT occur. (The residual gap is the
  non-canonical spelling BUG above, not the defaulting order.)
- **Argv ordering guarantee.** `buildExecutor` copies `cfg.Executor.Mounts` into
  a fresh slice index-by-index (executor.go:37-40); `runArgs` iterates `p.Mounts`
  in order, appended after caches and before the image (container.go:341-348).
  Order is deterministic; `TestParams_RunArgs_MountsAfterCachesBeforeImage` and
  `…MultipleMountsPreserveOrder` cover it.
- **`:ro` suffix.** Only appended when `ReadOnly`; `…MountReadOnlySuffix` proves
  both the rw (no suffix) and ro (`:ro`) shapes.
- **No slice/pointer aliasing with config.** `executor.Mount` is a flat value
  struct (two strings + bool); the mapping copies by value into a freshly
  allocated slice. Config is never shared or mutated afterward.
- **cmd/gauntlet wiring.** Mounts mapped only in the `container` branch; `nil`
  `cfg.Executor.Mounts` → `make(..., 0)` → empty `p.Mounts` → no extra `-v`
  (`TestParams_RunArgs_NoMounts` asserts image immediately follows the env args).
- **Docs accuracy (README's five claims).** All five check out: (1) local
  executor runs checks as host subprocesses, so testcontainers works with no
  gauntlet-side plumbing; (2) docker socket = root-equivalent — correct and
  well-established; (3) `:ro` affects the socket *inode's* fs metadata, not the
  daemon wire protocol — correct, `readonly` genuinely doesn't narrow the API;
  (4) Apple `container` uses per-container lightweight VMs with no shared daemon
  socket — correct; (5) sibling-container bind paths resolve on the *host* daemon
  against the host fs — correct for docker-out-of-docker. DESIGN.md ledger row
  matches the `| STATUS | title | rationale |` format and its "config-shaped
  exactly like cache" claim is accurate.
- **Build/vet/test** green across all three packages (no `-race`, per instructions).
