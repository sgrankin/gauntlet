# Landing changes

## Git ref submissions

Push a candidate using `for/<target>/<user>/<topic>`; the shorter
`for/<target>/<topic>` form omits the user. The target is a configured queue
name, which may differ from the destination branch.

```sh
git push origin HEAD:refs/heads/for/main/alice/widget-fix
```

With jj, create and push a bookmark with the same name. The source's raw
`change-id` header survives normalization; a squashed multi-commit submission
retains the head's identity.

## Review submissions

- [GitHub](github.md): comment `@gauntlet merge` on a standalone PR. For a stack,
  explicitly request `@gauntlet merge stack`, a ready prefix, or a fixed prefix.
- [Gerrit](gerrit.md): use Gerrit's submit/readiness model. One change becomes
  one target commit.

Normal landing requires configured policy, forge readiness, and passing
Gauntlet validation. Host checks and Gauntlet's check graph are separate gates.
The queue owns the target; protect it against concurrent human or bot pushes.

## Retry or cancel

A red verdict or real-base conflict parks that revision. Pushing a new SHA
clears the old park. Infrastructure errors receive one automatic retry by
default; [failure review](failure-review.md) can retry selected flaky checks
within a run.

```sh
gauntlet retry -target main -ref refs/heads/for/main/alice/widget-fix
gauntlet cancel -target main -ref refs/heads/for/main/alice/widget-fix
```

The dashboard provides the same actions. Slack run messages accept `:recycle:`
and `:x:` when enabled. Retry/cancel target the ref's current revision; use
[incident controls](incidents.md) for requests bound to an explicit revision
prefix, priority, or a validation waiver.
