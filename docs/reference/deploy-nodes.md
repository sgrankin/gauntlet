# Deploy nodes

The same spec file declares this revision's **deploy graph**: what runs when
an environment deploys *this* revision. Environments themselves are daemon
config (["Deployment"](deployment.md#deployment) in config.md, and
[design/deployment.md](../architecture/deployment.md) for the model); the commands are
repo config, here, as siblings of `check`:

```kdl
deploy "migrate" {
    command "./scripts/migrate"
}
deploy "app1" {
    command "./scripts/deploy" "app1"
    after "migrate"
    executor "deployer"        // optional named profile, exactly as for checks
}
deploy "app2" {
    command "./scripts/deploy" "app2"
    after "migrate"
}
```

These sit alongside the file's `check` nodes; a spec still needs at least one
`check` (a deploy-only spec is rejected with `no checks defined`), which is
no constraint in practice — the revision being deployed is one the queue
already tested.

A deploy node is a **name, a `command`, `after` edges, and an optional
`executor`** — those four, deliberately. No `image`, no `needs`: a deploy
runs against a landed revision on a real environment, not a candidate in a
sandbox. `after` has the same semantics and the same unconditional
validation as a check's (`deploy "x": after "y": no such deploy node
declared`, self-edges, duplicates, and cycles all rejected at spec load), and
the daemon's own run-graph scheduler runs the result — readiness by edges,
declaration-order starts under the environment's `max-parallel`, fail-fast
with a named culprit, `blocked` rows for nodes whose edges failed.

**Deploy names live in their own namespace**, disjoint from checks: a
`check "migrate"` and a `deploy "migrate"` may coexist, a check's `after`
can never resolve a deploy node, and a deploy's `after` can never resolve a
check. The `image:` and `receipt:` name prefixes are reserved and rejected
here too, since deploy nodes are scheduled into the same run-graph node
space.

**The graph is read from the revision being deployed**, out of its own tree
at the daemon's `check-spec` path — the deploy-side twin of "a candidate is
tested by its own check spec". This is load-bearing for older revisions:
rolling `prod` back to last week's SHA runs *last week's* graph — its
migration step, its apps, its edges — against last week's tree, not today's
graph against a tree it never knew. Each node runs against an export of that
tree, one shared export per graph run.

**Environment contract.** A deploy node gets the whole check contract
in the [environment reference](environment.md) — including `GAUNTLET_RESULT_FILE` and `GAUNTLET_GIT_DIR`, unchanged —
plus four deploy coordinates:

- `GAUNTLET_DEPLOY_ENV` — the environment being deployed (`prod`). One graph
  serves every environment; this is how `./scripts/migrate` knows *which*
  environment's databases to migrate.
- `GAUNTLET_DEPLOY_NODE` — this node's name (`app1`).
- `GAUNTLET_DEPLOY_SHA` — the revision being deployed (the environment's
  desired ref). The same commit is also exported as `GAUNTLET_MERGE_SHA` and
  `GAUNTLET_CANDIDATE_SHA`, so a script shared between checks and deploys can
  read either vocabulary.
- `GAUNTLET_DEPLOYED_SHA` — the environment's observed ref *before* this
  deploy, i.e. the last revision it finished deploying green. Also exported
  as `GAUNTLET_BASE_SHA`. **Empty on an environment's first-ever deploy, and
  only then** — set-but-empty, never omitted, so `[ -z
  "$GAUNTLET_DEPLOYED_SHA" ]` is the whole first-deploy test with no
  unset-vs-empty subtlety.

All four are exported together or not at all: a deploy node gets the set, an
ordinary check gets none of it. `GAUNTLET_REF` is the environment's desired
ref (`refs/heads/deploy/<env>`) and `GAUNTLET_RUN_ID` is the deploy run's own
ID (`deploy-<utc>-<seq>-<env>-<sha12>`).

**Affected-only deploys are the same one-liner**, and they are what keeps a
ten-app environment cheap — a revision that touched only `app1` skips the
other nine nodes in seconds:

```sh
if [ -n "$GAUNTLET_DEPLOYED_SHA" ] &&
   git --git-dir="$GAUNTLET_GIT_DIR" diff --quiet \
       "$GAUNTLET_DEPLOYED_SHA" "$GAUNTLET_DEPLOY_SHA" -- app1/; then
    echo skipped > "$GAUNTLET_RESULT_FILE"
    exit 0
fi
./scripts/deploy app1
```

**Skipped counts green**, exactly as in check graphs: it is the node's own
verdict that this revision needs nothing from it, it satisfies `after` edges,
and an all-green-or-skipped graph is what advances the environment's observed
ref. Both SHAs stay resolvable in `GAUNTLET_GIT_DIR` for the run's lifetime
(the daemon pins the deployed revision for the whole graph run, and observed
refs are fetched so a previous deploy's SHA resolves locally too). One thing
the diff protocol cannot help with: a **retry of the same revision** re-runs
the whole graph with desired and observed unchanged, so the diff is unchanged
too — resume state for an expensive step belongs to the tool that owns the
steps (a migration tool's applied-migrations ledger), never to gauntlet.

**Credentials.** Deploy commands are candidate-code class — landed and gated,
but still repo-authored — so the [operator-secret stripping](environment.md)
applies to them unchanged: a deploy node never sees the daemon's own
GitHub/Slack/OpenAI credentials. (Post-land hooks are the exemption, and
stay the only one — a hook's command comes from the operator's own config.)
Deploy credentials arrive the way check credentials do: fixed `env` on an
operator-owned executor profile, or workload identity on the builder host.
`executor "deployer"` selects such a profile by name, exactly as a check
does; a node naming a profile this daemon doesn't define makes the whole
graph a spec rejection — a park, never a red verdict — before any command
runs, as does a `nodes` selection naming a node this revision doesn't declare
or an environment finding no `deploy` nodes at all.

`gauntlet validate -checks .gauntlet.kdl` validates the deploy graph's own
structure along with the checks' (names, reserved prefixes, empty commands,
unknown/self/duplicate `after` edges, cycles). Its `-config` cross-check
mode does **not** yet cover deploy nodes, though: an `executor` profile a
deploy node names is verified against the daemon's profiles only when that
environment actually deploys the revision, where it surfaces as a
spec-rejection park.
