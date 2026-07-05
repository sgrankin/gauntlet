# gauntlet

Gauntlet is a merge queue. Push your branch to `for/<target>/<user>/<topic>`
and the daemon trial-merges it onto the live tip of `<target>`, runs the
named checks defined by your repo's own `.gauntlet.kdl`, and — if everything
comes back green — lands it as a `--no-ff` merge that preserves your commits
exactly as you wrote them. Red pings you; fix and re-push the same ref.

Requires git 2.38 or newer (`git merge-tree --write-tree`).

## Running

Build the daemon:

```sh
go build -o gauntlet ./cmd/gauntlet
```

There are two config files:

- **Daemon config** (admin-written, one per daemon instance) — points at the
  remote, the poll interval, the committer identity used for merge commits,
  and the target branches to reconcile. See [`gauntlet.kdl`](gauntlet.kdl)
  for a full example. Passed via `-config`.
- **Repo check spec** (adopter-written, lives in the repo the daemon
  watches) — the named checks a candidate must pass before it lands. See
  [`.gauntlet.kdl`](.gauntlet.kdl) for a full example. The daemon reads this
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
the [`Makefile`](Makefile) and [`Dockerfile`](Dockerfile).

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

**Cancellation** is ref deletion — nothing more:

```sh
git push origin --delete for/main/$USER/my-feature
# or: jj bookmark delete for/main/$USER/my-feature && jj git push -b for/main/$USER/my-feature
```

**Retry semantics.** Red (or a conflict) parks the ref at that SHA — the
daemon won't re-test it again on its own. To retry: push a new SHA (amend
and re-push the same ref name; the SHA change is what un-parks it), or, once
the Slack channel is configured (see "Configuration reference" below), react
`:recycle:` on the run's root message to re-queue the same SHA without a new
push.

## Configuration reference

The `history`, `dashboard`, `github`, `slack`, `otlp`, and container
`executor` nodes below (`docs/plans/phase23.md` §3) are all wired into the
daemon (`cmd/gauntlet`) alongside the phase-1 fields (`remote`,
`poll-interval`, `check-spec`, `committer`, `merge-message`, `target`). Each
new node is optional — absence disables the feature it configures, so an
existing phase-1 `gauntlet.kdl` keeps working unchanged.

```kdl
log-retention "720h"   // optional; default 30 days ("720h")

history "/var/lib/gauntlet/history.db" {
    sample-every "10s"
}

dashboard "localhost:8080" {
    url "https://gauntlet.internal.example"
}

github "acme/widgets" {
    token-env "GITHUB_TOKEN"
    api-url "https://api.github.com"
}

slack "C0123456789" {
    app-token-env "SLACK_APP_TOKEN"
    bot-token-env "SLACK_BOT_TOKEN"
}

otlp "localhost:4318" {
    insecure true
}

executor "container" {
    runtime "container"
    image "ghcr.io/acme/ci:latest"
    workdir "/workspace"
    cache "gocache"    path="/root/.cache/go-build"
    cache "gomodcache" path="/go/pkg/mod"
}

summarize {
    model "claude-haiku-4-5"
    api-key-env "ANTHROPIC_API_KEY"
    timeout "5s"
}
```

- **`log-retention <duration>`** — how long full per-check log files
  (`<state>/logs/<runID>/<check>.log`, written by the executor alongside
  every check's tail-capped in-band output — see "Check environment
  reference" below) are kept before cmd/gauntlet's periodic sweep deletes
  the run-log directory. Unlike every node below, this one has no "absent ⇒
  disabled" state: full logging is always wired up, so absence just means
  the default (30 days, `"720h"`) applies. Every value must be positive.
- **`history <path>`** — SQLite database file for run/check/queue-depth
  history (`internal/history`), read by the dashboard's history views.
  `sample-every` sets the queue-depth sampling interval; defaults to
  `poll-interval`. **Path absent ⇒ disabled**: no SQLite store is opened, and
  the daemon runs exactly as it does today.
- **`dashboard <bind>`** — starts the read-only web dashboard
  (`internal/dashboard`) on `<bind>` (e.g. `localhost:8080`). `url` is an
  optional public base URL used only for outbound links (e.g. the GitHub
  commit status `target_url`); defaults to `http://<bind>`, which is usually
  wrong once anything sits in front of the daemon (a proxy, a tailnet
  hostname), so set it explicitly whenever the dashboard is reachable at a
  different address than it binds. **Bind absent ⇒ disabled**: no HTTP
  server starts. The dashboard has no authentication of its own — put it
  behind your proxy/tailnet if it needs one. Whenever the dashboard is
  enabled it also serves each check's full, uncapped log at `GET
  /run/{runID}/log/{checkName}` (linked as "full log" on the run page),
  containment-checked under `<state>/logs`; a pruned or otherwise missing
  log file 404s with a friendly message rather than an error.
- **`github <owner/repo>`** — enables the GitHub commit-status channel
  (`internal/ghstatus`): one rollup status context `gauntlet/<target>`
  posted to the candidate SHA via the plain REST statuses API.
  `token-env` names the environment variable holding a PAT (default
  `GITHUB_TOKEN`); `api-url` is the REST API base (default
  `https://api.github.com`; override for GitHub Enterprise). **Repo absent
  ⇒ disabled**: no channel is constructed, no requests made. Once `repo` is
  set, an empty/unset `token-env` is a startup error, not a silent no-op —
  the daemon refuses to start rather than run a channel it can't
  authenticate.
- **`slack <channel-id>`** — enables the Slack channel (`internal/slack`):
  threaded run messages in the given channel ID, root edited to a
  pass/fail mark on landing, `:recycle:` on the root re-queues via retry.
  `app-token-env`/`bot-token-env` name the environment variables holding the
  app-level (socket mode) and bot tokens (defaults `SLACK_APP_TOKEN` /
  `SLACK_BOT_TOKEN`). **Channel absent ⇒ disabled.** Once `channel` is set,
  either token being empty/unset is a startup error, same rationale as
  `github` above.
- **`otlp <endpoint>`** — installs a real OTLP/HTTP span exporter
  (`internal/obs`) pointed at `<endpoint>`; `insecure` skips TLS (typical for
  a local collector). The daemon already emits spans via the OTel API in
  phase 1 with a no-op provider — this just gives them somewhere to go.
  **Endpoint absent ⇒ no-op** (phase-1 default): spans are emitted and
  immediately discarded, same as today.
- **`executor <kind>`** — selects the check executor. `"local"` (the
  default when the node is absent, or when written with no further
  configuration) runs checks as local subprocesses, same as phase 1.
  `"container"` runs each check via `runtime` (`"docker"`, `"podman"`, or
  `"container"` for Apple's `container` CLI; default `"container"`) against
  `image`, mounting the trial tree read-write at `workdir` (default
  `/workspace`) plus one named, persistent volume per `cache` entry (`name` +
  mount `path`) so warm caches (`GOCACHE`, module caches, …) survive across
  runs. `image` is required when `kind` is `"container"`.
- **`summarize`** — enables a Claude-written merge-commit body
  (`internal/summarize`); see [Summaries](#summaries) below. **Node absent
  ⇒ disabled**: merge commits get exactly the phase-1/2/3 subject +
  trailers, no body. Once the node is present (even empty, `summarize {}`),
  an empty/unset `api-key-env` is a startup error, same rationale as
  `github`/`slack` above.

## Summaries

`summarize` is an optional enricher: right before a trial lands, the daemon
asks Claude for a short prose summary of what the candidate branch actually
did — its own commit subjects/bodies and diffstat, `base..candidate` — and
inserts that summary as the merge commit's body, between the templated
subject line and the `Gauntlet-Ref`/`Gauntlet-Run` trailers. The
`--first-parent` ledger view (`DESIGN.md`) then carries a real one-paragraph
description of each landing, not just a topic/author subject line.

The summary is generated **synchronously, before checks run**, once per
clean trial (not just landings that go on to succeed): the merge commit has
to carry it, and landing the already-tested SHA forbids amending the commit
afterward to attach one. That call runs on gauntlet's single-threaded
reconcile loop, so its `timeout` bounds a stall of the *entire* loop —
every target, not just the one being summarized — for up to that duration
on every clean trial. Keep it well under `poll-interval`.

Configuration (all fields optional; defaults shown in the example above):

- **`model`** — the Claude model ID to call. Defaults to `claude-haiku-4-5`
  — Haiku-class, chosen because a 2-6 sentence summary is a cheap,
  classification-shaped task, not a coding or planning workload. Fully
  overridable for operators who want a different tier.
- **`api-key-env`** — the environment variable holding the Anthropic API
  key. Defaults to `ANTHROPIC_API_KEY`. The daemon reads this at startup,
  once; it is never read from the config file itself.
- **`timeout`** — the per-call budget for the Messages API request, and
  therefore the worst-case stall of the whole reconcile loop described
  above. Defaults to `5s`.

**Degradation guarantee:** summarization is best-effort, by contract, all
the way down. Any failure gathering the branch's own git history, any HTTP
error, any timeout, any refusal, or an empty model response is logged as a
single line and answered with an empty body — never an error, never a
retry, never a blocked or failed landing. A merge commit with no summary is
exactly as valid as one with one; enabling `summarize` can never turn a
green trial red.

**Cost:** one small Messages API completion per landing (not per trial,
not per check) — a single request against a handful of commit
subjects/bodies and a diffstat, capped at a few hundred output tokens.

## Hooks

A `target` may configure post-land hooks (`internal/hooks`): ordered
commands run against the *landed* tree once a candidate merges onto that
target — a deploy step, a notification, a cache warm, whatever your repo's
scripts do. This is a hard scope boundary (DESIGN.md's decision ledger,
"Deployments as post-land hooks"): **gauntlet never grows a CD system.** A
hook is "run this command and tell me if it failed", full stop; when
deployment needs grow past that (health checks, rollback, progressive
delivery), the hook *hands off* to a real CD system (Argo CD on k8s, a
terraform pipeline, whatever your environment runs) — gauntlet itself never
does.

```kdl
target "main" branch="main" {
    hook "deploy" {
        command "make" "deploy"
    }
    hook "notify" {
        command "curl" "-X" "POST" "https://example.com/notify"
    }
}
```

- Hooks are nested inside their `target` block, in the order they should
  run. A target with no `hook` nodes has no post-land behavior — the
  existing phase-1/2/3 behavior, unchanged.
- Each hook runs on the daemon host via the configured executor (`local` or
  `container`, same as checks), against an export of the landed merge
  commit's tree. It gets the same `GAUNTLET_*` environment contract a check
  does (see "Check environment reference" below) — `GAUNTLET_MERGE_SHA` is
  the commit that just landed.
- Hooks for one landing run **in order**, and **stop at the first
  failure**: a deploy step shouldn't run if an earlier step (say, a
  pre-deploy check) failed.
- A hook failure is reported to the daemon's channels (log, Slack,
  GitHub status if configured) exactly like a check failure — but it
  **never** touches the landing itself, the target branch, or the queue.
  The candidate already landed; a hook is something that happens *after*,
  and gauntlet's own bookkeeping doesn't know or care whether it succeeded.
- A slow hook only delays *later* hooks for the same landing (and, since
  landings for one target are already serial, later landings on that
  target) — it never blocks the reconcile loop itself.

## API

The dashboard (`internal/dashboard`) exposes a small JSON API under
`/api/v1`, mounted on the same handler/bind as the HTML pages (§4.2 above).
It exists for agents, scripts, and the MCP server below that want
machine-readable queue status and a way to trigger a retry without a
browser. Every response is `Content-Type: application/json`, with stable
lowerCamel field names; errors are always `{"error": "..."}`.

- **`GET /api/v1/status`** — every target's live queue state: name, branch,
  tip SHA, the in-flight run (ref/sha/runID/currentCheck/startedAt/
  checksDone, or `null` if idle), the waiting queue (ref/sha/seq, FIFO
  order), and parked refs (ref/sha/outcome/reason/at). `503
  {"error":"no snapshot yet"}` before the first reconcile pass completes.

  ```sh
  curl -s http://localhost:8080/api/v1/status | jq .
  ```

- **`GET /api/v1/runs?target=<name>&limit=<n>`** — a target's recent runs
  from history, newest first (`limit` defaults to 20). `target` is
  required (`400` if missing). `503 {"error":"history disabled"}` if no
  `history` store is configured.

  ```sh
  curl -s 'http://localhost:8080/api/v1/runs?target=main&limit=5' | jq .
  ```

- **`GET /api/v1/run/{id}`** — one run's full detail, including its
  per-check results. Each check carries `logPath` (the full log file's path
  on disk, or `""` if none was written) and, only when the dashboard is
  configured to actually serve it, `logUrl` (a relative link to `GET
  /run/{id}/log/{name}` — omitted from the JSON entirely otherwise).
  `404 {"error":"not found"}` for an unknown run ID; `503
  {"error":"history disabled"}` if no `history` store is configured.

  ```sh
  curl -s http://localhost:8080/api/v1/run/<run-id> | jq .
  ```

- **`POST /api/v1/retry`** — re-queues a parked ref at its current SHA,
  same effect as re-pushing it or reacting `:recycle:` in Slack (see
  "Retry semantics" above). Body: `{"target": "main", "ref":
  "refs/heads/for/main/alice/my-feature"}`. `202 {"status":"queued"}` on
  success; `400` if `target` or `ref` is missing or the body isn't valid
  JSON; `405` for any method but `POST`.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/retry \
    -H 'content-type: application/json' \
    -d '{"target":"main","ref":"refs/heads/for/main/alice/my-feature"}'
  ```

**`gauntlet status`** and **`gauntlet retry`** are thin CLI wrappers over
the same API (client-side porcelain, like `gauntlet land`):

```sh
gauntlet status -url http://localhost:8080                  # compact per-target summary
gauntlet status -url http://localhost:8080 -target main     # one target only
gauntlet status -url http://localhost:8080 -json            # raw API response

gauntlet retry -url http://localhost:8080 -target main -ref refs/heads/for/main/alice/my-feature
```

**Trust model.** Same as the dashboard itself: the API has no
authentication of its own, so bind it to a trusted interface and put it
behind your proxy/tailnet if you need one. `retry` is non-destructive — it
only re-queues an already-parked ref for another trial-merge-and-check
pass; it never touches the target branch, force-lands anything, or bypasses
a check.

## MCP

The daemon also exposes an MCP (Model Context Protocol) server
(`internal/mcp`) at `/mcp`, mounted on the same bind/port as the dashboard
and its JSON API above — there's no separate port to configure. It speaks
the standard Streamable HTTP transport, so any MCP-capable agent or client
can connect directly:

```sh
claude mcp add --transport http gauntlet http://localhost:8080/mcp
```

Four tools are exposed, mirroring the JSON API above (same lowerCamel field
names, so an agent reading both sees one vocabulary):

- **`status`** (`target` optional) — every target's live queue state, or
  just one target's if `target` is given. Same shape as `GET /api/v1/status`.
- **`runs`** (`target` required, `limit` optional, default 20) — a target's
  recent runs from history, newest first. Errors with `"history disabled"`
  if no `history` store is configured.
- **`run`** (`run_id` required) — one run's full detail, including every
  check's captured output — the JSON API's `GET /api/v1/run/{id}` omits
  output (it's meant for a human on the dashboard's run page); this tool is
  where an agent debugging a red run gets it. Each check also carries
  `logPath` and, when the dashboard is configured to serve it, `logUrl`,
  same as the JSON API's `GET /api/v1/run/{id}`.
- **`retry`** (`target` and `ref` required) — re-queues a parked ref at its
  current SHA, the same effect as `POST /api/v1/retry` or a Slack
  `:recycle:` reaction. Returns `{"status": "queued"}` on success, or an
  error if retry isn't wired up or the retry queue is full.

**Trust model.** Same as the dashboard and its JSON API: no authentication
of its own, so bind it to a trusted interface and put it behind your
proxy/tailnet if agents need to reach it remotely. `retry` is the only tool
that mutates anything, and it's non-destructive in the same way `POST
/api/v1/retry` is — see "Trust model" above.

## Manual verification / setup guides

These channels/executors have no fake to exercise in CI-style tests; verify
them by hand against the real service once, per docs/plans/phase23.md §5.

### GitHub commit status

1. Create a **fine-grained PAT** scoped to the one repository, with
   **Commit statuses: Read and write** and nothing else (GitHub adds
   **Metadata: Read-only** automatically — that's expected and sufficient;
   no other permission is needed unless the git remote itself also
   authenticates via this token, in which case add **Contents: Read and
   write** too — see [docs/deploy.md](docs/deploy.md#github-fine-grained-pat-minimal-permissions)
   for the full writeup).
2. Export it as `GITHUB_TOKEN` (or whatever `token-env` names) in the
   daemon's environment.
3. Add a `github "<owner>/<repo>"` node to `gauntlet.kdl`.
4. Push a candidate. You should see a `gauntlet/<target>` status appear
   `pending` on the candidate SHA once the trial merge is clean, flip to
   `success`/`failure`/`error` when the run finishes, and its description
   carry the rejection detail on failure. Visible on the commit and on any
   PR built from that branch.

### Slack app

1. Create a Slack app from a manifest with: **socket mode** enabled; bot
   scopes `chat:write` and `reactions:read`; app-level token scope
   `connections:write`; subscribed bot event `reaction_added`.
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
   without pushing anything.

### Container executor

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

### OTLP export

1. Point `otlp` at any OTLP/HTTP collector endpoint (e.g. a local
   `otel-collector` on `localhost:4318`; `insecure true` if it's plain
   HTTP).
2. Push a candidate. Spans should appear in whatever backend the collector
   forwards to: one root span per run, with children for the trial merge,
   each check, and the land. This is export only — SQLite `history` (if
   configured) is a separate, always-local store; OTLP doesn't feed it and
   isn't fed by it.

## Check environment reference

Every executor (local or container) sets five environment variables before
running a check's command, and provides a result file for reporting
`skipped`:

- `GAUNTLET_BASE_SHA` — the target tip the trial merge was built onto.
- `GAUNTLET_MERGE_SHA` — the tested merge commit (base + candidate).
- `GAUNTLET_CANDIDATE_SHA` — the candidate's own commit.
- `GAUNTLET_REF` — the candidate's queue-slot ref
  (`refs/heads/for/<target>/<user>/<topic>`).
- `GAUNTLET_RESULT_FILE` — path to a file the check may write to report a
  verdict other than pass/fail.

**Result-file protocol.** A non-zero exit is always a failure, full stop —
the result file is ignored on failure. On exit 0: a result file containing
`skipped` reports `CheckSkipped` (distinct from `passed` in history, so a
skipped check doesn't quietly count as green); an absent or empty file is
`CheckPassed`.

**Full per-check logs.** Every check's combined stdout+stderr is captured
twice: a 64KiB tail-capped copy inline (`Output` — the fast view: run
history, the run page, the `run` MCP tool), and, whenever `<state>/logs` is
writable, the complete, uncapped output as a file at
`<state>/logs/<runID>/<check>.log`. The full file is what the dashboard's
"full log" link and the JSON API/MCP `logPath`/`logUrl` fields point at
(see "API" and "MCP" above); it's pruned after `log-retention` (default 30
days, see "Configuration reference") regardless of whether history or the
dashboard are configured.

This is the whole mechanism for conditional/monorepo-style execution —
gauntlet has no path-filter config (see DESIGN.md "Decision ledger": path
globs, never). An affected-only check decides for itself, using the SHAs
it's handed:

```sh
if git diff --name-only "$GAUNTLET_BASE_SHA" "$GAUNTLET_MERGE_SHA" | grep -q '^services/web/'; then
    go test ./services/web/...
else
    echo skipped > "$GAUNTLET_RESULT_FILE"
fi
```

Note the check's working tree is a plain export (`git archive`, no `.git`),
so resolving that diff needs a git object store the check can reach on its
own — e.g. a clone the check maintains in a cache volume, or a shallow fetch
of just those two SHAs. Gauntlet hands you the SHAs; how you turn them into
a diff is repo-owned, same as everything else about what a check does.

**Status:** phase 1 is under construction.

See [DESIGN.md](DESIGN.md) for the full design and rationale.
