# Pull workers

Keep one recoverable scheduler and add execution capacity on other machines.
Workers pull matching checks; they do not choose queue order or publish Git refs.

## Scheduler

Keep the default executor local for operator commands. Add named remote profiles:

```kdl
workers {
    listen "127.0.0.1:8081"
    token-env "GAUNTLET_WORKER_TOKEN"
}
executor "sql" kind="remote" {
    labels "linux" "sql-v1"
    cpus "4"
    memory "8g"
}
```

Supply a shared token of at least 32 bytes through the environment. For workers
on other machines, configure `tls-cert` and `tls-key`, or terminate HTTPS at a
proxy forwarding to the loopback listener. Non-loopback listeners require TLS.
Keep the scheduler state directory on persistent storage; its existing lock
continues to enforce one scheduler per state directory.

## Worker

Create `gauntlet-worker.kdl` on each execution machine:

```kdl
scheduler "https://queue.example.com:8081"
labels "linux" "sql-v1"
cpus "8"
memory "24g"
max-executions 2
urgent-reserve 1

executor "sql" kind="container" {
    runtime "docker"
    image "ghcr.io/acme/sql-ci:stable"
    cpus "4"
    memory "8g"
}
services {
    allow "container"
    runtime "docker"
    max-instances 4
}
```

Run it with the same token environment variable:

```sh
gauntlet worker -config gauntlet-worker.kdl -state /var/lib/gauntlet-worker
```

The worker name defaults to its hostname; set `name` when hosts share a name.
Profile names and required labels must match the scheduler. Version labels such
as `sql-v1` are useful when changing runner images or toolchains: change the
scheduler's required label with the fleet to invalidate old execution context.
Workers need Git and their configured runtimes, with project tools installed in
the selected profile. They receive Git objects directly and need no GitHub push
credential. The worker token is withheld from local candidate environments.

Capacity values are scheduling reservations. Budget operating-system and warm
service overhead separately; container profile resource ceilings remain the
runtime's limits. Service pools stay on the worker, grouped by runtime and
reachability mode. `max-instances` bounds each pool. Idle instances are pruned
while the worker runs.

## Repository checks

```kdl
workspace "isolated"
max-parallel 4

check "sql-integration" {
    executor "sql"
    command "./ci/sql-tests"
    test-results "trx"
    retry-command "./ci/sql-tests-retry"
}
```

Remote graph nodes require isolated workspaces. Every execution exports the
exact constructed commit's **tree**, preserving the ordinary export contract.
The worker receives original source/base objects for Git inspection, restores
history timestamps when configured, and ensures declared services locally.
Container-image consumers require `supports-images true` on the remote profile
and a registry `@sha256:` reference for candidate-built images; local image IDs
cannot move between workers.

## Recovery and observation

- The scheduler persists jobs and leased attempts before dispatch. An expired
  lease permits reassignment; a late completion is accepted until that happens.
- Workers keep running through scheduler outages and save results before upload.
  The recovered scheduler adopts exact prepared SHAs and authorized retry chains.
- Reassignment rotates the attempt token. Cancelled or replaced attempts cannot
  supply a passing verdict. Workers stop them when their heartbeat is rejected.
- A reconstructed graph keeps its already-authorized child work while restoring
  prerequisite results. Unadopted work is cancelled after the recovery grace.
- Input-identity commands always execute fresh. Passing declared-input evidence
  follows the [reusable verification contract](reusable-checks.md).
- Unacknowledged worker results and unused source references expire after 24
  hours. Object cleanup runs while idle. Running jobs keep source leases.
- Worker shutdown stops new claims and drains active commands; `shutdown-timeout`
  bounds this before cancellation. Completed results are uploaded or retained.

The **Workers** dashboard page shows profiles, capacity, pending demand, and
worker health. `GET /api/v1/workers` and the MCP `workers` tool expose the same
state. Run details identify the worker and execution; each attempt retains its
raw uploaded log checksum and original test report. Diagnostic logs concatenate
attempts without altering those uploaded artifacts.

Remote scheduling does not change admission, Rego authorization, signing,
required checks, or the target compare-and-swap. The scheduler alone lands
changes. See the [worker reference](../reference/workers.md) for defaults.
