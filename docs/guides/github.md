# GitHub PRs and stacks

## GitHub admission and stacks

Enable admission explicitly; the existing `github` block alone continues
to provide status reporting without importing PRs:

```kdl
github "acme/widgets" {
    token-env "GITHUB_TOKEN"
    pull-requests {
        bot "gauntlet"
        approvals 0
        require-resolved-conversations true
        require-check "lint"
    }
}
target "main" {
    branch "master"
    landing "squash"
    mode "batch"
    max-batch 4
}
```

Under shipped Rego policy, a whole comment from someone with write, maintain,
or admin permission is a queue request. Custom [policy](../reference/policy.md)
can extend or replace authorization and readiness. The last authorized recognized command on each PR wins. Denied commands cannot
supersede an earlier authorized request. Commands are
replayed in comment order across the stack:

| Command | Meaning for A → B → C |
|---|---|
| `@gauntlet merge` on B | Reject while A is open; never implicitly request A |
| `@gauntlet merge stack` on B | Request A and B; both must be ready; C is excluded |
| `@gauntlet merge-stack` | Request the entire stack; all must be ready |
| `@gauntlet merge-ready` | Request the ready prefix from the bottom |
| `@gauntlet merge-prefix 2` | Request the bottom two unlanded PRs; both must be ready |
| `@gauntlet check` | Explain command authorization and current readiness without enqueueing |
| `@gauntlet cancel` | Withdraw this PR's request; its dependents cannot run without it |

`gauntlet land-pr -config gauntlet.kdl -pr 123` posts the same request.
Use `-stack` to request the stack through that PR, `-whole-stack` for the entire
stack, or `-ready`, `-prefix N`, and `-cancel` for the other commands.
Plain `merge` is rejected when a prerequisite PR remains open, even if it has
an independent queue request. The bot posts an explanation once per rejected
request; other PRs can still enter the queue. Closed prerequisites require
actual target landing evidence before their successor can enter.
The CLI caller's GitHub identity must have the required permission too.
For a stack-wide request, cancel on the PR that carries that request to
withdraw it; a separate later request may admit prerequisites again.

Native GitHub stacks are read from the stack API in bottom-first order.
Existing PRs based on other PR branches also work, provided the parent is
unambiguous and the stack has one successor per member when requesting the
whole stack. The bottom PR must target a configured branch. Fork heads are
fetched through the base repository's `refs/pull/<number>/head`.

The queue records explicit prerequisite slots and applies each child's
original base-to-head delta onto the preceding *normalized* commit. It
therefore does not rely on the original parent SHA surviving the squash.
A missing or parked prerequisite blocks its children. A closed predecessor
satisfies a dependency only if its original head is reachable from the
target, or its Gauntlet landing is proven in target history. An abandoned
PR or forged comment cannot satisfy a dependency. Rebase a stack whose
current parent revision is no longer an ancestor of its child.

Readiness requires a non-draft, open PR, the configured number of approvals
from repository writers **on the current head**, no outstanding writer
change request, and the configured status/check names. Checks accept
success, neutral, or skipped completed check runs; statuses require success.
Status history is paginated and the latest result per context wins. When
multiple apps or both APIs publish a required name, every matching producer
must be green; a passing result cannot hide another producer's failure.
Approval/readiness, head, title, description, and request identity are
rechecked immediately before the target CAS. Gauntlet then runs its normal
repo-defined verification graph on the constructed history.

This explicit policy does **not** reproduce every GitHub ruleset or CODEOWNERS
rule. Configure the admission policy deliberately and reserve target pushes
for the bot. The bot needs repository contents read/write, pull requests
read/write, issues read/write (comments), checks/statuses read, and access to
collaborator permissions; existing status/trial/receipt features may need
additional write permissions. Git transport must also be authenticated
(the existing App token transport works, as do configured Git credentials).

`approvals` defaults to one; set it to zero to require no positive approval.
A trusted reviewer's outstanding changes-requested review still blocks admission.
`require-resolved-conversations` defaults to false. When enabled, every review
thread must be resolved, including outdated threads. Issue comments without a
review thread are not resolution gates. Gauntlet reads all pages through GitHub's
GraphQL API and checks again before landing; permission errors, partial GraphQL
errors, or unavailable thread data block the operation rather than bypassing it.
This works independently of the approval count and configured required checks.
GitHub Enterprise uses its `/api/graphql` endpoint alongside REST `/api/v3`.

## Polling and optional webhooks

Queue ticks and GitHub admission refreshes have separate cadences. Without
webhooks, `pull-requests { poll-interval "30s" }` is the default. Successful
admission snapshots are reused between refreshes; an expired snapshot never
hides an API error. Validation immediately before landing always reads fresh
GitHub state. Requests and landing acknowledgements also invalidate the cache.

Refreshes list PR metadata but only read comments on open PRs and closed stack
prerequisites. Native stack membership is fetched once per stack per refresh;
a closed root's request survives even when its PR no longer reports a stack
field. Unrelated closed PR comment histories are not scanned. Large repositories
may still need incremental metadata intake or conditional API requests.

Webhooks are optional refresh hints, with periodic polling as recovery:

```kdl
dashboard "127.0.0.1:8080"
github "acme/widgets" {
    token-env "GITHUB_TOKEN"
    pull-requests {
        bot "gauntlet"
        webhook-secret-env "GITHUB_WEBHOOK_SECRET"
        poll-interval "5m"
    }
}
```

With `webhook-secret-env` set, the default fallback interval is five minutes;
an explicit `poll-interval` overrides either default (minimum one second).
The configured environment variable must be set at daemon startup and is
removed from candidate check environments.

Set a GitHub App's webhook URL, or a repository webhook URL, to
`https://<your-ingress>/hooks/github`, choose JSON, and configure the same random
secret on GitHub and in the daemon environment. Subscribe to pull requests,
issue comments, pull request reviews, review threads (`pull_request_review_thread`
for resolution changes), check runs/suites, statuses, and pushes.
Reverse-proxy that endpoint to the dashboard listener; expose only that route
publicly, keeping the existing dashboard, admin API, and MCP access private.
The daemon listener remains HTTP behind the HTTPS ingress.

Deliveries require a valid SHA-256 HMAC and the configured repository. Bodies
are bounded to 2 MiB. A valid delivery invalidates admission and coalesces an
early queue tick; it does not execute commands from the payload, authorize a
landing, or call GitHub before acknowledging. Duplicate and reordered deliveries
are harmless because the next poll reads current state. Missed deliveries and
restarts are recovered by polling, so delivery IDs need no persistent ledger.
Configure the webhook on GitHub separately; enabling the KDL setting does not
register one with GitHub.

## Outcomes

On success, the bot posts the exact landed commit link and closes the PR. GitHub
reports it as closed, not natively merged. Failures receive check context and
bounded model feedback when configured. See [completion experiments](../architecture/reviews.md#github-completion-experimental-evidence) and [incident commands](incidents.md).
