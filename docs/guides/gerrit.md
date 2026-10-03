# Gerrit changes

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
