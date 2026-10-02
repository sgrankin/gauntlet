package history

import (
	"context"
	"database/sql"
	_ "embed"
	"github.com/sgrankin/gauntlet/internal/core"
	"path/filepath"
	"testing"
	"time"
)

//go:embed testdata/schema-v14.sql
var schemaV14 string

func TestMigrateV14ReviewIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{schemaV14, `PRAGMA user_version = 14`, `INSERT INTO runs
 (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
 base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms)
 VALUES ('old', 'main', 'review', '', '', 'source', 'base', '', 1, 'rejected', 'failed', 1, 2, 1)`} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row, _, err := s.Run("old")
	if err != nil {
		t.Fatal(err)
	}
	if row.CandidateVersion != "" || row.CandidateSHA != "source" || row.Detail != "failed" {
		t.Fatalf("old row changed during migration: %+v", row)
	}
	verdicts, err := s.LatestTerminalPerRef("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 || verdicts[0].Version != "" {
		t.Fatalf("old verdicts: %+v", verdicts)
	}
}

func TestRetryIdentityResolvesMillisecondTie(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	ref := "refs/heads/for/main/author/change"
	record := sampleRecord("old", "main", now)
	record.Candidate.Ref = ref
	record.Outcome = core.OutcomeRejected
	record.StartedAt = now
	record.EndedAt = now
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Record: record}); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, core.Event{Kind: core.EventRetryRequested, Target: "main", Candidate: record.Candidate, At: now}); err != nil {
		t.Fatal(err)
	}
	verdicts, err := s.LatestTerminalPerRef("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 0 {
		t.Fatal("same-millisecond retry re-parked old failure")
	}
	record.RunID = "a-retry"
	record.Detail = "new failure"
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Record: record}); err != nil {
		t.Fatal(err)
	}
	verdicts, err = s.LatestTerminalPerRef("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 || verdicts[0].RunID != "a-retry" {
		t.Fatalf("new failure did not park: %+v", verdicts)
	}
}

func TestMigrationFailureRollsBackAllSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{schemaV14, `PRAGMA user_version = 14`, `DROP TABLE retry_intents`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db); err == nil {
		t.Fatal("broken historical schema migrated")
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 14 {
		t.Fatalf("partial migration version=%d", version)
	}
	if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN candidate_version TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal("failed migration left partial column", err)
	}
	if _, err := db.Exec(`ALTER TABLE runs DROP COLUMN candidate_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE retry_intents (target TEXT NOT NULL,ref TEXT NOT NULL,sha TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(target,ref))`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal("retry after repair failed", err)
	}
}
