# Deployment access and promotion

Environments reconcile desired refs at `refs/heads/deploy/<env>` and publish
successful observed revisions at `refs/gauntlet/deployed/<env>`. Configure lanes
with the [deployment reference](../reference/deployment.md), and define steps
with [deploy nodes](../reference/deploy-nodes.md).

## Protect the refs

1. Protect `deploy/**` with branch rules restricting updates and deletions to
   authorized deployment identities. Give production a narrower allowlist.
2. Give the daemon write access to desired refs for tracked environments, plus
   observed refs. A manual environment's desired ref may be human-owned.
3. Reserve `refs/gauntlet/deployed/*` for the daemon. These are custom refs,
   not branches covered by ordinary branch rulesets. Restrict repository write
   authority accordingly; changing one manually falsifies observed state.
4. Test desired-ref pushes as both an allowed and denied user before production use.

Git hosting authenticates manual desired-ref pushes, even while the daemon is
down. Rego additionally gates tracked promotion, deployment admission, graph
execution, and successful observed publication. Protect the source branch or
source environment and use [deployment policy](../reference/policy.md) for
conditions that apply to the daemon's operation.

## Select a revision

```sh
gauntlet deploy -env dev -rev main
gauntlet promote -from dev -to prod
```

Promotion uses the source environment's completed revision. These CLI commands
are Git clients; they CAS-push desired refs. The dashboard, HTTP API, and MCP
retry/cancel existing lane work but do not change desired refs.

Check lane state and observed revision after the graph completes. Re-pushing an
already observed revision does not rerun it; use deployment retry for that.
See [CLI flags](../reference/cli.md#gauntlet-deploy-gauntlet-promote) and
[deployment recovery](../architecture/deployment.md).
