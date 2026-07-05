# gauntlet

Gauntlet is a merge queue. Push your branch to `for/<target>/<user>/<topic>`
and the daemon trial-merges it onto the live tip of `<target>`, runs the
named checks defined by your repo's own `.gauntlet.kdl`, and — if everything
comes back green — lands it as a `--no-ff` merge that preserves your commits
exactly as you wrote them. Red pings you; fix and re-push the same ref.

Requires git 2.38 or newer (`git merge-tree --write-tree`).

## Running

Build the daemon:

```sh
go build -o gauntlet ./cmd/gauntlet
```

There are two config files:

- **Daemon config** (admin-written, one per daemon instance) — points at the
  remote, the poll interval, the committer identity used for merge commits,
  and the target branches to reconcile. See [`gauntlet.kdl`](gauntlet.kdl)
  for a full example. Passed via `-config`.
- **Repo check spec** (adopter-written, lives in the repo the daemon
  watches) — the named checks a candidate must pass before it lands. See
  [`.gauntlet.kdl`](.gauntlet.kdl) for a full example. The daemon reads this
  file out of each candidate's own trial tree, so a branch is always tested
  by its own check spec.

Run it:

```sh
gauntlet -config gauntlet.kdl -state ~/.cache/gauntlet
```

- `-config` (required) — path to the daemon config (`gauntlet.kdl`).
- `-state` — directory for the daemon's local bare-repo clone(s), keyed per
  remote. Defaults to `gauntlet` under `os.UserCacheDir()`.

**The land flow:** push your branch to `refs/heads/for/<target>/<user>/<topic>`.
Each poll tick the daemon trial-merges the candidate onto the live tip of
`<target>` and runs the checks from your repo's own `.gauntlet.kdl` against
that trial tree. All green lands it as a `--no-ff` merge onto `<target>`,
preserving your commits exactly as written, and deletes the `for/...` ref.
Red (or a conflict) parks the ref alone — nothing re-runs until you push a
new SHA to it.

The daemon shuts down cleanly on `SIGINT`/`SIGTERM`.

**Status:** phase 1 is under construction.

See [DESIGN.md](DESIGN.md) for the full design and rationale.
