# Test reports and targeted retries

Capture stable test identities so failure investigation can distinguish an
assertion from a check-wide failure. A repository can also supply a command that
reruns only failed tests or their containing shards.

```kdl
check "sql-integration" {
    command "./ci/sql-tests"
    test-results "trx"
    retry-command "./ci/sql-tests-retry"
}
```

1. The initial command writes its complete report to `GAUNTLET_TEST_RESULTS_FILE`.
2. When operator policy allows a retry, Gauntlet writes a JSON array of failed
   test IDs to `GAUNTLET_RETRY_TESTS_FILE` and invokes `retry-command`.
3. The retry script reads those IDs, chooses tests or shards, and writes a fresh
   report to `GAUNTLET_TEST_RESULTS_FILE`. Do not interpolate IDs into shell code.
4. Every selected failure must appear again as an executed test. Passing siblings
   retain their results. A missing or skipped selected test keeps the check red.

If no failed tests are identifiable, Gauntlet reruns the original command.
Without `retry-command`, it reruns the full command and requires coverage of every
previously executed test. Commands must be safe to repeat in the same workspace.
The operator's [failure-review allowlist and budgets](../reference/failure-review.md)
apply to both paths; declaring a retry command does not enable retries by itself.

## Formats and identities

| `test-results` | Identity |
|---|---|
| `json` | Repository-provided `id`; use a stable fully qualified test or shard name. |
| `junit` | Suite path, class, and testcase name, joined as `suite/path::class::name`. |
| `trx` | `UnitTestResult.testName`, including parameter values; generated GUIDs are ignored. |

The JSON format is:

```json
{"tests":[
  {"id":"billing/InvoiceTests/round_trip","status":"passed"},
  {"id":"billing/InvoiceTests/concurrent_update","status":"failed","message":"lock timeout"}
]}
```

Statuses are `passed`, `failed`, or `skipped`. Reports must contain 1–10,000 unique
tests and at most 1 MiB of input. IDs are at most 2,048 bytes and messages at most
8,192 bytes. Unsupported outcomes, duplicate IDs, missing reports, and malformed
reports fail the check. A reported failure fails even when the process exits zero;
a process failure stays red even when its report is green.

Test identities and original attempt reports persist in run history and appear
in the run API/MCP response. The dashboard summarizes test counts and attempts;
full logs retain the original failure and each retry.

## Fast recovery from observed flakes

Set `known-flake-retries 1` inside operator `failure-review` to allow one
history-backed retry without asking Codex first. It shares `max-retries`, the
run-wide budget, declared-input budgets, and the Rego `retry` decision.

Gauntlet requires the exact failed-test IDs and messages to have recovered on
retry in two distinct recent runs. A later failed retry invalidates this fast
path. Checks without reports use an exact whitespace-normalized output
fingerprint. History retains 30 days or 10,000 observations. Model labels alone
never establish a known flake; ambiguous cases still go to Codex. The policy
input identifies the decision source as `history` or `codex`.
