# Linear landings and forge reviews

Loaded daemon configurations now default to `landing "squash"`: one
single-parent commit per queue submission, GitHub PR, or Gerrit change.
Serial, batch, and speculate still construct the predicted target history,
verify it, and compare-and-swap the **exact tested tip** onto the target.
A changed target invalidates the work and causes a new trial. A batch of two
PRs produces two commits and one target push. `landing "merge"` retains the
old behavior for migration; review adapters require squash landings.

The target name can be `master`, `main`, or any configured branch. Protect it
so only the queue can push. The adapters do not use the host's merge button:
its commit generation cannot preserve the queue's tested SHAs.

## Commit identity and recovery

GitHub commits use the PR title (with its number) as the subject and the PR
body as the message body. Gerrit commits preserve the patch-set message,
including its `Change-Id` trailer. Ordinary ref submissions use the head
commit's message. Each keeps the head commit's original author and author
date, with Gauntlet as committer. Signatures are dropped because the new
parents and message invalidate them.

The raw Git `change-id` header written by jj is retained from the source
head. A multi-commit PR has only one resulting commit, so its head's jj
identity wins; the other changes do not get separate landing identities.
This header is distinct from Gerrit's `Change-Id` message trailer. GitHub
has no documented API contract that associates a PR through either header.

`Reviewed-on`, `Gauntlet-Ref`, `Gauntlet-Source`, `Gauntlet-Version`, and
`Gauntlet-Run` trailers identify review, input revision, metadata snapshot,
and verification run. Recovery searches the target's first-parent history
and parses trailers rather than trusting editable bot comments. Once a
review revision landed, later message/request edits do not make it land
again while host acknowledgement is retried. Before landing, those edits
invalidate the trial.

Original objects are also retained locally under
`refs/gauntlet/source/<sha>` for audit and delayed hook access. These refs
are never pushed and currently have no automatic retention limit. The
linear target itself does not reach the original junk history.

## GitHub admission and stacks

Enable admission explicitly; the existing `github` block alone continues
to provide status reporting without importing PRs:

```kdl
github "acme/widgets" {
    token-env "GITHUB_TOKEN"
    pull-requests {
        bot "gauntlet"
        approvals 1
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

A whole comment from someone with write, maintain, or admin permission is a
queue request. The last recognized command on each PR wins. Commands are
replayed in comment order across the stack:

| Command | Meaning for A → B → C |
|---|---|
| `@gauntlet merge` on B | Request A and B; both must be ready |
| `@gauntlet merge-stack` | Request the entire stack; all must be ready |
| `@gauntlet merge-ready` | Request the ready prefix from the bottom |
| `@gauntlet merge-prefix 2` | Request the bottom two unlanded PRs; both must be ready |
| `@gauntlet cancel` | Withdraw this PR's request; its dependents cannot run without it |

`gauntlet land-pr -config gauntlet.kdl -pr 123` posts the same request.
Use `-stack`, `-ready`, `-prefix N`, or `-cancel` for the other commands.
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

Polling requires no webhook ingress. This first adapter scans PR/comment
history so stack-wide requests survive root closure and daemon restarts;
that can consume substantial API quota in repositories with long histories.
Review parks are re-evaluated on restart because persisted park records
currently omit the review metadata version. Commands waiting on admission
have no separate acknowledgement comment;
trial/status events start once admitted. Webhook-backed intake and detailed
blocked-request feedback remain follow-up work.

## GitHub completion: experimental evidence

The October 2, 2026 experiment used isolated branches in
`sgrankin/gauntlet`, leaving `main` untouched:

- [PR #16](https://github.com/sgrankin/gauntlet/pull/16): normal native squash control.
- [PR #17](https://github.com/sgrankin/gauntlet/pull/17): pre-pushed the intended squash, then requested a native squash merge.
- [PR #18](https://github.com/sgrankin/gauntlet/pull/18): did the same after an unrelated target advance.

Both already-applied cases created an **extra empty commit**. GitHub recorded
that new commit as `merge_commit_sha`, rather than associating the prebuilt
commit. Its REST merge endpoint accepts an expected PR head but offers no
input for an existing landing commit or expected target tip. Native stack
merge requests likewise construct their own commits.

A second experiment tested rewriting the actual source branch to the tested
squash **before** pushing the target:

- [PR #22](https://github.com/sgrankin/gauntlet/pull/22) became natively merged, with the exact tested SHA and a merged timeline event.
- [PR #23](https://github.com/sgrankin/gauntlet/pull/23) stayed open after a real stale `--force-with-lease` target push. Restoring the original source ref and rebuilding on the new target then produced a native merge of the new tested SHA.
- Native stack [PR #24](https://github.com/sgrankin/gauntlet/pull/24) / [PR #25](https://github.com/sgrankin/gauntlet/pull/25): only the bottom PR became merged. The upper PR stayed open against the lower source branch. GitHub rejected retargeting it with “Cannot change the base branch because the pull request is part of a stack.”
- [PR #27](https://github.com/sgrankin/gauntlet/pull/27): pushing the target first and rewriting the source afterward closed the PR **without** a merged event or merged state.

This proves indirect association is possible for standalone PRs with
pre-CAS source normalization, but not a complete solution for native stacks.
The sequence introduces a second remote mutation before landing: it needs
CAS on the source ref, retained originals, guarded rollback after target
failure, and crash recovery before any retry. PR head metadata also lagged
ref writes in the probe. Approval dismissal could not be established with
one identity (GitHub rejects self-approval); comment reviews remained tied
to the original SHA. Fork write permissions were not tested. There is no
production source-rewrite mode in this implementation.

The shipped adapter posts an idempotent landing comment with the exact
commit and original revision, then closes the PR. The commit links back
through `Reviewed-on`. It preserves contributor branches. If the author
repushed during host acknowledgement, it leaves the newer revision open.
The PR is **closed, not marked merged**: there is no native merged event,
merged badge, or guaranteed `associatedPullRequests` relationship. Consumers
should use the landing comment, commit trailers, queue events, and receipts.
Issue-closing behavior on the default branch and emitted webhooks were not
verified by these isolated-branch experiments.

A live run of this adapter against native stack
[PR #19](https://github.com/sgrankin/gauntlet/pull/19) /
[PR #20](https://github.com/sgrankin/gauntlet/pull/20) landed exactly two
single-parent commits in one batch, kept both original source heads intact,
and closed both PRs with landing links. Temporary branches were removed;
`main` remained unchanged. Gerrit has fixture coverage, not a live run.

Rust's [Homu](https://github.com/rust-lang/homu) and
[Bors](https://bors.tech/documentation/) provide useful precedents for
permission-checked approval comments, rollups/batches, and checking against
the branch state about to land. The familiar bot-command UX follows that
pattern. We found no public specification sufficient to claim compatibility
with Stripe's internal merge bot.

## Gerrit

Gerrit already has the desired durable identity: a change has multiple patch
sets, each a full Git commit, and normally lands as one commit. Gerrit is
not universally "always rebased"; the project submit strategy controls that.
Gauntlet deliberately normalizes to a single-parent landing commit instead.

```kdl
gerrit "https://review.example.com" {
    project "widgets"
    username-env "GERRIT_USERNAME"
    token-env "GERRIT_TOKEN"
    verification-requirement "Verified"
}
```

Use a remote for that Gerrit project, configured squash targets, and
authenticated Git push access in addition to REST credentials. GitHub PR
admission and Gerrit admission cannot both be enabled in one daemon.

The adapter polls open changes and imports eligible current patch sets from
`refs/changes/...`. It excludes WIP changes, rejects multi-parent revisions
and missing/duplicate Change-Ids, and admits only changes with declared
submit requirements whose non-queue requirements are satisfied, overridden,
or not applicable. It ignores the configured verification requirement
*during admission*, votes `Verified +1` on the exact tested original patch
set just before landing, and then rechecks **all** submit requirements and
the current revision. Parent patch sets become explicit dependencies.

Gerrit documents that direct pushes to the target with a matching
`Change-Id`, repository, and branch update the patch set and mark the change
merged. This permits the queue's exact tested commit to land natively. The
adapter checks that Gerrit reported `MERGED` afterward. See
[Gerrit change identity and direct pushes](https://gerrit-review.googlesource.com/Documentation/user-changeid.html).

**Deployment prerequisite:** restrict target Push and Submit permissions to
the queue, grant its `Verified +1` vote, and configure the submit requirements
explicitly. A direct push bypasses Gerrit's submit action; the adapter's
requirement check supplies that gate. A vote precedes the target CAS, so a
stale CAS can leave the old patch set verified; other submitters must not
be able to race the queue through that window. This adapter has HTTP fixture
coverage but has not been validated against a live Gerrit installation.
