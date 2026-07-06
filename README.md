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
the Slack channel is configured (see "Configuration reference" below), react
`:recycle:` on the run's root message to re-queue the same SHA without a new
push — this works whether the run is still in flight or has long since
finished (the normal case for a ❌ root someone reacts to), and threads the
re-queued run's own progress under the same root rather than posting a new
one (see "Slack app" below). Not available on a batch root — see the
batch-root exception there.

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
the API/CLI: a Slack reaction can't name a single batch member (see
"Slack app" below), so use those for a batch. See "API"/"MCP" below for
the wire shape, and `docs/plans` for the full per-mode semantics.

Post-land hooks (below) have their own, separate cancel surface —
`POST /api/v1/hooks/cancel`, the MCP `hook_cancel` tool, or
`gauntlet hooks-cancel` — since a hook stage has no candidate ref to name,
only a target whose currently-running hook execution should be interrupted.
It only ever has anything to cancel for a target configured with
`hooks-policy "cancel"` (below) that has a landing's hooks running right
now — every other policy has no in-flight cancellation mechanism to wrap, so
the call reports a no-op rather than an error.

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
    depth-retention "336h"   // optional; default 14 days ("336h"); queue-depth series only
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
    // optional: restrict reaction commands to these member IDs
    allowed-users "U025FTHN3" "U0987ZYXWV"
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

services {
    allow "container"
    max-instances 8
    runtime "docker"   // only consulted when executor is "local" — see below
}

summarize {
    model "claude-sonnet-5"
    api-key-env "ANTHROPIC_API_KEY"
    effort "medium"
    timeout "5s"
}
```

- **`log-retention <duration>`** — how long full per-check log files
  (`<state>/logs/<runID>/<check>.log.zst`, zstd-compressed and written by
  the executor alongside every check's tail-capped in-band output — see
  "Check environment reference" below) are kept before cmd/gauntlet's
  periodic sweep deletes the run-log directory. Unlike every node below,
  this one has no "absent ⇒ disabled" state: full logging is always wired
  up, so absence just means the default (30 days, `"720h"`) applies. Every
  value must be positive.
- **`merge-message <template>`** — a Go `text/template` string for the merge
  commit's subject line (`internal/queue`). Available fields: `.Topic`,
  `.User`, `.Ref`, `.RunID`, `.Target`. **Absent ⇒ the built-in default**,
  the one place the daemon does its own variant switching: `"Merge {{.Topic}}
  ({{.User}})"` when the candidate ref carries a user (the normal case), or
  `"Merge {{.Topic}}"` when it doesn't (solo setups, or a ref with no user
  segment) — dropping the parens rather than rendering a bare
  `Merge topic ()`. A configured template gets none of that switching: it
  renders exactly as written for every candidate, empty `.User` included.
  Either way, the `Gauntlet-Ref: <ref>` / `Gauntlet-Run: <runID>` trailers
  are appended unconditionally after the subject (and any `summarize` body
  — see "Summaries" below), regardless of whether a template is configured.
- **`history <path>`** — SQLite database file for run/check/queue-depth
  history (`internal/history`), read by the dashboard's history views.
  `sample-every` sets the queue-depth sampling interval; defaults to
  `poll-interval`. `depth-retention` sets how long queue-depth samples are
  kept before the sampler prunes them; defaults to 14 days (`"336h"`),
  validated like every duration here. It prunes only the queue-depth sample
  series — `runs`/`checks`/`hooks` rows are never pruned (see DESIGN.md's
  decision ledger, "History grows unboundedly by design"). **Path absent ⇒
  disabled**: no SQLite store is opened, and the daemon runs exactly as it
  does today.
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
  pass/fail mark on landing, `:recycle:` on the root re-queues via retry,
  `:x:` on the root cancels it (see "Operator cancellation" above) — both
  work whether the run is still in flight or has already finished (root
  ownership is durable, carried in the message's own metadata, not just an
  in-memory map), except on a batch root, which needs the API/CLI to name a
  member (see "Slack app" below). `app-token-env`/`bot-token-env` name the
  environment variables holding the app-level (socket mode) and bot tokens
  (defaults `SLACK_APP_TOKEN` / `SLACK_BOT_TOKEN`). **Channel absent ⇒
  disabled.** Once `channel` is set, either token being empty/unset is a
  startup error, same rationale as `github` above. `allowed-users`, when
  present, restricts reaction commands to the listed Slack member IDs
  ("U…"/"W…" — profile → "Copy member ID"): reactions from anyone else are
  ignored silently (logged daemon-side only, so the channel doesn't reveal
  who is authorized). Absent ⇒ anyone who can react in the channel can
  command the queue — fine for a private team channel, set the list for
  anything broader. Only inbound commands are gated; posting is unaffected.
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
  runs. `image` is required when `kind` is `"container"`. `mount` entries
  (host path + `path` + optional `readonly`) add plain host bind mounts —
  see "Container executor" below for the docker-socket/testcontainers case
  and its trust implications.
- **`services`** — gates shared, cached service instances a check spec's
  `service`/`needs` nodes can request (`internal/services`); see
  ["Shared services"](#shared-services) below for the full contract.
  `allow` lists the driver kinds permitted on this box — phase A implements
  only `"container"` (`"artifact"`/`"oci-unpack"` are rejected as reserved
  for a future release). `max-instances` hard-caps the pool's live instance
  count (default 8) — a count cap only, not a memory/CPU bound. `runtime`
  (`"docker"` or `"podman"`; default `"docker"`) is **only consulted when
  `executor` is `"local"`**: under `executor "container"`, the executor's
  own `runtime` wins instead (and must itself be docker/podman — Apple's
  `container` CLI is a hard startup error for services in phase A), so
  setting both to conflicting values is a config error. **`allow` absent ⇒
  disabled**: a check spec declaring `service`/`needs` is then rejected at
  run time, loudly, like a malformed check spec — never silently ignored.
- **`summarize`** — enables a Claude-written merge-commit body
  (`internal/summarize`); see [Summaries](#summaries) below. **Node absent
  ⇒ disabled**: merge commits get exactly the phase-1/2/3 subject +
  trailers, no body. Once the node is present (even empty, `summarize {}`),
  an empty/unset `api-key-env` is a startup error, same rationale as
  `github`/`slack` above.
- **`target`'s `mode`, `max-batch`, `window`, `on-batch-red`** — per-target
  queueing discipline (serial/batch/speculate); see [Queue
  modes](#queue-modes) below.

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

- **`model`** — the Claude model ID to call. Defaults to `claude-sonnet-5`
  — prompt quality for this task was validated live against it, and its
  configurable `effort` (below) lets operators dial intelligence vs. cost
  rather than being stuck on a fixed tier. Fully overridable for operators
  who want a different model, including the former default,
  `claude-haiku-4-5` (see the `effort` note below if you do).
- **`effort`** — the `output_config.effort` value sent with every
  summarize call: one of `low`, `medium`, `high`, `xhigh`, `max`. Defaults
  to `medium` whenever the `summarize` section is present, regardless of
  `model` (same "node present ⇒ every field gets its default" rule as the
  rest of this section). Only valid on models that support it —
  `claude-sonnet-5` (the default model) does, but **`claude-haiku-4-5` and
  Sonnet 4.5 do not, at any effort value**: the Messages API rejects the
  request outright (a 400). If you switch `model` to one of those, this
  config layer has no way to suppress the `effort` field (there is no KDL
  syntax that round-trips to "explicitly empty" instead of "defaulted"),
  so every summarize call will 400. That failure is not silent — it hits
  the same degradation path as any other summarize error: logged as a
  single line, answered with an empty body, never a blocked landing — but
  it means summarization is effectively unusable with those models through
  this config node today. This is the intended tradeoff, not a bug to
  work around: pairing `effort` with a non-supporting model is the
  operator's responsibility, called out here so it isn't a surprise.
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

**Cost:** one small Messages API completion per **clean trial** — the
merge commit must carry its body before checks run (landing the tested
SHA forbids amending later), so a trial whose checks then fail has still
spent its summary call, and a re-queued candidate spends another on its
re-trial. Each call is a single request against a handful of commit
subjects/bodies and a diffstat, capped at a few hundred output tokens.
**Batch mode multiplies this:** forming a batch of N makes N sequential
summary calls on the reconcile loop before checks start (bounded by
N × `timeout`, stalling all targets); large `max-batch` plus summaries
means accepting that stall or disabling summaries on that daemon.
Plainly: at the defaults (`claude-sonnet-5`, `effort "medium"`), that call
costs on the order of **10x** what the old default (`claude-haiku-4-5`,
no effort/thinking) cost per landing — Sonnet's per-token price is several
times Haiku's, and `medium` effort spends some thinking tokens a
no-thinking Haiku call never did. In absolute terms this is still small —
a few hundred output tokens on one short completion per clean trial —
but it is a real, deliberate step up from the
previous default, made because prompt quality for this task was validated
live against `claude-sonnet-5`. Set `model "claude-haiku-4-5"` (see the
`effort` caveat above) or a lower `effort` if the per-landing cost
matters at your merge volume.

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
- Hooks get the same log/history treatment as checks (full parity):
  each hook's full combined-output log is written to
  `<state>/logs/<runID>/hook-<n>-<sanitized name>.log.zst` — the *same*
  per-run directory that landing's own check logs already live in, so
  `log-retention`'s sweep (above) covers hook logs for free, no separate
  configuration needed. Every hook result is also written to the run's
  history row (`internal/history`'s `hooks` table) alongside its checks,
  and the dashboard's run page renders a "Hooks" section — same status
  chip/duration/expandable-output/"full log" link treatment a check
  gets — whenever a run actually has hook rows (omitted entirely
  otherwise). `GET /api/v1/run/{id}` and the MCP `run` tool both gain a
  `hooks` array in the same shape as `checks`.

### Backlog policies

Hooks always run **serially** — one landing's hooks at a time, never two
landings' concurrently, no matter what's configured below. `hooks-policy`
only decides what happens to a target's *backlog* when landings outpace
hook execution: a `make deploy` that takes five minutes will always fall
behind a target that merges every thirty seconds. Set it inside the
`target` block, alongside its `hook` nodes:

```kdl
target "main" branch="main" {
    hooks-policy "coalesce"
    hook "deploy" {
        command "make" "deploy"
    }
}
```

| Policy | Behavior |
| --- | --- |
| `queue` (default) | Every landing's hooks run, in order — nothing is ever dropped. The original, unchanged behavior. |
| `coalesce` | A landing still *queued* (not yet started) is dropped once a newer landing for the same target is also queued behind it — only the newest queued landing runs next. Whatever is currently *running* always finishes undisturbed. Each drop is logged (`hooks: coalesced landing <topic>@<sha>, superseded by <topic>@<sha>`); no hook result is fabricated for a landing whose hooks never ran. |
| `cancel` | `coalesce`, plus: the landing currently *running* is cancelled — its in-flight hook command is killed — the instant a newer landing for the same target arrives, rather than waiting for it to finish. The cancelled hook still gets a normal `EventHookFinished` (carrying the `Err` the executor returns on cancellation, same shape as a failure) with a `superseded by ...` detail, and its remaining hooks are skipped, same as an ordinary hook failure. |

The motivating case is deploys slower than merges: three candidates land
onto `main` while `make deploy` is still running for the first. With
`queue`, all three deploys eventually run, back to back, each one already
stale by the time it starts. With `coalesce`, only the newest of the two
still-queued landings deploys once the first finishes — the operator's
"deploy the latest successful one next". With `cancel`, the in-progress
deploy for the *first* landing is killed as soon as the third arrives, so
the newest candidate starts deploying immediately instead of waiting out
a deploy that's already obsolete.

`hooks-policy` is only meaningful on a target that has at least one
`hook` — setting it on a target with none is a config error.

## Queue modes

Each `target` picks its own queueing discipline via `mode`
(`docs/plans/phase5.md`): `"serial"` (the default — one candidate tested
and landed at a time, byte-for-byte the phase-1 behavior), `"batch"`, or
`"speculate"`. Config is dumb data — the knobs below are validated at load
(node-named errors) but the daemon otherwise treats them as plain per-target
settings.

```kdl
// Default serial — unchanged from phase 1.
target "main" branch="main"

// Batch: test up to 8 queued candidates as one --no-ff chain; on red, fall
// back to serial until the culprit is found, then resume batching.
target "release" branch="release/v2" {
    mode "batch"
    max-batch 8
    on-batch-red "serial"
}

// Speculate: pipeline 4 candidates, each tested on the predicted landing of
// the ones ahead of it; checks overlap, landings stay strictly FIFO.
target "staging" branch="staging" {
    mode "speculate"
    window 4
}
```

- **`mode`** — `"serial"` (default, `""` also means serial) tests and
  lands one candidate at a time. `"batch"` merges up to `max-batch` queued
  candidates into a single `--no-ff` chain and runs one check suite over
  the combined tree — fewer CI runs per candidate, at the cost of coarser
  attribution when the batch goes red. `"speculate"` pipelines up to
  `window` runs, each testing its own candidate chained onto the
  predicted (not-yet-landed) tip of the run ahead of it — checks overlap
  across candidates, but landings still happen strictly FIFO, one at a
  time.
- **`max-batch`** — caps how many candidates one batch run combines.
  Legal only with `mode "batch"` (a config error otherwise); defaults to
  8 when left unset. Bounded to 1–64. Each additional batch member costs
  one more synchronous summarize call on the reconcile loop before checks
  even start (see "Summaries" above), so very large batches risk a
  longer whole-daemon stall on refill.
- **`window`** — the speculation pipeline depth: up to this many runs are
  in flight at once for the target. Legal only with `mode "speculate"`
  (a config error otherwise); defaults to 4 when left unset. Bounded to
  1–32. **`window` doubles as a builder-concurrency bound**: each
  speculative run executes at most one check at a time, so `window` is
  also the maximum number of concurrent check processes/containers this
  target drives against the configured executor — size it with your
  build capacity in mind, not just desired queue depth.
- **`on-batch-red`** — the batch red-recovery strategy. `"serial"`
  (default) is the only strategy phase 5 implements: on a red batch, every
  member re-queues unparked and the next refill for this target forms
  them one at a time (serial semantics) until the culprit is found and
  parked; batching resumes automatically once a landing occurs.
  `"bisect"` (split the failed set and recurse to find the culprit in
  fewer rounds) is a documented growth path only — it's accepted by
  config parsing so the knob is forward-compatible, but **`LoadDaemon`
  rejects it with a "reserved for a future release" error**; it is not
  silently treated as `"serial"`. Legal only with `mode "batch"`.
- **Reserved, rejected if set**: `window-start`, `window-max`, and
  `window-halve-on-red` reserve config surface for a possible future
  adaptive speculation-window governor (start small, grow on green, halve
  on red). Phase 5 ships only the fixed `window` above; setting any of
  these three on any target is a load-time error (same "reserved for a
  future release" rationale as `on-batch-red "bisect"`), so a config that
  names them fails loudly rather than silently no-opping.

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
  per-check results, plus a `hooks` array (its post-land hook results, same
  shape as `checks` — always present, empty when the run had no hooks).
  Each check/hook carries `logPath` (the full log file's path
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

- **`POST /api/v1/cancel`** — stops whatever is currently happening to a
  candidate and parks it at its current SHA (see "Operator cancellation" above), same
  effect as reacting `:x:` in Slack. Body: `{"target": "main", "ref":
  "refs/heads/for/main/alice/my-feature"}`. `202 {"status":"queued"}` on
  success; `400` if `target` or `ref` is missing or the body isn't valid
  JSON; `405` for any method but `POST`.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/cancel \
    -H 'content-type: application/json' \
    -d '{"target":"main","ref":"refs/heads/for/main/alice/my-feature"}'
  ```

- **`POST /api/v1/hooks/cancel`** — cancels a target's currently-running
  post-land hook execution, if any (see "Operator cancellation" above). Body:
  `{"target": "main"}`. `202 {"status":"cancelled"}` if a running landing
  was found and signalled, `202 {"status":"no-op"}` if nothing was running
  for that target (not an error); `400` if `target` is missing or the body
  isn't valid JSON; `503 {"error":"hooks disabled"}` if no target configures
  any hooks; `405` for any method but `POST`.

  ```sh
  curl -s -X POST http://localhost:8080/api/v1/hooks/cancel \
    -H 'content-type: application/json' \
    -d '{"target":"main"}'
  ```

**`gauntlet status`**, **`gauntlet retry`**, **`gauntlet cancel`**, and
**`gauntlet hooks-cancel`** are thin CLI wrappers over the same API
(client-side porcelain, like `gauntlet land`):

```sh
gauntlet status -url http://localhost:8080                  # compact per-target summary
gauntlet status -url http://localhost:8080 -target main     # one target only
gauntlet status -url http://localhost:8080 -json            # raw API response

gauntlet retry -url http://localhost:8080 -target main -ref refs/heads/for/main/alice/my-feature
gauntlet cancel -url http://localhost:8080 -target main -ref refs/heads/for/main/alice/my-feature
gauntlet hooks-cancel -url http://localhost:8080 -target main
```

**Trust model.** Same as the dashboard itself: the API has no
authentication of its own, so bind it to a trusted interface and put it
behind your proxy/tailnet if you need one. `retry` is non-destructive — it
only re-queues an already-parked ref for another trial-merge-and-check
pass; it never touches the target branch, force-lands anything, or bypasses
a check. `cancel`/`hooks-cancel` are the same kind of non-destructive
operational control — they park a ref or interrupt a hook command, never
delete anything or touch the target branch.

## MCP

The daemon also exposes an MCP (Model Context Protocol) server
(`internal/mcp`) at `/mcp`, mounted on the same bind/port as the dashboard
and its JSON API above — there's no separate port to configure. It speaks
the standard Streamable HTTP transport, so any MCP-capable agent or client
can connect directly:

```sh
claude mcp add --transport http gauntlet http://localhost:8080/mcp
```

Six tools are exposed, mirroring the JSON API above (same lowerCamel field
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
- **`cancel`** (`target` and `ref` required) — stops whatever is currently
  happening to a candidate and parks it, the same effect as
  `POST /api/v1/cancel` or a Slack `:x:` reaction. Returns
  `{"status": "queued"}` on success, or an error if cancel isn't wired up or
  the cancel queue is full.
- **`hook_cancel`** (`target` required) — cancels a target's currently
  running post-land hook execution, the same effect as
  `POST /api/v1/hooks/cancel`. Returns `{"status": "cancelled"}` or
  `{"status": "no-op"}` (nothing was running — not an error), or an error if
  hook cancellation isn't wired up.

**Trust model.** Same as the dashboard and its JSON API: no authentication
of its own, so bind it to a trusted interface and put it behind your
proxy/tailnet if agents need to reach it remotely. `retry`, `cancel`, and
`hook_cancel` are the only tools that mutate anything, and each is
non-destructive in the same way its `POST /api/v1/*` counterpart is — see
"Trust model" above.

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
     (see "Queue modes" below), reactions included.

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
     `colima start --mount /path/to/state:w`).
   - **The `osxkeychain` credential helper blocks headless pulls.** If an
     image isn't present locally, `docker run` pulls implicitly, the
     credential helper may pop a Keychain prompt — even for anonymous
     pulls of public images — and the check wedges until a human clicks.
     Pre-pull images used by checks (`docker pull` once, interactively),
     or drop `credsStore` from `~/.docker/config.json` on a headless
     builder.

### Shared services

Some test suites need a real backing service — SQL Server, a message
broker — that's too slow to spin up per check or per run. `services` lets a
check spec declare one, cached and reused across runs (and across daemon
restarts) instead of started fresh every time.

**Declare it in the repo, not the daemon.** Service instances are declared
in your check spec (the same `.gauntlet.kdl` the checks themselves live in),
read from the trial-merged tree exactly like `check` — a branch that bumps
an image tag or adds an env var is tested against its own declaration,
without touching anything else's warm instance:

```kdl
service "mssql" {
    image "ghcr.io/acme/mssql-fts:2022-cu14"
    port 1433
    env "ACCEPT_EULA" "Y"
    env "MSSQL_SA_PASSWORD" "gauntlet-scratch-pw1"
    ready-command "/opt/mssql-tools/bin/sqlcmd" "-S" "localhost" "-U" "sa" "-P" "gauntlet-scratch-pw1" "-Q" "SELECT 1"
    ready-timeout "90s"
    idle-ttl "2h"
}

check "test" {
    command "go" "test" "./..."
    needs "mssql"
}
```

`service`/`ready-command`/`env` are **multi-line child blocks only** —
kdl-go doesn't accept a single-line `service "x" { image "y" }` form. `needs`
takes one or more service names on a single node (`needs "mssql" "redis"`);
every name must match a declared `service` in the same spec, or the spec
fails to parse (the same loud, `OutcomeRejected` treatment as any other
malformed check spec). A check with no `needs` is wholly unaffected —
nothing here changes for it, cost or behavior.

The daemon must separately opt in with a `services` node (see
["Configuration reference"](#configuration-reference) above) — the repo
declares intent, the daemon config gates capability. **No `services` node ⇒
any `service`/`needs` in a check spec is rejected at run time**, loudly, so
an author can't believe a service was provided when it silently wasn't.

**What gauntlet guarantees, what your harness owns.** For each resolved
`needs`, the check gets `GAUNTLET_SVC_<NAME>_HOST`/`_PORT` (see ["Check
environment reference"](#check-environment-reference) above): an instance
matching your declaration, ready, reachable for the run's duration.
Everything *inside* the instance — per-test/per-run tenancy, cleanup,
concurrency safety — is the harness's job, using `GAUNTLET_RUN_ID` to
namespace what it creates (`CREATE DATABASE testdb_$GAUNTLET_RUN_ID`, …),
same as it would against any shared, reused test database.

**Trust, stated honestly.** The real change here isn't sandboxing — a
service instance runs in the same kind of container a check does — it's
**lifetime**. A check container dies with its run; a service instance
persists on the builder until `idle-ttl`, and can be kept warm indefinitely
by continued pushes, including from a branch that never lands. `env`
secrets in a service declaration (the `MSSQL_SA_PASSWORD` above) are
therefore **scratch secrets only** — throwaway credentials whose entire
dataset is generated test fixtures, reachable only from the builder, never
anything that protects something real. `max-instances` and `idle-ttl` are
the only bounds on this capability; `allow` is the switch operators who
don't want it on a given box simply never flip. Adoption at boot also
trusts on-box container names/labels not to have been forged by something
else running on the machine — same threat model as everything else here
(your own developers, not hostile tenants), named explicitly so it's a
decision, not an accident.

**`max-instances` bounds count, not resources.** It caps how many live
instances the pool will create — nothing enforces per-instance memory/CPU,
which is whatever the runtime defaults to (typically unlimited). A single
heavyweight service spec can still pressure the builder; that's a known,
documented gap in phase A, not a solved problem.

**Distroless/shell-less images need an explicit `ready-command`.** Omitting
it gets a default readiness probe — but that default execs *into* the
instance to check for a listening socket (there's no way for the daemon to
dial it directly on the container network), which needs *some* shell/binary
present. An image with no shell must declare its own `ready-command`, or
readiness will never be detected.

**Hooks can't declare services in phase A.** Post-land hooks have no
`needs` grammar at all — this is deliberate scope control for v1, not an
oversight; a hook's environment never carries `GAUNTLET_SVC_*` vars.

**Apple's `container` runtime is deferred for services.** Phase A's
docker/podman networking model (a shared user-defined network, service
containers as aliases on it) has no Apple `container` CLI equivalent yet.
A daemon configured for services under a container-networked mode with
runtime `"container"` fails at startup with:
`services require docker or podman in phase A; Apple container networking is deferred (docs/plans/services.md §9)`.
`executor "local"` plus `services { runtime "docker" }`
(services containerized, checks run as local subprocesses) works fine on
any box with docker/podman, Apple `container` included for the checks
themselves.

**Cross-repo sharing is deliberately impossible.** An instance's cache key
includes the daemon's configured `remote` — the same push-trust boundary
gauntlet already enforces everywhere else — so two repos on the same daemon
never share a service instance, even with byte-identical declarations. This
is a forfeited optimization, not a bug: an instance's single all-powerful
account (the `sa` above) has no per-repo partitioning, so sharing across
repos would let one repo's pushed branch read or drop another's fixtures.

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

Every executor (local or container) sets six environment variables before
running a check's command, and provides a result file for reporting
`skipped`:

- `GAUNTLET_BASE_SHA` — the target tip the trial merge was built onto.
- `GAUNTLET_MERGE_SHA` — the tested merge commit (base + candidate).
- `GAUNTLET_CANDIDATE_SHA` — the candidate's own commit.
- `GAUNTLET_REF` — the candidate's queue-slot ref
  (`refs/heads/for/<target>/<user>/<topic>`).
- `GAUNTLET_RESULT_FILE` — path to a file the check may write to report a
  verdict other than pass/fail.
- `GAUNTLET_RUN_ID` — this run's ID, stable across every check (and, for a
  batch, shared by every member) in it. A check's own test harness can use
  this to namespace shared external services per run — e.g. creating
  `testdb_$GAUNTLET_RUN_ID` on a shared SQL Server — so concurrent runs
  (the speculate window, or a batch's members) can't collide on the same
  external resource.

A check that declares `needs` (see ["Shared services"](#shared-services)
below) additionally gets one pair per resolved service:

- `GAUNTLET_SVC_<NAME>_HOST` / `GAUNTLET_SVC_<NAME>_PORT` — where to reach
  the service (`<NAME>` is the service's declared name, upcased,
  non-alphanumerics turned into `_`). Absent entirely for a check with no
  `needs`, and for hooks (which can't declare `needs` at all in phase A).

**Result-file protocol.** A non-zero exit is always a failure, full stop —
the result file is ignored on failure. On exit 0: a result file containing
`skipped` reports `CheckSkipped` (distinct from `passed` in history, so a
skipped check doesn't quietly count as green); an absent or empty file is
`CheckPassed`.

**Full per-check logs.** Every check's combined stdout+stderr is captured
twice: a 64KiB tail-capped copy inline (`Output` — the fast view: run
history, the run page, the `run` MCP tool), and, whenever `<state>/logs` is
writable, the complete, uncapped output as a zstd-compressed file at
`<state>/logs/<runID>/<check>.log.zst` (fastest zstd level, favoring
throughput over ratio since this is a supplementary record, not a
space-optimized archive). The full file is what the dashboard's "full log"
link and the JSON API/MCP `logPath`/`logUrl` fields point at (see "API" and
"MCP" above) — the dashboard decompresses it on the fly when serving; it's
pruned after `log-retention` (default 30 days, see "Configuration
reference") regardless of whether history or the dashboard are configured.

Post-land hooks (see "Hooks" above) get the identical treatment: each
hook's full log lands at `<state>/logs/<runID>/hook-<n>-<sanitized
name>.log.zst` — inside the *same* run directory its checks' logs already
live in, so it's covered by the exact same retention sweep and served
through the exact same `GET /run/{id}/log/{name}` route, with no separate
configuration.
To read one offline: `zstd -d <path>` (or `zstd -dc <path> | less`).

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

**Status:** feature-complete through phase 5 — serial/batch/speculate
modes, local+container executors, dashboard/API/MCP, Slack duplex with
reaction commands, GitHub statuses, post-land hooks, Claude merge
summaries, full log capture, and park persistence are all shipped;
post-completion consistency audit done.

See [DESIGN.md](DESIGN.md) for the full design and rationale.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
