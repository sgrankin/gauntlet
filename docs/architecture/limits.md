# Known limits

## Review hosts

- GitHub landing is a direct push of the tested commit. PR completion uses a
  comment and close; it does not receive GitHub's native merged state or recorded
  merge SHA. See [the experiments](reviews.md#github-completion-experimental-evidence).
- Webhooks accelerate GitHub discovery; periodic reconciliation remains necessary
  for lost deliveries, retries, and readiness changes.
- Direct target pushes require compatible forge permissions and branch rules.
  Gerrit submission is constrained by its server's policy and submit behavior.

## Operations

- The dashboard, HTTP API, and MCP have no built-in authentication. Place them
  behind trusted ingress before exposing operator commands.
- Run, check, and hook history rows and receipt notes have no automatic retention.
  Logs and unused local sources do. See [storage](../operations/storage.md).
- A single daemon owns a configured remote's queue. There is no distributed
  leader election or multi-writer scheduling protocol.
- Container mounts and local executors execute candidate code with the selected
  host authority. Containers alone are not a sandbox for hostile contributors.

## Validation and models

- Services support container drivers only, with compatible Docker/Podman networks.
- Failure classification uses Codex and bounded retry budgets. It can recommend
  likely suspects, not establish causality or waive validation automatically.
- Integration fakes cover deterministic adapter behavior. Live provider permissions,
  webhook ingress, container networking, and deployment credentials still need
  verification on the intended host.
