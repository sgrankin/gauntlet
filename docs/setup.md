# Integration setup guides

These channels/executors have no fake to exercise in CI-style tests; verify
them by hand against the real service once, per `docs/plans/phase23.md` §5.
Each section pairs the one-time external setup (tokens, apps, images) with
what a successful live run should look like. The config nodes referenced
here are documented in [config.md](config.md).

## GitHub commit status

1. Create a **fine-grained PAT** scoped to the one repository, with
   **Commit statuses: Read and write** and nothing else (GitHub adds
   **Metadata: Read-only** automatically — that's expected and sufficient;
   no other permission is needed unless the git remote itself also
   authenticates via this token, in which case add **Contents: Read and
   write** too — see [deploy.md](deploy.md#github-fine-grained-pat-minimal-permissions)
   for the full writeup).
2. Export it as `GITHUB_TOKEN` (or whatever `token-env` names) in the
   daemon's environment.
3. Add a `github "<owner>/<repo>"` node to `gauntlet.kdl`.
4. Push a candidate. You should see a `gauntlet/<target>` status appear
   `pending` on the candidate SHA once the trial merge is clean, flip to
   `success`/`failure`/`error` when the run finishes, and its description
   carry the rejection detail on failure. Visible on the commit and on any
   PR built from that branch.

## Slack app

1. Create a Slack app from a manifest with: **socket mode** enabled; bot
   scopes `chat:write`, `reactions:read`, `reactions:write`, and
   `channels:history`; app-level token scope `connections:write`;
   subscribed bot event `reaction_added`.
   - **`channels:history`** is required, not optional: every root message
     carries Slack message metadata identifying its (target, ref), and a
     reaction on a root *after* its run has already terminated (the common
     case — a human reacts to a finished ❌, not a still-running ⏳) can only
     be resolved by fetching that message back via `conversations.history`
     — the daemon's own in-memory run-tracking maps are deliberately
     forgotten the instant a run terminates (bounded memory), so they can't
     answer a reaction that arrives later. Without this scope, reacting on
     anything but a still-in-flight run silently does nothing. (For a
     **private** posting channel, that fetch needs `groups:history` instead —
     `channels:history` covers public channels only.)
   - `users:read` is optional — not required even for `allowed-users`
     (authorization matches the reaction event's raw member ID, no lookup
     needed), but would let a future version render the reacting human's
     display name instead of a bare user id in acknowledgment/guidance
     replies.
2. Install it to your workspace. You get two tokens: an app-level token
   (`xapp-…`, socket mode) and a bot token (`xoxb-…`). Export them as
   `SLACK_APP_TOKEN` / `SLACK_BOT_TOKEN` (or whatever `app-token-env` /
   `bot-token-env` name).
3. Invite the bot to the channel named in the `slack` node's channel ID.
4. Push a candidate. Expected thread flow: a root message posts once the
   trial merge is clean; each check posts a threaded reply as it finishes;
   the root is edited to a ✅/❌ (with a final thread reply) on landing or
   rejection; reacting `:recycle:` on the root re-queues that ref at its
   current SHA (a retry command), which you'll see as a fresh run starting
   — threaded under the SAME root, which is re-edited to show the retry in
   flight, rather than posting a new one — without pushing anything;
   reacting `:x:` on the root instead cancels it — the in-flight check
   aborts and the ref parks (a cancel command), visible as the root editing
   to ❌ with a "cancelled by operator" detail. Both reactions work whether
   the run is still in flight or has already finished (acknowledged with a
   👀 on the reacted message either way) — reacting on a long-since-finished
   ❌ is the normal case, not a corner case.
   - **Batch roots are the one exception.** A batch's root message
     represents every member of the batch at once, and a bare reaction
     can't say which single member it means — retrying or cancelling ALL of
     them from one reaction was considered and rejected as too blunt. A
     `:recycle:`/`:x:` reaction on a batch root instead gets a ❓ ack and a
     threaded reply pointing at the API/CLI (`POST /api/v1/retry`/`/cancel`
     or `gauntlet retry`/`gauntlet cancel`, naming that member's ref
     directly) to target one member. A batch of exactly one member is
     unaffected by this — it degrades to serial behavior byte for byte
     (see [config.md's "Queue modes"](config.md#queue-modes)), reactions
     included.

## Container executor

1. Only Apple's `container` CLI is expected to be present (no docker/podman
   assumed); start its background service: `container system start`.
   If the service isn't running, checks fail with an infra error
   (`CheckResult.Err`), not a red verdict — don't mistake one for the
   other.
2. Build or pick an image containing whatever the check spec's commands
   need (a Go toolchain, `make`, …) — the executor doesn't provision
   anything beyond running the image.
3. Configure `executor "container"` with `image` and one `cache` entry per
   directory you want to persist (e.g. `GOCACHE`, `GOMODCACHE`) — these are
   named volumes that survive across runs, which is the point (DESIGN.md:
   persistent warm builder beats hermetic-ephemeral on speed).
4. Push a candidate; you should see a container start and stop per check
   (`container list` while a run is in flight), and a second run reusing
   the same image show faster build steps once caches are warm.
5. Need a check to reach the host docker daemon (the concrete driver: a
   repo whose test suite uses testcontainers) — add a `mount`:

   ```kdl
   executor "container" {
       runtime "docker"
       image "ghcr.io/acme/ci:latest"
       mount "/var/run/docker.sock" path="/var/run/docker.sock"
   }
   ```

   This is docker-out-of-docker: the check container talks to the *host's*
   docker daemon over the mounted socket and spawns sibling containers
   against it, rather than nesting a second daemon inside the check
   container. A few things to know before reaching for it:

   - **Testcontainers already works today, with zero config, under the
     `local` executor** — checks there are just host subprocesses with
     direct access to the host socket already. This `mount` knob is only
     for repos that want *both* the container executor's isolation *and*
     testcontainers.
   - **Mounting the docker socket hands every check full control of the
     host docker daemon.** Any ref anyone can push to `for/…` gets a check
     run against that mount, and the docker socket API is root-equivalent
     on most setups (a container run with `-v /:/host` is a sandbox
     escape). Only do this if every pusher is as trusted as an operator
     with shell on the builder host.
   - **`readonly` does not restrict the socket API.** `readonly` affects
     filesystem metadata (the check can't unlink/replace the socket file)
     — it has no effect on what the check can *say* to the daemon over
     that socket. Don't rely on it as a safety boundary here.
   - **Apple's `container` CLI has no host daemon socket to mount** — each
     container is its own lightweight VM with no shared daemon. On macOS,
     use `runtime "docker"` (Docker Desktop or colima) for this, or fall
     back to the `local` executor.
   - **Sibling-container paths are host paths, not check-container
     paths.** A path you hand to testcontainers for a bind mount (e.g.
     `Testcontainers.WithBindMount(...)`) is resolved by the *host* docker
     daemon against the *host* filesystem — a path inside the check
     container's own bind-mounted trial tree means nothing to it.
     Testcontainers' file-copy APIs (`CopyToContainer`/`WithFiles`, per
     your client library) sidestep this because they stream bytes over the
     API instead of naming a host path.

6. **docker-on-macOS footguns** (both found by live testing, both silent):
   - **The daemon's `-state` dir must live under a path the docker VM
     shares from the host.** colima shares only `$HOME` and `/tmp/colima`
     by default (Docker Desktop has its own file-sharing list). Trial
     trees are exported under `-state`, and `docker run -v` against an
     unshared host path does not error — it bind-mounts an *empty*
     directory, so every check fails with a confusing
     module/file-not-found red instead of an infra error. Either keep
     `-state` under `$HOME` or share it explicitly (e.g.
     `colima start --mount /path/to/state:w`). Gauntlet now detects this on
     a failed check (a quick post-mortem listing of the mount) and reports
     it as an infra error instead of a rejected red.
   - **The `osxkeychain` credential helper blocks headless pulls.** If an
     image isn't present locally, `docker run` pulls implicitly, the
     credential helper may pop a Keychain prompt — even for anonymous
     pulls of public images — and the check wedges until a human clicks.
     Pre-pull images used by checks (`docker pull` once, interactively),
     or drop `credsStore` from `~/.docker/config.json` on a headless
     builder.

## OTLP export

1. Point `otlp` at any OTLP/HTTP collector endpoint (e.g. a local
   `otel-collector` on `localhost:4318`; `insecure true` if it's plain
   HTTP).
2. Push a candidate. Spans should appear in whatever backend the collector
   forwards to: one root span per run, with children for the trial merge,
   each check, and the land. This is export only — SQLite `history` (if
   configured) is a separate, always-local store; OTLP doesn't feed it and
   isn't fed by it.
