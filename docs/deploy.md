# Deploying gauntlet

This is the production guide: two deployment topologies, the git/auth
requirements both share, and the exposure/backup notes that apply regardless
of which one you pick. For what the daemon does and how to configure it, see
[README.md](../README.md) and [`gauntlet.kdl`](../gauntlet.kdl); for the
design rationale behind "refs are the queue" and "SQLite is disposable
history", see [DESIGN.md](../DESIGN.md).

Scope note: this doc covers packaging and operating the daemon itself.
Deployments run *as* post-land hooks (README's ["Hooks"](../README.md#hooks)
section) — ordered commands the daemon runs against the landed tree, via
the same executor that runs checks — but gauntlet itself never grows a CD
system past that (DESIGN.md's decision ledger, "Deployments as post-land
hooks"): a hook that needs more (health checks, rollback, progressive
delivery) hands off to a real CD system. Auth in front of the dashboard and
anything past a single running instance are explicitly out of scope for
gauntlet (docs/plans/phase23.md §8) — front the daemon with your own
reverse proxy/CD system for those.

## Topology (a): warm builder VM — recommended

This is the fly.io-builder pattern from DESIGN.md: a dedicated box (cloud or
local) with persistent caches on fast local storage (`GOCACHE`,
`GOMODCACHE`, NuGet, docker/podman image layers, ...) runs the plain
`gauntlet` binary directly, with the toolchains your checks need already
installed on the host. This is the fast path — no per-run container cold
start, caches genuinely warm across runs — and the one gauntlet is designed
around as primary. Use this unless you have a specific reason to
containerize (topology (b), below).

### Setup

1. **Install git ≥ 2.38** (see "Required git version" below) and whatever
   toolchains your check spec's commands need (Go, Node, a JDK, ... —
   gauntlet itself only needs `git` on `$PATH`; it provisions nothing).
2. **Build a static binary** and copy it to the box:

   ```sh
   make build                       # produces ./gauntlet, version from `git describe`
   scp gauntlet builder-host:/usr/local/bin/gauntlet
   ```

   `CGO_ENABLED=0` means this binary has no shared-library dependencies —
   copying it is the entire "install" step.
3. **Lay out state** (suggested; any absolute paths work, adjust the unit
   below to match):

   ```
   /etc/gauntlet/gauntlet.kdl        daemon config (see gauntlet.kdl for a full example)
   /etc/gauntlet/gauntlet.env        secrets: GITHUB_TOKEN, SLACK_APP_TOKEN, SLACK_BOT_TOKEN, ...
   /var/lib/gauntlet/state/          -state: bare repo clone(s) + trials/ scratch + logs/
   /var/lib/gauntlet/history.db      SQLite history, if the `history` node is configured
   ```

   Everything under `-state` is one of: a bare git clone (durable, but
   trivially re-clonable — it's just a cache of the remote); the `trials/`
   scratch dir (ephemeral by design: the daemon removes and recreates it on
   every startup, see README's "Running" section); or `logs/` — full,
   uncapped per-check log files, zstd-compressed at the fastest level
   (`logs/<runID>/<check>.log.zst`, DESIGN.md "Full per-check log files"),
   unconditionally written and served (decompressed on the fly) by the
   dashboard's "full log" link regardless of which optional sections are
   configured. To read one directly off disk, `zstd -d` it. Unlike
   `trials/`, `logs/` is meant to survive restarts: it's aged out by the
   `log-retention` config node instead (default 30 days, `"720h"`; see
   README's "Configuration reference"), swept once at startup and then
   hourly for the rest of the process's lifetime. None of this needs
   backing up; see "Backups" below.

   **Log files are keyed by absolute path.** History's per-check rows store
   each log file's full path as written at run time — moving or renaming
   the `-state` directory (or the volume it lives on) orphans every
   existing history row's log link: the dashboard's "full log" link 404s as
   if the file had been pruned, since from the server's perspective it now
   is missing. Retention pruning has the same dangling-link shape by
   design — once a run's log directory ages out, its history rows keep
   existing (history has no retention of its own) but their log links 404
   the same way. Neither case is treated as an error; both just mean "no
   log file to serve for this row anymore."
4. **Install the systemd unit** (adjust paths/`ExecStart` to match step 3):

   ```ini
   # /etc/systemd/system/gauntlet.service
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

   # Hardening basics. DynamicUser gives the daemon a fresh, unprivileged
   # UID with no login shell and no other host access; StateDirectory
   # creates and chowns /var/lib/gauntlet/state to that UID automatically
   # (and exposes it as $STATE_DIRECTORY, which the unit doesn't need to
   # use since -state is passed explicitly above). If your toolchains need
   # a stable, known UID instead (e.g. a Docker-group membership for the
   # container executor), replace DynamicUser with a dedicated `gauntlet`
   # system user/group and drop StateDirectory in favor of a pre-created,
   # pre-chowned directory.
   DynamicUser=yes
   StateDirectory=gauntlet/state
   NoNewPrivileges=yes
   ProtectSystem=strict
   ProtectHome=yes

   [Install]
   WantedBy=multi-user.target
   ```

   ```sh
   sudo systemctl daemon-reload
   sudo systemctl enable --now gauntlet
   ```

5. **Logs**: the daemon logs to stderr (the always-on log channel, see
   README's "Configuration reference"); under systemd that's journald:

   ```sh
   journalctl -u gauntlet -f              # follow
   journalctl -u gauntlet --since -1h      # recent history
   ```

### Upgrade procedure

```sh
make build
scp gauntlet builder-host:/usr/local/bin/gauntlet.new
ssh builder-host mv /usr/local/bin/gauntlet.new /usr/local/bin/gauntlet
ssh builder-host systemctl restart gauntlet
```

This is safe by design, not by luck: gauntlet's queue has no durable
in-flight state (DESIGN.md invariant 4 — "reconcile is idempotent"). The
refs on the remote *are* the queue; a restart rescans them from scratch,
reattaches or reruns whatever was in flight, and the `trials/` scratch dir
is swept unconditionally at startup. A candidate mid-trial-merge when you
restart just gets tried again a few seconds later — no state to lose, no
drain sequence to orchestrate.

## Topology (b): container deployment

Use the provided [`Dockerfile`](../Dockerfile) when you'd rather manage
gauntlet as a container than a host-installed binary — e.g. it's one
service among many on a box already run that way, or your checks are
entirely dispatched to a separate builder (GitHub Actions, or the container
executor talking to a host docker/podman/`container` socket) so gauntlet
itself never needs a local toolchain.

**The toolchain caveat, stated plainly:** the image's runtime stage carries
only `git`, `openssh-client`, `ca-certificates`, and `tzdata` — no Go, no
`make`, nothing else. If your check spec's `local` executor commands need a
toolchain, this image cannot run them; either point the `executor
"container"` config at a separate image that has what checks need (docs
"Container executor" section, README), or don't containerize the daemon
itself — use topology (a).

```sh
make image                          # docker/podman/container, whichever is on PATH
# or directly:
docker build --build-arg VERSION="$(git describe --always --dirty)" -t gauntlet:latest .

mkdir -p /srv/gauntlet/state
chown -R 1000:1000 /srv/gauntlet/state   # matches the image's fixed gauntlet UID/GID

docker run -d --name gauntlet \
  --restart unless-stopped \
  -v /srv/gauntlet/state:/data \
  -v /etc/gauntlet/gauntlet.kdl:/data/gauntlet.kdl:ro \
  --env-file /etc/gauntlet/gauntlet.env \
  -p 127.0.0.1:8080:8080 \
  gauntlet:latest
```

- The `/data` volume holds everything `-state` would hold directly on a
  host install (bare clones under `repos/`, ephemeral `trials/`, retention-
  pruned `logs/`), plus the config file and, if you configure `history`,
  `history.db` — same disposability rules as topology (a) apply
  per-subpath.
- Secrets (`GITHUB_TOKEN`, `SLACK_APP_TOKEN`, `SLACK_BOT_TOKEN`, ...) come in
  via `--env-file`/`-e`, same env-var names the `token-env`/`app-token-env`/
  `bot-token-env` config fields point at.
- Bind the dashboard port to `127.0.0.1` on the host (as above) unless a
  reverse proxy or the container network already restricts access — see
  "Exposure guidance" below.
- Upgrade is `docker pull`/`make image` + `docker rm -f gauntlet` + re-run
  the `docker run` above (or your orchestrator's equivalent rolling
  restart); the "no durable in-flight state" argument from topology (a)'s
  upgrade procedure applies identically.

## Required git version

Gauntlet needs **git ≥ 2.38** on `$PATH` for `git merge-tree --write-tree`,
the primitive its ephemeral trial-merge is built on (DESIGN.md's decision
ledger). The daemon checks this itself at startup and refuses to run with a
clear error naming the requirement, rather than failing confusingly on the
first trial merge — but check it yourself before deploying:

```sh
git --version
```

Alpine's packaged `git` (used by the Dockerfile) and any current Debian/RHEL
point release are well past 2.38; only very old LTS bases (e.g.
Ubuntu 20.04's default git) need a backport/PPA or a locally-built git.

## SSH key vs HTTPS-PAT remote auth

Gauntlet shells out to the plain `git` binary for every remote operation
(`ls-remote`, `fetch`, the CAS `push`) — it has no auth mechanism of its
own, so whichever the ambient git environment on the host (or in the
container) supports is what gauntlet gets:

- **SSH remote** (`git@github.com:acme/widgets.git`): a deploy key or an
  agent-forwarded key, with the host key already trusted (`~/.ssh/known_hosts`,
  or `ssh-keyscan` baked into provisioning). Simplest for a dedicated warm
  builder box — no token to rotate in gauntlet's own config, key management
  is ordinary host/SSH-agent hygiene.
- **HTTPS + PAT** (`https://x-access-token:<PAT>@github.com/acme/widgets.git`,
  or a `git credential` helper backing a plain `https://` URL): needed when
  SSH egress is blocked, or when you'd rather manage one token than a key.
  If you go this route, see the PAT permissions note below — an HTTPS+PAT
  remote needs **Contents: Read and write** in addition to whatever the
  commit-status channel needs, since the remote-auth PAT is what performs
  the actual fetch/push.

Either way the credential lives in the daemon's own environment (or,
container-side, the image/volume it can read) — DESIGN.md's "workload
identity lives on the builder host" applies to the remote-auth credential
the same as to executor credentials.

## GitHub fine-grained PAT: minimal permissions

If you enable the GitHub commit-status channel (`github "<owner>/<repo>"`
in `gauntlet.kdl`; README's "Configuration reference" and "GitHub commit
status" setup guide), create a **fine-grained** PAT scoped to that one
repository with exactly:

- **Commit statuses: Read and write** — this is the only permission the
  channel itself needs; it posts the `gauntlet/<target>` rollup status via
  the plain REST statuses API. (GitHub adds **Metadata: Read-only**
  automatically to every fine-grained PAT — you don't select it yourself,
  and it grants nothing beyond basic repo visibility.)

**Only if** the git remote itself is configured as HTTPS+PAT (see above) —
i.e. this same token, or a second one, is also what fetches/pushes the
repository — add:

- **Contents: Read and write** — needed for the fetch/push gauntlet's core
  loop performs; not needed for the commit-status channel on its own.

Nothing else. In particular, no Pull requests, Actions, Administration, or
organization-level permissions — gauntlet never opens PRs, dispatches
workflows, or touches repo settings.

## Slack token summary

If you enable the Slack channel, see README's ["Slack app" setup
guide](../README.md#slack-app) for the manifest (socket mode, `chat:write` +
`reactions:read` bot scopes, `connections:write` app-level scope,
`reaction_added` event subscription) and the two resulting tokens
(`SLACK_APP_TOKEN` / `SLACK_BOT_TOKEN`) referenced by `gauntlet.kdl`'s
`app-token-env` / `bot-token-env`.

## Dashboard / API / MCP exposure guidance

The dashboard, its JSON API (`/api/v1/*`), and the MCP server (`/mcp`) all
share one bind/port (`dashboard "<bind>"` in config) and have **no
authentication of their own** — this is a deliberate scope boundary
(DESIGN.md, README's "Trust model" notes), not an oversight.

- **Bind to localhost** (`dashboard "localhost:8080"`, the example in
  `gauntlet.kdl`) unless you have your own access control in front.
- **Front it with Tailscale (or an equivalent tailnet) or a reverse proxy**
  that adds auth if remote/team access is needed. Set `dashboard { url
  "https://..." }` to the address it's actually reachable at once something
  sits in front — outbound links (e.g. the GitHub commit status's
  `target_url`) use this, and default to the bind address otherwise, which
  is wrong the moment anything proxies it.
- **`POST /api/v1/retry`** (and the MCP `retry` tool) is the *only* mutating
  route either surface exposes, and it's non-destructive: it only re-queues
  an already-parked ref for another trial-merge-and-check pass. It never
  touches the target branch, force-lands anything, or bypasses a check —
  worst case of an unauthenticated retry is a wasted CI run.

## Backup notes

**`history.db` (the SQLite history store) is disposable telemetry, not the
record of anything.** It exists purely to feed the dashboard's run history
and red-rate views (DESIGN.md: "SQLite for history only... never the
source of truth for anything live"). Losing it loses queryable history, not
correctness — the daemon rebuilds its live queue state from the git remote
on every restart regardless of whether `history.db` exists.

**The repo (your git remote) is the record.** Landed commits, the branch
history they preserve, and the queue-slot refs themselves are the only
durable state gauntlet depends on — back that up the same way you already
back up any git remote (hosted-provider durability, or your own mirror/
replication if self-hosted). Nothing gauntlet-specific needs a separate
backup job beyond that.
