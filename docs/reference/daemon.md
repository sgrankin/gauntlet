# Daemon configuration

The operator-owned `gauntlet.kdl` selects a remote, targets, execution profiles,
and optional integrations. Pass it with `-config`. The repository-owned
[check spec](checks.md) describes candidate validation; it cannot change operator
credentials, mounts, or policy.

```kdl
remote "git@github.com:acme/widgets.git"
poll-interval "10s"
check-spec ".gauntlet.kdl"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
```

## Core settings

| Node | Default | Contract |
|---|---|---|
| `remote` | Required | Git URL to fetch and push. App authentication requires matching HTTPS. |
| `poll-interval` | `"10s"` | Positive duration between queue reconciliations. GitHub intake has a separate refresh interval. |
| `check-spec` | `".gauntlet.kdl"` | Check definition read from the exact trial tree. |
| `committer` | Required | Name and email for generated commits. Source author is preserved. |
| `target` | At least one | Target name and branch; see [targets](targets.md). |
| `shutdown` | `"drain"` | `drain` finishes admitted work and queued hooks; `kill` cancels immediately. A second signal forces shutdown. |
| `log-retention` | `"720h"` | Positive retention for full check logs. |
| `source-retention` | `"720h"` | Positive retention for unused source archives and review-cache refs. Active work holds leases. |
| `emergency-merges` | `false` | Enables explicit validation waivers; see [incident controls](../guides/incidents.md). |
| `auto-retry-errors` | `true` | One automatic infrastructure retry per ref/SHA in a daemon lifetime. Never retries a red verdict by itself. |

## Configuration map

- [Targets and scheduling](targets.md): landing mode, batching, speculation.
- [Execution](execution.md): profiles, capacity, mounts, services, export timestamps.
- [History, dashboard, and telemetry](observability.md).
- [GitHub and Gerrit](github.md): admission, credentials, trial refs, receipt notes.
- [Summaries and hooks](automation.md): Codex messages and post-land commands.
- [Failure review](failure-review.md): bounded Codex classification and retries.
- [Incident configuration](incidents.md): infrastructure breaker settings.
- [Policy](policy.md): Rego rules at command, admission, execution, and publication gates.
- [Signing](signing.md) and [deployment](deployment.md).

See [Slack setup](../guides/slack.md) for channel configuration.
Run `gauntlet validate -config gauntlet.kdl` after editing and
`gauntlet doctor -config gauntlet.kdl` on the intended host.
