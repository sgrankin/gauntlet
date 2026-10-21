# Priorities and reordering

Use priority for a hotfix that can wait for current verification; use explicit
preemption when it must move ahead of active work.

| Control | Effect |
|---|---|
| `low`, `normal`, `high`, `urgent` | Select waiting work by class, then FIFO. Existing runs continue. |
| Move before/after | Reorder within a priority class. Reject positions that put a child before its prerequisite. |
| Preempt | Cancel and rebuild the affected pipeline suffix. Require explicit confirmation and a reason. |
| Urgent reserve | Keep configured execution slots available for urgent work. |

Requested prerequisites inherit their descendants' priority. Aging promotes low
and normal work one class every 30 minutes, up to high. An explicit priority
change restarts its aging clock. After three urgent
landings, eligible ordinary work gets a turn. Raising priority does not request
additional PRs, retry parked failures, bypass checks, or override a pause.

## Dashboard

1. Open the target and select the requested prefix.
2. Set its priority, or enter an anchor ref and choose **before** or **after**.
3. Enable **Preempt** only when cancelling active work is intended. Priority
   preemption requires `urgent`; a move can preempt within its existing class.
4. Review the proposed order, affected runs, check counts, and recorded execution
   time. Confirm to apply. A changed queue requires a new preview.

Matching [declared-input verification](reusable-checks.md) survives reconstruction.
SHA-bound checks rerun when the constructed commit changes. Recorded time is
context for the decision, not a prediction of wasted compute.

## GitHub

```
@gauntlet merge high
@gauntlet merge stack urgent
@gauntlet merge stack urgent preempt -- unblock CI
```

`preempt` requires `urgent` and a reason. Each request is applied once, including
after a scheduler restart; another preemption requires a new or edited request.
Stacks still require an explicit stack command and readiness of the selected
prefix. GitHub's command and submission policy both apply.

## API and CLI

Read exact refs, SHAs, and versions from the target's status. Preview without
changing the queue:

```sh
gauntlet control -target main -reason 'unblock CI' \
  -priority urgent -preempt -preview \
  -revisions '[{"ref":"refs/heads/for/main/alice/fix","sha":"FULL_SHA","version":""}]' \
  priority
```

Apply the same command without `-preview`, adding `-plan-token TOKEN` from the
preview to reject a changed queue. For moves, use `-before REF move` or
`-after REF move` (flags precede the positional command).

The HTTP endpoints are `POST /api/v1/control/preview` and
`POST /api/v1/control`. Controls require an actor, reason, and exact revisions.
Admin ingress supplies the authenticated principal; an actor label is only audit
context. Rego receives `input.command.priority`, `preempt`, `before`, and `after`.
It may authorize priority changes separately from preemption. For example, add
this requirement to a custom command policy:

```rego
preemption_ok if not input.command.preempt
preemption_ok if input.principal.permission == "admin"
```

See [policy examples](policy.md) for the complete decision envelope.
