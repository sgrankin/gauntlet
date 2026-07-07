# gauntlet

Gauntlet is a merge queue. Push your branch to `for/<target>/<user>/<topic>`
and the daemon trial-merges it onto the live tip of `<target>`, runs the
named checks defined by your repo's own `.gauntlet.kdl`, and — if everything
comes back green — lands it as a `--no-ff` merge that preserves your commits
exactly as you wrote them. Red pings you; fix and re-push the same ref.

Requires git 2.38 or newer (`git merge-tree --write-tree`).

## Documentation

- [DESIGN.md](DESIGN.md) — the design: model, decision ledger, invariants.
- [docs/config.md](docs/config.md) — daemon configuration reference
  (history, dashboard, GitHub, Slack, OTLP, executors, services,
  summaries, hooks, queue modes).
- [docs/checks.md](docs/checks.md) — writing checks: the check spec, the
  `GAUNTLET_*` environment contract, logs, conditional execution, shared
  services.
- [docs/api.md](docs/api.md) — the JSON API, CLI, idle signal, and MCP
  server.
- [docs/setup.md](docs/setup.md) — one-time integration setup and live
  verification: GitHub PAT, Slack app, container executor, OTLP.
- [docs/deploy.md](docs/deploy.md) — production deployment guide, plus
  step-by-step [runbooks](docs/runbooks/).
- [docs/design/](docs/design/) — feature design docs: the queue core,
  queue modes (batch/speculate), shared services, and scaling.

## Running

Build the daemon:

```sh
go build -o gauntlet ./cmd/gauntlet
```

There are two config files:

- **Daemon config** (admin-written, one per daemon instance) — points at the
  remote, the poll interval, the committer identity used for merge commits,
  and the target branches to reconcile. See [`gauntlet.kdl`](gauntlet.kdl)
  for a full example and [docs/config.md](docs/config.md) for the
  reference. Passed via `-config`.
- **Repo check spec** (adopter-written, lives in the repo the daemon
  watches) — the named checks a candidate must pass before it lands. See
  [`.gauntlet.kdl`](.gauntlet.kdl) for a full example and
  [docs/checks.md](docs/checks.md) for the reference. The daemon reads this
  file out of each candidate's own trial tree, so a branch is always tested
  by its own check spec.

Run it:

```sh
gauntlet -config gauntlet.kdl -state ~/.cache/gauntlet
```

- `-config` (required) — path to the daemon config (`gauntlet.kdl`).
- `-state` — directory for the daemon's local bare-repo clone(s), keyed per
  remote, plus a `trials/` scratch directory (see below). Defaults to
  `gauntlet` under `os.UserCacheDir()`.

At startup the daemon probes `git --version` and refuses to run below git
2.38 (the `git merge-tree --write-tree` requirement above) — a clear error
naming the requirement, rather than a confusing failure the first time a
trial merge runs. It also removes and recreates `<state>/trials`, the
scratch directory each candidate's trial tree is exported into: it only ever
holds ephemeral exports for whatever run is currently in flight, never
anything that needs to survive a restart, so sweeping it on every startup is
always safe and cleans up anything an earlier crash left behind.

**The land flow:** push your branch to `refs/heads/for/<target>/<user>/<topic>`.
Each poll tick the daemon trial-merges the candidate onto the live tip of
`<target>` and runs the checks from your repo's own `.gauntlet.kdl` against
that trial tree. All green lands it as a `--no-ff` merge onto `<target>`,
preserving your commits exactly as written, and deletes the `for/...` ref.
Red (or a conflict) parks the ref alone — nothing re-runs until you push a
new SHA to it.

The daemon shuts down cleanly on `SIGINT`/`SIGTERM`.

`gauntlet -version` (or the `version` subcommand) prints the daemon's
version, the Go toolchain and GOOS/GOARCH it was built with, and — when
built with `go build` from a VCS checkout — the exact commit, straight from
`runtime/debug.BuildInfo`.

## Deploying

See [docs/deploy.md](docs/deploy.md) for the production guide: the
recommended warm-builder-VM topology (systemd unit included) and a
container-based alternative, plus git-version/remote-auth requirements,
GitHub PAT permissions, dashboard/API/MCP exposure guidance, and backup
notes. `make build` (version stamped from `git describe`), `make test`, and
`make image` (docker/podman/`container`) are the build entry points; see
the [`Makefile`](Makefile) and [`Dockerfile`](Dockerfile). Tagged releases
(binaries and a `ghcr.io/sgrankin/gauntlet` image, built via GitHub Actions
and goreleaser) are also available — see docs/deploy.md's ["Releases"](docs/deploy.md#releases)
section.

## Landing changes

Queue slot = ref name, SHA = what gets tested (see [DESIGN.md](DESIGN.md)
"The model"). Landing a change is just pushing to
`for/<target>/<user>/<topic>`; everything below is porcelain around that one
push.

**`gauntlet land`** does it for you:

```sh
gauntlet land -target main -topic my-feature
```

- `-target` (required) — the target name from the daemon's `gauntlet.kdl`.
- `-topic` — defaults to the current branch name.
- `-remote` — defaults to `origin`.

It derives `<user>` from `git config user.name` (falling back to `$USER`),
slugifies it, and runs `git push <remote> HEAD:refs/heads/for/<target>/<user>/<topic>`.

**Git alias**, if you'd rather not build the subcommand:

```sh
git config alias.land '!f() { git push origin "HEAD:refs/heads/for/${1:?target}/${USER}/${2:?topic}"; }; f'
```

```sh
git land main my-feature
```

**jj equivalent** — jj is first-class client-side even though the daemon
never touches it (DESIGN.md "Decision ledger": jj was killed as the daemon's
VCS backend, kept for clients). A candidate ref is just a bookmark pushed
into the `for/` namespace:

```sh
jj bookmark set for/main/$USER/my-feature -r @
jj git push -b for/main/$USER/my-feature
```

(`-r @` if you're landing the change you just described; `-r @-` if you've
already moved on to a new empty commit on top of it.)

**Author cancellation** is ref deletion — nothing more:

```sh
git push origin --delete for/main/$USER/my-feature
# or: jj bookmark delete for/main/$USER/my-feature && jj git push -b for/main/$USER/my-feature
```

(See "Operator cancellation" below for the other kind — an operator stopping
someone else's in-flight candidate without touching the ref at all.)

**Retry semantics.** Red (or a conflict) parks the ref at that SHA — the
daemon won't re-test it again on its own. To retry: push a new SHA (amend
and re-push the same ref name; the SHA change is what un-parks it), or, once
the Slack channel is configured (see [docs/config.md](docs/config.md)),
react `:recycle:` on the run's root message to re-queue the same SHA without
a new push — this works whether the run is still in flight or has long since
finished (the normal case for a ❌ root someone reacts to), and threads the
re-queued run's own progress under the same root rather than posting a new
one (see the [Slack app guide](docs/setup.md#slack-app)). Not available on
a batch root — see the batch-root exception there.

An `OutcomeError` park — a daemon-side infra failure (executor unreachable, a
service failing to come up, a service dying mid-run), never a red verdict or
a trial conflict — is additionally auto-retried once per `(ref, SHA)`
without any operator action, using this exact same retry machinery
(`auto-retry-errors`, default on — see [docs/config.md](docs/config.md)). If
that automatic retry also errors, the park sticks around for a human exactly
as before; a fresh push (new SHA) always gets its own fresh auto-retry
budget. Set `auto-retry-errors false` to disable this, so every
infra-error park waits for an operator.

**Operator cancellation.** An operator (not the author) can stop
a candidate that's currently being tested, or pull one out of the queue
before it's ever picked up, without deleting the ref: react `:x:` on the
run's root message in Slack, `POST /api/v1/cancel`, the MCP `cancel` tool, or
`gauntlet cancel`. This parks the ref at its current SHA exactly like a red
verdict (`Detail: "cancelled by operator"`) — the same retry semantics above
clear it. Per queueing discipline: serial and speculate park the cancelled
run itself (a speculation window's suffix behind it re-queues, unparked,
same as a real bubble); batch parks only the named member and re-queues its
batch-mates (unparked, "batch member cancelled") — but only when driven via
the API/CLI: a Slack reaction can't name a single batch member (see the
[Slack app guide](docs/setup.md#slack-app)), so use those for a batch. See
[docs/api.md](docs/api.md) for the wire shape; the full per-mode
cancellation semantics are recorded in [docs/design/queue-modes.md's
"Cancellation semantics per mode"](docs/design/queue-modes.md#cancellation-semantics-per-mode).

Post-land hooks have their own, separate cancel surface
(`POST /api/v1/hooks/cancel`, the MCP `hook_cancel` tool, or
`gauntlet hooks-cancel`), since a hook stage has no candidate ref to name —
see [docs/config.md's "Hooks"](docs/config.md#hooks).

## Configuring the daemon

Every optional daemon feature is a node in `gauntlet.kdl`, and absence
disables it — a minimal config (remote, committer, targets) runs a plain
single-lane daemon. The optional nodes: SQLite run
`history`, the web `dashboard`, `github` commit statuses, a duplex `slack`
channel with reaction commands, `otlp` span export, the container
`executor`, shared `services`, Claude merge `summarize`, per-target queue
`mode` (serial/batch/speculate), and post-land `hook`s with backlog
policies. See [docs/config.md](docs/config.md) for the full reference.

## Writing checks

A check is a named command in your repo's `.gauntlet.kdl`, run against an
export of the trial-merged tree with a small `GAUNTLET_*` environment
contract: the base/merge/candidate SHAs, the candidate ref, a run ID for
namespacing shared resources, and a result file for reporting `skipped`.
Checks that need a real backing service (a database, a broker) can declare
`service`/`needs` and get a warm, pooled instance. See
[docs/checks.md](docs/checks.md) for the full contract, including full-log
capture and the affected-only/monorepo pattern.

## Operating

The dashboard serves human-readable queue state, run history, and per-check
stats; the same bind also exposes a JSON API under `/api/v1` and an MCP
server at `/mcp`, and `gauntlet status`/`retry`/`cancel`/`hooks-cancel` are
thin CLI wrappers over the API — see [docs/api.md](docs/api.md). One-time
integration setup (GitHub PAT, Slack app manifest, container executor,
OTLP) is walked through in [docs/setup.md](docs/setup.md).

## Status

Feature-complete — serial/batch/speculate modes,
local+container executors, dashboard/API/MCP, Slack duplex with reaction
commands, GitHub statuses, post-land hooks, Claude merge summaries, full
log capture, and park persistence are all shipped; post-completion
consistency audit done.

See [DESIGN.md](DESIGN.md) for the full design and rationale.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
