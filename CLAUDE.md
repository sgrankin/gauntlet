# CLAUDE.md

Gauntlet is a merge-queue daemon. Read [README.md](README.md) for what it
does and [DESIGN.md](DESIGN.md) — especially the decision ledger and
invariants — before any structural change; the ledger records what was
already tried and killed. Mechanism-level design (queue core, batch and
speculate modes, shared services, scaling) lives in
[docs/design/](docs/design/).

## Commands

- Build: `make build` (version-stamped) or `go build ./cmd/gauntlet`.
- Test: `make test` — runs `go test -race -count=1 ./...`. Always test with
  `-race`; plain `go test ./...` is not the house test run.

## Conventions

- Commits: `area: subject` (e.g. `queue: park cancelled batch members`) —
  not conventional-commits.
- Testing style (see DESIGN.md's "Go-team testing style" entry):
  - Test at the API layer (`ReconcileOnce`, `LoadDaemon`), not internals.
  - Fakes, not mocks: doubles are real implementations with affordances
    (gated executor, recording channel); the git layer runs against real
    bare repos, never a stubbed interface.
  - Deterministic stepping via injected ticks — no wall-clock sleeps.
  - Queue scenarios are testscript/txtar files in
    `internal/queue/testdata/script/`, run by both a fake-git and a
    real-git harness; prefer adding a scenario over a bespoke Go test.
- Config is dumb data (KDL): no conditionals, loops, or path globs in
  config — logic belongs in the repo's own check scripts.
- Agents/subagents never run `jj` or `git` mutations in this repo; leave
  VCS operations to the session owner.
