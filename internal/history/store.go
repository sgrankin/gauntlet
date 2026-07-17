// Package history implements gauntlet's SQLite run history: a core.Channel
// that writes one run row plus its per-check rows on every terminal event,
// and a periodically-sampled queue-depth series, alongside read methods the
// dashboard queries. It is output-only on the Channel side (Commands never
// yields, matching channel.LogChannel) and is not the queue's source of
// truth (Invariant 4) — restart still reconstructs in-memory state from git,
// never from this database.
package history

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is the current PRAGMA user_version. Bump it and add a case
// to migrate's switch whenever schema.sql changes.
const schemaVersion = 11

var _ core.Channel = (*Store)(nil)

// Store is a core.Channel backed by a SQLite database: Emit writes terminal
// run records, RecordDepth samples queue depth, and the Recent*/Run/Check*/
// Depth* methods serve read-only dashboard queries.
type Store struct {
	db   *sql.DB
	cmds chan core.Command
}

// Open opens (creating if absent) the SQLite database at path, applies the
// embedded schema if it hasn't been applied yet, and returns a ready Store.
//
// WAL journaling, a 5s busy timeout, and foreign keys are set via DSN
// pragmas. Schema versioning uses PRAGMA user_version with no migration
// framework: version 0 means "apply schema.sql and stamp version 1".
//
// The pool is capped at 4 connections, not 1: Emit runs inline on the
// reconcile goroutine, so a writer that has to wait behind a slow dashboard
// read (e.g. CheckStats's JOIN) would block
// reconcile progress on an unrelated HTTP request. WAL mode is built for
// exactly this — readers never block the writer and the writer never blocks
// readers — so capping at 1 connection only self-inflicted serialization
// that WAL doesn't require. 4 gives the dashboard's concurrent read handlers
// room without letting an unbounded pool exhaust file descriptors; the busy
// timeout above still covers the rare writer-vs-writer contention WAL does
// serialize.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{
		db:   db,
		cmds: make(chan core.Command),
	}, nil
}

// migrate brings db from whatever PRAGMA user_version it's at up to
// schemaVersion, one step at a time, re-reading user_version after every
// step so a database several versions behind actually walks every
// intermediate step (rather than applying only the first matching case) on
// its way to schemaVersion:
//
//   - 0 (fresh database, no tables yet): apply the embedded schema.sql
//     (already at the current shape) and stamp schemaVersion directly —
//     there's no history to step through.
//   - 1 (schema v1: checks has no output column): ALTER TABLE checks ADD
//     COLUMN output, stamp user_version=2, loop.
//   - 2 (schema v2: checks has no log_path column): ALTER TABLE checks ADD
//     COLUMN log_path, stamp user_version=3, loop.
//   - 3 (schema v3: no hooks table): CREATE TABLE hooks (log/history parity
//     for post-land hooks, internal/hooks), stamp user_version=4, loop.
//   - 4 (schema v4: runs has no batch columns): ALTER TABLE runs ADD COLUMN
//     batch_id/position/batch_size (batch-aware CheckStats), stamp
//     user_version=5, loop.
//   - 5 (schema v5: no retry_intents/ignored_refs/hook_runs tables): CREATE
//     TABLE all three (a persisted retry intent, a durable ignored-ref
//     capture, and a durable hook owed/skipped marker), stamp
//     user_version=6, loop.
//   - 6 (schema v6: runs has no speculated/recovered columns): ALTER TABLE
//     runs ADD COLUMN speculated/recovered (core.RunRecord.Speculated/
//     Recovered persistence, closing the "announced live, dropped at
//     landing" gap), stamp user_version=7, loop.
//   - 7 (schema v7: checks has no command column): ALTER TABLE checks ADD
//     COLUMN command (run.html's command echo, core.CheckResult.Command),
//     stamp user_version=8, loop.
//   - 8 (schema v8: checks has no blocked_by/waited_ms columns): ALTER
//     TABLE checks ADD COLUMN blocked_by (comma-joined prerequisite names
//     for a CheckBlocked row) and waited_ms (CheckResult.Waited — slot-
//     starvation wait, distinct from duration), stamp user_version=9, loop.
//   - 9 (schema v9: checks has no image column): ALTER TABLE checks ADD
//     COLUMN image (the immutable candidate-built image identity a build
//     node produced / a consumer ran in — CheckResult.Image), stamp
//     user_version=10, loop.
//   - 10 (schema v10: checks has no materialize_ms column): ALTER TABLE
//     checks ADD COLUMN materialize_ms (isolated-workspace materialization
//     cost — CheckResult.Materialized, issue #9), stamp user_version=11,
//     loop.
//   - schemaVersion: already current, no-op.
//
// Add new cases above the schemaVersion case, oldest first, when schema.sql
// next changes — each new case stamps the version it upgrades *to* and lets
// the loop re-examine, rather than assuming it's the last step needed.
func migrate(db *sql.DB) error {
	for {
		var version int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("history: read user_version: %w", err)
		}

		switch version {
		case 0:
			if _, err := db.Exec(schemaSQL); err != nil {
				return fmt.Errorf("history: apply schema: %w", err)
			}
			if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
				return fmt.Errorf("history: set user_version: %w", err)
			}
			return nil
		case 1:
			if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN output TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("history: migrate v1->v2 (checks.output): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
				return fmt.Errorf("history: set user_version=2: %w", err)
			}
		case 2:
			if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN log_path TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("history: migrate v2->v3 (checks.log_path): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
				return fmt.Errorf("history: set user_version=3: %w", err)
			}
		case 3:
			if _, err := db.Exec(`
CREATE TABLE hooks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',
  log_path    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
)`); err != nil {
				return fmt.Errorf("history: migrate v3->v4 (hooks table): %w", err)
			}
			if _, err := db.Exec(`CREATE INDEX idx_hooks_name ON hooks(name)`); err != nil {
				return fmt.Errorf("history: migrate v3->v4 (hooks index): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
				return fmt.Errorf("history: set user_version=4: %w", err)
			}
		case 4:
			if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN batch_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("history: migrate v4->v5 (runs.batch_id): %w", err)
			}
			if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN position INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("history: migrate v4->v5 (runs.position): %w", err)
			}
			if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN batch_size INTEGER NOT NULL DEFAULT 1`); err != nil {
				return fmt.Errorf("history: migrate v4->v5 (runs.batch_size): %w", err)
			}
			if _, err := db.Exec(`CREATE INDEX idx_runs_batch_id ON runs(batch_id) WHERE batch_id != ''`); err != nil {
				return fmt.Errorf("history: migrate v4->v5 (runs.batch_id index): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
				return fmt.Errorf("history: set user_version=5: %w", err)
			}
		case 5:
			if _, err := db.Exec(`
CREATE TABLE retry_intents (
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  sha    TEXT NOT NULL,
  at     INTEGER NOT NULL,
  PRIMARY KEY (target, ref)
)`); err != nil {
				return fmt.Errorf("history: migrate v5->v6 (retry_intents table): %w", err)
			}
			if _, err := db.Exec(`
CREATE TABLE ignored_refs (
  at     INTEGER NOT NULL,
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (at, target, ref)
)`); err != nil {
				return fmt.Errorf("history: migrate v5->v6 (ignored_refs table): %w", err)
			}
			if _, err := db.Exec(`
CREATE TABLE hook_runs (
  run_id      TEXT PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
  target      TEXT NOT NULL,
  owed_count  INTEGER NOT NULL,
  started_at  INTEGER NOT NULL,
  skipped     INTEGER NOT NULL DEFAULT 0,
  skip_reason TEXT NOT NULL DEFAULT ''
)`); err != nil {
				return fmt.Errorf("history: migrate v5->v6 (hook_runs table): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 6`); err != nil {
				return fmt.Errorf("history: set user_version=6: %w", err)
			}
		case 6:
			if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN speculated INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("history: migrate v6->v7 (runs.speculated): %w", err)
			}
			if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN recovered INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("history: migrate v6->v7 (runs.recovered): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
				return fmt.Errorf("history: set user_version=7: %w", err)
			}
		case 7:
			if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN command TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("history: migrate v7->v8 (checks.command): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 8`); err != nil {
				return fmt.Errorf("history: set user_version=8: %w", err)
			}
		case 8:
			if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN blocked_by TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("history: migrate v8->v9 (checks.blocked_by): %w", err)
			}
			if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN waited_ms INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("history: migrate v8->v9 (checks.waited_ms): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 9`); err != nil {
				return fmt.Errorf("history: set user_version=9: %w", err)
			}
		case 9:
			if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN image TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("history: migrate v9->v10 (checks.image): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 10`); err != nil {
				return fmt.Errorf("history: set user_version=10: %w", err)
			}
		case 10:
			if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN materialize_ms INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("history: migrate v10->v11 (checks.materialize_ms): %w", err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 11`); err != nil {
				return fmt.Errorf("history: set user_version=11: %w", err)
			}
		case schemaVersion:
			return nil
		default:
			return fmt.Errorf("history: unknown user_version %d (want 0..%d)", version, schemaVersion)
		}
	}
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Emit writes ev's carried RunRecord, or — for EventHookFinished — one
// post-land hook result (internal/hooks; log/history parity with checks), or
// — for EventRetryRequested/EventIgnoredRef/EventHookStarted/
// EventHookSkipped — one of the v6 durability rows. Every
// other non-terminal event (Record == nil and none of the above, including a
// crash-recovered EventLanded whose record didn't survive) is silently
// ignored — Emit never blocks or fails the reconcile loop for history-side
// reasons. Terminal events are written in one transaction: the run row and
// its check rows via INSERT OR REPLACE, keyed on run_id (and run_id+seq for
// checks), so re-emitting the same RunRecord is a no-op beyond redundant
// writes.
func (s *Store) Emit(ctx context.Context, ev core.Event) error {
	switch ev.Kind {
	case core.EventHookFinished:
		// EventHookFinished carries no RunRecord at all (Record is nil on
		// it, same as every other non-terminal event) — its whole payload
		// is the one finished hook in ev.Check, so it's keyed off Kind+Check
		// rather than Record like every other branch here.
		if ev.Check != nil {
			return s.writeHookResult(ctx, ev)
		}
		return nil
	case core.EventRetryRequested:
		return s.writeRetryIntent(ctx, ev)
	case core.EventIgnoredRef:
		return s.writeIgnoredRef(ctx, ev)
	case core.EventHookStarted:
		return s.writeHookStarted(ctx, ev)
	case core.EventHookSkipped:
		return s.writeHookSkipped(ctx, ev)
	}
	if ev.Record == nil {
		return nil
	}
	return s.writeRecord(ctx, ev.Record)
}

// Commands returns a channel that never yields, matching
// channel.LogChannel's documented zero-command behavior: a real channel
// value, created once and closed over here, that nothing ever sends on or
// closes. The Store is output-only.
func (s *Store) Commands() <-chan core.Command {
	return s.cmds
}

func (s *Store) writeRecord(ctx context.Context, rec *core.RunRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("history: begin tx: %w", err)
	}
	defer tx.Rollback()

	started := rec.StartedAt.UnixMilli()
	ended := rec.EndedAt.UnixMilli()
	duration := rec.EndedAt.Sub(rec.StartedAt).Milliseconds()

	if _, err := tx.ExecContext(ctx, insertRunSQL,
		rec.RunID,
		rec.Target,
		rec.Candidate.Ref,
		rec.Candidate.User,
		rec.Candidate.Topic,
		rec.Candidate.SHA,
		rec.BaseOID,
		rec.MergeSHA,
		boolToInt(rec.Trial.Clean),
		outcomeString(rec.Outcome),
		rec.Detail,
		started,
		ended,
		duration,
		rec.BatchID,
		rec.Position,
		batchSizeOrDefault(rec.BatchSize),
		boolToInt(rec.Speculated),
		boolToInt(rec.Recovered),
	); err != nil {
		return fmt.Errorf("history: insert run: %w", err)
	}

	// Per-check data is captured exclusively from the terminal RunRecord's
	// own Checks slice, right here, never as a per-event row of its own:
	// Store.Emit's Record == nil branch (above) already drops
	// EventCheckStarted/EventCheckFinished before writeRecord is ever
	// called, and by the time a run's terminal event arrives, rec.Checks
	// already holds every check that ran, in order — so there is nothing a
	// per-event check row would capture that this one transaction doesn't
	// already write. Deliberate and correct by construction, not a gap:
	// mirrors ghstatus's and hooks.Runner's own documented "ignores this
	// event kind on purpose" stance.
	for i, cr := range rec.Checks {
		// seq is the check's spec-declaration position: CheckResult.Seq
		// (1-based, stamped by the queue's materialization) when present,
		// else the slice index — identical for any contiguous spec-prefix
		// record (every pre-Seq record; every hand-built test record), and
		// only Seq keeps gapped records (an externally concluded parallel
		// run) aligned with the `<seq>-<name>.log.zst` filename prefix.
		seq := i
		if cr.Seq > 0 {
			seq = cr.Seq - 1
		}
		if _, err := tx.ExecContext(ctx, insertCheckSQL,
			rec.RunID,
			seq,
			cr.Name,
			checkStatusString(cr.Status),
			cr.Duration.Milliseconds(),
			errString(cr.Err),
			// Stored verbatim for every check regardless of status: green
			// output is diagnostics too ("is it actually doing the thing").
			// The executor's 64KiB tail cap (executor.outputCap) is the only
			// bound — no history-side re-cap.
			cr.Output,
			// LogPath is "" whenever no full log file was written (no
			// Config.LogDir configured, or the file couldn't be created) —
			// see core.CheckResult.LogPath's doc for the log-less fallback.
			cr.LogPath,
			joinCommand(cr.Command),
			// blocked_by: comma-joined (names are validated non-empty and
			// comma-free is not guaranteed, but names never contain commas
			// in practice; the canonical structured source remains the
			// terminal event's Record) — set only on CheckBlocked rows.
			strings.Join(cr.BlockedBy, ","),
			cr.Waited.Milliseconds(),
			cr.Image,
			cr.Materialized.Milliseconds(),
		); err != nil {
			return fmt.Errorf("history: insert check: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("history: commit: %w", err)
	}
	return nil
}

// writeHookResult inserts one row into hooks for a finished post-land hook
// (core.EventHookFinished, internal/hooks). Unlike checks — written all at
// once from RunRecord.Checks when the run's terminal event arrives — each
// hook result arrives as its own event with no RunRecord at all (Record is
// nil on EventHookFinished) and no seq of its own: core.Event carries only
// CheckName, not a position. seq is instead derived by counting hooks
// already recorded for ev.RunID inside this same transaction.
//
// That count-in-tx approach is only correct because hooks.Runner guarantees
// at most one landing's hooks run at a time for any given target (its own
// doc: "hooks always run serially"), and every hook's EventHookFinished for
// one landing is emitted synchronously, in order, before the next hook
// starts (internal/hooks/hooks.go's runLanding) — so by construction there
// is never a second writer racing this count for the same run_id, and
// re-emitting the same event twice (which normal operation never does) is
// the one case this would double-count rather than replace; INSERT OR
// REPLACE here guards against exact-same-seq collisions, not against that.
//
// GUARD: the safety margin here is actually stronger than the
// paragraph above states — hooks.Runner.Run drains a single global landing
// queue in one goroutine (internal/hooks/hooks.go), so hook execution is
// serial across every target at once, not merely "one landing per target at
// a time". Either invariant alone is enough to make this count-in-tx seq
// safe today, but if hook execution is ever sharded or parallelized (e.g.
// one Runner goroutine per target, or a concurrent backlog drain within
// PolicyQueue), this COUNT(*) can race across two writers for the same
// run_id and must be replaced with an explicit sequence column instead.
//
// ev.RunID is expected to already have a matching runs row by the time this
// runs: hooks only ever fire off an EventLanded whose Record history's own
// Emit branch (writeRecord, above) already wrote synchronously — before
// hooks.Runner's async queue even processes that same event — because
// queue.Daemon's emit (internal/queue/daemon.go) calls every configured
// channel's Emit in registration order, and cmd/gauntlet/main.go registers
// the history store ahead of the hooks Runner. The hooks.run_id foreign key
// should therefore never fail in practice; a run_id with no matching runs
// row (e.g. a hand-built event in a test) surfaces as an ordinary FK
// constraint error here, not a panic.
func (s *Store) writeHookResult(ctx context.Context, ev core.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("history: begin tx: %w", err)
	}
	defer tx.Rollback()

	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hooks WHERE run_id = ?`, ev.RunID).Scan(&seq); err != nil {
		return fmt.Errorf("history: count hooks for %s: %w", ev.RunID, err)
	}

	cr := ev.Check
	if _, err := tx.ExecContext(ctx, insertHookSQL,
		ev.RunID,
		seq,
		ev.CheckName,
		checkStatusString(cr.Status),
		cr.Duration.Milliseconds(),
		errString(cr.Err),
		cr.Output,
		cr.LogPath,
	); err != nil {
		return fmt.Errorf("history: insert hook: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("history: commit: %w", err)
	}
	return nil
}

// writeRetryIntent upserts one retry_intents row (core.EventRetryRequested):
// the latest retry for (target, ref) always wins — INSERT ... ON
// CONFLICT(target, ref) DO UPDATE — since only the most recent retry's
// timestamp matters to LatestTerminalPerRef's seed-park suppression (a
// retry superseded by a later one is no longer the operative "don't re-park
// until at least this terminal" boundary).
func (s *Store) writeRetryIntent(ctx context.Context, ev core.Event) error {
	if _, err := s.db.ExecContext(ctx, upsertRetryIntentSQL,
		ev.Target, ev.Candidate.Ref, ev.Candidate.SHA, ev.At.UnixMilli(),
	); err != nil {
		return fmt.Errorf("history: write retry intent: %w", err)
	}
	return nil
}

// writeIgnoredRef inserts one ignored_refs row (core.EventIgnoredRef): a
// well-formed candidate ref whose target segment names no configured
// target. Unlike retry_intents, this is a pure append (INSERT OR REPLACE
// keyed on (at, target, ref), never an update) — every occurrence is its own
// durable fact, so an operator can discover a misconfiguration after the
// fact even if it happened between two dashboard visits.
func (s *Store) writeIgnoredRef(ctx context.Context, ev core.Event) error {
	if _, err := s.db.ExecContext(ctx, insertIgnoredRefSQL,
		ev.At.UnixMilli(), ev.Target, ev.Candidate.Ref, ev.Detail,
	); err != nil {
		return fmt.Errorf("history: write ignored ref: %w", err)
	}
	return nil
}

// writeHookStarted upserts hook_runs' "owed" row for ev.RunID on the FIRST
// core.EventHookStarted hooks.Runner emits for a landing: ON
// CONFLICT(run_id) DO NOTHING means every subsequent hook in the same
// landing's chain is a no-op here — only the first hook's owed_count/
// started_at are ever recorded, which is exactly right since owed_count is
// the landing's total configured hook count (ev.HookCount), constant across
// every hook of the same landing.
func (s *Store) writeHookStarted(ctx context.Context, ev core.Event) error {
	if _, err := s.db.ExecContext(ctx, upsertHookRunStartedSQL,
		ev.RunID, ev.Target, ev.HookCount, ev.At.UnixMilli(),
	); err != nil {
		return fmt.Errorf("history: write hook run started: %w", err)
	}
	return nil
}

// writeHookSkipped upserts hook_runs' row for ev.RunID on
// core.EventHookSkipped (a recovery-synthesized landing whose hooks were
// never run at all): skipped=1 and skip_reason=ev.Detail mark it so
// HookRunSummaries never mistakes "deliberately never attempted" for
// "crashed mid-chain" (owed_count > done_count). ON CONFLICT(run_id) DO
// UPDATE rather than DO NOTHING: unlike writeHookStarted, a landing's own
// RunID is unique per recoverLanded call (queue/reconcile.go mints a fresh
// one), so no prior hook_runs row is expected here in practice — the update
// path exists only for re-emission idempotency, matching every other Emit
// branch's re-emit-is-a-no-op-beyond-redundant-writes contract.
func (s *Store) writeHookSkipped(ctx context.Context, ev core.Event) error {
	if _, err := s.db.ExecContext(ctx, upsertHookRunSkippedSQL,
		ev.RunID, ev.Target, ev.HookCount, ev.At.UnixMilli(), ev.Detail,
	); err != nil {
		return fmt.Errorf("history: write hook run skipped: %w", err)
	}
	return nil
}

// RecordDepth appends one sample to the queue_depth series. Idempotent per
// (at, target): a re-sample at the same instant replaces the prior row.
func (s *Store) RecordDepth(at time.Time, target string, waiting, inFlight, parked int) error {
	_, err := s.db.Exec(insertDepthSQL, at.UnixMilli(), target, waiting, inFlight, parked)
	if err != nil {
		return fmt.Errorf("history: record depth: %w", err)
	}
	return nil
}

// PruneDepth deletes queue_depth samples recorded before cutoff. This is
// retention for the depth series only: runs and checks are never pruned by
// this or any other Store method, deliberately — they're gauntlet's
// audit-quality historical record of what actually happened, while
// queue_depth exists purely to feed recent-trend charts and is the one table
// that grows in proportion to wall-clock time rather than to actual queue
// activity, so it's the one that needs a retention bound.
func (s *Store) PruneDepth(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM queue_depth WHERE at < ?`, cutoff.UnixMilli())
	if err != nil {
		return fmt.Errorf("history: prune depth: %w", err)
	}
	return nil
}

// PruneIgnoredRefs deletes ignored_refs rows recorded before cutoff: unlike
// queue_depth, ignored_refs had no retention at all — it's a pure append
// (writeIgnoredRef's INSERT OR REPLACE never
// updates an existing row, every occurrence is its own durable fact), and
// the in-memory dedup in checkIgnoredRefs (internal/queue/reconcile.go) only
// suppresses repeat emissions within a single process's lifetime, not
// across restarts or across distinct SHAs on a chronically misconfigured
// ref. Left unbounded, it's the one other table (besides queue_depth) that
// can grow in proportion to wall-clock time/restart count rather than to
// real merge activity, so it gets the same retention treatment: called
// alongside PruneDepth, with the same depth-retention window
// (cfg.History.DepthRetention), from cmd/gauntlet's depth sampler. Like
// PruneDepth (and unlike runs/checks), this is a best-effort trend/discovery
// aid, not part of gauntlet's audit-quality historical record.
func (s *Store) PruneIgnoredRefs(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM ignored_refs WHERE at < ?`, cutoff.UnixMilli())
	if err != nil {
		return fmt.Errorf("history: prune ignored refs: %w", err)
	}
	return nil
}

// batchSizeOrDefault normalizes RunRecord.BatchSize for storage: callers that
// never touch batching (serial's tryStartTrial/rejectRun/rejectPreMerge/
// recoverLanded, internal/queue) leave BatchSize at its Go zero value, but
// the documented semantics (core.RunRecord.BatchSize's doc, schema.sql's
// column default) are "1 otherwise" — a lone run is a batch of one. Only an
// explicit BatchSize <= 0 gets this treatment; a real batch always sets it
// to len(members) >= 1 already (internal/queue/reconcile.go).
func batchSizeOrDefault(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// joinCommand renders a check's argv (core.CheckResult.Command) as one
// display string for run.html's command echo: plain space-joining, not
// shell-quoted — this is a read-only echo of what ran, never re-parsed or
// re-executed, so an argument containing a space renders ambiguously but
// harmlessly rather than needing real shell-quoting machinery. Nil/empty
// (a result built by hand without going through queue/reconcile.go's
// startCheck, or a pre-v8 row) yields "", which run.html treats as "render
// no echo line" rather than a blank one.
func joinCommand(argv []string) string {
	return strings.Join(argv, " ")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func outcomeString(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "landed"
	case core.OutcomeRejected:
		return "rejected"
	case core.OutcomeConflict:
		return "conflict"
	case core.OutcomeSkipped:
		return "skipped"
	case core.OutcomeError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

func checkStatusString(s core.CheckStatus) string {
	switch s {
	case core.CheckPassed:
		return "passed"
	case core.CheckFailed:
		return "failed"
	case core.CheckSkipped:
		return "skipped"
	case core.CheckBlocked:
		return "blocked"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}
