# Deploying gauntlet

This is the production guide: two deployment topologies, the git/auth
requirements both share, and the exposure/backup notes that apply regardless
of which one you pick. For what the daemon does and how to configure it, see
[README.md](../README.md), [config.md](config.md), and
[`gauntlet.kdl`](../gauntlet.kdl); for the
design rationale behind "refs are the queue" and "SQLite is disposable
history", see [DESIGN.md](../DESIGN.md). See [docs/runbooks/](runbooks/) for
step-shaped runbooks distilled from this guide.

Scope note: this doc covers packaging and operating the daemon itself.
Deployments run *as* post-land hooks (config.md's ["Hooks"](config.md#hooks)
section) — ordered commands the daemon runs against the landed tree, via
the same executor that runs checks — but gauntlet itself never grows a CD
system past that (DESIGN.md's decision ledger, "Deployments as post-land
hooks"): a hook that needs more (health checks, rollback, progressive
delivery) hands off to a real CD system. Auth in front of the dashboard and
anything past a single running instance are explicitly out of scope for
gauntlet — front the daemon with your own
reverse proxy/CD system for those.

If a target's deploy hook runs slower than that target merges (a builder
box running `make deploy` against a five-minute deploy while candidates
land every thirty seconds is the common case), see config.md's ["Backlog
policies"](config.md#backlog-policies) (`hooks-policy`): the default
(`queue`) runs every landing's deploy, back to back, however stale each
one is by the time it starts; `coalesce`/`cancel` let the backlog collapse
to "deploy the latest successful one next" instead.

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
   [config.md](config.md)), swept once at startup and then
   hourly for the rest of the process's lifetime. None of this needs
   backing up; see "Backups" below.

   Post-land hooks (`internal/hooks`, if any `target` configures `hook`
   nodes) write their own full logs into this same `logs/` tree, one per
   hook, at `logs/<runID>/hook-<n>-<sanitized name>.log.zst` — inside the
   *same* per-run directory (`logs/<runID>/`) that landing's own check logs
   already live in, not a separate hooks directory. That placement is
   deliberate: the age-based sweep above prunes whole `logs/<runID>/`
   directories, so hook logs are covered by the exact same retention pass
   as check logs, with nothing extra to configure or back up.

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
   [config.md](config.md)); under systemd that's journald:

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
"container"` config at a separate image that has what checks need (the
["Container executor" guide](setup.md#container-executor)), or don't
containerize the daemon itself — use topology (a).

If checks need the host docker socket (testcontainers-based suites), that's
a separate `mount` entry on `executor "container"` — see the ["Container
executor" guide](setup.md#container-executor) for the config and, more importantly, the trust
implication: mounting the socket hands every check full control of whatever
docker daemon it points at.

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

## Releases

Both topologies above can be fed from tagged releases instead of a local
`make build`/`make image`. Cutting one is one command:

```sh
make release VERSION=v1.4.0
```

This validates `VERSION` (must match `v[0-9]...`), refuses if the working
copy has uncommitted changes or has diverged from `origin/main`, then tags
and pushes — pushing a `v*` tag is what triggers
`.github/workflows/release.yml`, which drives `goreleaser`
(`.goreleaser.yaml`) to publish, on the GitHub release page, raw
`gauntlet_linux_amd64` / `gauntlet_linux_arm64` / `gauntlet_darwin_arm64`
binaries (no archive/extraction step — just the executable) plus a
`checksums.txt`, and, to `ghcr.io/sgrankin/gauntlet`, a multi-arch
(`linux/amd64`+`linux/arm64`) image tagged both `<version>` and `latest`.
Every push to `main` and every pull request separately runs
`.github/workflows/ci.yml` (`go mod tidy` drift check, `go build`, `go vet`,
`go test -race`) — the release workflow only runs on a tag push, and does not
re-run the test suite itself. `make release-snapshot` (see the Makefile) is
the local dry-run of the goreleaser pipeline, skipping publish and docker.

**Asset naming, read carefully before scripting against it:** the binary
name_template is `{{ .ProjectName }}_{{ .Os }}_{{ .Arch }}` — it carries
**no version**. What distinguishes one release's assets from another's is
which GitHub release (tag) they're attached to, not the filename itself; the
version only ever appears in the release tag / download URL path segment
(`.../releases/download/v1.4.0/gauntlet_linux_amd64`), never in the asset
name. Getting this backwards (expecting a versioned filename) produces a
404, not a wrong-version download.

- **Topology (a)** (warm builder VM): `curl -fsSL -o /usr/local/bin/gauntlet`
  the release binary for your arch from the GitHub release page and `chmod
  +x` it, instead of `scp`-ing a locally built one; everything else in that
  topology's setup/upgrade steps is unchanged. See
  [azure-vm.md](runbooks/azure-vm.md) for the exact fetch commands.
- **Topology (b)** (container): `docker pull ghcr.io/sgrankin/gauntlet:<version>`
  (or `:latest`) instead of `make image`; the `docker run` invocation and
  volume/state layout above are identical either way.
- **Why the release image isn't built from the top-level [`Dockerfile`](../Dockerfile):**
  goreleaser's docker builder copies a prebuilt binary into a throwaway
  context rather than running a multi-stage build, so releases use a
  separate, runtime-stage-only `Dockerfile.release` that mirrors this
  Dockerfile's runtime contract (packages, fixed UID, `/data` volume,
  entrypoint) byte-for-byte; the original `Dockerfile` stays the one used for
  from-source builds (`make image`). A plain `ko` build was rejected instead,
  since ko's default base images ship no `git`, and the daemon shells out to
  `git` at runtime for every trial merge.
- `gauntlet -version` prints the same version either way — ldflags-stamped
  from the pushed tag by goreleaser, or from `git describe` by `make build`.

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
in `gauntlet.kdl`; [config.md](config.md) and the ["GitHub commit
status" guide](setup.md#github-commit-status)), create a **fine-grained** PAT scoped to that one
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

If you enable the Slack channel, see the ["Slack app" setup
guide](setup.md#slack-app) for the manifest (socket mode, bot scopes
`chat:write`, `reactions:read`, `reactions:write`, and `channels:history`,
`connections:write` app-level scope, `reaction_added` event subscription)
and the two resulting tokens (`SLACK_APP_TOKEN` / `SLACK_BOT_TOKEN`)
referenced by `gauntlet.kdl`'s `app-token-env` / `bot-token-env`. All four
bot scopes are required, not just `chat:write` — `reactions:write` is what
lets the daemon acknowledge a reaction command with its own 👀, and
`channels:history` is what lets it resolve a reaction on a root message
*after* that run has already terminated (the common case — a human reacts
to a finished ❌, not a still-running ⏳), since the daemon's own in-memory
run-tracking is deliberately forgotten the instant a run terminates. Without
`channels:history`, reacting on anything but a still-in-flight run silently
does nothing. For a **private** posting channel, that fetch needs
`groups:history` instead — `channels:history` covers public channels only.

## Dashboard / API / MCP exposure guidance

The dashboard, its JSON API (`/api/v1/*`), and the MCP server (`/mcp`) all
share one bind/port (`dashboard "<bind>"` in config) and have **no
authentication of their own** — this is a deliberate scope boundary
(DESIGN.md, [api.md](api.md)'s "Trust model" notes), not an oversight.

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

### Object accumulation (batch/speculate)

Trial merges and chain links are commits no branch references; abandoned
ones (red batches, invalidated windows, crashes) sit in the daemon's bare
repo as loose objects until `git gc` collects them. Harmless — but a busy
queue's state dir benefits from an occasional
`git -C <state>/repos/<key> gc`.

**Why this is safe while the daemon runs.** Every *in-flight* run pins its
chain's tip commit with a local ref (`refs/gauntlet/pin/<tip>`) from the
moment the trial merge is created until the run's terminal outcome — or,
for a landing, until the next fetch makes the chain reachable through the
remote-tracking target ref. One pin covers the whole chain (a commit
reaches its parents), so from git's perspective nothing an active run — or
a check reading `GAUNTLET_GIT_DIR`, or a queued post-land hook — still
needs ever looks unreachable. Any gc, **including `git gc --prune=now`**,
is therefore safe to run against a live daemon's repo: it collects only
genuinely abandoned trial objects, which is exactly what you want. (Pins
stranded by a crash are swept at the next daemon start; every interrupted
run re-runs from scratch anyway.)

Two residual cautions. A `--prune=now` run discards the `gc.pruneExpire`
grace period (default two weeks) for *abandoned* objects — fine, they're
garbage, but it forecloses any forensic `git show` on a just-failed trial's
merge you hadn't inspected yet. And the pins protect the daemon's own
objects, not its refs: don't delete `refs/gauntlet/pin/*` by hand while the
daemon runs — that reintroduces the exact mid-run object loss the pins
exist to prevent.
