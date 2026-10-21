# Autoscaling workers

Gauntlet can manage dedicated pools through an external program or Azure VM
Scale Sets. One recoverable scheduler runs the capacity loop. Workers continue
to pull checks; cloud instances never receive publication credentials.

## Configure a pool

1. Build a worker image with `gauntlet worker` supervised at boot, its executor
   profiles and services installed, and the scheduler URL and token configured.
   Use the hostname as the worker name.
2. Create a dedicated pool with automatic cloud scaling and automatic instance
   replacement disabled. Keep the scheduler and its state disk outside this pool.
3. Add matching remote profiles and a pool to the daemon configuration:

```kdl
max-executions 32
executor "sql" kind="remote" {
    labels "linux" "sql-v1"
    cpus "4"
    memory "8g"
}
workers {
    listen "0.0.0.0:8081"
    tls-cert "workers.crt"
    tls-key "workers.key"
    pool "sql" {
        provider "azure-vmss"
        resource-id "/subscriptions/SUB/resourceGroups/ci/providers/Microsoft.Compute/virtualMachineScaleSets/sql"
        profiles "sql"
        service-profiles "sql"
        labels "linux" "sql-v1"
        cpus "16"
        memory "32g"
        slots 4
        urgent-reserve 1
        warm-nodes 0
        max-nodes 8
        price-per-hour 0.50
        max-hourly-cost 4.00
        idle-timeout "5m"
        startup-timeout "10m"
    }
}
```

The daemon's `max-executions` bounds all local and remote check invocations,
including remote jobs awaiting a worker. Size it for the fleet; repository
`max-parallel` and the target's window/batch settings also bound demand.

The pool describes **one node**, and must match its actual worker advertisement.
Set `image-profiles` for profiles that support candidate container images and
`service-profiles` for profiles with worker-local service pools. CPU and memory
are reservation budgets, not runtime limits; configure runtime limits in worker
profiles. In this example ordinary checks can occupy three slots per node;
urgent checks can use the fourth and its reserved resources.

4. Give the scheduler's noninteractive Azure CLI identity permission to read,
   update and delete instances in this scale set. Managed identity or an existing
   service-principal login works; the daemon never opens an interactive login.
5. Open **Workers** to inspect queued demand, nodes, desired capacity, limits,
   estimated hourly cost and provider errors. The same data is in
   `GET /api/v1/workers` and the `workers` MCP tool.

Azure support targets public Azure **Uniform** scale sets. Growth uses ARM's
`If-Match` ETag guard; deletion names a specific VM instance and checks its
immutable VM ID immediately beforehand. The scale set must have one owner:
external replacement or resizing can race instance deletion. This is not an
adapter for an existing shared production scale set.

## Capacity and recovery

- Pending checks can wake a pool from zero. Matching considers profile, labels,
  image/service capabilities, slots, CPU, memory and urgent reservations.
- Pools are tried in configuration order. Each pending check contributes to one
  pool's demand; running checks remain charged to their actual pool. Manual
  workers remain usable, but do not count toward managed capacity planning.
- `warm-nodes` is the idle floor. `max-nodes` and the optional hourly cost cap
  bound growth. Costs are estimates from the configured node price; they do not
  include disks, network, discounts or provider billing delays.
- Scale-down requires an idle timeout, a fresh zero-usage worker advertisement,
  no leased work, and an atomic drain before deleting that specific node.
- Nodes that never connect, or remain unreachable past `startup-timeout`, can be
  replaced. Expired leases are requeued and old tokens revoked before removal.
  Unexpired work and the scheduler reconnect grace prevent removal.
- Action intent and idle clocks persist in `worker-capacity.json`. After an
  uncertain response or scheduler restart, inventory reconciles the action.
  Unconverged actions retry after `startup-timeout` when the provider is settled.
- Provider errors appear in pool status. State persistence failures prevent new
  mutations. Candidate checks never run the provider command or receive its
  Azure access token.

## Another provider

Use a program instead of embedding cloud-specific scripts in checks:

```kdl
pool "linux" {
    provider "command"
    command "/opt/ci/capacity" "--pool" "linux"
    profiles "ci"
    labels "linux"
    cpus "8"
    memory "16g"
    slots 2
    max-nodes 12
}
```

Implement the [capacity provider protocol](../reference/capacity.md). The program
owns cloud authentication, immutable instance identification and conditional
mutations; Gauntlet owns demand, draining and bounds. Adapters are exercised
against fakes in tests; the Azure adapter has not been validated against a live
subscription as part of this implementation.
