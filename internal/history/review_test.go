package history

import (
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"
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
