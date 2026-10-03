# Conditional checks

## Conditional execution

The [check environment](../reference/environment.md) contract is the whole mechanism for
conditional/monorepo-style execution — gauntlet has no path-filter config
. An affected-only
check decides for itself, using the SHAs it's handed. The check's working
tree is a plain export (`git archive`, no `.git`), so point git at
`GAUNTLET_GIT_DIR` instead:

```sh
if git --git-dir="$GAUNTLET_GIT_DIR" diff --name-only "$GAUNTLET_BASE_SHA" "$GAUNTLET_MERGE_SHA" | grep -q '^services/web/'; then
    go test ./services/web/...
else
    echo skipped > "$GAUNTLET_RESULT_FILE"
fi
```

For a batch run this diff is exactly what the run's verdict covers: the
base is the target tip the whole chain was built on and the merge SHA is
the chain tip, so `base..merge` is every member's changes together — the
right unit for skipping, since a batch's members land (or fail) on this one
shared suite.

The same object store also serves content-based test caching without
hand-maintained input manifests. For a cache *key*, prefer the content
identity itself — the tree OID of the inputs, straight from the merge being
tested:

```sh
key=$(git --git-dir="$GAUNTLET_GIT_DIR" rev-parse "$GAUNTLET_MERGE_SHA:services/web")
```

Two trials whose `services/web` trees are byte-identical get the same key,
including across a revert that restores earlier content — that's what makes
it an identity. The last-*changing*-commit query answers a different
question, provenance ("which commit last touched these inputs?"):

```sh
git --git-dir="$GAUNTLET_GIT_DIR" log -1 --format=%H "$GAUNTLET_MERGE_SHA" -- services/web/
```

It also works as a cache key, just a conservative one — a revert produces a
new commit and so a fresh key even though the content (and any correct
cached result) is unchanged. Use it when you want the commit for humans or
logs; use `rev-parse` when you want maximal cache hits.

Some build/test tools you can't rewire key their caches on file *metadata*
(path + mtime + size) rather than content, and a plain export gives every
file extraction wall time — a guaranteed miss on every run. For those, the
daemon operator can turn on deterministic history-derived mtimes for all
exported trees with the top-level `export { mtimes "history" }` block; see
[daemon configuration](../reference/daemon.md) for the exact semantics. It's an operator knob, not
something the check spec can request.

Every SHA in the environment contract stays resolvable in
`GAUNTLET_GIT_DIR` for your check's entire lifetime — the daemon pins the
trial chain against `git gc` for the whole run, and a landed chain stays
anchored through the fetched target ref afterwards — so a long check never
has an object vanish mid-query.

Gauntlet hands you the SHAs and the object store; which paths matter to
which check is repo-owned code, same as everything else about what a check
does.
