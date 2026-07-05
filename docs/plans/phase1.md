# Gauntlet — Phase 1 Implementation Plan

**Scope:** the phase-1 daemon only, per `DESIGN.md` ("Build phases" §1, amended 2026-07-04). `DESIGN.md` is authoritative; this plan operationalizes it. Plan drafted by the planning agent (Opus), reviewed and amended by the design reviewer — **§9 amendments are authoritative where they touch earlier sections.**

**Toolchain (verified on this machine):** Go `1.26.4`, git `2.55.0`. Module `github.com/sgrankin/gauntlet`, `go 1.26`. Minimum git: **2.38** (first `git merge-tree --write-tree`); recommend ≥2.40. Dependencies (all verified to resolve): `github.com/sblinch/kdl-go` (config; see Spike 3 + §9.8), and `go.opentelemetry.io/otel` + `.../otel/trace` `v1.44.0` (API only, no SDK/exporter in phase 1). Everything else is standard library.

---

## 0. What phase 1 is

One long-running process, one remote, N target branches. Each reconcile tick it: fetches the remote; derives the queue from `refs/heads/for/<target>/<user>/<topic>` refs; picks the head candidate per target; trial-merges it onto the current target tip in a bare repo (`merge-tree --write-tree`); builds the `--no-ff` merge commit (`commit-tree`); reads the candidate's **own** named-check spec out of the trial tree; exports the tree and runs each check (a plain shell command) sequentially via the local executor; and on all-green CAS-pushes the target to the tested merge commit and CAS-deletes the candidate ref. Red / conflict / missing-check-spec leaves the target alone and emits an event. Every run produces a structured **run record** (stable run ID; per-check name/verdict/duration) surfaced both as OTel spans (no-op provider) and on channel events. Config is KDL; the one channel logs events. Executor and Channel are interfaces with one impl each. No SQLite, containers, Slack, GitHub API, dashboard, or OTLP exporter.

---

## 1. Repo layout

```
gauntlet/
  go.mod  go.sum                module github.com/sgrankin/gauntlet, go 1.26
  README.md                     stub (what it is, how to run, min git version, the two config files)
  gauntlet.kdl                  example DAEMON config (admin-written; also a config-package fixture)
  .gauntlet.kdl                 example REPO check spec (adopter-written; fixture + integration tests)
  cmd/gauntlet/main.go          flags, config load, dependency wiring, run loop
  internal/
    core/                       domain types + interfaces + run record; ZERO internal deps
      types.go                  Candidate, TrialMerge, CheckJob, CheckResult, RunRecord, Outcome, Event, Command, Identity
      interfaces.go             GitRepo, Executor, Channel
    config/                     both config files -> plain structs; the ONLY package touching KDL
      daemon.go  checks.go  config_test.go
    gitx/                       implements core.GitRepo via the git CLI (plumbing only)
      git.go  git_test.go
    executor/                   implements core.Executor (runs ONE check)
      local.go  gate.go        LocalExecutor + GatedExecutor test double
    channel/                    implements core.Channel
      log.go  record.go        LogChannel + RecordingChannel test double
    obs/                        OTel tracer helper + run-record<->span mapping
      trace.go
    queue/                      reconcile loop, per-target state machine, check sequencing, run records
      daemon.go  reconcile.go  message.go  daemon_test.go
    testutil/remote.go          bare-repo harness
  docs/plans/phase1.md          this document
```

**Dependency direction** (acyclic; arrows = imports):

```
cmd/gauntlet ─▶ config, queue, gitx, executor, channel, obs, core
queue        ─▶ core, obs        (interfaces + domain types + tracer)
obs          ─▶ core, otel API
gitx/executor/channel ─▶ core
config       ─▶ core             (reuses core.Identity)
core         ─▶ stdlib only
testutil     ─▶ stdlib + git CLI (imported only by *_test.go)
```

`core` is the shared vocabulary. The queue imports only `core`+`obs`, never a concrete impl — that is the mechanism for **Invariant 8**. Responsibilities: **core** nouns/verbs, no behavior; **config** both files → validated structs, isolates the config language (swappable); **gitx** the entire VCS surface, only place `git` runs; **executor** run one named check against a tree; **channel** events out / commands in; **obs** thin OTel wrapper (no-op by default); **queue** serialize FIFO per target, drive trial→read-checks-from-tree→run-checks→aggregate→land, own crash recovery + run record, never knows what "green" means beyond "all checks passed"; **cmd** compose.

---

## 2. Core types and interfaces

*(Amended per §5A: `CheckStatus` replaces `Passed bool`; `CheckJob.BaseSHA` and env constants added.)*

`internal/core/types.go`:

```go
package core
import "time"

// Ref name = durable identity; SHA = what is tested.
type Candidate struct {
	Ref, Target, User, Topic, SHA string // Ref="refs/heads/for/<target>/<user>/<topic>"; User may be "" (§9.3)
}
type TrialMerge struct {
	Clean     bool
	TreeOID   string   // valid iff Clean
	Conflicts []string // conflicted paths iff !Clean
}
type CheckJob struct {
	RunID     string   // stable per run; shared by every check in the run
	Target, Name string
	Command   []string // argv
	Dir       string   // exported trial tree
	BaseSHA   string   // target tip the trial merged onto
	MergeSHA  string
	Candidate Candidate
	Clean     bool     // reserved for phase-4 clean-build escape hatch; always false in phase 1
}

type CheckStatus int
const ( CheckPassed CheckStatus = iota; CheckFailed; CheckSkipped )

// Status meaningful only when Err==nil. Err = daemon-caused non-verdict
// (ctx cancel, executor I/O failure): see §9.2 — exec-start failures are
// CheckFailed (a verdict), not Err.
type CheckResult struct {
	Name     string
	Status   CheckStatus
	Output   string        // tail-capped, §9.6
	Duration time.Duration
	Err      error
}

// Check environment contract — every executor exports these.
const (
	EnvBaseSHA      = "GAUNTLET_BASE_SHA"      // = job.BaseSHA
	EnvMergeSHA     = "GAUNTLET_MERGE_SHA"     // = job.MergeSHA
	EnvCandidateSHA = "GAUNTLET_CANDIDATE_SHA" // = job.Candidate.SHA
	EnvRef          = "GAUNTLET_REF"           // = job.Candidate.Ref
	EnvResultFile   = "GAUNTLET_RESULT_FILE"   // path to a file the executor creates; a check writes "skipped" to skip
)

type Outcome int
const (
	OutcomeLanded Outcome = iota // CAS-pushed, slot deleted
	OutcomeRejected              // a check failed, or check spec missing/invalid; target untouched
	OutcomeConflict              // trial merge conflicted; target untouched
	OutcomeSkipped               // ref/target moved mid-flight or lost a CAS race; re-queued
	OutcomeError                 // daemon-side failure; parks like Rejected (§9.2)
)
// Single structured fact per run. Source of truth that BOTH the OTel span tree
// (phase 1) and a future SQLite writer / OTLP exporter (phase 3) build from, unchanged.
type RunRecord struct {
	RunID     string
	Target    string
	Candidate Candidate
	BaseOID   string        // target tip merged onto
	MergeSHA  string        // tested merge commit
	Trial     TrialMerge
	Checks    []CheckResult // per-check name/status/duration, in run order
	Outcome   Outcome
	Detail    string
	StartedAt, EndedAt time.Time
}
type Identity struct{ Name, Email string }
type EventKind int
const (
	EventQueued EventKind = iota
	EventTrialClean
	EventTrialConflict
	EventCheckStarted
	EventCheckFinished
	EventLanded
	EventRejected
	EventSkipped
	EventError
)
// Terminal events (Landed/Rejected/Conflict/Skipped/Error) carry the finished *RunRecord.
type Event struct {
	Kind      EventKind
	At        time.Time
	Target    string
	Candidate Candidate
	RunID, CheckName string
	Record    *RunRecord
	Detail    string
}
type Command struct{ Kind, Target, Ref string } // Invariant 8; no phase-1 channel produces one
```

`internal/core/interfaces.go`:

```go
package core
import ("context"; "errors")
var ErrCASStale = errors.New("gitx: CAS failed, ref moved")

type GitRepo interface {
	Fetch(ctx context.Context) error                                   // fetch --prune; tick snapshot
	ListRefs(ctx context.Context) (map[string]string, error)           // remote ref -> OID, post-fetch
	MergeTree(ctx context.Context, base, candidate string) (TrialMerge, error)
	CommitTree(ctx context.Context, tree string, parents []string, message string, who Identity) (string, error) // ONLY object we create (Inv 6)
	ReadFileFromTree(ctx context.Context, tree, path string) ([]byte, error) // candidate's own check spec, from the trial tree
	IsAncestor(ctx context.Context, maybeAncestor, ref string) (bool, error) // crash-recovery: already landed? (Inv 4)
	ExportTree(ctx context.Context, tree, dir string) error
	CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error // CAS; newOID=="" deletes; ErrCASStale on race (Inv 2,3)
}
type Executor interface {
	// Queue owns sequencing, aggregation, per-check spans, the run record — so
	// per-check observability lives in the core, not every executor impl.
	RunCheck(ctx context.Context, job CheckJob) CheckResult
}
type Channel interface {
	Emit(ctx context.Context, ev Event) error // logs; must not block the loop
	Commands() <-chan Command                  // phase-1 LogChannel: never yields
}
```

Queue state (`internal/queue/daemon.go`): `Daemon{ git, exec, chans, tr trace.Tracer, cfg, now, work, runs map[string]*run, order map[string]int64, seq, done map[string]string }`. The in-flight `run` per target (Invariant 4's state) carries `cand, baseOID, mergeOID, runID, dir, checks []config.Check, rec *core.RunRecord, rootSpan trace.Span, idx int, cur *checkInFlight{name,cancel,result chan CheckResult,span,start}`. Public API: `New(...)`, `ReconcileOnce(ctx) error` (one non-blocking full pass), `Run(ctx, tick <-chan time.Time) error`.

**Concurrency:** the reconcile pass is single-threaded and never overlaps itself. The only other goroutines are per-check executor runs, touching the daemon solely by sending once on `checkInFlight.result`; `ReconcileOnce` reads non-blockingly. No locks. This makes every test deterministic — the test controls when passes happen and when each check's verdict lands.

---

## 3. Reconcile loop as a state machine

Per target, per tick. **Ground truth each tick = post-`Fetch` remote-tracking refs** (`ListRefs`). In-memory between ticks: the single in-flight `run` per target, `order`, `done` — all reconstructible; losing them costs at most some re-tests, never correctness.

**Per-tick (`reconcileTarget`):**
1. **Snapshot.** `Fetch`, `ListRefs`. Extract `targetTip=refs["refs/heads/<branch>"]` and candidates matching `for/<name>/...` (grammar: §9.3). Assign `order[ref]` for new refs; drop `order`/`done` entries for vanished refs.
2. **Reconcile in-flight run** (before touching the current check's verdict): candidate gone or moved (SHA≠run.cand.SHA) → cancel, Skipped (**Inv 5**); target moved (≠run.baseOID) → cancel, Skipped; current check verdict ready → record it, end span; `Err` → Error, park (**§9.2**); `Failed` → Rejected + park, **short-circuit** (remaining checks not run), target untouched; `Passed`/`Skipped`+more → start next check; `Passed`/`Skipped`+last → **Land**. If a run survived, return (one lane).
3. **Pick head + start trial** (only if no in-flight run): head = smallest `order[ref]` (tie-break lexical) whose current SHA is not parked in `done` (**§9.1**). `IsAncestor(cand.SHA,targetTip)` true → already landed, CAS-delete slot, EventLanded(recovered) (**Inv 4**). Else `MergeTree`: conflict → Conflict + park; clean → gen `runID` (**§9.4**), build message, `CommitTree(tree,[targetTip,cand.SHA],msg,committer)` → `mergeOID` (**Inv 1**), `ReadFileFromTree(tree, check-spec)` → parse; missing/invalid → Rejected + park (author must fix; don't spin); else `ExportTree`, start root span, start check[0].
4. **Land** (all checks passed/skipped): `CASUpdate(targetRef, run.baseOID, run.mergeOID)` — `ErrCASStale` → Skipped, **do not delete slot**, rebuild next tick (**Inv 2**); success → target holds exactly the tested merge commit (**Inv 1,2**). Then `CASUpdate(candRef, run.cand.SHA, "")` — `ErrCASStale` → slot survives at new SHA, re-queues (**Inv 3**); success. `done` entries for *other* refs are NOT cleared (**§9.1**). Finish run record, end root span, EventLanded(Record), remove dir, drop run.

**Merge message:** Go `text/template` (`merge-message`, default `"Merge {{.Topic}} ({{.User}})"`; when `User` is empty the default degrades to `"Merge {{.Topic}}"` — §9.3), then trailers `Gauntlet-Ref: for/...` and `Gauntlet-Run: <runID>`. Fields `{Topic,User,Ref,RunID,Target}`.

**Observability:** queue holds `otel.Tracer("gauntlet")`. Root span `run` per run (attrs run.id/target/candidate.ref/candidate.sha/merge.sha) held in `run.rootSpan` across ticks/goroutines, ended on terminal transition with status from Outcome. Child spans: `trial-merge`, one `check` each, `land`. Goroutine child spans parented via `trace.ContextWithSpan(ctx, run.rootSpan)`. `RunRecord` is source of truth; `obs` maps it to span attrs. No provider set → no-op (verified `IsRecording()==false`, API-only deps). Phase 3 adds SDK/OTLP + SQLite consuming the same RunRecord. Terminal channel events already carry `*RunRecord`.

**Crash recovery:** no durable in-flight state; restart rescans and heals. Crash mid-check → orphaned dir wiped on startup, trial rebuilt/rerun (**Inv 4**). Crash between land and delete → next tick `IsAncestor` true → CAS-delete slot, no redundant re-merge (**Inv 4**). Duplicate daemon → CAS lets one push win, loser gets `ErrCASStale` and rebuilds; no corruption possible since every mutation is CAS (**Inv 2**). Previously-red candidate re-tested once after restart (correct, occasionally wasteful; fixed with persistence in phase 3).

---

## 4. Config schema (two files)

Two files, two authors, two purposes (`DESIGN.md`: job spec in the repo; daemon config = operations only). Both parse in the isolated `config` package. Language: **KDL** for both (§9.8; Spike 3).

**Daemon config — `gauntlet.kdl`** (admin-written, `-config` flag):
```kdl
remote "https://github.com/acme/widgets.git"
poll-interval "10s"
check-spec ".gauntlet.kdl"        // path within each repo tree; optional, default ".gauntlet.kdl"
committer {
    name "Gauntlet"
    email "gauntlet@ci.acme.example"
}
merge-message "Merge {{.Topic}} ({{.User}})"   // optional
target "main"    branch="main"
target "release" branch="release/v2"
```

**Repo check spec — `.gauntlet.kdl`** (every adopter writes this; read from the trial tree):
```kdl
check "lint"  { command "golangci-lint" "run" }
check "test"  { command "go" "test" "./..." }
check "build" { command "go" "build" "./..." }
```

Structs (`internal/config`), tags verified against kdl-go in Spike 3:
```go
type Daemon struct {
	Remote    string        `kdl:"remote"`
	Poll      time.Duration `kdl:"poll-interval,format:units"`
	CheckSpec string        `kdl:"check-spec"`     // default ".gauntlet.kdl"
	Committer core.Identity `kdl:"committer"`      // Name `kdl:"name"`, Email `kdl:"email"`
	MergeMsg  string        `kdl:"merge-message"`  // optional
	Targets   []Target      `kdl:"target,multiple"`
}
type Target struct{ Name string `kdl:",arg"`; Branch string `kdl:"branch"` }
type CheckSpec struct{ Checks []Check `kdl:"check,multiple"` }
type Check struct{ Name string `kdl:",arg"`; Command []string `kdl:"command,child"` }
```
`LoadDaemon(path)` and `ParseChecks(bytes)` unmarshal then **validate** (the Go validation pass compensating for kdl-go's thin validation): daemon → remote non-empty, poll>0 (default 10s), ≥1 target each with name+branch, committer name/email set, template parses, check-spec non-empty; checks → ≥1 check, each non-empty name+command, names unique. Errors name the offending node.

---

## 5A. Check verdicts and environment (delta absorbed into §2/§3 above)

Conditional execution (monorepo "only web changed") is the **check script's** job, never config's — no path globs in gauntlet config, ever. A check has three outcomes (`passed`/`failed`/`skipped`), signalled via the result file, never exit-code conventions. Run verdict: green = every check `Passed` **or** `Skipped` and no `Err`; a single `Failed` → Rejected. `Skipped` is recorded distinctly in the `RunRecord` so history doesn't lie.

`LocalExecutor.RunCheck`: create an empty result file in a temp dir, export the five env vars (inherited env + these), run `Command` in `job.Dir` (own process group, §9.5); then map, in precedence order — ctx cancelled or executor I/O failure → `Err`; exec-start failure → `CheckFailed` with explanatory Output (**§9.2**); exit non-zero → `CheckFailed` **regardless of the result file** (the file is ignored on failure — it is not an exit-code convention, it only splits the exit-0 case); exit 0 and result file contains `skipped` → `CheckSkipped`; exit 0 and file empty/absent → `CheckPassed`. `Duration` always set. Output tail-capped (**§9.6**).

**Config-that-computes KILLED:** both config files are dumb data. (If CUE were ever adopted it is constrained to plain-data mode only — moot for phase 1, KDL chosen.)

---

## 5. Test strategy

Two tiers. **CAS semantics, merge-tree, read-file-from-tree, and crash recovery run against real bare git repos** — faking them would fake away the point. State-machine ordering/aggregation is unit-tested with fakes.

**Harness (`internal/testutil`), real git, `t.TempDir()`, no network:** `NewRemote`, `Seed(branch,files)`, `PushCandidate(target,user,topic,files)` (files include `.gauntlet.kdl`), `MoveCandidate(ref,files)` (re-push, new SHA), `DeleteCandidate(ref)`, `DirectPush(branch,files)` (simulate human/2nd daemon), `Ref(ref)`, `BareClone()` (daemon's bare repo dir).

**Deterministic stepping:** `GatedExecutor.RunCheck` registers by `(RunID,Name)` and blocks until the test calls `Release(runID,name,CheckResult)` — opens a "check running" window and steps check-by-check. `RecordingChannel` captures every `Event` + terminal `RunRecord`. Tests build a `Daemon` with **real gitx** + gated executor + recording channel and step with explicit `ReconcileOnce`. Green multi-check landing: push candidate+`.gauntlet.kdl` → `ReconcileOnce` (trial clean, `lint` gated) → `Release(lint,Passed)` → `ReconcileOnce` (records lint, starts `test`) → `Release(test,Passed)` → `ReconcileOnce` (CAS-push, CAS-delete; assert target==mergeOID, slot gone, RunRecord 2 passed).

**Invariant + delta tests (integration, real gitx):**

| Test | Setup | Assert | Inv/Delta |
|---|---|---|---|
| Green multi-check land | checks `[lint,test]` both pass | target=tested mergeOID; `parent[1]`==candidate SHA verbatim; slot deleted; RunRecord 2 passed | 1, 6; d1,d2 |
| Check spec from trial tree | candidate's `.gauntlet.kdl` adds a check vs target's | candidate's own spec used (extra check runs) | d1 |
| Skipped check | check writes `skipped` to `$GAUNTLET_RESULT_FILE`, exit 0 | RunRecord shows Skipped (not Passed); run still lands | d3 |
| Check env exported | check asserts the 4 SHA/REF env vars are set/correct | check passes only when env matches expected OIDs | d3 |
| Missing/invalid check spec | candidate without `.gauntlet.kdl` | EventRejected (names the file); target & slot untouched; parked until re-push | d1 |
| Short-circuit on fail | `lint` fails | `test` never starts; EventRejected; target untouched | d1 |
| Failure parks per SHA | after Rejected, more ticks + an unrelated land | no re-test of the parked SHA; re-push (new SHA) re-enters the queue | §9.1 |
| Exec-start failure is a verdict | check command doesn't exist | CheckFailed (not Err); EventRejected; parked; no retry loop | §9.2 |
| Merge conflict | candidate edits target's lines | EventTrialConflict; target & slot untouched; not retried until SHA changes | — |
| Concurrent direct push | `DirectPush` during a check | CAS-push stale → target holds human commit, slot survives, EventSkipped; rebuild next tick | 2 |
| Target moved mid-check (tick path) | `DirectPush`, then `ReconcileOnce` while check gated | run cancelled at the tick (before land attempt), EventSkipped, rebuild | 2, 5 |
| Re-push mid-check | `MoveCandidate` while check gated | run cancelled, EventSkipped, new trial on new SHA; old verdict discarded | 5 |
| Candidate deleted mid-check | `DeleteCandidate` while check gated | run cancelled, dir cleaned, no land, no author ping beyond EventSkipped | 5 |
| Re-push at land boundary | move candidate after green, before delete | CAS-delete stale → slot survives at new SHA, re-queues; landed commit still has tested SHA | 3 |
| Crash between land and delete | step to CAS-push success, then fresh `Daemon`, `ReconcileOnce` | IsAncestor true → slot CAS-deleted; no new commit; no re-test | 4 |
| Duplicate daemon | two Daemons; d1 lands, d2 (old base) tries | one merge commit on target; d2 stale, rebuilds; no corruption | 2, 4 |
| Cancelled check kills process group | check spawns a child that outlives the parent | after cancel, child is dead; export dir removable | §9.5 |
| FIFO + skip-failed-head | A then B; A fail/conflict, B clean | A first; B proceeds while A parked; A re-enters on re-push | model |
| Run record shape | any terminal | RunRecord: stable RunID, per-check name/status/duration, outcome, timings | d2 |

**Unit tests (fakes, no git):** queue (fake `GitRepo`+gated executor+recording channel) — ordering, park-skip, move detection, sequencing/short-circuit, skipped-counts-as-green aggregation, run-record assembly, events, templating (incl. empty-User); config — parse both examples, reject each invalid variant, duration/template validation; executor/local — passed/failed/skipped/infra/cancel mapping, env vars set, output cap, `Duration` populated; channel/log — event+RunRecord formatting, `Commands()` never yields; obs — RunRecord→attributes, no-op tracer emits nothing. All pass under `go test -race ./...`.

---

## 6. Work breakdown

Ordered, sized for independent agents. Each: depends-on, acceptance.

- **C0 Scaffold+core** (blocks all) — `go.mod`, `README` stub, `internal/core/{types,interfaces}.go` (incl. `CheckStatus`, env constants, `CheckJob.BaseSHA`). Accept: `go build ./...`, `go vet ./...` clean; core imports only stdlib.
- **C1 config** (dep C0; adds kdl-go) — `internal/config/{daemon,checks,config_test}.go`, `gauntlet.kdl`, `.gauntlet.kdl`. Accept: `go test ./internal/config/` parses both examples; rejects each invalid variant with a node-named error.
- **C2 gitx** (dep C0; highest risk — hand it §7 Spike 1) — `internal/gitx/{git,git_test}.go`, `internal/testutil/remote.go`. Accept: `go test ./internal/gitx/` against real bare repos — clean+conflict MergeTree (exit-code+paths), two-parent CommitTree with trailers, ReadFileFromTree (present+missing), IsAncestor both ways, ExportTree, CASUpdate **rejects wrong old-OID / accepts right** for set and delete (ErrCASStale on loss), Fetch --prune.
- **C3 executor** (dep C0) — `internal/executor/{local,gate}.go`, `_test.go`. Accept: `go test ./internal/executor/` — passed/failed/skipped(via result file)/exec-start-failure→Failed/cancel→Err mapping; four env vars + `GAUNTLET_RESULT_FILE` set; process-group kill on cancel (§9.5); output cap (§9.6); `Duration` populated; `GatedExecutor` releases per `(RunID,Name)`.
- **C4 channel** (dep C0) — `internal/channel/{log,record}.go`, `_test.go`. Accept: structured event+RunRecord output; `Commands()` never yields.
- **C5 obs** (dep C0; adds otel API) — `internal/obs/trace.go`, `_test.go`. Accept: no-op tracer yields no spans/no error; RunRecord→attributes covered.
- **C6 queue core** (dep C0,C5; fakes) — `internal/queue/{daemon,reconcile,message,daemon_test}.go`. Accept: `go test ./internal/queue/` — full transition table (§3, incl. §9.1/§9.2 park semantics), FIFO, skip-parked-head, move detection, sequencing+short-circuit, skipped aggregation, run-record assembly, no-op spans, ref grammar (§9.3).
- **C7 integration+invariants** (dep C2,C3,C6) — `internal/queue/integration_test.go` (build-tagged), extends testutil. Accept: `go test ./internal/queue/ -run Integration -race` — every §5 row green.
- **C8 cmd wiring** (dep C1–C6) — `cmd/gauntlet/main.go`. Accept: `go build ./cmd/gauntlet`; smoke: point at a local bare remote with a candidate carrying `.gauntlet.kdl` → target advances to the merge commit, slot deleted, EventLanded logged with a run record.

**Parallelization:** C0 first; then **C1,C2,C3,C4,C5 in parallel**; C6 starts against interfaces once C0+C5 land (parallel with C2); C7 after C2+C3+C6; C8 last. Critical path C0→C2→C7 with C6 alongside.

---

## 7. Spike findings

**Spike 1 — git plumbing → full path works on git 2.55; pin ≥2.38.** Verified in scratch bare repos:
- `merge-tree --write-tree`: clean → exit 0, stdout line1 = tree OID; conflict → exit 1, stage lines `<mode> <oid> <stage>\t<path>` + message block; >1 = error. Branch on exit code; collect distinct conflicted paths for the event.
- `commit-tree`: two-parent `--no-ff` merge; `-c user.name/-c user.email` set author+committer; trailers via stdin; candidate SHA verbatim as `parent[1]` (**Inv 6**).
- `ReadFileFromTree`: `git cat-file -p <tree>:<path>` reads the candidate's own `.gauntlet.kdl` from the trial tree, no checkout — the mechanism for "tested by its own definition". Missing path → non-zero → treat as missing spec.
- **CAS set:** `git push origin <new>:refs/heads/main --force-with-lease=refs/heads/main:<old>` — wrong old → "stale info"/exit1; correct → succeeds. Explicit-OID lease checks the **real remote value at push time** = true CAS (**Inv 2**).
- **CAS delete:** `git push origin :refs/heads/for/... --force-with-lease=refs/heads/for/...:<old>` — same semantics (**Inv 3**). One `CASUpdate` covers both (`newOID==""` ⇒ delete).
- **Export:** `git archive --format=tar <tree>` piped into extraction; **decision:** capture stdout, extract with stdlib `archive/tar` (avoids BSD/GNU tar drift).
- **Snapshot:** `Fetch --prune` then `for-each-ref refs/remotes/origin/*` — objects guaranteed local; land-time CAS re-checks the live remote, so no separate `ls-remote`.

**Spike 2 — OTel API → API-only, no-op by default.** `go.opentelemetry.io/otel`+`.../trace` `v1.44.0`. No provider set → `otel.Tracer("gauntlet")` is a **no-op** (verified `IsRecording()==false`); `go list -m all` pulls only API modules (no SDK, no OTLP). Root-span-held-across-goroutines via `trace.ContextWithSpan` compiles, free under no-op. Instrument now; SDK+exporter deferred to phase 3, same `RunRecord`.

**Spike 3 — Config language: KDL vs CUE (head-to-head).** Both evaluated on the actual two files.

| Axis | `sblinch/kdl-go` | `cuelang.org/go` |
|---|---|---|
| Maturity | single-maintainer, **untagged** (`v0.0.0-2026-01-21…`), ~6mo stale, **KDL v1 only** (spec at v2) | **first-party**, `v0.17.0` dated 2026-06-22 (~2wks), regular tags |
| Unmarshal ergonomics | json-style tags; verified `,arg`/`,child`/`,multiple`/`format:units` map our structs cleanly. Rough edge: single-line `;` node separators error — use newlines | compile → `Decode(&struct)` or unify-with-schema; defaults auto-apply; closed structs catch unknown fields; a bit more Go ceremony |
| Schema/validation | none built in; a Go validation pass in `config` supplies it | built-in & strong: types, `!=""` constraints, closed structs, defaults |
| Typo: misspelled key | `no struct field … node "poll-intervall"` — names node, **no line/col** | `config.pollIntervall: field not allowed` + `errors.Details` gives `file:L:C` — path **and** exact location |
| Typo: wrong type | `poll-interval: time: invalid duration "go test"` — names field+value, **no line/col** | `conflicting values 10 and string (mismatched types int and string): config.cue:4:16` — path, both types, source position (caveat: a `\|`-disjunction default masks type errors → handle defaults in Go) |
| Repo-side file for strangers | `check "test" { command "go" "test" "./..." }` — reads like config, minimal punctuation, order preserved. **Most legible** | list/map-of-structs; cleaner in map form but still code-shaped; ordered decode needs `Value.Fields()` |

**Recommendation (planner): KDL for both files in phase 1.** Rationale in the project's value order (clarity, simplicity, concision, maintainability, consistency): (1) the repo-side `.gauntlet.kdl` is the **adoption surface** — every team writes it — and KDL's node syntax is decisively the most legible; CUE's one real weakness (concept load) lands on exactly this most-written file. (2) One language / one dependency / one mental model beats a split for a new tool. (3) KDL's weaknesses are mitigable+isolated: thin validation → the Go validation pass with field-named errors; maturity risk → the `config` package isolation makes a swap to CUE proven-cheap (identical target structs), so it's reversible.

Alternatives documented for the record: **split** (CUE for the admin file, KDL for the repo file — the libs' weaknesses are complementary) and **CUE-both** (maximal maturity/validation). Rejected in review: see §9.8.

---

## 8. Invariant → mechanism map (the review checklist)

1. **Land exactly the tested SHA** — `CommitTree` builds `mergeOID` once; the executor tests that exact export; `CASUpdate(target, base, mergeOID)` pushes that same OID. Never re-merged.
2. **CAS everywhere** — every target push is `--force-with-lease=<ref>:<baseOID>`; stale → `ErrCASStale` → Skipped + rebuild. Verified: wrong old-OID rejected.
3. **Slot deletion is CAS** — `CASUpdate(candRef, cand.SHA, "")`; if re-pushed mid-test the delete is stale → slot survives at new SHA and re-queues. Verified.
4. **Reconcile idempotent** — in-flight state is only `(cand, tested SHA, runID)`; no persistence; recovery = rescan refs; `IsAncestor` heals a crash between land and delete; duplicate daemon resolved by CAS.
5. **Ref moves mid-test detected** — every tick re-reads ground truth; `cand.SHA` change (or target move) cancels the running check (process-group kill, §9.5), discards the verdict, re-queues.
6. **Never rewrite candidate commits** — the *only* object created is the merge commit; candidate SHA appears verbatim as `parent[1]`. No rebase/squash/amend anywhere.
7. **Cache escape hatch** — architected-for, not active: `CheckJob.Clean` reserved (always false in phase 1); no warm cache to poison and no command channel to trigger it yet. Phase 2/4.
8. **Core is executor/channel-agnostic** — `queue` imports only `core`+`obs` (interfaces); adding Slack/Actions/containers touches no core logic. Enforced by package boundaries.

---

## 9. Review amendments (authoritative)

Applied by the design reviewer after full-plan review. Where these touch earlier sections, **§9 wins** (earlier sections have been edited to match; this section records the rulings and rationale).

1. **`done` is a park list, sticky per (ref, SHA).** An entry means "this SHA received a verdict (Rejected/Conflict/Error); do not re-test it." Entries clear ONLY when the ref's SHA changes (re-push) or the ref vanishes. Landing some *other* candidate never clears parks — otherwise one sleeping author's red branch re-runs and re-pings on every landing. Phase-1 retry = push a new SHA (amend suffices); a channel `retry` command (phase 2) will clear parks explicitly. (Restart clears parks as a side effect of no persistence — acceptable, documented.)
2. **Exec-start failure is a verdict, not infra.** `Command[0]` missing/not-executable → `CheckFailed` with explanatory Output: it's the author's spec bug and must park, not retry forever. `CheckResult.Err` is reserved for daemon-caused conditions (ctx cancellation, executor I/O failures like tempdir creation). `OutcomeError` also parks the (ref, SHA) — distinct event so operators can tell infra from red — no unbounded retry loops in phase 1; backoff/auto-retry is phase 2.
3. **Ref grammar.** `refs/heads/for/<target>/<rest>`. If `<rest>` has ≥2 segments: `user` = first, `topic` = remainder verbatim (slashes allowed: `for/main/alice/feat/foo` → user `alice`, topic `feat/foo`). Single segment: `user` = "", `topic` = the segment (solo setups). The default merge-message template degrades to `Merge <topic>` when user is empty. Target names may not contain `/` (validated in config); the target's *branch* may.
4. **Run-ID scheme.** `<UTC yyyymmddThhmmssZ>-<trialTreeOID[:12]>` — unique across restarts with no persistence (same trial re-tested after restart gets a new timestamp), and content-addressed to exactly the tree the checks test. *(Corrected during C6: the original `mergeOID` form was circularly impossible — the run ID is embedded in the merge commit's own message via the `Gauntlet-Run` trailer, and a commit's OID hashes its message. The tree OID is known before the commit is built.)* Pre-trial outcomes (conflict, pre-merge infra errors) use the candidate SHA as the stand-in.
5. **Process-group kill.** `exec.CommandContext` alone kills only the direct child; a cancelled `go test` leaves grandchildren holding the export dir. `LocalExecutor` starts each check in its own process group (`Setpgid`) and on cancel kills the group (negative-pid kill via `Cmd.Cancel`, plus `WaitDelay` so `Wait` can't hang on inherited pipes). Integration test asserts a spawned child dies with the check.
6. **Bounded check output.** `CheckResult.Output` keeps a tail cap of 64 KiB per check (constant in phase 1; configurable later). Full streaming/log storage is a phase-3 concern.
7. **Two test additions** (already merged into the §5 table): candidate ref *deleted* mid-check (cancel + cleanup); target moved detected at the tick (the early-cancel path, distinct from losing the land-time CAS).
8. **Config language decided: KDL for both files.** The planner's adoption-surface argument holds and matches the project's value order; the split option (two config languages in one small tool) buys validation at the cost of permanent "which syntax is this file?" friction, and CUE-both taxes every adopter for a maturity benefit the isolated `config` package already contains. kdl-go's staleness is accepted with the stated mitigations (Go-side validation; swap-cheap structs; vendor/fork as last resort). `DESIGN.md` ledger updated OPEN → KEPT.

---

## Deliberate non-goals (phase 1 does NOT do)

No SQLite/queryable history; no OTLP exporter/OTel SDK (API+no-op only); no parallel checks or configurable short-circuit (sequential, fail-fast); **no path-filter globs in config — conditional execution is the check script's job via the exported env vars**; no containers/warm builder/cache volumes; no Slack/GitHub status·Checks·Actions/dashboard (only LogChannel); no inbound commands wired (`Commands()`/`Command` exist for Inv 8 but nothing produces them); Inv 7 clean-build not triggerable; no speculative pipelining; no rebase/squash/message rewriting; single remote (N targets; multi-repo deferred); no `land` porcelain, post-land hooks, or Claude summaries; no release tooling or CI-for-gauntlet beyond `go test ./...`. Don't gold-plate: no retry/backoff frameworks, metrics beyond OTel spans, config hot-reload, or pluggable ref-naming.
