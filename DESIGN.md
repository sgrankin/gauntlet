# Gauntlet design

The canonical design notes are in [docs/architecture](docs/architecture/overview.md).

- [Queue reconciliation and recovery](docs/architecture/queue.md).
- [Serial, batch, and speculative modes](docs/architecture/queue-modes.md).
- [Review identity and forge completion](docs/architecture/reviews.md).
- [Service ownership](docs/architecture/services.md) and [deployment](docs/architecture/deployment.md).
- [Scaling](docs/architecture/scaling.md) and [known limits](docs/architecture/limits.md).

Read the [invariants](docs/architecture/overview.md#invariants) before changing
landing or emergency paths. Configuration belongs in the
[reference](docs/reference/daemon.md), and user workflows in the
[guides](docs/index.md).
