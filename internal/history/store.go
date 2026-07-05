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
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is the current PRAGMA user_version. Bump it and add a case
// to migrate's switch whenever schema.sql changes.
const schemaVersion = 5

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
// reconcile goroutine (docs/plans/phase23.md §4.1), so a writer that has to
// wait behind a slow dashboard read (e.g. CheckStats's JOIN) would block
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
//     batch_id/position/batch_size (docs/plans/phase5.md §10 amendment 1,
//     batch-aware CheckStats), stamp user_version=5, loop.
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
// post-land hook result (internal/hooks; log/history parity with checks).
// Every other non-terminal event (Record == nil and not a hook result,
// including a crash-recovered EventLanded whose record didn't survive) is
// silently ignored — Emit never blocks or fails the reconcile loop for
// history-side reasons. Terminal events are written in one transaction: the
// run row and its check rows via INSERT OR REPLACE, keyed on run_id (and
// run_id+seq for checks), so re-emitting the same RunRecord is a no-op
// beyond redundant writes.
func (s *Store) Emit(ctx context.Context, ev core.Event) error {
	// EventHookFinished carries no RunRecord at all (Record is nil on it,
	// same as every other non-terminal event) — its whole payload is the one
	// finished hook in ev.Check, so it's keyed off Kind+Check rather than
	// Record like every other branch here.
	if ev.Kind == core.EventHookFinished && ev.Check != nil {
		return s.writeHookResult(ctx, ev)
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
	); err != nil {
		return fmt.Errorf("history: insert run: %w", err)
	}

	for i, cr := range rec.Checks {
		if _, err := tx.ExecContext(ctx, insertCheckSQL,
			rec.RunID,
			i,
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
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}
