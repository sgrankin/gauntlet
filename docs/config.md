# Configuration reference

This is the full reference for the **daemon config** (`gauntlet.kdl`,
passed via `-config` — see [`gauntlet.kdl`](../gauntlet.kdl) in the repo
root for a complete example). The repo-side check spec (`.gauntlet.kdl`)
is covered in [checks.md](checks.md).

The core fields (`remote`, `poll-interval`, `check-spec`, `committer`,
`merge-message`, `target`) are all a daemon strictly needs. Every other
node below is optional — absence disables the feature it configures, so a
minimal `gauntlet.kdl` keeps working unchanged as features are added.

```kdl
log-retention "720h"   // optional; default 30 days ("720h")
auto-retry-errors true // optional; default true — set false to disable (see README's "Retry semantics")

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
  [checks.md](checks.md#check-environment-reference)) are kept before
  cmd/gauntlet's periodic sweep deletes the run-log directory. Unlike every
  node below, this one has no "absent ⇒ disabled" state: full logging is
  always wired up, so absence just means the default (30 days, `"720h"`)
  applies. Every value must be positive.
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
  — see [Summaries](#summaries) below), regardless of whether a template is
  configured.
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
  `api-url` is the REST API base (default `https://api.github.com`;
  override for GitHub Enterprise, e.g. `https://ghe.example.com/api/v3`).
  **Repo absent ⇒ disabled**: no channel is constructed, no requests
  made. Two authentication modes, mutually exclusive:

  - **Static token** (the default): `token-env` names the environment
    variable holding a PAT (default `GITHUB_TOKEN`). It authenticates
    the status channel only; git fetch/push keeps whatever ambient
    authentication the host already has (SSH, a credential helper).
    Once `repo` is set, an empty/unset `token-env` is a startup error,
    not a silent no-op — the daemon refuses to start rather than run a
    channel it can't authenticate.
  - **GitHub App** (`internal/ghauth`, issue #6):

    ```kdl
    github "acme/widgets" {
        auth "app" {
            app-id 12345
            installation-id 67890
            private-key-file "/run/credentials/gauntlet-app.pem"
        }
    }
    ```

    Short-lived installation tokens are minted lazily from an RS256 App
    JWT, cached until a safety window before their one-hour expiry, and
    refreshed without restart; concurrent operations share one mint. The
    same provider authenticates **both** the status channel and git
    fetch/push against the configured `remote` — which must therefore be
    an HTTPS URL (installation tokens cannot authenticate SSH),
    credential-free, and canonicalize to the same host and owner/repo as
    this block; any mismatch is a startup error, never a silent fallback
    to ambient auth. Credentials reach git through an ephemeral,
    secretless `GIT_ASKPASS` helper whose host match is anchored on the
    configured host (a credential prompt for any other host — an
    unexpected redirect, a rogue submodule — is refused, including
    look-alikes like `github.com.evil.example`). The token is never in a
    process argument, the persistent remote URL, git config, or anything
    a check can read through `GAUNTLET_GIT_DIR`. A clearly rejected
    credential (HTTP 401) is invalidated and retried exactly once with a
    fresh mint; a 403 (valid but under-privileged) is not. The private
    key file must not be group/other-accessible (`chmod 0600`, or
    `0400`); rotating it requires a daemon restart, token refresh does
    not.

    Two deployment notes. Because the permission check rejects any
    group/other access, a Kubernetes secret volume needs `defaultMode:
    0400` and must avoid `fsGroup` (which makes kubelet chmod projected
    files group-readable on a read-only mount you cannot `chmod` back) —
    a `0400` systemd `LoadCredential` or an init-container copy to a
    private path both work. And because the askpass helper is `exec`'d, a
    `noexec` `TMPDIR`/`/tmp` breaks authenticated git; point `TMPDIR` at
    an exec-capable directory if the host hardens `/tmp`.

    Minimal App repository permissions: **Contents: Read and write**
    (fetch, trial-merge pushes, landing the target branch) and **Commit
    statuses: Read and write**. If candidates may change files under
    `.github/workflows/`, GitHub additionally requires **Workflows:
    Read and write** to push those — without it the land push is
    rejected.

  A `trial-refs` child block enables trial-ref publication (issue #7):

    ```kdl
    github "acme/widgets" {
        trial-refs {
            prefix "refs/gauntlet/trials"
            retention "24h"
        }
    }
    ```

  The commit Gauntlet actually tests is a synthetic merge of the
  candidate and a specific target tip — a commit that initially exists
  only in Gauntlet's bare repository. With `trial-refs` on, each run
  publishes that merge under an immutable ref `<prefix>/<run-id>` before
  its checks start, so GitHub can display and status **exactly the bytes
  that ran**, and the rollup commit status moves from the candidate SHA
  to the **merge SHA** — a statement about verification of that exact
  merge, not the queue slot and not post-land deployment: pending once
  the merge is published, success when the required checks pass (posted
  *before* the landing CAS, so a "tested SHA must be green before the ref
  moves" branch-protection rule can key on it), failure on a source
  rejection, error on infrastructure. A trial *conflict* produces no
  synthetic commit, so it still reports against the candidate SHA. A
  landing deletes the now-redundant ref immediately; a non-landing run
  keeps its ref for `retention` (default 24h) so a failed merge stays
  inspectable, after which a reaper CAS-deletes it. Crash-orphaned refs
  are swept at daemon startup. `retention` unset **and** an explicit
  `"0s"` both resolve to the 24h default (there is no config spelling for
  immediate deletion — a short window is always kept so a failed merge
  stays inspectable); set a small duration like `"5m"` for a shorter one.

  `prefix` (default `refs/gauntlet/trials`) **must** start with `refs/`
  and is deliberately a **custom namespace, not `refs/heads/**`**: a
  `refs/heads/**` prefix is accepted but publishes branches that clutter
  branch UIs and **can match repository workflow triggers** (a
  `push`-triggered Action would fire on every trial) — a verified spike
  confirmed a custom `refs/gauntlet/*` push is accepted by GitHub,
  resolvable, statusable, and triggers no workflow. The publish is a
  synchronous CAS round trip per run start (latency recorded on the run
  span as `gauntlet.trialref.publish_ms`); a publish failure is an
  infrastructure error (park + retry), never a candidate rejection. Trial
  refs add to the repository's ref count and assume a single daemon owns
  the remote (Gauntlet's model). With App auth configured, publication
  uses the same refreshable credential provider.
- **`slack <channel-id>`** — enables the Slack channel (`internal/slack`):
  threaded run messages in the given channel ID, root edited to a
  pass/fail mark on landing, `:recycle:` on the root re-queues via retry,
  `:x:` on the root cancels it (see README's ["Landing
  changes"](../README.md#landing-changes)) — both work whether the run is
  still in flight or has already finished (root ownership is durable,
  carried in the message's own metadata, not just an in-memory map),
  except on a batch root, which needs the API/CLI to name a member (see
  the [Slack app setup guide](setup.md#slack-app)).
  `app-token-env`/`bot-token-env` name the environment variables holding
  the app-level (socket mode) and bot tokens (defaults `SLACK_APP_TOKEN` /
  `SLACK_BOT_TOKEN`). **Channel absent ⇒ disabled.** Once `channel` is
  set, either token being empty/unset is a startup error, same rationale
  as `github` above. `allowed-users`, when present, restricts reaction
  commands to the listed Slack member IDs ("U…"/"W…" — profile → "Copy
  member ID"): reactions from anyone else are ignored silently (logged
  daemon-side only, so the channel doesn't reveal who is authorized).
  Absent ⇒ anyone who can react in the channel can command the queue —
  fine for a private team channel, set the list for anything broader. Only
  inbound commands are gated; posting is unaffected.
- **`otlp <endpoint>`** — installs a real OTLP/HTTP span exporter
  (`internal/obs`) pointed at `<endpoint>`; `insecure` skips TLS (typical for
  a local collector). The daemon always emits spans via the OTel API —
  this just gives them somewhere to go.
  **Endpoint absent ⇒ no-op**: spans are emitted and
  immediately discarded.
- **`executor <kind>`** — selects the check executor. `"local"` (the
  default when the node is absent, or when written with no further
  configuration) runs checks as local subprocesses.
  `"container"` runs each check via `runtime` (`"docker"`, `"podman"`, or
  `"container"` for Apple's `container` CLI; default `"container"`) against
  `image`, mounting the trial tree read-write at `workdir` (default
  `/workspace`) plus one named, persistent volume per `cache` entry (`name` +
  mount `path`) so warm caches (`GOCACHE`, module caches, …) survive across
  runs. `image` is required when `kind` is `"container"`. `mount` entries
  (host path + `path` + optional `readonly`) add plain host bind mounts —
  see the [container executor guide](setup.md#container-executor) for the
  docker-socket/testcontainers case and its trust implications. Both
  executors also hand every check the daemon's bare repo as
  `GAUNTLET_GIT_DIR` (see [checks.md](checks.md#check-environment-reference));
  under `"container"` that's an automatic read-only mount at the fixed path
  `/gauntlet-git`, which — like `workdir` and the `/gauntlet` result dir —
  is reserved: an operator `mount` at or under it is a config error.

- **`executor <name> kind="local"|"container"` — named execution
  profiles.** Beside the (at most one) kind-less default block above, any
  number of *named* profiles may be declared; a repo check selects one
  with `executor "<name>"` ([checks.md](checks.md#executor-profiles)), so
  one candidate can mix containerized checks with host-local ones:

  ```kdl
  executor "container" {          // the default profile (arg = KIND)
      runtime "docker"
      image "go-ci:latest"
  }
  executor "host" kind="local"    // a named profile (arg = NAME)
  executor "it" kind="container" {
      image "go-ci:latest"
      mount "/var/run/docker.sock" path="/var/run/docker.sock"
      add-host "host.docker.internal" "host-gateway"
      env "TESTCONTAINERS_HOST_OVERRIDE" "host.docker.internal"
      memory "8g"
      cpus "4"
  }
  ```

  A profile carries every option the default executor does, plus fixed
  non-secret `env` pairs (both kinds; the `GAUNTLET_*` namespace is
  reserved and gauntlet's values win any other collision), and — container
  only — `add-host` aliases and `--memory`/`--cpus` ceilings. Profile
  names `local`/`container` are rejected (that spelling means the default
  block's kind). **Defining a profile is what allows repos to select it**,
  and selecting one grants the check everything attached to it (a socket
  mount is host-authoritative — same trust math as the default executor's
  mounts), so prefer several small profiles over one all-powerful one. The
  repo side can only *name* a profile; mounts, images, env, and resource
  ceilings all stay here, operator-owned. A spec naming an undefined
  profile is rejected before any of its commands start. One caveat: the
  shared-services machinery still follows the *default* executor — its
  kind picks the endpoint mode, and its **runtime** owns the shared
  service network. Checks that declare `needs` should therefore run on
  the default executor or on profiles matching its kind *and* runtime: a
  container-profile check on a local-default daemon would get
  host-published endpoints it may not be able to reach, and a podman
  profile beside a docker default would be handed a `--network` that
  doesn't exist in its runtime.

- **`max-executions`** (top level) caps how many bounded commands the
  daemon runs concurrently host-wide — candidate checks and post-land
  hooks, across every target, mode, speculation window, and executor
  profile (long-lived shared *service* containers don't count; their own
  limits apply). Unset means unlimited, the compatibility default — a
  production deployment should set an explicit value sized to the host,
  especially once any repo raises `max-parallel`
  ([checks.md](checks.md#ordering-and-parallelism)): total demand becomes
  Σ per-target `window` × per-candidate `max-parallel`, and this cap is
  what actually bounds it. A check that sits ready waiting for a slot
  records that wait in history (`waited` vs `duration`), so capacity
  starvation is visible rather than mistaken for slow commands.
- **`shutdown`** (top level) — the first-SIGTERM behavior (issue #8).
  `"drain"` (the default) begins a **graceful drain**: the daemon stops
  admitting new candidates and extending speculation, lets the
  already-admitted set finish its required checks and one landing CAS
  each, runs the entire queued post-land hook backlog, then exits in the
  normal teardown order. A **second signal** forces the immediate kill (a
  crash-equivalent stop, healed on restart by the idempotent reconcile).
  `"kill"` restores the legacy behavior where the first signal cancels
  everything at once. Because a drain waits for in-flight work,
  systemd's `TimeoutStopSec` is the outer hard bound — set it long enough
  for your slowest expected checks. A drain is also reachable without a
  signal (so orchestration can begin it and poll before stopping the
  service) via `POST /api/v1/drain` (optional `{"deadline": "<RFC3339>"}`
  to force the kill at a chosen instant), the MCP `drain` tool, and
  `gauntlet drain [--wait] [--deadline <dur>]`; the lifecycle
  (`running`/`draining`/`drained`), the in-flight run/check counts, and
  the drain window appear in `GET /api/v1/status` and the MCP `status`
  tool, so a readiness probe can tell "draining — alive but not admitting
  new work" from an unhealthy daemon. There is no undrain — a restart
  resumes admission.
- **`export`** (top level) — configures how trial trees are materialized
  on disk, for *every* export: check trees, candidate-built image trees,
  and hook trees.

  ```kdl
  export {
      mtimes "history"
  }
  ```

  `mtimes` selects file-timestamp behavior. Absent (the default), files
  carry extraction wall time — the classic behavior, so two exports of
  the same commit differ in metadata. `"history"` sets every tracked
  file's mtime to the committer time of the last commit that changed
  that path (git-restore-mtime semantics, computed against the exact
  synthetic merge commit), so re-exports of the same merge are metadata-
  identical and path+metadata-keyed build/test caches stop missing on
  every unrelated commit. Details worth knowing: committer time, not
  author time, and future-dated commits are stamped verbatim (a cache
  may decline to reuse until the clock catches up — deterministic beats
  plausible); a path a merge changed relative to *all* its parents (an
  auto-merge product, including gauntlet's own trial merge) gets that
  merge's time, anything matching a parent keeps its deeper
  history-derived time — in particular, a trial merge whose tree equals
  the candidate's (the up-to-date-candidate shape) owns nothing, so
  re-trials of the same content restamp identically; a rename counts as
  a change at the new path; symlink timestamps are set without
  following the link; directory mtimes are untouched; only what the
  export materialized is stamped (`export-ignore`'d paths and submodule
  gitlinks are simply absent). One disclosed approximation, matching
  the standard `git-restore-mtime` tool: "last commit that changed the
  path" is resolved in commit-date order across the whole history, so
  when a merge discarded one side's change, a discarded-but-newer
  commit can win the stamp where `git log -1 -- path` would follow the
  surviving lineage — still fully deterministic per commit (the
  property caches key on), just not always the content-lineage time.
  The pass costs one bounded history walk per export (a single git
  subprocess, stopping as soon as every path is stamped; cost is
  recorded on the run's trace span). A walk failure fails the trial as
  an infrastructure error — there is deliberately no silent wall-clock
  fallback. This is an operator knob, not a repo-spec one: the repo
  must not impose a potentially expensive host-side walk on the daemon.
- **`services`** — gates shared, cached service instances a check spec's
  `service`/`needs` nodes can request (`internal/services`); see
  [checks.md's "Shared services"](checks.md#shared-services) for the full
  contract. `allow` lists the driver kinds permitted on this box — only
  `"container"` is implemented (`"artifact"`/`"oci-unpack"` are rejected
  as reserved for a future release). `max-instances` hard-caps the pool's
  live instance count (default 8) — a count cap only, not a memory/CPU
  bound. `runtime` (`"docker"` or `"podman"`; default `"docker"`) is
  **only consulted when `executor` is `"local"`**: under `executor
  "container"`, the executor's own `runtime` wins instead (and must itself
  be docker/podman — Apple's `container` CLI is a hard startup error for
  services), so setting both to conflicting values is a config
  error. **`allow` absent ⇒ disabled**: a check spec declaring
  `service`/`needs` is then rejected at run time, loudly, like a malformed
  check spec — never silently ignored.
- **`summarize`** — enables a Claude-written merge-commit body
  (`internal/summarize`); see [Summaries](#summaries) below. **Node absent
  ⇒ disabled**: merge commits get exactly the plain subject +
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
  summarize call: one of `none`, `low`, `medium`, `high`, `xhigh`, `max`.
  Defaults to `medium` whenever the `summarize` section is present,
  regardless of `model` (same "node present ⇒ every field gets its
  default" rule as the rest of this section). Only valid on models that
  support it — `claude-sonnet-5` (the default model) does, but
  **`claude-haiku-4-5` and Sonnet 4.5 do not, at any nonzero effort
  value**: the Messages API rejects the request outright (a 400). Set
  `effort "none"` to omit the `output_config.effort` field from the
  request entirely — the escape hatch for exactly this case — if you
  switch `model` to one of those. Forgetting to set it (leaving the
  `medium` default paired with a non-supporting model) is not silent — it
  hits the same degradation path as any other summarize error: logged as
  a single line, answered with an empty body, never a blocked landing —
  but it does mean every summarize call 400s until `effort "none"` is set.
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
**Batch mode multiplies this:** forming a batch of N makes N summary
calls before checks start, run concurrently (capped at 4 in flight) on
the reconcile loop, so the stall is bounded by `ceil(N/4) × timeout`
(stalling all targets) rather than N separate calls back to back; large
`max-batch` plus summaries still means accepting that stall — smaller
now, but not zero — or disabling summaries on that daemon.
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
  run. A target with no `hook` nodes has no post-land behavior.
- Each hook runs via the daemon's **default** executor — always, with no
  profile selection: hooks are operator config with no repo-side spec to
  name a profile from. Mind this when restructuring a single `executor
  "container" {...}` into named profiles: if every check moves to a
  profile and the kind-less default block disappears, the default becomes
  the implicit *local* executor — and your hooks, written for the
  container image, silently start exec'ing on the daemon host. Keep the
  default block shaped for the hooks. Each hook runs against an export of
  the landed merge commit's tree and gets the same `GAUNTLET_*`
  environment contract a check does (see
  [checks.md](checks.md#check-environment-reference)) —
  `GAUNTLET_MERGE_SHA` is the commit that just landed.
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
- A landing recovered after a daemon crash (before its hooks could run)
  still skips hooks entirely — no automatic re-run — but its history row
  now records the actual merge commit that landed, so an operator can
  locate it and re-run its hooks manually, rather than hunting for the
  commit out of band.
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

Hooks have their own, separate cancel surface —
`POST /api/v1/hooks/cancel`, the MCP `hook_cancel` tool, or
`gauntlet hooks-cancel` (see [api.md](api.md)) — since a hook stage has no
candidate ref to name, only a target whose currently-running hook execution
should be interrupted. It only ever has anything to cancel for a target
configured with `hooks-policy "cancel"` (below) that has a landing's hooks
running right now — every other policy has no in-flight cancellation
mechanism to wrap, so the call reports a no-op rather than an error.

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

**Batch mode + a deploy-style hook needs a non-default policy.** A batch
of N members lands as N separate `EventLanded`s, so a hook still fires
once per member — under the default `queue` policy, that's N hook runs
per batch, and the first N-1 of them run against *intermediate* chain-merge
trees that the check suite never tested in isolation (checks run once,
against the chain's tip). For a `hook "deploy"` on a target running
`mode "batch"`, that means deploying N-1 commits nobody actually validated
on their own. `coalesce` (or `cancel`) is the intended pairing here: both
collapse a batch's queued landings down to the newest, so only the
already-checked tip ever gets deployed.

## Queue modes

Each `target` picks its own queueing discipline via `mode`
(see [design/queue-modes.md](design/queue-modes.md) for the mechanics):
`"serial"` (the default — one candidate tested
and landed at a time), `"batch"`, or
`"speculate"`. Config is dumb data — the knobs below are validated at load
(node-named errors) but the daemon otherwise treats them as plain per-target
settings.

```kdl
// Default serial.
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
  even start (see [Summaries](#summaries) above), so very large batches
  risk a longer whole-daemon stall on refill.
- **`window`** — the speculation pipeline depth: up to this many runs are
  in flight at once for the target. Legal only with `mode "speculate"`
  (a config error otherwise); defaults to 4 when left unset. Bounded to
  1–32. **`window` doubles as a builder-concurrency bound** while
  candidates keep the default serial checks: each speculative run then
  executes at most one check at a time, so `window` is also the maximum
  number of concurrent check processes/containers this target drives —
  size it with your build capacity in mind, not just desired queue depth.
  A repo spec that raises `max-parallel` changes that arithmetic (up to
  `window × max-parallel` per target); the executor's `max-executions`
  cap is the knob that restores a real host-wide bound.
- **`on-batch-red`** — the batch red-recovery strategy. `"serial"`
  (default) is the only strategy implemented: on a red batch, every
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
  on red). Only the fixed `window` above is implemented; setting any of
  these three on any target is a load-time error (same "reserved for a
  future release" rationale as `on-batch-red "bisect"`), so a config that
  names them fails loudly rather than silently no-opping.
