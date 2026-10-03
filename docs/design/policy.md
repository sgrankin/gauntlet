# Operator policy

Rego is Gauntlet's decision layer. The daemon always loads shipped defaults,
which preserve its ordinary command, review, execution, deployment, and retry
behavior. Candidate-owned check specs cannot supply policy. Go collects facts
and enforces mechanics: valid graphs, supported executors, explicit stack requests,
revision binding, required receipts, signing, retention, retry budgets, and CAS.

## Named decisions

| Entry point | Evaluation |
| --- | --- |
| `command` | GitHub comment selection, Slack reaction commands, queue admin commands |
| `submission` | Candidate admission and freshly validated landing |
| `execution` | Before materializing/running candidate check or deployment graphs |
| `deployment` | Before tracked desired-ref promotion, lane admission, and observed-ref publication |
| `retry` | After a valid Codex classification, before spending a retry |

Custom queries are `data.gauntlet.<entry point>`. Each returns `allow` and
`requirements`, an array of `{name, satisfied, reason}` objects. Any unsatisfied
requirement denies even when `allow` is true. Missing or malformed decisions,
evaluation errors, and timeouts deny.

Configure `policy { rego r#"..."# }` or `rego-file "policy.rego"`, exclusively.
Files resolve beside the operator config. `extend "command" "submission"`
combines those custom decisions with their defaults; both must allow. With no
lists, a custom policy extends `submission`. `replace "command" "submission"`
makes the specified custom decisions authoritative. Other decisions keep their
defaults. Each name may occur once, in one list.

The engine compiles once before activation, shared by the adapters, queue,
deployment tracker, and retrier. `gauntlet validate` compiles the same policy.
Evaluation defaults to 100 ms, with a configurable maximum of one second.
Network, runtime, and nondeterministic builtins are unavailable. Facts collection
has separate bounds. Rego sees data, never an API client or credentials.

## Facts, schema version 1

| Field | Meaning |
| --- | --- |
| `schema_version`, `phase` | Version `1` and operation phase |
| `principal` | Transport-established `source`, `id`, `authenticated`, raw `permission`, configured `teams`, Slack `allowed_by_config` |
| `command` | `kind`, request `id`, prefix `count`, `urgent`, `reason` |
| `settings` | Approvals, required host checks, conversation resolution, emergency enablement |
| `target`, `branch`, `base_sha` | Queue target, real branch, run base |
| `candidate` | Ref, SHA, version, source, source base, dependency, requester, urgency |
| `stack` | Requested membership at forge intake; predecessor chain at queue admission; actual run members at landing |
| `checks` | Gauntlet check names and statuses; empty at admission |
| `emergency` | Independent `skip_checks` and `override_pause` flags |
| `paths`, `paths_available` | Complete changed paths when collected; renames include old paths |
| `forge` | Adapter facts; empty for Git-ref intake |
| `adapter_validated` | An adapter without the shared fact contract owns source validation; source identity is retained |
| `execution` | Graph kind, nodes (name/kind/executor/command/dependencies), services; check workspace and parallelism |
| `deployment` | Environment/mode/source, proposed desired SHA and prior observed SHA; custom rules also receive current fetched refs and source tip |
| `retry` | Classifier action, confidence/threshold, kind, check, attempt, reason, evidence |

Facts vary by entry point; unavailable fields are empty rather than fabricated.
For execution, `stack` contains the tested candidates; deployment execution uses
`deployment` for revision/environment identity. Deployment phases are `promotion`,
`admission`, and `publication`; its execution decision uses phase `execution`.
Current deployment refs mean the latest fetched view, with final publication
protected by an observed-ref CAS. A direct human push to a manual desired ref
is authenticated by Git hosting; policy gates the daemon's subsequent execution.

GitHub facts include PR number/head/state/draft, requester/request ID/permission,
latest substantive reviews keyed by login with raw reviewer permission and
current-revision status, conversation resolution, checks with producer identity,
paths, and configured team memberships. Go does not interpret review readiness
or writer permission. Shipped submission rules require an open, non-draft PR,
no trusted veto, the configured current approvals, resolved conversations when
configured, and all matching producers of each required host check to pass.
Shipped command rules require write/maintain/admin for merge/cancel, permit
readers to run `check`, and require operator enablement for emergencies.

One GitHub scan shares immutable fact snapshots between command and readiness
rules. Landing performs one fresh scan for the batch and then evaluates
`submission` with actual check results. A revoked permission, edited request,
changed revision/dependency, or newly failing readiness condition stops landing.
Custom command/submission rules collect complete paths and conversations;
incomplete lists and failed membership lookups fail closed.

Gerrit supplies current change, submit requirements, and paths to custom
submission rules, while its adapter retains native eligibility and submit
validation. Git-ref intake supplies changed paths to custom submission rules.
Branch on `candidate.source` where facts differ; avoid requiring completed
Gauntlet checks during admission, when they do not exist yet.

## Identity and diagnostics

GitHub identity comes from authenticated forge comments and permission APIs;
Slack identity comes from authenticated Socket Mode events. `Actor` is audit
text, never a principal. Control JSON cannot supply `Principal`. Admin/CLI/MCP
commands use the trusted ingress service principal by default. The embedded
HTTP handler supports `WithPrincipalResolver` for identities established by a
trusted authenticator; it does not trust arbitrary request headers. Keep the
standalone admin endpoint on its trusted interface/proxy boundary.

`@gauntlet check` reports named command and current PR readiness requirements,
without enqueueing. Denied GitHub commands receive policy feedback and cannot
supersede an earlier authorized request. The target dashboard and
`GET /api/v1/policy-decisions` expose queue decisions. The control file retains
up to 500 changed queue decisions, subject to the storage bound: policy/input
hashes, target, ref, SHA, phase, requirements, and errors, without credentials or
full review payloads. Deployment denial appears in lane/run details; retry denial
appears in check output.

`gauntlet policy-check -decision command -config gauntlet.kdl -input facts.json`
evaluates a local fixture, prints the policy hash and decision, and exits
unsuccessfully on denial. Keep fixtures alongside operator configuration;
previews do not replace fresh landing checks.
