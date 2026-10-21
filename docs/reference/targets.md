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
land in order. All modes publish the exact verified tip with a compare-and-swap;
a changed target invalidates the trial.

`max-executions` in [execution configuration](execution.md) bounds host-wide
command concurrency. `window × max-parallel` can exceed the host's capacity.

See [priorities and reordering](../guides/priorities.md) for FIFO/aging fairness,
non-preempting priority changes, and explicit suffix preemption.

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

## Adaptive admission

Enable gradual sizing within the existing `window` or `max-batch` ceiling:

```kdl
target "main" branch="main" {
    mode "speculate"
    window 12
    adaptive {
        start 2
        successes 4
    }
}
```

| `adaptive` field | Default | Contract |
|---|---|---|
| `start` | `1` | Initial depth or batch size, at least 1 and no greater than the mode's ceiling. |
| `successes` | `4` | Clean landed suites needed to grow by one, 1–1,000. |

- A clean, newly executed landed suite advances growth; a batch counts once.
- A recovered retry resets the clean streak and holds the limit. Reused evidence
  and emergency validation waivers do not train growth.
- Failed validation halves the limit, down to one. Invalidated trials with
  discarded execution do the same. Matching retained verification is not waste.
- Local execution saturation or a remote backlog older than one lease temporarily
  caps new admission at `start`. Existing trials continue; bounds never cancel
  active work. Pending remote checks still wake pools from zero.
- Learned limits, counters, and a bounded feedback deduplication ledger persist
  with queue controls. A smaller configured ceiling immediately caps new work.
- The target page, HTTP status and MCP expose `scheduling`: learned limit,
  ceiling, current admission cap/capacity pressure, clean streak, landed/retried/failed/invalidated counts, execution cost
  estimates and the last adjustment reason.

Cost estimates use completed command CPU time when measured, otherwise command
elapsed time. Cancelled ordinary checks contribute elapsed time up to cancellation;
this can include remote waiting. Shared/reused verification is excluded from waste.
These counters describe this governor's observed trials, not a cloud billing meter.
Without an `adaptive` block, admission uses the fixed configured limit.
