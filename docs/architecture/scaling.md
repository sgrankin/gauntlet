# Scaling

Scale execution capacity before adding coordinators. Each target needs one
ordered publication stream; checks are the expensive part.

## Available now

- Increase host capacity, then size `max-executions`, repo `max-parallel`, and
  speculation `window` together. Measure slot wait separately from command duration.
- Keep container images, module caches, and reusable services warm.
- Run one daemon per repository with separate state directories. Do not run
  competing daemons against the same queue remote.
- Deallocate an idle Azure builder while preserving its disks. Wake detection
  must run elsewhere because a stopped daemon cannot discover submissions.
  See [Azure operations](../runbooks/azure-operations.md).

## Recoverable scheduler and pull workers

[Pull workers](../guides/workers.md) execute checks without holding repository
publication credentials. The scheduler chooses matching jobs; workers advertise
profiles, labels, and reserved capacity. Durable leases and retry lineage allow
an exact prepared run to reconnect after scheduler restart. Workers retain
completed results through outages. Reassignment revokes the prior attempt token.

Git object packs bind source to the exact proposed commit. Graph nodes use
private tree exports and worker-local service pools. Source caches and retained
results are pruned live. Git CAS protects publication; no consensus or leader
election is needed. Operator state remains local to one recoverable scheduler.

[Reusable verification](../guides/reusable-checks.md) already separates passing
evidence and running local checks from queue attempts. The scheduler persists
FIFO order and completed evidence. Prepared commits and run IDs recover within
a 24-hour, 2,048-entry checkpoint, bound to source, base, configuration, and batch
composition. Restarted local commands run again; their retry budgets remain
bound to the recovered run. Terminal attempts retire their prepared identity so
an explicit retry receives a fresh trial. This is a
recoverable single scheduler, with no leader election. Completed remote attempts
and running leases can be adopted when that exact prepared identity matches.

Infrastructure errors are distinct from red validation and receive a bounded
automatic retry. The [infrastructure breaker](../reference/incidents.md) suspends
unhealthy execution across revisions without clearing manual incident pauses.
