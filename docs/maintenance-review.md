# Maintenance review — October 2, 2026

This review covered contributor guidance, comments in the core contracts and
queue, queue structure, forge admission, test duplication and coverage, and
release hygiene. It does not establish that live Gerrit or container-service
integration works on an adopter's infrastructure.

## Changes made

- Replaced `CLAUDE.md` with `AGENTS.md`: portable guidance, current verification
  commands, and comments focused on contracts rather than development history.
- Removed about 1,000 net comment lines from the main contracts, configuration,
  queue, container executor, and CLI. Compared Go token streams to prove that
  the comment pass changed no executable code.
- Split queue reconciliation into orchestration (`reconcile.go`), check workers
  (`checkrun.go`), spec gates (`checkspec.go`), and landing/recovery (`landing.go`).
  Kept the same state owner and functions; added no new abstraction layer.
- Consolidated CLI dispatch's repeated error and exit handling.
- Paginated GitHub status history and kept the latest status per context.
  Required names now require every matching check/status producer to pass.
  Added admission tests for pagination, collisions, unfinished checks, approval
  dismissal, stale approvals, comments, and change requests. Two collision
  regressions were also verified to fail against the previous implementation.
- Removed the duplicate Go integration happy path already exercised by
  `green_multi_check_land.txtar`. Strengthened the script's real-Git assertion
  to check both parents. Kept the fake-based test's record and CAS-order checks.
- Made `make release` reject staged changes and untracked files as well as
  unstaged edits. Verified all three cases in disposable local repositories,
  without fetching, tagging, or publishing.

## Architecture worth keeping

The single-threaded reconcile loop and worker result channels give queue
state a clear owner. Exact tested SHAs, compare-and-swap writes, and recovery
from target history are the correctness boundary. Executor, Git, review, and
service-driver interfaces sit at real external boundaries; they do not need
another framework wrapped around them.

Serial, batch, and speculate have different admission and failure semantics.
Keep those differences visible. Likewise, pre-land checks, post-land hooks,
and deployment reconciliation have different lifetimes; a universal runner
would obscure them. Share small rules when duplication causes drift, rather
than combining the state machines.

## Remaining priorities

1. **Forge intake cost and feedback.** GitHub scans all PR/comment history and
   repeats admission polling before landing. Persistent request intake and
   bounded polling would reduce API use; blocked-request feedback would help
   users. Preserve the final readiness check and crash-safe request replay.
2. **Restart semantics.** History-backed parks omit review metadata versions,
   so review parks re-evaluate after restart. Persist that identity with a
   schema migration and restart test before promising durable review parks.
3. **Source retention.** `refs/gauntlet/source/<sha>` has no retention limit.
   Add an explicit policy that respects hook/audit reachability before pruning.
4. **Landing defaults.** Loaded configuration defaults to squash; hand-built
   zero-valued queue targets retain legacy merge behavior. Make landing mode
   explicit in new tests and converge the defaults when retiring compatibility.
5. **Documentation history.** The decision ledger remains useful, but older
   entries describe superseded behavior. Prefer current feature documents for
   operational guidance; future edits should retire stale claims rather than
   append another explanation to code comments.

## Tests

The suite's strongest coverage is in queue behavior, config parsing, and the
older lifecycle features. The review adapter is newer and needs more boundary
coverage. Priorities are GitHub authentication refresh and API failures,
branch-stack ambiguity and forks, Gerrit dependencies and post-vote races,
Gerrit's merged-state acknowledgement, daemon startup/shutdown, and real
container-service lifecycle tests. Live forge tests need dedicated fixtures;
HTTP test servers cannot prove host-side merge association.

Some happy paths were repeated through fake Git, real Git, and scripts. Keep
one scenario per boundary, plus distinct assertions such as CAS ordering and
record fidelity. Historical SQLite schema fixtures are intentionally frozen
compatibility inputs: generating them from today's schema would weaken the
migration tests. Keep them despite their size. Do not raise coverage by adding
assertions about private implementation steps or trivial accessors.

Verification uses the race detector across all packages, `go vet ./...`,
`go mod tidy -diff`, and the static `make build`. Process-group tests require
an init/subreaper in this managed workspace; run them under Tini as documented
in `AGENTS.md`. Coverage percentages are a guide to inspection, not a gate.
