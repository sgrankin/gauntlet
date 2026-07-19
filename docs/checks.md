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

## Ordering and parallelism

By default checks run **one at a time, in declaration order** — upgrading
gauntlet never races commands that relied on that order. A candidate opts
into overlap with `max-parallel`, and declares real ordering constraints
with `after`:

```kdl
max-parallel 4

check "unit" {
    command "./ci/unit"
}
check "lint" {
    command "./ci/lint"
}
check "package" {
    command "./ci/package"
    after "unit" "lint"
}
```

`unit` and `lint` run together; `package` becomes ready only once both end
green (`passed` or `skipped` — the same results that keep a candidate
green). Undeclared orderings are **independent by design**: once you raise
`max-parallel`, declare every edge that matters. Edges are validated even
while `max-parallel` is 1 (unknown names, self-dependencies, duplicates,
and cycles are spec errors), so raising it later can never reveal a
latently invalid graph. This is the entire dependency grammar — no
conditions, no matrices, no dataflow; a check that needs those implements
them in its own command.

When a check fails, the run fails fast: still-running checks are cancelled
and everything unfinished is recorded as **`blocked`** — a distinct status
naming the prerequisite (or root failure) that stopped it, never confused
with `skipped` (a check's own successful "nothing to do" verdict) and never
silently absent. Every declared check gets exactly one history row, in
declaration order, whatever order they actually ran.

The operator's daemon-wide `max-executions` cap
([config.md](config.md#daemon-config-gauntletkdl)) still bounds the whole
host; `max-parallel` only widens one candidate's slice of it. A check that
sat ready waiting for host capacity records that wait separately from its
own duration, so a slow host and a slow command are distinguishable in
history.

## Workspace isolation

By default every node in a run — each check and each `image:` build —
shares **one** writable export of the merge tree. That makes ordering
parallel without making filesystems independent: two nodes running at once
can collide through ordinary tool output (dependency dirs, `bin`/`obj`
trees, coverage files, a Docker build context read while another command
mutates it), and the failure mode is a *nondeterministic green* — one
check accidentally consuming another's half-written files — not a clean
error.

A top-level `workspace "isolated"` switches the run to **one private
workspace per node**:

```kdl
workspace "isolated"
max-parallel 4

check "test" {
    command "./ci/test"
}
check "lint" {
    command "./ci/lint"
}
```

Each node gets a fresh materialization of the run's exact chain-tip tree —
same contents, modes, symlinks, and (with `export { mtimes "history" }`)
the same history-derived mtimes — so no node ever observes another's
mutations, whether they overlap or are related by `after`. **In isolated
mode `after` is verdict ordering only, not shared dataflow**: a file a
prerequisite writes is deliberately absent from its dependent. Durable
handoff belongs in an immutable image identity, a content-addressed
cache/artifact, or a repository program — never an accidental shared
working tree.

Details worth knowing:

- **Absent = shared, unchanged.** Omitting the policy preserves today's
  single writable export, including intentional sequential filesystem
  handoff, byte-for-byte — even at `max-parallel 1`.
- **Export, not clone.** The private workspace is a `git archive` of the
  chain-tip **tree** with no `.git`; git queries still go through
  `GAUNTLET_GIT_DIR` and the exact `GAUNTLET_*_SHA` values, as always. It is
  the same tree object shared mode exports — archiving the merge *commit*
  would differ under `.gitattributes` `export-subst` (which rewrites
  `$Format:…$` placeholders against the commit), so isolated nodes see the
  literal tree bytes, byte-for-byte identical to shared mode.
- **Stable container path.** Distinct host directories are still bound at
  the profile's fixed `workdir` (normally `/workspace`), so tools whose
  caches embed absolute source paths see the same in-container path across
  nodes and runs. A local executor gets its private host path directly;
  path-sensitive checks should select a container profile.
- **Candidate-image builds are isolated too**, and their consumers still
  receive only the captured immutable image identity — never files the
  build happened to write beside its Docker context.
- **Bounded and cleaned.** A node's workspace is materialized only once it
  wins a `max-executions` slot (so the cap bounds concurrent archives too)
  and removed after its command's process/container fully stops;
  crash-orphaned node directories are swept at daemon startup. An archive
  or mtime failure is an infrastructure error (park-as-error), never a
  silent fallback to a shared directory. Materialization cost is recorded
  separately from slot-wait and command time (history's `materialize_ms`,
  a trace attribute) so you can tell whether isolation is actually
  material before optimizing.
- **Not a security sandbox.** Isolation prevents accidental cross-node
  filesystem coupling; it does not defend mutually hostile commands.
  Executor profiles, cache mounts, service endpoints, and Docker-socket
  authority are unchanged.

## Executor profiles

When the daemon defines named execution profiles
([config.md](config.md#daemon-config-gauntletkdl)), a check selects one by
name — so containerized checks (stable paths, warm caches) and host-local
ones (host identity, private networks, installed tooling) can coexist in
one candidate:

```kdl
check "test" {
    command "./ci/test"
    executor "it"
}
check "publish-receipt" {
    command "./ci/publish-receipt"
    executor "host"
    after "test"
}
```

Omitting `executor` runs the check on the daemon's default executor — the
pre-profiles behavior, unchanged. The name is ALL the repo side can say:
what a profile mounts, which image it runs, its fixed environment, and its
resource ceilings are operator-owned daemon config. Selecting a profile
grants the check everything attached to it, and a spec naming an undefined
profile is rejected before any of its commands start (a configuration
error, like an undeclared `needs` service — never a red verdict). The
`GAUNTLET_*` environment contract, result-file protocol, log capture,
timeouts, and cancellation are identical on every profile.

## Candidate-built images

A container check normally runs in its profile's static image. When the
toolchain image evolves *with* the code, declare a named image and let the
candidate build it in the same merge transaction — no operator pre-publish
step, no mutable tag meaning different bytes to two checks:

```kdl
image "go-ci" {
    command "./ci/images/go-ci/build"
}

check "unit" {
    command "./ci/unit"
    image "go-ci"
}
check "lint" {
    command "./ci/lint"
    image "go-ci"
}
```

The build command is opaque repo code, run against the trial tree with the
normal `GAUNTLET_*` variables plus `GAUNTLET_IMAGE_RESULT_FILE` (in place
of `GAUNTLET_RESULT_FILE` — builds have no skipped verdict). It must exit
0 **and** write exactly one *immutable* reference there — a local image ID
or a digest-pinned registry reference; a mutable tag is rejected:

```sh
#!/bin/sh
set -eu
docker buildx build --load --iidfile "$GAUNTLET_IMAGE_RESULT_FILE" \
    -f ci/images/go-ci/Dockerfile ci/images/go-ci
```

The build is scheduled as a node named `image:go-ci` in the same
dependency graph as your checks (that name prefix is reserved): built at
most once per run, an implicit `after` prerequisite of every check naming
it, taking one `max-parallel`/`max-executions` slot like any command.
Every consumer then runs by the captured identity — recorded in history
for both the build and its consumers, so a run always says exactly which
bytes ran. A failed build (non-zero exit, or an empty/mutable/multi-line
result) is ONE root cause: the build's own red row, with every consumer
`blocked` on it — never N unrelated consumer failures. Validation is
syntactic: gauntlet checks the reference's *shape*, not that the runtime
can actually see it — a build that writes a well-formed ID for an image
it never loaded (e.g. `buildx` without `--load`) surfaces as the first
consumer's infrastructure error instead.

Boundaries: consumers need a container-kind executor profile (gated at
run start); a build runs on an ordinary operator profile (typically one
with the docker socket) and can never itself depend on a candidate-built
image — multi-image relationships belong in your Dockerfiles or build
program. Capturing an ID makes this run exact; it does not make a
floating `FROM` reproducible forever — pin base images by digest when
that matters. BuildKit owns cross-run layer caching; a pruned image just
rebuilds. Gauntlet never learns Dockerfiles, contexts, or cache keys.

One combination to avoid: `executor "<name>"` together with `needs`.
Shared-service endpoints are wired for the daemon's *default* executor
(its kind picks published-port vs shared-network mode; its runtime owns
the network), so a `needs` check on a profile of a different kind or
runtime can receive endpoints it cannot reach. Keep service-dependent
checks on the default executor unless your operator confirms the profile
matches it.

## Receipts

The `check "publish-receipt" { executor "host" }` pattern above is one
repository giving a host profile its own publishing identity — still
appropriate when you're willing to expose that credential to a candidate
command. A `receipt` node (issue #13) is the alternative for operators who
don't want to: it produces bytes, and the *daemon* — never the command —
publishes them as a git note on the tested merge SHA, using the daemon's
own authenticated transport.

```kdl
receipt "deployment" {
    command "./ci/write-candidate-receipt"
    executor "host"
    after "unit" "lint" "artifacts"
}
```

`command`, `executor`, `image`, and `after` mean exactly what they mean on
a check: the same executor-profile selection, the same candidate-built
`image` (with its implicit `after` on the image build), and `after` edges
against check names, validated the same way (unknown names, duplicates —
spec errors). **At most one** `receipt` node per spec — a second is a
parse error, not last-wins; the name is provenance (history, logs, graph
diagnostics), never a selector.

**Terminal by construction.** A receipt's name lives outside the check
namespace: `after` only ever resolves against declared *checks*, so no
check can name a receipt in its own `after` — it gets the ordinary
unknown-name error, exactly as if it had typo'd a check name. Nothing can
depend on a receipt in turn; it is scheduled as a `receipt:<name>` node
(prefix reserved, mirroring `image:`) after every check, and only ever
sits at the end of the graph.

**A different result-file protocol, not the check protocol.** The command
gets `GAUNTLET_RECEIPT_RESULT_FILE` *instead of* `GAUNTLET_RESULT_FILE` —
the same "distinct protocol, never conflated" contract as
`GAUNTLET_IMAGE_RESULT_FILE`. A receipt has no `skipped` verdict: non-zero
exit is red regardless of the file's contents, and exit 0 requires the
file to contain non-empty bytes within the operator's configured cap
(`receipt-notes { max-bytes ... }` in daemon config — see
[config.md](config.md)). An empty, unreadable, or oversized result on a
zero exit is the receipt node's own red row — one root cause, not a
publication failure discovered later.

**The payload is opaque.** Gauntlet never parses, schemas, or interprets
the bytes your command writes — no deployment-manifest format, no JSON
merge strategy, no artifact graph. Your command owns constructing and
validating its own payload; gauntlet's whole job is capturing it exactly
and publishing it exactly, unmodified, as the note's content.

**The producer never sees the daemon's own credentials.** A `receipt`
node is candidate code like any check, so it runs under the same
config-named operator-secret stripping described in ["Check environment
reference"](#check-environment-reference) below — publication itself is
the *daemon's* job, using its own in-process authenticated transport, not
something the producer command does or needs credentials for.

**Policy handshake.** A `receipt` node only ever runs under a daemon
configured with a `receipt-notes` policy (`github { receipt-notes { ... }
}` — [config.md](config.md)) for the target it lands against. Both
mismatch directions are rejected at spec load, loudly, before any command
starts:

- the daemon requires a receipt but the spec declares none:
  `this daemon requires a receipt (receipt-notes is configured) but the check spec declares none`
- the spec declares a receipt but the daemon has no policy for it:
  `check spec declares receipt "<name>" but this daemon has no receipt-notes policy`

Both directions reject rather than silently diverging on purpose: a
`receipt` declaration is a correctness claim ("this run has a durable
handoff before landing"), and running the producer while quietly
discarding the handoff would make one repository spec mean different
things on different daemons — worse than the rollout inconvenience of
failing closed. This also shapes rollout order: upgrade the daemon, turn
on `receipt-notes`, *then* land the candidate that adds the `receipt`
node — a candidate lacking it fails closed during that window, same as an
undeclared `needs` service.

Publication itself — when it runs, what it publishes onto, how a
deployment consumer reads it back — is a daemon-config and operations
concern; see [config.md](config.md)'s `receipt-notes` reference and
deploy.md's ["Receipt read
path"](deploy.md#receipt-read-path-consuming-pre-land-receipts).

## Check environment reference

Every executor (local or container) sets these environment variables before
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
- `GAUNTLET_GIT_DIR` — a git dir holding every object the SHAs above name
  (the daemon's own bare repo — the trial merge commit is created there
  whether or not it ever lands, so `GAUNTLET_MERGE_SHA` always resolves).
  Usable as `GIT_DIR` or `git --git-dir`; see ["Conditional
  execution"](#conditional-execution) below. **Read-only by contract**: the
  container executor mounts it `:ro` at the fixed path `/gauntlet-git`; the
  local executor hands you the daemon's live repo path and trusts your
  script not to write to it. Honesty note, same spirit as services' "trust,
  stated honestly": the git dir's `config` file carries the daemon's remote
  URL verbatim — if that URL embeds a credential (rather than using a
  credential helper or SSH agent, both of which keep secrets out of the
  URL), every check can read it. Local checks could already read it off
  disk; this extends that visibility to container checks too.

**Operator secrets are stripped from candidate-code environments.** The
daemon's own credential env vars — `github`'s `token-env` in static-token
mode, `slack`'s `app-token-env`/`bot-token-env`, `summarize`'s
`api-key-env` (config-named, never a hardcoded list — see
[config.md](config.md)) — are removed by exact name from a candidate
command's environment on the local executor, before this contract's own
`GAUNTLET_*` variables are added. This covers every candidate-code job: an
ordinary check, an `image` build, and a `receipt` producer (see
["Receipts"](#receipts) above) alike — none of them ever needs the
daemon's own GitHub/Slack/Anthropic credentials to do its job, and a
repository's own commands are effectively attacker-controlled the moment
anyone can push a `for/` ref. **Post-land hooks are exempt**: a hook's
command comes from the daemon's own operator-written config
([config.md's "Hooks"](config.md#hooks)), never a candidate's repo spec,
and legitimately uses these same credentials — a deploy hook driving `gh`,
say. **Container profiles never had host env in the first place** and so
need no such filter: the container executor only ever passes explicit
`NAME=VALUE` pairs into the container (its fixed profile `env`, this
contract's own `GAUNTLET_*` set, and resolved `needs` env), never the
daemon's own ambient environment. This is a by-exact-name filter over the
*command's own environment*, not a sandbox: it closes the ordinary,
by-design channel (a candidate command's own `os.Environ()`), but it does
not change what a same-UID process can see through other means (e.g.
reading another process's environment off `/proc` on a platform that
allows it) — the same own-code threat model the rest of the local
executor already runs under (see `DESIGN.md`'s decision ledger).

A check that declares `needs` (see ["Shared services"](#shared-services)
below) additionally gets one pair per resolved service:

- `GAUNTLET_SVC_<NAME>_HOST` / `GAUNTLET_SVC_<NAME>_PORT` — where to reach
  the service (`<NAME>` is the service's declared name, upcased,
  non-alphanumerics turned into `_`). Absent entirely for a check with no
  `needs`, and for hooks (which can't declare `needs` at all).

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
check decides for itself, using the SHAs it's handed. The check's working
tree is a plain export (`git archive`, no `.git`), so point git at
`GAUNTLET_GIT_DIR` instead:

```sh
if git --git-dir="$GAUNTLET_GIT_DIR" diff --name-only "$GAUNTLET_BASE_SHA" "$GAUNTLET_MERGE_SHA" | grep -q '^services/web/'; then
    go test ./services/web/...
else
    echo skipped > "$GAUNTLET_RESULT_FILE"
fi
```

For a batch run this diff is exactly what the run's verdict covers: the
base is the target tip the whole chain was built on and the merge SHA is
the chain tip, so `base..merge` is every member's changes together — the
right unit for skipping, since a batch's members land (or fail) on this one
shared suite.

The same object store also serves content-based test caching without
hand-maintained input manifests. For a cache *key*, prefer the content
identity itself — the tree OID of the inputs, straight from the merge being
tested:

```sh
key=$(git --git-dir="$GAUNTLET_GIT_DIR" rev-parse "$GAUNTLET_MERGE_SHA:services/web")
```

Two trials whose `services/web` trees are byte-identical get the same key,
including across a revert that restores earlier content — that's what makes
it an identity. The last-*changing*-commit query answers a different
question, provenance ("which commit last touched these inputs?"):

```sh
git --git-dir="$GAUNTLET_GIT_DIR" log -1 --format=%H "$GAUNTLET_MERGE_SHA" -- services/web/
```

It also works as a cache key, just a conservative one — a revert produces a
new commit and so a fresh key even though the content (and any correct
cached result) is unchanged. Use it when you want the commit for humans or
logs; use `rev-parse` when you want maximal cache hits.

Some build/test tools you can't rewire key their caches on file *metadata*
(path + mtime + size) rather than content, and a plain export gives every
file extraction wall time — a guaranteed miss on every run. For those, the
daemon operator can turn on deterministic history-derived mtimes for all
exported trees with the top-level `export { mtimes "history" }` block; see
[config.md](config.md) for the exact semantics. It's an operator knob, not
something the check spec can request.

Every SHA in the environment contract stays resolvable in
`GAUNTLET_GIT_DIR` for your check's entire lifetime — the daemon pins the
trial chain against `git gc` for the whole run, and a landed chain stays
anchored through the fetched target ref afterwards — so a long check never
has an object vanish mid-query.

Gauntlet hands you the SHAs and the object store; which paths matter to
which check is repo-owned code, same as everything else about what a check
does.

## Self-checking your spec

`gauntlet validate -checks .gauntlet.kdl` parses and validates a check spec
with no daemon, no network, and no side effects — the same
`config.ParseChecks` the daemon itself runs against every trial tree. A repo
can declare a check that runs it against its own spec, so an edit that
breaks the spec (a typo'd `after`, a dependency cycle, a duplicate
`workspace`) fails its own merge trial instead of silently landing a broken
spec for the next candidate to trip over:

```kdl
check "validate-spec" {
    command "gauntlet" "validate" "-checks" ".gauntlet.kdl"
}
```

Without `-config`, only the spec's own internal validity is checked.
Cross-file properties — whether an `executor` name it references actually
exists, whether an `image` runs on a container-kind profile, whether
`service`/`needs` are usable at all, whether a `receipt` node is required
(or forbidden) by the daemon's `receipt-notes` policy — depend on the
daemon's config and can't be checked from the spec alone; run `gauntlet
validate -config gauntlet.kdl -checks .gauntlet.kdl` (with the operator's
real config) to catch those too. See `gauntlet validate -h` for the full
set of modes.

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
