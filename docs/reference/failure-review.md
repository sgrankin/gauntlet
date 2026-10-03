# Failure review configuration

## Failure review

Absent `failure-review` disables model classification and check retries. Enable
it only for explicitly named checks whose commands are safe to repeat:

```kdl
failure-review {
    auth "api-key"
    model "YOUR_OPENAI_MODEL"
    token-env "OPENAI_API_KEY"
    checks "sql-integration" "browser-tests"
    max-retries 1
    min-confidence 0.8
    timeout "30s"
    max-output-bytes 16384
}
```

`model` and `checks` are required. `auth` defaults to `api-key`; the other shown
values are defaults. `max-retries` permits 1–3 extra executions per check per
queue run. `max-run-retries` (default 3, range 1–20) adds a persistent shared
budget across all selected checks in one run. `timeout` bounds each classification (positive, at most 5 minutes),
not the check command. `max-output-bytes` limits the output tail sent to the
model (256–65536 bytes). `api-url` defaults to `https://api.openai.com/v1` and
must identify a Responses API endpoint base; use HTTPS for remote credentials.
Both authentication modes require Codex CLI. API keys use an isolated temporary
Codex credential file. There is no separate direct HTTP classification path.
There is no automatic retry on provider errors.

For a ChatGPT workspace service account, create a Codex-scoped access token
and supply it through the daemon's environment:

```kdl
failure-review {
    auth "chatgpt"
    model "YOUR_CODEX_MODEL"
    token-env "GAUNTLET_CODEX_TOKEN"
    codex "/usr/local/bin/codex"
    checks "sql-integration"
    max-retries 1
    min-confidence 0.8
    timeout "60s"
}
```

`token-env` defaults to `CODEX_ACCESS_TOKEN` in ChatGPT mode; `codex` defaults
to `codex` on PATH. Install a recent Codex CLI with service-account access-token
support and the `exec --ignore-user-config`, `--ignore-rules`, `--ephemeral`, and
`--output-schema` options. Service-account tokens require CLI 0.142.0 or later;
use a current release for these isolation options. Choose a model available to
that account. Gauntlet passes the token as `CODEX_ACCESS_TOKEN` only to the
classifier process; it does not store a browser login or scrape ChatGPT endpoints.
See [OpenAI's service-account setup](https://developers.openai.com/codex/enterprise/service-accounts).

The configured token variable is withheld from local candidate commands, as
with Gauntlet's other configured operator credentials. This environment filtering
is not a security sandbox for same-user local code. `gauntlet doctor` checks the
configured token variable and executable without making a billable model call.
Authentication, model availability, and live provider behavior are checked when
an actual selected check fails.

Under shipped Rego policy, only `retry` above the confidence threshold triggers
another execution. Custom retry policy can replace this decision; hard budgets
and actual passing verification remain required. `abort`
and `abstain`, low confidence, invalid output, or an unavailable model keep the
failure. A rerun must pass to unblock the queue. Confidence is model-reported,
not calibrated. Retries preserve the same tested commit, workspace, services,
and successful sibling checks, avoiding a speculation bubble or batch fallback
when a flaky check recovers. Decisions and every attempt appear in check output
and full logs. Classifications hold the check's execution slot, so the existing
`max-executions` cap also bounds concurrent model calls. See the
[failure-review design](../guides/failure-review.md) for the detailed contract.

### Investigation tools and history

Set `tools true` inside `failure-review` to expose bounded, read-only Git object,
path, diff, and failure-history tools to Codex. Shell and web tools stay disabled.
The model sees the run's actual member identities and source bases. Classification
can report failure kind, suspects, and evidence, all labeled as hypotheses.
[The investigation contract](../guides/failure-review.md) defines limits and isolation.

`failure-review.db` keeps 30 days or 10,000 observations. The failed-run page links
to history for each check; observed retry passes and model confidence are separate.
`GET /api/v1/failures?check=NAME` returns up to 20 recent observations. Investigation
traces live beside run logs and use existing log retention. No model call is needed
for ordinary GitHub admission/failure feedback.
