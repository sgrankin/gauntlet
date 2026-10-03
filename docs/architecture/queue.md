# Queue reconciliation

## Candidate ref grammar

Candidates use `refs/heads/for/<target>/<topic>` or
`refs/heads/for/<target>/<user>/<topic>`. Discovery orders eligible work and
respects review dependencies and explicit priority. A candidate revision includes
its source SHA and, for reviews, the metadata version that supplied its message
and admission facts.

## Trial construction

1. Fetch the target and candidate revisions.
2. Recover submissions already present in the target's first-parent provenance.
3. Evaluate admission policy and construct each chain link on the selected base.
4. Read the check spec from the exact trial tip and execute its graph.
5. Recheck revision currency, forge readiness, and publication policy.
6. Publish required receipts, then CAS-push the tested tip.
7. Remove submitted refs using their expected SHA and notify channels.

Squash links have one parent. `landing "merge"` creates two-parent links for
ref submissions. A speculative base is the predicted tip of its predecessor.
Workers never mutate the queue directly; results are consumed by reconciliation.

## Parks and retries

A real-base conflict or failed validation parks the current revision. A ref
update clears its old park. Cancellation parks the named member; unrelated
batch members are requeued. Infrastructure errors are distinct from rejections
and receive one automatic retry per ref/SHA by default.

History seeds failed ref parks at startup, validated against current refs.
Review parks have durable state. Retry records suppress stale park seeding.
Incident pause and emergency requests have their own durable control state.
See [storage](../operations/storage.md) and [incident controls](../guides/incidents.md).

## Recovery

Remote target history is authoritative. Provenance trailers identify the
submission, source revision, review version, and run. If a push succeeded before
a crash, the next pass recognizes the landed input and retries host acknowledgement
without generating another landing. Cleanup and publication use expected old SHAs.
A changed target or predecessor invalidates affected work and its dependent suffix.

## Read surfaces

The queue publishes immutable snapshots for the dashboard, API, and MCP. History
stores completed runs and captured check output. The idle signal represents
reconciled queue activity; it is not a promise that no future candidate will arrive.
Post-land hook cancellation is handled by the hook runner because it has no
candidate ref to address.

## Run identity and events

Run IDs are generated independently of the eventual landing SHA: provenance in
that commit includes the run ID, so deriving the ID from the commit would be
circular. Batch members have distinct run IDs and a shared batch ID.

Channels consume lifecycle events and emit commands; they do not choose queue
outcomes. Terminal records carry the verification result. Retry events are
persisted so a restart does not restore the superseded failed park. Snapshot
publication separates concurrent readers from reconcile-owned state.
