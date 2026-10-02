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

## Follow-up passes

- Review parks now persist their metadata/request version. The restart test
  covers an initially ineligible review, readiness loss and return, and an
  edited version becoming eligible for a new trial. Exact retry-run identity
  resolves millisecond ties; migrations now commit or roll back together.
  Randomly seeded run counters prevent deterministic same-second restart
  collisions; tied verdicts follow insertion order rather than ID spelling.
- GitHub has signed webhook wakeups, independent cached admission polling,
  and closed-comment-history filtering. Native stack roots retain requests
  after losing their stack field. Real `full_name` JSON decoding and fork
  boundaries are now covered. Final landing validation still reads fresh state.
- Source expiration now runs automatically at startup and hourly. Trials,
  workers through cancellation cleanup, and running/backlogged hooks hold
  source leases. Shared locking protects fetch and normalization races.
  Real-Git tests prove live sources survive pruning and GC, then become
  collectable after the last consumer releases them. Removed the offline
  maintenance command and runbook.
- Added GitHub auth-refresh, webhook, cache-expiry, and concurrency tests;
  Gerrit post-vote race and merged-state acknowledgement tests. Removed the
  redundant second fetch for Gerrit root patch sets.
- Fixed the execution-cap test to reconcile while waiting for a worker's
  deferred slot release, rather than assuming result delivery releases it.
- Live Docker service tests passed against Docker 28.4.0 and Redis 7: published
  ports, ready-command probes, network-mode discovery, inspection, and cleanup.
  Docker Hub rate-limited the pull; the official Redis image was obtained from
  Google's public registry mirror. Live Gerrit is still unverified.

## Remaining priorities

1. **Intake scalability and feedback.** Refreshes still list all PR metadata;
   final validation can repeat scans for batch members. Conditional requests,
   incremental intake, and shared fresh batch validation are next optimizations.
   Blocked requests still need clearer acknowledgement and reasons.
2. **Live Gerrit.** API fixtures cover important gates, but cannot prove the
   server's Change-Id association or restricted-submit setup.
3. **Landing defaults.** Loaded configuration defaults to squash; hand-built
   zero-valued queue targets retain legacy merge behavior. Make landing mode
   explicit in new tests and converge the defaults when retiring compatibility.
4. **Documentation history.** Older decision-ledger entries describe superseded
   behavior. Prefer current feature documents for operational guidance.

## Tests

The suite's strongest coverage is in queue behavior, config parsing, and the
older lifecycle features. Further useful tests include daemon startup/shutdown,
Gerrit dependencies and live submission, GitHub API failure variants, and
additional stack ambiguity cases. Live forge tests need dedicated fixtures;
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
