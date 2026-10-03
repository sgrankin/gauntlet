# Shared services

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
[daemon configuration](daemon.md)) — the repo declares intent,
the daemon config gates capability. **No `services` node ⇒ any
`service`/`needs` in a check spec is rejected at run time**, loudly, so
an author can't believe a service was provided when it silently wasn't.

**What gauntlet guarantees, what your harness owns.** For each resolved
`needs`, the check gets `GAUNTLET_SVC_<NAME>_HOST`/`_PORT` (see ["Check
environment reference"](environment.md#check-environment-reference) above): an instance
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
heavyweight service spec can still pressure the builder unless it sets the
ceilings below.

**`memory`/`cpus` put a ceiling on that.** `memory "2g"` is passed
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

**Hooks can't declare services.** Post-land hooks have no
`needs` grammar at all — this is deliberate scope control, not an
oversight; a hook's environment never carries `GAUNTLET_SVC_*` vars.

**Apple's `container` runtime is unsupported for services.** The
docker/podman networking model services rely on (a shared user-defined
network, service containers as aliases on it) has no Apple `container`
CLI equivalent yet. A daemon configured for services with
runtime `"container"` fails at startup with:
`services require docker or podman; Apple's container CLI lacks the shared container network services need`.
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
