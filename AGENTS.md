# Working on Gauntlet

Gauntlet builds, verifies, and lands target history. Read [README.md](README.md)
for usage and [DESIGN.md](DESIGN.md) for the invariants before changing the
queue. Consult the relevant [design document](docs/design/) for a feature;
older decision-ledger entries may be superseded by later decisions.

## Verification

- Build: `make build` (includes the version) or `go build ./cmd/gauntlet`.
- Test: `make test` runs `go test -race -count=1 ./...`. Use `-race` for
  focused tests too. Run the packages affected by a change; run the full
  suite before finishing changes to shared contracts or queue behavior.
- Maintenance checks: `go vet ./...` and `go mod tidy -diff`.
- In the managed cloud workspace, source `/workspace/.gauntlet-cloud/activate.sh`
  if present, use `umask 022`, and run process tests under
  `/usr/local/bin/docker-init -s -- make test` if that binary is available.
  This supplies the Go toolchain and reaps orphaned subprocesses.

## Design and code

- Land the exact tested commit. Push target and candidate refs with an
  expected old SHA; recover from remote history after interrupted writes.
- Keep the reconcile loop single-threaded. Workers return results rather
  than mutating queue state. Publish snapshots for concurrent readers.
- Keep KDL declarative. Conditions and loops belong in check scripts.
- Prefer concrete code and small interfaces at external boundaries. Add
  abstractions when another implementation or a clear test boundary needs them.
- Comments should explain contracts, ownership, ordering, or surprising
  choices. Keep design history in `docs/design/`, not beside each statement.

## Tests and commits

- Test observable behavior through APIs such as `ReconcileOnce` and
  `LoadDaemon`. Unit tests suit pure helpers and boundary cases.
- Use working fakes, gated executors, and recording channels rather than
  call-count mocks. Test Git plumbing against real bare repositories.
- Prefer queue scenarios in `internal/queue/testdata/script/` when both
  harnesses can express the case; use Go integration tests for other boundaries.
- Control ordering with injected ticks or explicit rendezvous. Avoid sleeps
  used to guess when work finished. Bound tests involving real processes.
- Test each behavior at the layer that owns it. Do not repeat the same cases
  through every adapter unless the adapter changes their meaning.
- Commit subjects use `area: subject`, not conventional-commit prefixes.
  Make commits when requested; preserve unrelated work and do not rewrite
  published history without authorization.
