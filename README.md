# gauntlet

Gauntlet is a merge queue that constructs target history, verifies the exact
commit it will publish, and advances the branch with a compare-and-swap. It
supports Git ref submissions, GitHub PRs and stacks, Gerrit changes, and
Git-ref-driven deployment.

- One commit per submission by default, with serial, batch, or speculative validation.
- Repository-owned KDL check graphs; operator-owned executors, credentials, and Rego policy.
- Container services, receipts, optional commit signing, and bounded Codex failure review.
- Dashboard, history, HTTP API, CLI, MCP, GitHub status reporting, and Slack notifications.
- Incident pause/resume, priority, and explicit emergency validation waivers.

## Documentation

Browse the [documentation site](https://sgrankin.github.io/gauntlet/), start with
the [quickstart](docs/guides/quickstart.md), or read the
[Markdown index](docs/index.md) on GitHub. References cover
[daemon configuration](docs/reference/daemon.md), [checks](docs/reference/checks.md),
and [policy](docs/reference/policy.md). The [architecture](docs/architecture/overview.md)
explains correctness and recovery; [known limits](docs/architecture/limits.md)
covers deployment constraints.

## Build and run

Use the Go version in `go.mod` and Git 2.40 or newer:

```sh
make build
./gauntlet validate -config gauntlet.kdl
./gauntlet -config gauntlet.kdl -state /var/lib/gauntlet
```

Edit the [example config](gauntlet.kdl) for your remote and identity first.
Keep the dashboard private or behind authenticated ingress.
See [hosting](docs/operations/hosting.md) for a persistent service.

## License

See [LICENSE](LICENSE).
