# Deployment model

Deployment is level-triggered reconciliation over Git refs. Each environment
has a desired revision at `refs/heads/deploy/<env>` and an observed successful
revision at `refs/gauntlet/deployed/<env>`. A graph runs when desired differs
from observed, or an explicit retry clears a park.

## Sources and promotion

- A tracked environment follows a configured branch or another environment's
  observed ref. It advances its desired ref through policy and CAS.
- A manual environment runs what an operator pushes to its desired ref.
  `gauntlet deploy` and `gauntlet promote` are Git clients, independent of the
  daemon API. The host authenticates those pushes.
- Promotion from another environment uses its completed revision, not its latest
  request. Intermediate desired revisions may be skipped when a newer one arrives.

## Revision-owned graph

Deployment nodes come from the desired revision's check spec. This preserves the
commands, dependencies, receipts, and image definitions belonging to that version,
including on rollback. Nodes use the shared executor and dependency scheduler;
conditions and application-specific rollout logic remain in scripts.

A successful graph publishes the observed ref with CAS. Custom Rego decisions
run before tracked promotion, admission, execution, and observed publication.
A manual desired-ref push does not bypass the daemon's subsequent policy gates.
See [policy facts](../reference/policy.md#facts-schema-version-1).

## Concurrency, cancellation, and parks

Each environment has one active graph. Environments may overlap subject to
host execution capacity. Superseding work follows the configured backlog policy;
queued intermediates can be coalesced and active work can be cancelled when
configured. Cancellation requires scripts to handle partial completion safely.

A failed graph parks that revision. A new desired revision or explicit retry
allows another run. Retrying reruns the graph rather than resuming arbitrary
application steps. Scripts must make repeated migrations, artifact publication,
and rollouts idempotent. The system does not provide application transactionality
or automatically undo partially executed steps.

## Surfaces

Snapshots expose desired/observed revisions, lane state, and active nodes.
History records completed graphs, outcomes, and output. The dashboard, HTTP API,
and MCP offer retry/cancel; Git clients select desired revisions.

See [deployment configuration](../reference/deployment.md),
[deploy nodes](../reference/deploy-nodes.md), and
[branch protection setup](../guides/deployment.md).
