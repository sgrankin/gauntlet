# Shared service pool

Services are reusable test dependencies, leased by checks rather than recreated
for each command. The check spec describes a service and its dependents; the
operator enables the container driver and sets pool capacity.

## Ownership and reuse

- A check acquires instances for its `needs` before it starts and releases them
  after completion or cancellation cleanup.
- A service's identity includes its configuration and resolved inputs. Sharing
  requires an equivalent identity, not merely an equal display name.
- Idle instances can remain warm for reuse; active leases protect instances
  against reclamation.
- Pool capacity bounds instance count. It does not bound container memory or CPU.
- Health/startup failure blocks the dependent check and is reported distinctly
  from a failing candidate command.

## Isolation and endpoints

Container checks reach services through the configured runtime's network. Local
checks use host-published endpoints. Profiles used with services must match the
default executor's kind and runtime; otherwise an endpoint or network may be
unreachable. Apple `container` is supported for checks but not the service pool.

Reuse is appropriate for dependencies whose state can be safely reset between
checks. Keep test namespaces isolated and perform application-level cleanup;
a warm SQL server is not automatically an empty database.

The [service reference](../reference/services.md) defines repo nodes and exported
variables. [Execution configuration](../reference/execution.md) defines the
operator's allowlist and limits.
