# Architecture

The daemon owns the target branch. Each reconciliation derives work from remote
refs and review state, constructs the intended target history, and verifies its
exact tip before publication.

## Invariants

- Publish the exact tested commit with an expected old target SHA. A concurrent
  target update invalidates work; rebuild and verify against the new tip.
- Default to one single-parent commit per submission, PR, or Gerrit change.
- Read checks from the trial tree, so validation changes validate themselves.
- Keep the reconcile loop single-threaded. Workers return results; snapshots
  serve concurrent readers without exposing mutable queue state.
- Treat Rego as the decision layer for commands, submission, execution,
  deployment, and retries. Git mechanics, revision binding, provenance, and
  publication CAS remain mandatory safeguards.
- A model can recommend bounded retries and failure suspects. It cannot declare
  a failed test green or prove that a change caused a batch failure.
- Emergency validation waivers require explicit configuration and revision-bound
  operator intent. Retain provenance, fresh policy checks, and the publication CAS.

## Components

| Component | Responsibility |
|---|---|
| Queue | Reconcile candidates, build trials, advance lanes, publish, recover. |
| Forge adapters | Fetch review state and commands; report outcomes after landing. |
| Executors | Run the candidate's dependency graph with bounded resources. |
| Services | Lease reusable service instances to checks. |
| History and snapshots | Durable run results and concurrent read surfaces. |
| Policy | Evaluate operator rules against facts supplied by the owning boundary. |
| Deployment | Reconcile desired refs through revision-owned deployment graphs. |

## Design choices

Git refs are the durable source of landing truth. A successful push remains
recoverable even if recording history or acknowledging a review is interrupted.
History improves diagnostics and park recovery; it does not manufacture a
successful landing. Candidate source objects are retained separately for audits
and delayed hooks, then pruned automatically when unused.

KDL describes commands and dependencies; scripts contain conditions and loops.
Executor profiles, credentials, mounts, and policy stay operator-owned. Keep
concrete implementations and small interfaces at external boundaries.

Read the [queue](queue.md), [queue modes](queue-modes.md), [review completion](reviews.md),
[service pool](services.md), [deployment](deployment.md), and [scaling](scaling.md)
notes for the contracts behind these choices. [Known limits](limits.md) records
remaining constraints rather than a diary of completed work.
