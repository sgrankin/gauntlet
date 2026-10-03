# CLI

**`gauntlet status`**, **`gauntlet retry`**, **`gauntlet cancel`**,
**`gauntlet hooks-cancel`**, and **`gauntlet drain`** are thin CLI wrappers
over the same API (client-side porcelain, like `gauntlet land`):

```sh
gauntlet status -url http://localhost:8080                  # compact per-target summary
gauntlet status -url http://localhost:8080 -target main     # one target only
gauntlet status -url http://localhost:8080 -json            # raw API response

gauntlet retry -url http://localhost:8080 -target main -ref refs/heads/for/main/alice/my-feature
gauntlet cancel -url http://localhost:8080 -target main -ref refs/heads/for/main/alice/my-feature
gauntlet hooks-cancel -url http://localhost:8080 -target main

gauntlet drain -url http://localhost:8080                   # begin a graceful drain, return
gauntlet drain -url http://localhost:8080 -wait             # block until lifecycle=drained
gauntlet drain -url http://localhost:8080 -deadline 30m     # force the kill 30m out if unfinished
```

`gauntlet drain` fails with a clear error if there is no reachable admin
endpoint (a daemon with no `dashboard` bind drains by signal only — a first
SIGTERM), rather than pretending a drain began.

### Deploy and promote

These two are **git porcelain, not API clients** — the only CLI verbs in
this document that never talk to the daemon. Deploying an environment *is*
pushing `refs/heads/deploy/<env>`, so they resolve a revision on the remote
and compare-and-swap that ref, nothing more. They work against a remote
whose daemon is down, they are subject to branch protection, and the push is
attributed to whoever ran them (see
[setup guide](../guides/deployment.md#protect-the-refs)).

```sh
gauntlet deploy -env prod -rev main            # deploy main's tip, as the remote has it
gauntlet deploy -env prod -rev v1.4.2          # a tag (annotated tags deploy the commit)
gauntlet deploy -env prod -rev 9f1c…           # a full SHA, taken verbatim
gauntlet deploy -env prod -from-env dev        # promote what dev finished deploying
gauntlet promote -from dev -to prod            # exactly the line above, spelled better
```

- **`-env` is required**, and so is **exactly one of `-rev` / `-from-env`**.
  `gauntlet deploy` requires an explicit revision or source environment: with no daemon round-trip there is nothing to read an environment's
  `source` *from*. Naming the revision (or the environment to promote) is
  the price of a client that needs no daemon.
- **`-rev`** is resolved on the **remote**, not in your local clone: a
  branch or tag name goes through `git ls-remote`, so `-rev main` deploys
  the main the remote has even from a stale checkout. A name matching more
  than one ref (a branch *and* a tag called `rel`) is an error naming both,
  never a guess; a full 40/64-hex SHA is taken verbatim with no round trip.
- **`-from-env dev`** reads `refs/gauntlet/deployed/dev` — what `dev` has
  actually *finished deploying green*, not what it was last asked to run. An
  environment that has never completed a deploy has no such ref, and the
  error says exactly that.
- The push is `--force-with-lease` against the value just read, so two
  people deploying different revisions at once cannot silently lose one:
  the loser is told the ref moved and re-runs. A revision your clone doesn't
  have yet is fetched first (which every promotion needs — a normal clone
  never fetches `refs/gauntlet/deployed/*`).
- `-remote` defaults to `origin`. Re-pushing the value the ref already holds
  is reported as a no-op rather than a deploy: the daemon is
  level-triggered, so re-running an already-deployed revision is
  `POST /api/v1/deploy/retry` (or MCP `deploy_retry`, or the dashboard
  button), not a ref push.

There is no `gauntlet deploy-retry` / `deploy-cancel` client: the two
mutating deploy routes are reachable from the dashboard, the MCP tools, or
[HTTP API](http-api.md).

## Other commands

| Command | Purpose |
|---|---|
| `gauntlet -config FILE -state DIR` | Run the daemon. |
| `land`, `land-pr` | Submit Git refs or post GitHub queue requests; see [landing](../guides/landing.md). |
| `control` | Pause/resume, priority, and revision-bound emergency requests; see [incidents](../guides/incidents.md). |
| `validate` | Validate daemon/check configs, profiles, and policy without starting the daemon. |
| `doctor` | Probe configured host capabilities and credentials; see [validation](../guides/validating.md). |
| `policy-check` | Evaluate a named policy against local facts. |
| `fmt` | Format KDL whitespace; see below. |
| `version` | Print build version. |

Use `gauntlet COMMAND -h` for current flags. API clients default to the local
admin endpoint and accept `-url`; Git clients use the configured/local remote.

## Formatting

`gauntlet fmt FILE` prints normalized KDL. `-w` rewrites, `-d` shows a diff,
and `-l` lists differing files and exits nonzero. Formatting preserves comments,
node order, quoting, and string content. It normalizes indentation, trailing
whitespace, blank lines, and the final newline; it does not rearrange syntax.
