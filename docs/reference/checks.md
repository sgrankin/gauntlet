# Writing checks

This is the reference for the **repo-side check spec** (`.gauntlet.kdl`,
committed to the repo the daemon watches — see [`.gauntlet.kdl`](https://github.com/sgrankin/gauntlet/blob/main/.gauntlet.kdl)
in this repo for a working example). The daemon reads the spec out of each
candidate's own trial tree, so a branch is always tested by its own check
spec. Daemon-side configuration lives in [daemon configuration](daemon.md).

A check is a **named command** — there is deliberately no pipeline DSL (see
the [architecture](../architecture/overview.md)): structure (matrices, setup, ordering) belongs
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
[result-file protocol](environment.md#check-environment-reference)). One kdl-go quirk to know: child blocks must be
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
([daemon configuration](daemon.md)) still bounds the whole
host; `max-parallel` only widens one candidate's slice of it. A check that
sat ready waiting for host capacity records that wait separately from its
own duration, so a slow host and a slow command are distinguishable in
history.

**Resource usage.** A check run by the local executor also records its peak
memory (resident-set size) and CPU time (user and kernel, separately) beside
its duration, read back from the child process's rusage after it exits —
best-effort observability, never an input to the check's own pass/fail
verdict. These show up as a "peak … / cpu … user / … sys" annotation on the
run page, as max/median columns on the per-check stats view
(`/checks`), and on the `run`/`checks` MCP tools and JSON API alongside
duration. A check run by a **container** executor (docker, podman, or
Apple's `container` CLI) never records any of this in v1: `--rm` removes the
container synchronously at exit, before there's a window to read its
terminal cgroup stats, and the client process's own rusage measures the
client, not the containerized workload — see `ContainerExecutor`'s doc
comment (`internal/executor/container.go`) for the full investigation and
the one cheap-but-not-free option (trading `--rm` for inspect-then-`rm`) left
for a future version. Zero always means "not measured", never "measured
zero" — nothing in history, the dashboard, or the API reports a bare zero
for these fields; they're simply absent.

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
([daemon configuration](daemon.md)), a check selects one by
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
