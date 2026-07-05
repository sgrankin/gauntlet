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
// The connection is single-writer by construction: WAL journaling, a 5s busy
// timeout, and foreign keys are set via DSN pragmas, and the pool is capped
// to one connection (modernc.org/sqlite is a single-conn-per-writer driver;
// see docs/plans/phase23.md §1 Spike A). Schema versioning uses PRAGMA
// user_version with no migration framework: version 0 means "apply
// schema.sql and stamp version 1".
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{
		db:   db,
		cmds: make(chan core.Command),
	}, nil
}

// migrate applies schema.sql exactly once, gated by PRAGMA user_version.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("history: read user_version: %w", err)
	}
	if version != 0 {
		return nil
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("history: apply schema: %w", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
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
