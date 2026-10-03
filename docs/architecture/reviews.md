# Review identity and host completion

## Commit identity and recovery

GitHub commits use the PR title (with its number) as the subject and the PR
body as the message body. Gerrit commits preserve the patch-set message,
including its `Change-Id` trailer. Ordinary ref submissions use the head
commit's message. Each keeps the head commit's original author and author
date, with Gauntlet as committer. Source signatures are invalidated by the new parents and message. Optional
[signing](../reference/signing.md) signs the generated commit before verification.

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
are never pushed. The daemon automatically expires unused source archives
and review-cache refs at startup and hourly. `source-retention` defaults to
thirty days. Trials, check workers (including cancellation cleanup), and
running/backlogged hooks hold leases that exclude their inputs from pruning.
Fetches, normalization, lease acquisition, and pruning share a lock. Expired objects become eligible for Git's normal garbage collection; pruning
never changes remote refs, normalized target history, or SQLite records.
The linear target itself does not reach the original junk history.

## Local persistence

Enable `history "/var/lib/gauntlet/history.db"` to preserve failed review parks.
A park applies to the same source SHA and metadata/request version. New heads,
edited landing messages, and new requests invalidate it. Temporary loss of
readiness hides the inactive review but keeps its failure verdict, including
when the daemon restarts before readiness returns. Pre-v15 records have
no version and are safely re-evaluated once after upgrading. Retry intents name
the exact superseded terminal run (v16+), avoiding a lost retry when failure and
retry share a millisecond. Schema upgrades run in one transaction.

Git is the authority for target contents and landing provenance. SQLite stores
completed run/check records, parks, and retry intent; GitHub comments hold queue
requests. In-flight trials are rebuilt after restart, and interrupted forge
acknowledgements are retried from target history. This does not serialize live
processes or automatically resume post-land hooks.

Commands waiting on admission have no separate acknowledgement comment;
trial/status events start once admitted. Detailed blocked-request feedback
remains follow-up work.

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
