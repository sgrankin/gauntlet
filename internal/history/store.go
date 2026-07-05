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
const schemaVersion = 2

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
// schemaVersion, one step at a time:
//
//   - 0 (fresh database, no tables yet): apply the embedded schema.sql
//     (already at the current shape) and stamp schemaVersion directly —
//     there's no history to step through.
//   - 1 (schema v1: checks has no output column): ALTER TABLE checks ADD
//     COLUMN output, then fall through to stamp schemaVersion.
//   - schemaVersion: already current, no-op.
//
// Each case falls through to the next so a database several versions behind
// walks every intermediate step before landing on schemaVersion. Add new
// cases above the schemaVersion case, oldest first, when schema.sql next
// changes.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("history: read user_version: %w", err)
	}

	switch version {
	case 0:
		if _, err := db.Exec(schemaSQL); err != nil {
			return fmt.Errorf("history: apply schema: %w", err)
		}
	case 1:
		if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN output TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("history: migrate v1->v2 (checks.output): %w", err)
		}
	case schemaVersion:
		return nil
	default:
		return fmt.Errorf("history: unknown user_version %d (want 0..%d)", version, schemaVersion)
	}

	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("history: set user_version: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Emit writes ev's carried RunRecord. Non-terminal events (Record == nil,
// including a crash-recovered EventLanded whose record didn't survive) are
// silently ignored — Emit never blocks or fails the reconcile loop for
// history-side reasons. Terminal events are written in one transaction: the
// run row and its check rows via INSERT OR REPLACE, keyed on run_id (and
// run_id+seq for checks), so re-emitting the same RunRecord is a no-op
// beyond redundant writes.
func (s *Store) Emit(ctx context.Context, ev core.Event) error {
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
		); err != nil {
			return fmt.Errorf("history: insert check: %w", err)
		}
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
