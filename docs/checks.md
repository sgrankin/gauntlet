# Writing checks

This is the reference for the **repo-side check spec** (`.gauntlet.kdl`,
committed to the repo the daemon watches — see [`.gauntlet.kdl`](../.gauntlet.kdl)
in this repo for a working example). The daemon reads the spec out of each
candidate's own trial tree, so a branch is always tested by its own check
spec. Daemon-side configuration lives in [config.md](config.md).

A check is a **named command** — there is deliberately no pipeline DSL (see
DESIGN.md's decision ledger): structure (matrices, setup, ordering) belongs
in your repo's own scripts.

```kdl
check "vet" {
    command "go" "vet" "./..."
}
check "test" {
    command "go" "test" "./..."
}
```

A candidate passes when every check exits 0 (or reports `skipped` — see the
result-file protocol below). One kdl-go quirk to know: child blocks must be
**multi-line** — a single-line `check "vet" { command "go" "vet" }` fails to
parse (with an unhelpful "line 0" error), so always write the braces across
lines as above.

## Check environment reference

Every executor (local or container) sets six environment variables before
running a check's command, and provides a result file for reporting
`skipped`:

- `GAUNTLET_BASE_SHA` — the target tip the trial merge was built onto.
- `GAUNTLET_MERGE_SHA` — the tested merge commit (base + candidate).
- `GAUNTLET_CANDIDATE_SHA` — the candidate's own commit.
- `GAUNTLET_REF` — the candidate's queue-slot ref
  (`refs/heads/for/<target>/<user>/<topic>`).
- `GAUNTLET_RESULT_FILE` — path to a file the check may write to report a
  verdict other than pass/fail.
- `GAUNTLET_RUN_ID` — this run's ID, stable across every check (and, for a
  batch, shared by every member) in it. A check's own test harness can use
  this to namespace shared external services per run — e.g. creating
  `testdb_$GAUNTLET_RUN_ID` on a shared SQL Server — so concurrent runs
  (the speculate window, or a batch's members) can't collide on the same
  external resource.

A check that declares `needs` (see ["Shared services"](#shared-services)
below) additionally gets one pair per resolved service:

- `GAUNTLET_SVC_<NAME>_HOST` / `GAUNTLET_SVC_<NAME>_PORT` — where to reach
  the service (`<NAME>` is the service's declared name, upcased,
  non-alphanumerics turned into `_`). Absent entirely for a check with no
  `needs`, and for hooks (which can't declare `needs` at all in phase A).

**Result-file protocol.** A non-zero exit is always a failure, full stop —
the result file is ignored on failure. On exit 0: a result file containing
`skipped` reports `CheckSkipped` (distinct from `passed` in history, so a
skipped check doesn't quietly count as green); an absent or empty file is
`CheckPassed`.

**Full per-check logs.** Every check's combined stdout+stderr is captured
twice: a 64KiB tail-capped copy inline (`Output` — the fast view: run
history, the run page, the `run` MCP tool), and, whenever `<state>/logs` is
writable, the complete, uncapped output as a zstd-compressed file at
`<state>/logs/<runID>/<check>.log.zst` (fastest zstd level, favoring
throughput over ratio since this is a supplementary record, not a
space-optimized archive). The full file is what the dashboard's "full log"
link and the JSON API/MCP `logPath`/`logUrl` fields point at (see
[api.md](api.md)) — the dashboard decompresses it on the fly when serving;
it's pruned after `log-retention` (default 30 days, see
[config.md](config.md)) regardless of whether history or the dashboard are
configured.

Post-land hooks (see [config.md's "Hooks"](config.md#hooks)) get the
identical treatment: each hook's full log lands at
`<state>/logs/<runID>/hook-<n>-<sanitized name>.log.zst` — inside the
*same* run directory its checks' logs already live in, so it's covered by
the exact same retention sweep and served through the exact same
`GET /run/{id}/log/{name}` route, with no separate configuration.
To read one offline: `zstd -d <path>` (or `zstd -dc <path> | less`).

## Conditional execution

The environment contract above is the whole mechanism for
conditional/monorepo-style execution — gauntlet has no path-filter config
(see DESIGN.md "Decision ledger": path globs, never). An affected-only
check decides for itself, using the SHAs it's handed:

```sh
if git diff --name-only "$GAUNTLET_BASE_SHA" "$GAUNTLET_MERGE_SHA" | grep -q '^services/web/'; then
    go test ./services/web/...
else
    echo skipped > "$GAUNTLET_RESULT_FILE"
fi
```

Note the check's working tree is a plain export (`git archive`, no `.git`),
so resolving that diff needs a git object store the check can reach on its
own — e.g. a clone the check maintains in a cache volume, or a shallow fetch
of just those two SHAs. Gauntlet hands you the SHAs; how you turn them into
a diff is repo-owned, same as everything else about what a check does.

## Shared services

Some test suites need a real backing service — SQL Server, a message
broker — that's too slow to spin up per check or per run. `services` lets a
check spec declare one, cached and reused across runs (and across daemon
restarts) instead of started fresh every time.

**Declare it in the repo, not the daemon.** Service instances are declared
in your check spec (the same `.gauntlet.kdl` the checks themselves live in),
read from the trial-merged tree exactly like `check` — a branch that bumps
an image tag or adds an env var is tested against its own declaration,
without touching anything else's warm instance:

```kdl
service "mssql" {
    image "ghcr.io/acme/mssql-fts:2022-cu14"
    port 1433
    env "ACCEPT_EULA" "Y"
    env "MSSQL_SA_PASSWORD" "gauntlet-scratch-pw1"
    ready-command "/opt/mssql-tools/bin/sqlcmd" "-S" "localhost" "-U" "sa" "-P" "gauntlet-scratch-pw1" "-Q" "SELECT 1"
    ready-timeout "90s"
    idle-ttl "2h"
    memory "2g"
    cpus "1.5"
}

check "test" {
    command "go" "test" "./..."
    needs "mssql"
}
```

`service`/`ready-command`/`env` are **multi-line child blocks only** —
kdl-go doesn't accept a single-line `service "x" { image "y" }` form. `needs`
takes one or more service names on a single node (`needs "mssql" "redis"`);
every name must match a declared `service` in the same spec, or the spec
fails to parse (the same loud, `OutcomeRejected` treatment as any other
malformed check spec). A check with no `needs` is wholly unaffected —
nothing here changes for it, cost or behavior.

The daemon must separately opt in with a `services` node (see
[config.md](config.md#configuration-reference)) — the repo declares intent,
the daemon config gates capability. **No `services` node ⇒ any
`service`/`needs` in a check spec is rejected at run time**, loudly, so
an author can't believe a service was provided when it silently wasn't.

**What gauntlet guarantees, what your harness owns.** For each resolved
`needs`, the check gets `GAUNTLET_SVC_<NAME>_HOST`/`_PORT` (see ["Check
environment reference"](#check-environment-reference) above): an instance
matching your declaration, ready, reachable for the run's duration.
Everything *inside* the instance — per-test/per-run tenancy, cleanup,
concurrency safety — is the harness's job, using `GAUNTLET_RUN_ID` to
namespace what it creates (`CREATE DATABASE testdb_$GAUNTLET_RUN_ID`, …),
same as it would against any shared, reused test database.

**Trust, stated honestly.** The real change here isn't sandboxing — a
service instance runs in the same kind of container a check does — it's
**lifetime**. A check container dies with its run; a service instance
persists on the builder until `idle-ttl`, and can be kept warm indefinitely
by continued pushes, including from a branch that never lands. `env`
secrets in a service declaration (the `MSSQL_SA_PASSWORD` above) are
therefore **scratch secrets only** — throwaway credentials whose entire
dataset is generated test fixtures, reachable only from the builder, never
anything that protects something real. `max-instances` and `idle-ttl` are
the only bounds on this capability; `allow` is the switch operators who
don't want it on a given box simply never flip. Adoption at boot also
trusts on-box container names/labels not to have been forged by something
else running on the machine — same threat model as everything else here
(your own developers, not hostile tenants), named explicitly so it's a
decision, not an accident.

**`max-instances` bounds count, not resources.** It caps how many live
instances the pool will create — nothing enforces per-instance memory/CPU,
which is whatever the runtime defaults to (typically unlimited). A single
heavyweight service spec can still pressure the builder; that's a known,
documented gap in phase A, not a solved problem.

**`memory`/`cpus` (phase B) put a ceiling on that.** `memory "2g"` is passed
to the container runtime's `--memory` verbatim; `cpus "1.5"` likewise to
`--cpus`. Both are optional — omit either and no flag is emitted at all, the
runtime's own (typically unlimited) default applies, exactly as before.
Because these join the service's cache key like every other field, the
first upgrade to a gauntlet version that adds a new spec field recycles the
*entire* pool once: instances started under the old key just age out via
`idle-ttl` and get recreated fresh under the new one — slower that one time,
never wrong.

**Distroless/shell-less images need an explicit `ready-command`.** Omitting
it gets a default readiness probe — but that default execs *into* the
instance to check for a listening socket (there's no way for the daemon to
dial it directly on the container network), which needs *some* shell/binary
present. An image with no shell must declare its own `ready-command`, or
readiness will never be detected.

**Hooks can't declare services in phase A.** Post-land hooks have no
`needs` grammar at all — this is deliberate scope control for v1, not an
oversight; a hook's environment never carries `GAUNTLET_SVC_*` vars.

**Apple's `container` runtime is deferred for services.** Phase A's
docker/podman networking model (a shared user-defined network, service
containers as aliases on it) has no Apple `container` CLI equivalent yet.
A daemon configured for services under a container-networked mode with
runtime `"container"` fails at startup with:
`services require docker or podman in phase A; Apple container networking is deferred (docs/plans/services.md §9)`.
`executor "local"` plus `services { runtime "docker" }`
(services containerized, checks run as local subprocesses) works fine on
any box with docker/podman, Apple `container` included for the checks
themselves.

**Cross-repo sharing is deliberately impossible.** An instance's cache key
includes the daemon's configured `remote` — the same push-trust boundary
gauntlet already enforces everywhere else — so two repos on the same daemon
never share a service instance, even with byte-identical declarations. This
is a forfeited optimization, not a bug: an instance's single all-powerful
account (the `sa` above) has no per-repo partitioning, so sharing across
repos would let one repo's pushed branch read or drop another's fixtures.

**Sizing `idle-ttl`/`max-instances` needs visibility, not guesswork.** Every
live instance (name, image, endpoint, age, last-used, refcount, and a
cumulative reuse-hit counter — "is reuse actually happening") plus the
pool's own cap and pending-create count are all surfaced on the dashboard's
index page (a "Services" section, since the pool is per-daemon rather than
per-target), `GET /api/v1/services` (JSON; 503 when no services are
configured), and the MCP `services` tool — the same three surfaces every
other operator-visible fact on this daemon appears on.
