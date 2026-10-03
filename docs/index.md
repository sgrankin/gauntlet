# Gauntlet documentation

Gauntlet builds the target history it intends to publish, verifies that exact
commit, and advances the branch with a compare-and-swap. It supports Git ref
submissions, GitHub PRs and stacks, Gerrit changes, and desired-state deployment.

## Start here

- [Quickstart](guides/quickstart.md): build the daemon and land a first change.
- [Landing changes](guides/landing.md): submit, retry, cancel, and request stacks.
- [GitHub](guides/github.md) and [Gerrit](guides/gerrit.md): connect a review host.
- [Writing checks](reference/checks.md): ordering, isolation, and executor selection.

## Configure

- [Daemon settings](reference/daemon.md) and [targets](reference/targets.md).
- [Executors](reference/execution.md), [services](reference/services.md), and [images](reference/images.md).
- [Policy](reference/policy.md), [signing](reference/signing.md), and [failure review](guides/failure-review.md).
- [Deployment](guides/deployment.md) and [receipts](reference/receipts.md).

## Operate

- [Incident controls](guides/incidents.md): pause, priority, and explicit emergency merges.
- [Hosting](operations/hosting.md), [security](operations/security.md), and [storage](operations/storage.md).
- [HTTP API](reference/http-api.md), [CLI](reference/cli.md), and [MCP](reference/mcp.md).
- [Host runbooks](runbooks/verify.md) and [releases](operations/releases.md).

## Understand and contribute

- [Architecture](architecture/overview.md), [queue modes](architecture/queue-modes.md), and [known limits](architecture/limits.md).
- [Maintaining the documentation](operations/documentation.md).
