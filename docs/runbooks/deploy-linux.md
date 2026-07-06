# Runbook: deploy gauntlet on Linux (production)

**What you get:** the `gauntlet` binary running as a systemd unit on a warm
builder box, checks executed in containers (docker), commit statuses posted
to GitHub, dashboard/API bound to localhost.

**Prerequisites**

- A Linux box (Ubuntu/Debian-ish) you can `sudo` on, with `docker` installed
  and running (`docker version` succeeds).
- `git` ≥ 2.38 on `$PATH` (`git --version`). Ubuntu 20.04's packaged git is
  too old — use a PPA/backport if you're on it.
- The repo's git remote URL, and either an SSH deploy key or an HTTPS PAT
  that can fetch/push it (see step 5).
- (Optional) a GitHub fine-grained PAT for commit statuses (step 6), and/or
  a Slack app's two tokens (step 7).

This runbook targets `docs/deploy.md`'s "Topology (a): warm builder VM" —
read that doc for the *why*; this is the *how*, in order.

---

## Phase 1 — Install

1. Build a static binary (on any machine with Go, or on the target box
   itself) and copy it to the target:

   ```sh
   make build
   scp gauntlet <TARGET_HOST>:/usr/local/bin/gauntlet
   ```

   `CGO_ENABLED=0` means this binary has no shared-library deps — copying it
   is the entire install step. No package manager involved.

2. **VERIFY**

   ```sh
   ssh <TARGET_HOST> gauntlet -version
   # expect: "gauntlet <version>" then a go1.x line then a vcs commit hash
   ssh <TARGET_HOST> git --version
   # expect: git version 2.38 or newer
   ```

## Phase 2 — Lay out state and secrets

1. Create the state layout:

   ```sh
   ssh <TARGET_HOST> 'sudo mkdir -p /etc/gauntlet /var/lib/gauntlet/state'
   ```

2. Write `/etc/gauntlet/gauntlet.env` (secrets only — never commit this
   file):

   ```sh
   # /etc/gauntlet/gauntlet.env
   GITHUB_TOKEN=<GITHUB_PAT>            # only if using the github channel or HTTPS+PAT remote auth
   SLACK_APP_TOKEN=<SLACK_APP_TOKEN>     # only if using the slack channel — xapp-...
   SLACK_BOT_TOKEN=<SLACK_BOT_TOKEN>     # only if using the slack channel — xoxb-...
   ```

   Where to get each: `<GITHUB_PAT>` from step 6 below; the two Slack tokens
   from step 7 below. Omit any line for a channel you're not enabling.

3. **VERIFY**

   ```sh
   ssh <TARGET_HOST> 'test -d /var/lib/gauntlet/state && test -f /etc/gauntlet/gauntlet.env && echo OK'
   # expect: OK
   ```

## Phase 3 — Configure the daemon

1. Write `/etc/gauntlet/gauntlet.kdl` on the target host. Start from this
   skeleton — replace every `<PLACEHOLDER>`:

   ```kdl
   remote "<REMOTE_URL>"          // e.g. https://x-access-token:<GITHUB_PAT>@github.com/<OWNER>/<REPO>.git
                                   // or git@github.com:<OWNER>/<REPO>.git for SSH remote auth
   poll-interval "10s"
   check-spec ".gauntlet.kdl"     // path within each repo tree; this is the default, shown explicitly

   committer {
       name "Gauntlet"
       email "gauntlet@<YOUR_DOMAIN>"
   }

   target "main" branch="main"    // repeat one target block per branch gauntlet lands onto

   dashboard "localhost:8080" {
       url "https://<DASHBOARD_PUBLIC_URL>"   // only if something proxies/tailnet-fronts this bind; omit otherwise
   }

   history "/var/lib/gauntlet/state/history.db" {
       sample-every "10s"
   }

   // Only if posting GitHub commit statuses (step 6):
   github "<OWNER>/<REPO>" {
       token-env "GITHUB_TOKEN"
   }

   // Only if posting to Slack (step 7):
   slack "<SLACK_CHANNEL_ID>" {
       app-token-env "SLACK_APP_TOKEN"
       bot-token-env "SLACK_BOT_TOKEN"
   }

   // Container executor — checks run in docker, not as host subprocesses.
   // Omit this whole block to run checks as local subprocesses instead
   // (needs toolchains installed on this host directly).
   executor "container" {
       runtime "docker"
       image "<CHECK_BUILDER_IMAGE>"   // must contain whatever your check spec's commands need (Go, make, ...)
       cache "gocache"    path="/root/.cache/go-build"
       cache "gomodcache" path="/go/pkg/mod"

       // OPTIONAL, TRUST-SENSITIVE — only if a check needs the host docker
       // daemon (testcontainers-based suites). Uncomment only if every
       // pusher is as trusted as an operator with shell on this box: the
       // docker socket API is root-equivalent, so mounting it hands every
       // check full control of the host docker daemon. `readonly` on this
       // mount would only protect the socket *file*, not the daemon API it
       // reaches — do not rely on it as a safety boundary.
       // mount "/var/run/docker.sock" path="/var/run/docker.sock"
   }

   // Only if your check spec declares `service`/`needs` blocks (e.g. a
   // real SQL Server for integration tests). See phase 3b below for a
   // worked example.
   services {
       allow "container"
       max-instances 8
   }
   ```

2. Point `remote` at your repo, adjust `target` blocks to match the branches
   gauntlet lands onto, and delete any optional block (`github`, `slack`,
   `services`, `executor`) you aren't using — absence disables the feature
   cleanly.

3. **VERIFY** (dry parse — the daemon validates config at startup and exits
   nonzero on a bad file; a bad `-state` doesn't matter for this check)

   ```sh
   ssh <TARGET_HOST> 'timeout 2 gauntlet -config /etc/gauntlet/gauntlet.kdl -state /var/lib/gauntlet/state; echo "exit: $?"'
   # expect: no config-parse error printed before the timeout kills it (exit: 124 is fine — that's the timeout, not a config error)
   ```

### Phase 3b — Container executor + a real backing service (optional)

If a check needs a real SQL Server (or similar) instead of a fake, declare
it in the **repo's** check spec (`.gauntlet.kdl`, not the daemon config) —
the daemon config above only needs the `services { allow "container" }`
gate:

```kdl
service "mssql" {
    image "ghcr.io/<ORG>/mssql-fts:2022-cu14"
    port 1433
    env "ACCEPT_EULA" "Y"
    env "MSSQL_SA_PASSWORD" "<SCRATCH_PASSWORD>"   // scratch secret only — see warning below
    // The readiness probe MUST perform a real login, not just wait for the
    // "ready for client connections" log line — that line appears in SQL
    // Server's log roughly 250ms before a login actually succeeds, which is
    // enough to flake a check that races it. A plain TCP/port check is not
    // sufficient here.
    ready-command "/opt/mssql-tools/bin/sqlcmd" "-S" "localhost" "-U" "sa" "-P" "<SCRATCH_PASSWORD>" "-Q" "SELECT 1"
    ready-timeout "90s"
    idle-ttl "2h"
    memory "2g"
    cpus "1.5"
}

check "test" {
    command "go" "test" "./..."
    needs "mssql"
}
```

**Trust note:** `env` values in a `service` block (the SA password above)
are scratch secrets only — this instance persists on the builder across
runs (that's the point, it's reused), reachable by any pushed `for/` branch
including one that never lands. Never put a real credential here.

## Phase 4 — Repo check spec

The daemon reads `.gauntlet.kdl` out of each candidate's own trial tree —
nothing to install on the daemon host for this part. In the repo being
gauntlet'd, commit a minimal spec if one doesn't exist yet:

```kdl
check "test" {
    command "go" "test" "./..."
}
```

See [README.md](../../README.md#configuration-reference) for the full
check-spec grammar (`service`/`needs`, multi-command checks, etc.) — one
minimal check is enough to verify the pipeline end-to-end before adding more.

**VERIFY**

```sh
git -C <REPO_CHECKOUT> show HEAD:.gauntlet.kdl
# expect: your check-spec content, no error
```

## Phase 5 — Install and start the systemd unit

1. Write `/etc/systemd/system/gauntlet.service`:

   ```ini
   [Unit]
   Description=gauntlet merge-queue daemon
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=simple
   ExecStart=/usr/local/bin/gauntlet -config /etc/gauntlet/gauntlet.kdl -state /var/lib/gauntlet/state
   EnvironmentFile=/etc/gauntlet/gauntlet.env
   Restart=on-failure
   RestartSec=5s
   DynamicUser=yes
   StateDirectory=gauntlet/state
   NoNewPrivileges=yes
   ProtectSystem=strict
   ProtectHome=yes

   [Install]
   WantedBy=multi-user.target
   ```

   If checks need a stable UID (e.g. docker-group membership for the
   container executor), replace `DynamicUser=yes` with a dedicated
   `gauntlet` system user and drop `StateDirectory` for a pre-chowned dir.

2. Enable and start:

   ```sh
   sudo systemctl daemon-reload
   sudo systemctl enable --now gauntlet
   ```

3. **VERIFY**

   ```sh
   systemctl is-active gauntlet
   # expect: active
   journalctl -u gauntlet -n 30 --no-pager
   # expect: startup lines, no fatal error, no repeated crash-loop timestamps
   ```

## Phase 6 — GitHub commit status (optional)

1. Create a **fine-grained PAT** scoped to this one repo:
   - **Commit statuses: Read and write** — always needed for this channel.
   - **Contents: Read and write** — add this **only if** the same token
     also authenticates the git remote itself (HTTPS+PAT remote auth).
   - GitHub adds **Metadata: Read-only** automatically — that's expected,
     select nothing else.
   - Get it at github.com → Settings → Developer settings → Personal
     access tokens → Fine-grained tokens.
2. Put the token in `/etc/gauntlet/gauntlet.env` as `GITHUB_TOKEN` (phase 2)
   and confirm the `github "<owner>/<repo>"` block is uncommented in
   `gauntlet.kdl` (phase 3), then restart:

   ```sh
   sudo systemctl restart gauntlet
   ```

3. **VERIFY** — push a candidate branch and watch for the status:

   ```sh
   git push origin HEAD:refs/heads/for/main/<YOU>/<TOPIC>
   # then, once the trial merge is clean (a few poll intervals):
   curl -s "https://api.github.com/repos/<OWNER>/<REPO>/commits/<CANDIDATE_SHA>/status" \
     -H "Authorization: Bearer <GITHUB_PAT>" | jq '.statuses'
   # expect: a "gauntlet/main" context, state "pending" then "success"/"failure"
   ```

## Phase 7 — Slack channel (optional)

1. Create a Slack app from a manifest with: **socket mode** enabled; bot
   scopes `chat:write`, `reactions:read`, `reactions:write`,
   `channels:history` (add `groups:history` too if the posting channel is
   private); app-level token scope `connections:write`; subscribed bot
   event `reaction_added`. All of these are required — `channels:history`
   in particular is what lets a `:recycle:`/`:x:` reaction on an
   already-finished run (the common case) get resolved at all.
2. Install to your workspace — you get two tokens: `xapp-...` (app-level)
   and `xoxb-...` (bot). Put them in `/etc/gauntlet/gauntlet.env` as
   `SLACK_APP_TOKEN` / `SLACK_BOT_TOKEN` (phase 2).
3. Invite the bot to the channel named in `gauntlet.kdl`'s `slack` node,
   then restart:

   ```sh
   sudo systemctl restart gauntlet
   ```

4. **VERIFY** — push a candidate and confirm a thread appears in the
   channel, root message flips to ✅/❌ on completion.

## Phase 8 — First-run end-to-end check

1. Push a candidate ref (if you haven't already in phase 6/7):

   ```sh
   git push origin HEAD:refs/heads/for/main/<YOU>/<TOPIC>
   ```

2. **VERIFY** — status API shows it queued, then in flight, then gone
   (landed) or parked (red):

   ```sh
   curl -s http://localhost:8080/api/v1/status | jq '.targets[] | select(.name=="main")'
   # expect: your ref under "queue" or "current", then absent once landed —
   # or under "parked" with an "outcome" if a check failed
   ```

3. **VERIFY** — the merge landed on the target branch:

   ```sh
   git -C <REPO_CHECKOUT> log --first-parent -1 main
   # expect: a --no-ff merge commit carrying "Gauntlet-Ref:" / "Gauntlet-Run:" trailers
   ```

## Operations

**Restart / upgrade** — safe at any time, no drain needed (the queue's
state lives in git refs, not daemon memory):

```sh
make build
scp gauntlet <TARGET_HOST>:/usr/local/bin/gauntlet.new
ssh <TARGET_HOST> mv /usr/local/bin/gauntlet.new /usr/local/bin/gauntlet
ssh <TARGET_HOST> sudo systemctl restart gauntlet
```

**Logs:**

```sh
journalctl -u gauntlet -f              # follow stderr
journalctl -u gauntlet --since -1h      # recent history
zstd -d /var/lib/gauntlet/state/logs/<runID>/<check>.log.zst   # full uncapped per-check log
```

**CLI status/retry/cancel** (thin HTTP clients — always pass `-url`
explicitly; the CLI's own default is `http://localhost:8899`, which won't
match a daemon configured with `dashboard "localhost:8080"` as above):

```sh
gauntlet status -url http://localhost:8080
gauntlet status -url http://localhost:8080 -target main
gauntlet retry  -url http://localhost:8080 -target main -ref refs/heads/for/main/<user>/<topic>
gauntlet cancel -url http://localhost:8080 -target main -ref refs/heads/for/main/<user>/<topic>
```

**Where things live under `/var/lib/gauntlet/state`:**

| Path | What | Safe to delete? |
| --- | --- | --- |
| `repos/` | bare clone(s) of the remote | Yes — re-clones on next start (slower once) |
| `trials/` | scratch exports of in-flight trial merges | Yes, and the daemon does this itself on every start |
| `logs/<runID>/*.log.zst` | full per-check/hook logs | Yes — pruned automatically after `log-retention` (default 30d) |
| `history.db` | SQLite run/check history for the dashboard | Yes — disposable telemetry; losing it loses history, not correctness |

**Two daemons on one state dir refuse to start** — this is an intentional
flock, not a bug. If a restart hangs on "already running", check for a
stale process holding the lock before assuming corruption.

**Never run `git gc --prune=now`** against a repo under `repos/` while the
daemon is running against it — it can reap an unpushed batch/speculation
chain link out from under an in-flight run. A plain `git gc` (default grace
period) is always safe.

See [docs/deploy.md](../deploy.md) for the full production guide this
runbook distills, and [README.md](../../README.md) for the complete
configuration reference.
