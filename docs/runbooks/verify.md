# Runbook: gauntlet acceptance checklist

Run this after ANY deploy (fresh install, upgrade, or config change) to
confirm the daemon is actually healthy — not just running. Each check names
a command and the expected output shape. Replace `<DASHBOARD_URL>` with
wherever the dashboard binds (`http://localhost:8080` in both other
runbooks — always pass it explicitly, since the `gauntlet status` CLI's own
default, `http://localhost:8899`, is a different port).

Substitute `<TARGET>` with a real target name from your `gauntlet.kdl`, and
`<CHECK_BUILDER_IMAGE>` etc. with your actual config values.

## 1. Daemon boots quiet

```sh
journalctl -u gauntlet -n 50 --no-pager    # systemd (Linux)
# or, foreground/launchd: tail the terminal/log file
```

**Expect:** startup lines (config loaded, remote reachable, targets
reconciling), no fatal error, no repeated crash-loop timestamps a few
seconds apart.

## 2. Status API responds

```sh
curl -s -o /dev/null -w '%{http_code}\n' <DASHBOARD_URL>/api/v1/status
```

**Expect:** `200`. (`503` briefly right after startup, before the first
reconcile pass completes, is normal — retry once.)

```sh
curl -s <DASHBOARD_URL>/api/v1/status | jq '.targets[] | {name, branch, tip}'
```

**Expect:** one object per configured `target`, each with a non-empty `tip`
SHA.

## 3. A candidate lands end-to-end

```sh
git push origin HEAD:refs/heads/for/<TARGET>/<YOU>/verify-$(date +%s)
```

Poll status until the ref disappears from `queue`/`current` (landed) or
appears in `parked` (red):

```sh
watch -n 5 "curl -s <DASHBOARD_URL>/api/v1/status | jq '.targets[] | select(.name==\"<TARGET>\")'"
```

**Expect:** the ref moves `queue` → `current` → gone (landed) within a few
poll intervals. Confirm the land:

```sh
git -C <REPO_CHECKOUT> fetch origin <TARGET> && git -C <REPO_CHECKOUT> log --first-parent -1 origin/<TARGET>
```

**Expect:** a `--no-ff` merge commit whose message carries
`Gauntlet-Ref:` / `Gauntlet-Run:` trailers.

## 4. A red run parks, and retry/`:recycle:` un-parks it

Push a candidate whose check spec fails (or reuse one you know is red).

```sh
curl -s <DASHBOARD_URL>/api/v1/status | jq '.targets[] | select(.name=="<TARGET>") | .parked'
```

**Expect:** the ref listed under `parked`, with a non-empty `reason`.

Retry it via API (or react `:recycle:` on the run's Slack root message, if
Slack is configured):

```sh
curl -s -X POST <DASHBOARD_URL>/api/v1/retry \
  -H 'content-type: application/json' \
  -d '{"target":"<TARGET>","ref":"refs/heads/for/<TARGET>/<YOU>/<TOPIC>"}'
```

**Expect:** `202 {"status":"queued"}`, and the ref disappears from `parked`
back into `queue`/`current` on the next status poll.

## 5. Services pool populates (only if `services` is configured)

```sh
curl -s <DASHBOARD_URL>/api/v1/services | jq .
```

**Expect:** (before any check with `needs` has run) `200` with an empty or
absent instance list — not `503 {"error":"services disabled"}`, which would
mean the `services { allow "container" }` block isn't wired up. After a
check with `needs "mssql"` (or similar) has run at least once:

```sh
curl -s <DASHBOARD_URL>/api/v1/services | jq '.[] | {image, endpoint, age, refcount, hits}'
```

**Expect:** one entry per declared service, `hits` incrementing across
repeated runs (confirms reuse, not a fresh container every time).

## 6. Auto-retry on infra error is visible

Hard to force deliberately — the practical check is retrospective: after
running for a while, look for an `OutcomeError` park (executor unreachable,
a service failing to start) followed by an automatic re-queue with no
operator action:

```sh
curl -s '<DASHBOARD_URL>/api/v1/runs?target=<TARGET>&limit=20' | jq '.[] | select(.outcome=="error")'
```

**Expect:** if any appear, each `(ref, sha)` pair has at most one
unattended error park before either a subsequent run or a human retry —
i.e. it wasn't stuck waiting silently. If `auto-retry-errors false` is set,
skip this check — parks wait for an operator by design in that mode.

## 7. Dashboard is reachable

```sh
curl -s -o /dev/null -w '%{http_code}\n' <DASHBOARD_URL>/
```

**Expect:** `200`. Open it in a browser and confirm the target list and at
least one run render (visual spot-check, not scriptable).

## 8. MCP endpoint answers

```sh
claude mcp add --transport http gauntlet-verify <DASHBOARD_URL>/mcp
claude mcp list
```

**Expect:** `gauntlet-verify` listed as connected. Or, without a Claude
Code client handy, confirm the endpoint at least speaks HTTP (a bare GET
returns a protocol-level response, not a connection refused):

```sh
curl -s -o /dev/null -w '%{http_code}\n' <DASHBOARD_URL>/mcp
```

**Expect:** any HTTP response code (`400`/`406` are typical for a bare GET
against a Streamable-HTTP MCP endpoint) — not a connection error.

## 9. GitHub commit status (if configured)

```sh
curl -s "https://api.github.com/repos/<OWNER>/<REPO>/commits/<CANDIDATE_SHA>/status" \
  -H "Authorization: Bearer <GITHUB_PAT>" | jq '.statuses[] | select(.context | startswith("gauntlet/"))'
```

**Expect:** a `gauntlet/<TARGET>` context with `state` matching the run's
actual outcome (`success`/`failure`/`pending`).

## 10. Slack channel (if configured)

Push a candidate, confirm in the channel:

**Expect:** a threaded root message appears once the trial merge is clean;
per-check replies thread under it; the root edits to ✅/❌ on completion.
Then react `:recycle:` on a finished root and confirm a fresh run starts
threaded under the SAME root (not a new message) with no new push.

---

If any check fails, cross-reference [deploy-linux.md](deploy-linux.md) or
[deploy-macos-dev.md](deploy-macos-dev.md)'s phase that configures the
failing surface, and [deploy.md](../deploy.md) for the full rationale.
