# Capacity pools and providers

See [autoscaling workers](../guides/autoscaling.md) for setup and safety contracts.
Pools are optional children of the daemon's `workers` block.

| Field | Default | Contract |
|---|---|---|
| `scale-interval` (on `workers`) | `30s` | Capacity loop interval, 1 second–1 hour. |
| `pool "NAME"` | None | Unique named dedicated pool. |
| `provider` | Required | `command` or `azure-vmss`. |
| `command` | Required for `command`; `az` for Azure | Argument vector, no shell. Azure uses it as the CLI prefix. |
| `resource-id` | Required for Azure | Full Uniform VMSS resource ID; one pool per scale set. |
| `profiles` | Required | Remote executor names installed on each worker. |
| `image-profiles`, `service-profiles` | None | Subsets of profiles supporting those capabilities. |
| `labels` | None | Labels every node advertises. |
| `cpus`, `memory`, `slots` | Required | Per-node reservation capacity; slots 1–4,096. |
| `urgent-reserve` | `0` | Slots and proportional resources held for urgent checks; smaller than slots. |
| `warm-nodes` | `0` | Idle floor. |
| `max-nodes` | Required | Growth ceiling, 1–10,000. |
| `idle-timeout` | `5m` | Idle period before deletion, 1 second–24 hours. |
| `startup-timeout` | `10m` | Startup/offline grace and uncertain action retry interval, 1 second–24 hours. |
| `price-per-hour` | `0` | Nonnegative estimated cost per node. |
| `max-hourly-cost` | Disabled | Positive cap requires a positive price; must cover warm floor. Same currency as price. |

An already oversized pool shrinks only through safe idle-node removal. Lowering
limits never kills active work. Keep node clocks and capacity action state with
the rest of the scheduler state directory.

## External program protocol

Gauntlet invokes the configured argument vector once per operation. The request
is one JSON object on stdin; stdout must contain exactly one JSON response.
There is a one-minute deadline, a 4 MiB response limit and an 8 KiB diagnostic
limit on stderr. Exit nonzero or return `error` to report failure.

Inventory request:

```json
{"version":1,"operation":"inventory"}
```

Response:

```json
{
  "version": 1,
  "inventory": {
    "generation": "opaque-provider-version",
    "capacity": 1,
    "busy": false,
    "nodes": [{"id":"node-7","incarnation":"immutable-vm-id","worker":"ci-7"}]
  }
}
```

`capacity` includes provisioning nodes. `busy` prohibits new operations while a
previous operation is converging. Node IDs and worker names must be unique;
`incarnation` distinguishes a replacement reusing a node ID.

Growth request:

```json
{"version":1,"operation":"grow","generation":"opaque-provider-version","desired":3}
```

Removal request:

```json
{"version":1,"operation":"remove","generation":"opaque-provider-version","node":{"id":"node-7","incarnation":"immutable-vm-id","worker":"ci-7"}}
```

Mutation success: `{"version":1}`. Error: `{"version":1,"error":"diagnostic"}`.

The provider must compare the inventory generation before mutation, **never
shrink during grow**, and delete only the exact named incarnation. Reapplying the same capacity must be safe, including retrying partially failed
provisioning. Operations
must be idempotent: a response can be lost, and retries use fresh inventory.
Deleting an already absent incarnation succeeds; a replacement must survive.
The adapter must list only nodes owned by its dedicated pool. Read-only
inventory must not create resources.
