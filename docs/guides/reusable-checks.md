# Reusing verification across queue changes

An unrelated predecessor failing should not require rerunning successful work.
A check can declare a command that identifies its complete semantic inputs:

```kdl
workspace "isolated"

check "billing" {
    command "./ci/test-billing"
    inputs "./ci/billing-inputs"
}
```

Without `inputs`, checks execute on each constructed commit as usual.

## Calculate the input identity

The inputs command runs in a fresh export of the proposed commit, using the
check's executor and candidate-built image. It must exit zero and write a
nonempty identity, at most 4096 bytes, to `GAUNTLET_INPUTS_RESULT_FILE`.
A missing, oversized, or failed result prevents verification; Gauntlet never
falls back to using an older identity. The identity command gets no shared-service
connections and is not subject to automatic failure-review retries.

For a repository whose billing tests depend only on the following tracked
inputs, a script could contain:

```sh
#!/bin/sh
set -eu
{
    git --git-dir="$GAUNTLET_GIT_DIR" rev-parse \
        "$GAUNTLET_MERGE_SHA:billing" \
        "$GAUNTLET_MERGE_SHA:shared" \
        "$GAUNTLET_MERGE_SHA:ci" \
        "$GAUNTLET_MERGE_SHA:go.mod" \
        "$GAUNTLET_MERGE_SHA:go.sum"
    go version
} > "$GAUNTLET_INPUTS_RESULT_FILE"
```

Use your build system's dependency graph when dependencies are dynamic.
Include test sources, scripts, shared libraries, lockfiles, toolchains,
configuration, and every other input that can affect the verdict. Account for
base/candidate history if the command uses it. Use immutable images and
version external dependencies. Non-overlapping changed paths alone do not prove
independence. Checks with unbounded external inputs should omit `inputs`.

Gauntlet additionally includes the check graph and concurrency settings, target, executor context,
service definitions, dependency verification identities, and consumed image
identity. A dependency without reusable evidence conservatively binds its
dependents to that constructed commit. Local ambient environment and operator
profile changes invalidate persisted evidence after restart. Repository tooling
still owns correctness of the declared identity, including changes to tools
or external inputs that are invisible to operator configuration.

## What survives reconstruction

Given `main → A(web) → B(billing) → C(search)`, rejecting A constructs
`main → B′ → C′`. Gauntlet recomputes each check's identity:

- Matching passing evidence satisfies the check without executing it again.
- A matching running execution continues on its original commit. The new run
  waits for its real result, retaining the original workspace, services, and
  source objects.
- Changed inputs start new work. Explicit change dependencies remain enforced.
- Failed, skipped, cancelled, expired, or unreadable evidence cannot satisfy a
  check. A reusable check must actually pass; `skipped` is not passing evidence.

The inputs command runs again for every attempted check. Its workspace is removed
before actual verification starts, so its writes cannot leak into the check.
`workspace "isolated"` is required: reused checks cannot provide filesystem
side effects to their dependents. Use immutable external artifacts for handoff,
and include their identities in the declared inputs.

## Recovery, retention, and provenance

Passing evidence is stored in `<state>/verification.db`, independently of
optional run history. FIFO order, parks, and infrastructure retry state are
checkpointed alongside incident controls in `queue-controls.json`. It survives scheduler restart, with automatic retention of 24 hours
and at most 10,000 entries. Local processes cannot survive a daemon/host restart;
unfinished local verification runs again. Only one daemon may own the state
directory; the existing process lock remains required.

Queue reconstruction detaches a run from its executions. Work with no remaining
waiters gets a two-minute adoption grace period, then is cancelled on reconciliation.
Shutdown cancels outstanding local executions and waits for resource cleanup.
The real execution retains its capacity slot until cleanup finishes; attaching to
it does not start another test process.

History, the JSON run API, and MCP record the verification identity, original run and commit, and whether
it was reused. The dashboard labels reused verification and serves the original
log through the new run's log link, subject to ordinary log retention. A reused
result is evidence about identical declared inputs, not a claim that its command
executed on the new commit. Final policy checks, provenance, and target CAS remain
mandatory; receipts and image producers always execute on their own run.

Failure-review retries remain check-local. Their persistent input budget also
survives queue reconstruction, while the ordinary run-wide budget still applies.
