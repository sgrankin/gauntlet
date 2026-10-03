# Failure investigation

Failure review is an optional operator-owned wrapper around named, repeatable
check commands. It retains the check worker's execution slot, workspace, services,
and exact constructed revision until a final verdict is available. A model can
request another execution; it cannot return a passing check or authorize landing.

Codex CLI supplies both API-key and ChatGPT service-account authentication.
Each invocation uses an empty workspace, private temporary home, explicit model,
structured output schema, disabled shell/web tools, and a filtered environment.
No user rules, config, or MCP servers are inherited. Invalid decisions, provider
errors, cancellation, and exhausted budgets retain the failure. Shipped retry
policy also requires the configured confidence threshold; custom Rego can extend
or replace that decision. See [failure-review settings](../reference/failure-review.md)
and [policy](../reference/policy.md).
The check must actually pass; a skipped retry cannot clear a previous failure.

## Tools and evidence

`tools true` installs only Gauntlet's investigation MCP server for that invocation.
It can read file blobs, list/filter tree paths, compare approved commits, and read
recent observations for the failing check. The capability file names immutable
base, constructed-tip, original-head, and source-base objects for that run. It
contains no credentials. Git replacement objects, external diffs, text conversion,
and lazy fetch are disabled. Symlinks remain blobs rather than host paths.

Tools share limits of 24 calls, 256 KiB total output, 32 KiB per result, and five
seconds per Git read. The classification deadline bounds the whole investigation.
Truncated results are labeled; absence in a truncated result is not evidence.
Source and logs are untrusted evidence, never instructions. Codex's bounded JSON
trace lives beside the check log and follows run-log retention.

## Decisions and observed history

Decisions contain `action` (`retry`, `abort`, `abstain`), confidence, reason,
kind (`flake`, `regression`, `infrastructure`, `unknown`), suspected candidate refs,
and evidence references. Suspects are filtered against the run's actual members.
Confidence and classification are model hypotheses. Actual rerun outcomes are
recorded separately in `failure-review.db`, with bounded output and whitespace
normalized failure fingerprints. These are framework-neutral observations, not
inferred test-case identities. Retention is 30 days or 10,000 observations.

Per-check retry limits combine with an atomic, persistent run-wide retry budget.
A missing budget store never grants a retry in configured daemon operation.
The dashboard's failure-history links and `GET /api/v1/failures?check=NAME` expose
recent observations and actual retry success counts independently of confidence.

## Batch recovery

With `on-batch-red "bisect"`, an unsuccessful batch requeues without declaring
individual culprits. The next real verification uses a smaller dependency-valid
prefix, normally half the failed group. Model suspects may shorten that prefix.
A passing prefix lands its exact tested tip; remaining changes get new checks.
A red single-member run against the real target may park that revision. Neither
model confidence nor a failed group's verdict is transferred onto another group.
Interactions remain possible: removing one change and observing a pass does not
prove that another change is defective in isolation.

GitHub feedback reports ordinary failures even without a model. It adds available
model evidence as unconfirmed investigation output, then updates the durable
comment with the landing link if the revision later lands.
