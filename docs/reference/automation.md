# Summaries and post-land hooks

## Summaries

Summaries apply to `landing "merge"` targets. Squash landings use source or
review messages instead.

```kdl
summarize {
    model "gpt-5.4"
    timeout "5s"
}
```

`summarize` is an optional enricher: while constructing a trial, the daemon
asks Codex for a short prose summary of what the candidate branch actually
did — its own commit subjects/bodies and diffstat, `base..candidate` — and
inserts that summary as the merge commit's body, between the templated
subject line and the `Gauntlet-Ref`/`Gauntlet-Run` trailers.

The summary is generated **synchronously, before checks run**, once per
clean trial (not just landings that go on to succeed): the merge commit has
to carry it, and landing the already-tested SHA forbids amending the commit
afterward to attach one. That call runs on gauntlet's single-threaded
reconcile loop, so its `timeout` bounds a stall of the *entire* loop —
every target, not just the one being summarized — for up to that duration
on every clean trial. Keep it well under `poll-interval`.

Configuration (all fields optional; defaults listed below):

- **`model`** — Codex model ID; defaults to `gpt-5.4`.
- **`effort`** — Codex reasoning effort (`low`, `medium`, `high`, `xhigh`);
  defaults to `medium`. `none` uses the model default.
- **`auth`** — `api-key` (default) or `chatgpt`, as in failure review.
- **`token-env`** — credential environment variable, read at startup.
  Defaults to `OPENAI_API_KEY`, or `CODEX_ACCESS_TOKEN` for ChatGPT auth.
- **`codex`** — executable path/name; defaults to `codex`.
- **`api-url`** — Responses endpoint base URL for API-key authentication;
  defaults to `https://api.openai.com/v1`.
- **`timeout`** — bounds the synchronous Codex process; defaults to `5s`.

Both summaries and failure review use the same isolated Codex runner, with
an empty home/workspace, shell and web disabled, and structured output.
Summaries do not enable investigation tools.

**Degradation guarantee:** summarization is best-effort, by contract, all
the way down. Any failure gathering the branch's own git history, any Codex
error, any timeout, any refusal, or an empty model response is logged as a
single line and answered with an empty body — never an error, never a
retry, never a blocked or failed landing. A merge commit with no summary is
exactly as valid as one with one; enabling `summarize` can never turn a
green trial red.

**Cost:** one Codex execution per clean trial, including trials that later
fail checks. Each execution receives commit messages and a diffstat; no
source-inspection tools are enabled. Model pricing and reasoning effort
determine cost. Output is bounded to 8 KiB.

Batch mode runs up to four summaries concurrently before checks start, so
the stall is bounded by `ceil(N/4) × timeout` for a batch of N. Keep the
timeout below `poll-interval`, or disable summaries to avoid that stall.

## Hooks

A `target` may configure post-land hooks (`internal/hooks`): ordered
commands run against the *landed* tree once a candidate merges onto that
target — a notification, a cache warm, a small deploy step, whatever your
repo's scripts do. A hook is "run this command and tell me if it failed",
full stop.

Hooks are for **reactions to a landing**. Deployment as an ongoing concern —
multiple environments, arbitrary revisions, promotion, re-deploy — is
[deployment configuration](deployment.md), and is where a `hook "deploy"` should
move once it grows past "fire and forget on this landing" .

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
  [check reference](environment.md#check-environment-reference)) —
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
  the configured log retention covers hook logs for free, no separate
  configuration needed. Every hook result is also written to the run's
  history row (`internal/history`'s `hooks` table) alongside its checks,
  and the dashboard's run page renders a "Hooks" section — same status
  chip/duration/expandable-output/"full log" link treatment a check
  gets — whenever a run actually has hook rows (omitted entirely
  otherwise). `GET /api/v1/run/{id}` and the MCP `run` tool both gain a
  `hooks` array in the same shape as `checks`.

Hooks have their own, separate cancel surface —
`POST /api/v1/hooks/cancel`, the MCP `hook_cancel` tool, or
`gauntlet hooks-cancel` (see [HTTP API](http-api.md)) — since a hook stage has no
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
