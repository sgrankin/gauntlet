# Targets and scheduling

```kdl
target "main" branch="main" {
    landing "squash"
    mode "batch"
    max-batch 8
    on-batch-red "bisect"
}
```

| Field | Default | Values and meaning |
|---|---|---|
| Target argument | Required | Name used by submission refs, commands, and status contexts. |
| `branch` | Required | Remote destination branch. |
| `landing` | `"squash"` | One single-parent commit per submission. `"merge"` creates two-parent commits for ref submissions; review adapters require squash. |
| `mode` | `"serial"` | `serial`: one candidate at a time. `batch`: one suite for a combined chain. `speculate`: overlapping suites on predicted predecessors. |
| `max-batch` | `8` | Batch only; 1–64 members. |
| `on-batch-red` | `"serial"` | Batch only; `serial` retests individually, `bisect` tests smaller dependency-valid prefixes. |
| `window` | `4` | Speculate only; 1–32 in-flight runs. |

A batch of two PRs produces two commits and one target push. Speculative runs
land in order. All modes publish the exact tested tip with a compare-and-swap;
a changed target invalidates the trial.

`max-executions` in [execution configuration](execution.md) bounds host-wide
command concurrency. `window × max-parallel` can exceed the host's capacity.

## Commit messages

Squash targets use the source head's message, the GitHub PR title/body, or the
Gerrit patch-set message. The source author and jj `change-id` header are retained.
A multi-commit PR becomes one commit; only its head's jj identity survives.

For `landing "merge"`, `merge-message` is a Go text template with `.Topic`,
`.User`, `.Ref`, `.RunID`, and `.Target`. Its default is `Merge {{.Topic}}`
with ` ({{.User}})` when a user is present. [Summaries](automation.md) may add a
body. Configured templates render literally, including an empty `.User`.
Gauntlet appends provenance trailers in both modes.

See [landing changes](../guides/landing.md) for submission and retry commands,
and [queue modes](../architecture/queue-modes.md) for scheduling and recovery.
