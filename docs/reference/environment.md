# Check environment reference

Every executor (local or container) sets these environment variables before
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
- `GAUNTLET_GIT_DIR` — a git dir holding every object the SHAs above name
  (the daemon's own bare repo — the trial merge commit is created there
  whether or not it ever lands, so `GAUNTLET_MERGE_SHA` always resolves).
  Usable as `GIT_DIR` or `git --git-dir`; see ["Conditional
  execution"](../guides/conditional-checks.md#conditional-execution) below. **Read-only by contract**: the
  container executor mounts it `:ro` at the fixed path `/gauntlet-git`; the
  local executor hands you the daemon's live repo path and trusts your
  script not to write to it. Honesty note, same spirit as services' "trust,
  stated honestly": the git dir's `config` file carries the daemon's remote
  URL verbatim — if that URL embeds a credential (rather than using a
  credential helper or SSH agent, both of which keep secrets out of the
  URL), every check can read it. Local checks could already read it off
  disk; this extends that visibility to container checks too.

**Operator secrets are stripped from candidate-code environments.** The
daemon's own credential env vars — `github`'s `token-env` in static-token
mode, `slack`'s `app-token-env`/`bot-token-env`, `summarize`'s
`token-env` (config-named, never a hardcoded list — see
[daemon configuration](daemon.md)) — are removed by exact name from a candidate
command's environment on the local executor, before this contract's own
`GAUNTLET_*` variables are added. This covers every candidate-code job: an
ordinary check, an `image` build, and a `receipt` producer (see
["Receipts"](receipts.md#receipts)) alike — none of them ever needs the
daemon's own GitHub/Slack/OpenAI credentials to do its job, and a
repository's own commands are effectively attacker-controlled the moment
anyone can push a `for/` ref. **Post-land hooks are exempt**: a hook's
command comes from the daemon's own operator-written config
([config.md's "Hooks"](automation.md#hooks)), never a candidate's repo spec,
and legitimately uses these same credentials — a deploy hook driving `gh`,
say. **Container profiles never had host env in the first place** and so
need no such filter: the container executor only ever passes explicit
`NAME=VALUE` pairs into the container (its fixed profile `env`, this
contract's own `GAUNTLET_*` set, and resolved `needs` env), never the
daemon's own ambient environment. This is a by-exact-name filter over the
*command's own environment*, not a sandbox: it closes the ordinary,
by-design channel (a candidate command's own `os.Environ()`), but it does
not change what a same-UID process can see through other means (e.g.
reading another process's environment off `/proc` on a platform that
allows it) — the same own-code threat model the rest of the local
executor already runs under (see [security](../operations/security.md)).

A check that declares `needs` (see ["Shared services"](services.md#shared-services)
below) additionally gets one pair per resolved service:

- `GAUNTLET_SVC_<NAME>_HOST` / `GAUNTLET_SVC_<NAME>_PORT` — where to reach
  the service (`<NAME>` is the service's declared name, upcased,
  non-alphanumerics turned into `_`). Absent entirely for a check with no
  `needs`, and for hooks (which can't declare `needs` at all).

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
link and the JSON API/MCP `logPath`/`logUrl` fields point at (see
[HTTP API](http-api.md)) — the dashboard decompresses it on the fly when serving;
it's pruned after `log-retention` (default 30 days, see
[daemon configuration](daemon.md)) regardless of whether history or the dashboard are
configured.

Post-land hooks (see [config.md's "Hooks"](automation.md#hooks)) get the
identical treatment: each hook's full log lands at
`<state>/logs/<runID>/hook-<n>-<sanitized name>.log.zst` — inside the
*same* run directory its checks' logs already live in, so it's covered by
the exact same retention sweep and served through the exact same
`GET /run/{id}/log/{name}` route, with no separate configuration.
To read one offline: `zstd -d <path>` (or `zstd -dc <path> | less`).
