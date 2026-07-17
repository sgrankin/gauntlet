package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func sampleRecord(runID, target string, started time.Time) *core.RunRecord {
	return &core.RunRecord{
		RunID:  runID,
		Target: target,
		Candidate: core.Candidate{
			Ref:    "refs/heads/for/" + target + "/alice/feat",
			Target: target,
			User:   "alice",
			Topic:  "feat",
			SHA:    "cafef00dcafef00dcafef00dcafef00dcafef00d",
		},
		BaseOID:  "base000000000000000000000000000000000000",
		MergeSHA: "merge0000000000000000000000000000000000",
		Trial:    core.TrialMerge{Clean: true, TreeOID: "tree000000000000000000000000000000000000"},
		Checks: []core.CheckResult{
			{Name: "lint", Status: core.CheckPassed, Duration: 500 * time.Millisecond, Output: "all clean"},
			{Name: "test", Status: core.CheckFailed, Duration: 2500 * time.Millisecond, Output: "boom"},
		},
		Outcome:   core.OutcomeRejected,
		Detail:    "test failed",
		StartedAt: started,
		EndedAt:   started.Add(3 * time.Second),
	}
}

func TestOpen_AppliesSchemaOnceAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := s1.RecordDepth(time.Now(), "main", 1, 1, 0); err != nil {
		t.Fatalf("RecordDepth: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening an already-migrated database must not error (user_version
	// gate must skip re-applying schema.sql, which would fail on the
	// already-existing tables).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer s2.Close()

	points, err := s2.DepthSeries("main", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("DepthSeries: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("DepthSeries after reopen = %d points, want 1", len(points))
	}
}

// schemaV1SQL is schema.sql as it existed before chunk E1 added checks.output
// (schema.sql's own header now describes the *current* shape, not this one).
// Kept here, verbatim, so TestMigrate_V1ToV2 can build a genuine v1 database
// by hand and prove the running code migrates it forward correctly.
const schemaV1SQL = `
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);
`

// TestMigrate_V1ToV2 builds a v1 database by hand (schemaV1SQL, no
// checks.output column, user_version stamped to 1 — exactly what an
// already-deployed v1 Store would have on disk) and confirms that opening it
// with the current code migrates it in place: user_version lands on
// schemaVersion, the pre-existing row survives with the new output column
// readable (empty, since it predates the column), and a fresh write against
// the migrated database round-trips output correctly.
func TestMigrate_V1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV1SQL); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("stamp user_version=1: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000)`,
	); err != nil {
		t.Fatalf("seed v1 run: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO checks (run_id, seq, name, status, duration_ms, err) VALUES ('run-old', 0, 'lint', 'passed', 500, '')`,
	); err != nil {
		t.Fatalf("seed v1 check: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v1 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	run, checks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if run.RunID != "run-old" {
		t.Errorf("Run() after migrate = %+v", run)
	}
	if len(checks) != 1 || checks[0].Output != "" {
		t.Errorf("checks after migrate = %+v, want 1 check with empty Output", checks)
	}

	// A fresh write against the migrated database must exercise the new
	// output column correctly.
	ctx := context.Background()
	rec := sampleRecord("run-new", "main", time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "run-new", Record: rec}); err != nil {
		t.Fatalf("Emit after migrate: %v", err)
	}
	_, newChecks, err := s.Run("run-new")
	if err != nil {
		t.Fatalf("Run(run-new): %v", err)
	}
	if len(newChecks) != 2 || newChecks[1].Output != "boom" {
		t.Errorf("Run(run-new) checks = %+v, want checks[1].Output = %q", newChecks, "boom")
	}
}

// schemaV2SQL is schema.sql as it existed before chunk F-b added
// checks.log_path: checks has output but not log_path. Kept here, verbatim,
// so TestMigrate_V2ToV3 can build a genuine v2 database by hand and prove
// the running code migrates it forward correctly.
const schemaV2SQL = `
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);
`

// TestMigrate_V2ToV3 builds a v2 database by hand (schemaV2SQL, no
// checks.log_path column, user_version stamped to 2 — exactly what an
// already-deployed v2 Store would have on disk) and confirms that opening it
// with the current code migrates it in place: user_version lands on
// schemaVersion, the pre-existing row survives with the new log_path column
// readable (empty, since it predates the column), and a fresh write against
// the migrated database round-trips LogPath correctly.
func TestMigrate_V2ToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV2SQL); err != nil {
		t.Fatalf("apply v2 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("stamp user_version=2: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000)`,
	); err != nil {
		t.Fatalf("seed v2 run: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO checks (run_id, seq, name, status, duration_ms, err, output) VALUES ('run-old', 0, 'lint', 'passed', 500, '', 'clean')`,
	); err != nil {
		t.Fatalf("seed v2 check: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v2 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	_, checks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if len(checks) != 1 || checks[0].Output != "clean" || checks[0].LogPath != "" {
		t.Errorf("checks after migrate = %+v, want 1 check with Output=clean LogPath=\"\"", checks)
	}

	// A fresh write against the migrated database must exercise the new
	// log_path column correctly.
	ctx := context.Background()
	rec := sampleRecord("run-new", "main", time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	rec.Checks[0].LogPath = "/var/lib/gauntlet/logs/run-new/lint.log"
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "run-new", Record: rec}); err != nil {
		t.Fatalf("Emit after migrate: %v", err)
	}
	_, newChecks, err := s.Run("run-new")
	if err != nil {
		t.Fatalf("Run(run-new): %v", err)
	}
	if len(newChecks) != 2 || newChecks[0].LogPath != "/var/lib/gauntlet/logs/run-new/lint.log" {
		t.Errorf("Run(run-new) checks = %+v, want checks[0].LogPath = %q", newChecks, "/var/lib/gauntlet/logs/run-new/lint.log")
	}
}

// schemaV3SQL is schema.sql as it existed before chunk P5-B added the hooks
// table: checks has both output and log_path, but there is no hooks table
// at all. Kept here, verbatim, so TestMigrate_V3ToV4 can build a genuine v3
// database by hand and prove the running code migrates it forward
// correctly.
const schemaV3SQL = `
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',
  log_path    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);
`

// TestMigrate_V3ToV4 builds a v3 database by hand (schemaV3SQL, no hooks
// table at all, user_version stamped to 3 — exactly what an
// already-deployed v3 Store would have on disk) and confirms that opening it
// with the current code migrates it in place: user_version lands on
// schemaVersion, the pre-existing run/check rows survive untouched, and both
// a fresh hook write (Emit with an EventHookFinished) and Hooks() round-trip
// correctly against the newly-created table.
func TestMigrate_V3ToV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV3SQL); err != nil {
		t.Fatalf("apply v3 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("stamp user_version=3: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000)`,
	); err != nil {
		t.Fatalf("seed v3 run: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO checks (run_id, seq, name, status, duration_ms, err, output, log_path)
		 VALUES ('run-old', 0, 'lint', 'passed', 500, '', 'clean', '')`,
	); err != nil {
		t.Fatalf("seed v3 check: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v3 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	run, checks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if run.RunID != "run-old" || len(checks) != 1 || checks[0].Output != "clean" {
		t.Errorf("Run(run-old) after migrate = %+v / %+v, want the pre-existing row untouched", run, checks)
	}

	// The hooks table must exist and be usable now: no rows yet for
	// run-old (it predates hooks entirely).
	hooks, err := s.Hooks("run-old")
	if err != nil {
		t.Fatalf("Hooks(run-old) after migrate: %v", err)
	}
	if len(hooks) != 0 {
		t.Errorf("Hooks(run-old) after migrate = %+v, want none", hooks)
	}

	// A fresh hook write against the migrated database must round-trip
	// correctly.
	ctx := context.Background()
	ev := core.Event{
		Kind: core.EventHookFinished, Target: "main", RunID: "run-old", CheckName: "deploy",
		Check: &core.CheckResult{Status: core.CheckPassed, Duration: 250 * time.Millisecond, Output: "deployed", LogPath: "/var/lib/gauntlet/logs/run-old/hook-1-deploy.log.zst"},
	}
	if err := s.Emit(ctx, ev); err != nil {
		t.Fatalf("Emit hook after migrate: %v", err)
	}
	hooks, err = s.Hooks("run-old")
	if err != nil {
		t.Fatalf("Hooks(run-old) after hook write: %v", err)
	}
	if len(hooks) != 1 || hooks[0].Name != "deploy" || hooks[0].Status != "passed" ||
		hooks[0].Output != "deployed" || hooks[0].LogPath != "/var/lib/gauntlet/logs/run-old/hook-1-deploy.log.zst" {
		t.Errorf("Hooks(run-old) = %+v, want one passed deploy hook", hooks)
	}
}

// TestMigrate_V1ToV3_MultiHop builds a v1 database (predating both
// checks.output and checks.log_path) and confirms Open walks both
// intermediate steps in one call, landing on schemaVersion — the case
// migrate()'s loop restructuring (chunk F-b) exists to get right: a single
// switch pass over the original version would apply only the v1->v2 step
// and stamp the database straight to schemaVersion without ever adding
// log_path.
func TestMigrate_V1ToV3_MultiHop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV1SQL); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("stamp user_version=1: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v1 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (multi-hop migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after multi-hop migrate = %d, want %d", version, schemaVersion)
	}

	// Both output and log_path columns must be usable now, not just
	// whichever the (buggy) single-pass switch would have added. Uses Emit
	// (rather than raw SQL) so the parent runs row satisfies the checks
	// table's foreign key.
	ctx := context.Background()
	rec := sampleRecord("run-multihop", "main", time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	rec.Checks[0].LogPath = "/var/lib/gauntlet/logs/run-multihop/lint.log"
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-multihop", Record: rec}); err != nil {
		t.Fatalf("Emit exercising both output and log_path columns: %v", err)
	}
	_, checks, err := s.Run("run-multihop")
	if err != nil {
		t.Fatalf("Run(run-multihop): %v", err)
	}
	if len(checks) != 2 || checks[0].Output != "all clean" || checks[0].LogPath != "/var/lib/gauntlet/logs/run-multihop/lint.log" {
		t.Errorf("checks after multi-hop migrate = %+v, want checks[0].Output=%q LogPath=%q",
			checks, "all clean", "/var/lib/gauntlet/logs/run-multihop/lint.log")
	}
}

// schemaV4SQL is schema.sql as it existed before chunk P5-J added the
// batch_id/position/batch_size columns to runs: hooks exists (v4) but runs
// has none of the batch columns. Kept here, verbatim, so
// TestMigrate_V4ToV5 can build a genuine v4 database by hand and prove the
// running code migrates it forward correctly.
const schemaV4SQL = `
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',
  log_path    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

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
);
CREATE INDEX idx_hooks_name ON hooks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);
`

// TestMigrate_V4ToV5 builds a v4 database by hand (schemaV4SQL, runs has no
// batch_id/position/batch_size columns, user_version stamped to 4 — exactly
// what an already-deployed v4 Store would have on disk) and confirms that
// opening it with the current code migrates it in place: user_version lands
// on schemaVersion, the pre-existing run row survives with the new columns
// readable at their documented defaults (batch_id="", position=0,
// batch_size=1), and a fresh batch write against the migrated database
// round-trips the new columns correctly.
func TestMigrate_V4ToV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV4SQL); err != nil {
		t.Fatalf("apply v4 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatalf("stamp user_version=4: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000)`,
	); err != nil {
		t.Fatalf("seed v4 run: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO checks (run_id, seq, name, status, duration_ms, err, output, log_path)
		 VALUES ('run-old', 0, 'lint', 'passed', 500, '', 'clean', '')`,
	); err != nil {
		t.Fatalf("seed v4 check: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v4 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	run, checks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if run.RunID != "run-old" || len(checks) != 1 || checks[0].Output != "clean" {
		t.Errorf("Run(run-old) after migrate = %+v / %+v, want the pre-existing row untouched", run, checks)
	}
	if run.BatchID != "" || run.Position != 0 || run.BatchSize != 1 {
		t.Errorf("Run(run-old).BatchID/Position/BatchSize = %q/%d/%d, want \"\"/0/1 (column defaults)",
			run.BatchID, run.Position, run.BatchSize)
	}

	// A fresh batch write against the migrated database must round-trip the
	// new columns correctly.
	ctx := context.Background()
	rec := sampleRecord("run-new", "main", time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	rec.BatchID = "batch-xyz"
	rec.Position = 1
	rec.BatchSize = 3
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-new", Record: rec}); err != nil {
		t.Fatalf("Emit after migrate: %v", err)
	}
	newRun, _, err := s.Run("run-new")
	if err != nil {
		t.Fatalf("Run(run-new): %v", err)
	}
	if newRun.BatchID != "batch-xyz" || newRun.Position != 1 || newRun.BatchSize != 3 {
		t.Errorf("Run(run-new) batch fields = %q/%d/%d, want batch-xyz/1/3",
			newRun.BatchID, newRun.Position, newRun.BatchSize)
	}
}

// schemaV5SQL is schema.sql as it existed before the retry_intents/
// ignored_refs/hook_runs tables were added: runs/checks/hooks/queue_depth
// exist with every column through the batch columns, but none of the v6
// durability tables exist yet. Kept here, verbatim, so TestMigrate_V5ToV6
// can build a genuine v5 database by hand and prove the running code
// migrates it forward correctly.
const schemaV5SQL = `
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL,
  batch_id     TEXT NOT NULL DEFAULT '',
  position     INTEGER NOT NULL DEFAULT 0,
  batch_size   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);
CREATE INDEX idx_runs_batch_id ON runs(batch_id) WHERE batch_id != '';

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',
  log_path    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

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
);
CREATE INDEX idx_hooks_name ON hooks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);
`

// TestMigrate_V5ToV6 builds a v5 database by hand (schemaV5SQL, none of
// retry_intents/ignored_refs/hook_runs exist yet, user_version stamped to 5
// — exactly what an already-deployed v5 Store would have on disk) and
// confirms that opening it with the current code migrates it in place:
// user_version lands on schemaVersion, the pre-existing run row survives
// untouched, and all three new v6 tables are usable — a fresh
// EventRetryRequested/EventIgnoredRef/EventHookStarted/EventHookSkipped Emit
// each round-trips through the newly-created tables.
func TestMigrate_V5ToV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV5SQL); err != nil {
		t.Fatalf("apply v5 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 5`); err != nil {
		t.Fatalf("stamp user_version=5: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
			batch_id, position, batch_size)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000, '', 0, 1)`,
	); err != nil {
		t.Fatalf("seed v5 run: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v5 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	run, _, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if run.RunID != "run-old" {
		t.Errorf("Run(run-old) after migrate = %+v, want the pre-existing row untouched", run)
	}

	ctx := context.Background()

	// EventRetryRequested -> retry_intents.
	retryAt := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventRetryRequested, At: retryAt, Target: "main",
		Candidate: core.Candidate{Ref: "refs/heads/for/main/alice/feat", SHA: "newsha"},
	}); err != nil {
		t.Fatalf("Emit EventRetryRequested after migrate: %v", err)
	}

	// EventIgnoredRef -> ignored_refs.
	ignoredAt := time.Date(2026, 7, 5, 10, 1, 0, 0, time.UTC)
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventIgnoredRef, At: ignoredAt, Target: "typoed",
		Candidate: core.Candidate{Ref: "refs/heads/for/typoed/alice/x", SHA: "sha1"},
		Detail:    `target "typoed" is not configured`,
	}); err != nil {
		t.Fatalf("Emit EventIgnoredRef after migrate: %v", err)
	}
	refs, err := s.IgnoredRefs(10)
	if err != nil {
		t.Fatalf("IgnoredRefs after migrate: %v", err)
	}
	if len(refs) != 1 || refs[0].Ref != "refs/heads/for/typoed/alice/x" {
		t.Errorf("IgnoredRefs after migrate = %+v, want one row", refs)
	}

	// EventHookStarted -> hook_runs (owed row).
	startedAt := time.Date(2026, 7, 5, 10, 2, 0, 0, time.UTC)
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventHookStarted, At: startedAt, Target: "main",
		RunID: "run-old", CheckName: "deploy", HookIndex: 0, HookCount: 2,
	}); err != nil {
		t.Fatalf("Emit EventHookStarted after migrate: %v", err)
	}
	summaries, err := s.HookRunSummaries("main", 10)
	if err != nil {
		t.Fatalf("HookRunSummaries after migrate: %v", err)
	}
	if len(summaries) != 1 || summaries[0].OwedCount != 2 || summaries[0].Skipped {
		t.Errorf("HookRunSummaries after migrate = %+v, want one unskipped owed=2 row", summaries)
	}

	// EventHookSkipped -> hook_runs (skipped row), a different run_id.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
			batch_id, position, batch_size)
		 VALUES ('run-skipped', 'main', 'refs/heads/for/main/bob/x', 'bob', 'x', 'deadbeef',
			'', '', 0, 'landed', '', 0, 1000, 1000, '', 0, 1)`,
	); err != nil {
		t.Fatalf("seed run-skipped: %v", err)
	}
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventHookSkipped, At: startedAt, Target: "main",
		RunID: "run-skipped", Detail: "recovered landing; hooks not run", HookCount: 3,
	}); err != nil {
		t.Fatalf("Emit EventHookSkipped after migrate: %v", err)
	}
	summaries, err = s.HookRunSummaries("main", 10)
	if err != nil {
		t.Fatalf("HookRunSummaries after skip: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("HookRunSummaries after skip = %d rows, want 2", len(summaries))
	}
}

// schemaV6SQL is schema.sql as it existed before the runs.speculated/
// runs.recovered columns were added: every v6 table/column is present
// (through hook_runs), but runs has neither speculated nor recovered. Kept
// here, verbatim, so TestMigrate_V6ToV7 can build a genuine v6 database by
// hand and prove the running code migrates it forward correctly.
const schemaV6SQL = `
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL,
  batch_id     TEXT NOT NULL DEFAULT '',
  position     INTEGER NOT NULL DEFAULT 0,
  batch_size   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);
CREATE INDEX idx_runs_batch_id ON runs(batch_id) WHERE batch_id != '';

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',
  log_path    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

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
);
CREATE INDEX idx_hooks_name ON hooks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);

CREATE TABLE retry_intents (
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  sha    TEXT NOT NULL,
  at     INTEGER NOT NULL,
  PRIMARY KEY (target, ref)
);

CREATE TABLE ignored_refs (
  at     INTEGER NOT NULL,
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (at, target, ref)
);

CREATE TABLE hook_runs (
  run_id      TEXT PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
  target      TEXT NOT NULL,
  owed_count  INTEGER NOT NULL,
  started_at  INTEGER NOT NULL,
  skipped     INTEGER NOT NULL DEFAULT 0,
  skip_reason TEXT NOT NULL DEFAULT ''
);
`

// TestMigrate_V6ToV7 builds a v6 database by hand (schemaV6SQL, runs has no
// speculated/recovered columns, user_version stamped to 6 — exactly what an
// already-deployed v6 Store would have on disk) and confirms that opening it
// with the current code migrates it in place: user_version lands on
// schemaVersion, the pre-existing run row survives with the new columns
// readable at their documented defaults (speculated=false, recovered=false),
// and a fresh write against the migrated database round-trips both new
// columns correctly.
func TestMigrate_V6ToV7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV6SQL); err != nil {
		t.Fatalf("apply v6 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatalf("stamp user_version=6: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
			batch_id, position, batch_size)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000, '', 0, 1)`,
	); err != nil {
		t.Fatalf("seed v6 run: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v6 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	run, _, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if run.RunID != "run-old" {
		t.Errorf("Run(run-old) after migrate = %+v, want the pre-existing row untouched", run)
	}
	if run.Speculated || run.Recovered {
		t.Errorf("Run(run-old).Speculated/Recovered = %v/%v, want false/false (column defaults)", run.Speculated, run.Recovered)
	}

	// A fresh write against the migrated database must round-trip both new
	// columns correctly.
	ctx := context.Background()
	rec := sampleRecord("run-new", "main", time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	rec.Speculated = true
	rec.Recovered = true
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-new", Record: rec}); err != nil {
		t.Fatalf("Emit after migrate: %v", err)
	}
	newRun, _, err := s.Run("run-new")
	if err != nil {
		t.Fatalf("Run(run-new): %v", err)
	}
	if !newRun.Speculated || !newRun.Recovered {
		t.Errorf("Run(run-new).Speculated/Recovered = %v/%v, want true/true", newRun.Speculated, newRun.Recovered)
	}
}

// schemaV7SQL is schema.sql as it existed before chunk 4 (dashboard visual
// refresh) added checks.command: every v7 table/column is present (runs has
// speculated/recovered), but checks has no command column yet. Kept here,
// verbatim, so TestMigrate_V7ToV8 can build a genuine v7 database by hand and
// prove the running code migrates it forward correctly.
const schemaV7SQL = `
CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL,
  batch_id     TEXT NOT NULL DEFAULT '',
  position     INTEGER NOT NULL DEFAULT 0,
  batch_size   INTEGER NOT NULL DEFAULT 1,
  speculated   INTEGER NOT NULL DEFAULT 0,
  recovered    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);
CREATE INDEX idx_runs_batch_id ON runs(batch_id) WHERE batch_id != '';

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',
  log_path    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

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
);
CREATE INDEX idx_hooks_name ON hooks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);

CREATE TABLE retry_intents (
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  sha    TEXT NOT NULL,
  at     INTEGER NOT NULL,
  PRIMARY KEY (target, ref)
);

CREATE TABLE ignored_refs (
  at     INTEGER NOT NULL,
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (at, target, ref)
);

CREATE TABLE hook_runs (
  run_id      TEXT PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
  target      TEXT NOT NULL,
  owed_count  INTEGER NOT NULL,
  started_at  INTEGER NOT NULL,
  skipped     INTEGER NOT NULL DEFAULT 0,
  skip_reason TEXT NOT NULL DEFAULT ''
);
`

// TestMigrate_V7ToV8 builds a v7 database by hand (schemaV7SQL, checks has no
// command column, user_version stamped to 7 — exactly what an
// already-deployed v7 Store would have on disk) and confirms that opening it
// with the current code migrates it in place: user_version lands on
// schemaVersion, the pre-existing check row survives with the new command
// column readable at its documented default (""), and a fresh write against
// the migrated database round-trips Command correctly.
func TestMigrate_V7ToV8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV7SQL); err != nil {
		t.Fatalf("apply v7 schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatalf("stamp user_version=7: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
			batch_id, position, batch_size, speculated, recovered)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000, '', 0, 1, 0, 0)`,
	); err != nil {
		t.Fatalf("seed v7 run: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO checks (run_id, seq, name, status, duration_ms, err, output, log_path)
		 VALUES ('run-old', 0, 'lint', 'passed', 500, '', 'clean', '')`,
	); err != nil {
		t.Fatalf("seed v7 check: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v7 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	run, checks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if run.RunID != "run-old" || len(checks) != 1 || checks[0].Output != "clean" {
		t.Errorf("Run(run-old) after migrate = %+v / %+v, want the pre-existing row untouched", run, checks)
	}
	if checks[0].Command != "" {
		t.Errorf("checks[0].Command = %q, want \"\" (column default for a pre-v8 row)", checks[0].Command)
	}

	// A fresh write against the migrated database must round-trip the new
	// command column correctly.
	ctx := context.Background()
	rec := sampleRecord("run-new", "main", time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC))
	rec.Checks[0].Command = []string{"go", "build", "./..."}
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-new", Record: rec}); err != nil {
		t.Fatalf("Emit after migrate: %v", err)
	}
	_, newChecks, err := s.Run("run-new")
	if err != nil {
		t.Fatalf("Run(run-new): %v", err)
	}
	if newChecks[0].Command != "go build ./..." {
		t.Errorf("Run(run-new) checks[0].Command = %q, want %q", newChecks[0].Command, "go build ./...")
	}
}

// TestMigrate_V8ToV9 builds a v8 database by hand (schemaV7SQL plus v7->v8's
// own ALTER, the exact shape a real v8 db has) and proves Open migrates it:
// checks gains blocked_by/waited_ms with pre-existing rows untouched, and a
// fresh write round-trips both new columns.
func TestMigrate_V8ToV9(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(schemaV7SQL); err != nil {
		t.Fatalf("apply v7 schema: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE checks ADD COLUMN command TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("apply v7->v8 alter: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 8`); err != nil {
		t.Fatalf("stamp user_version=8: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
			batch_id, position, batch_size, speculated, recovered)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000, '', 0, 1, 0, 0)`,
	); err != nil {
		t.Fatalf("seed v8 run: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO checks (run_id, seq, name, status, duration_ms, err, output, log_path, command)
		 VALUES ('run-old', 0, 'lint', 'passed', 500, '', 'clean', '', 'true')`,
	); err != nil {
		t.Fatalf("seed v8 check: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v8 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	_, checks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if len(checks) != 1 || checks[0].Output != "clean" || checks[0].Command != "true" {
		t.Errorf("Run(run-old) checks after migrate = %+v, want the pre-existing row untouched", checks)
	}
	if checks[0].BlockedBy != "" || checks[0].Waited != 0 {
		t.Errorf("pre-v9 row BlockedBy/Waited = %q/%v, want column defaults", checks[0].BlockedBy, checks[0].Waited)
	}

	// A fresh write against the migrated database must round-trip the new
	// columns: a blocked row's prerequisites and a slot-starved check's
	// wait.
	ctx := context.Background()
	rec := sampleRecord("run-new", "main", time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC))
	rec.Checks[0].Waited = 1500 * time.Millisecond
	rec.Checks = append(rec.Checks, core.CheckResult{Name: "deploy-dryrun", Status: core.CheckBlocked, BlockedBy: []string{"lint", "test"}})
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "run-new", Record: rec}); err != nil {
		t.Fatalf("Emit after migrate: %v", err)
	}
	_, newChecks, err := s.Run("run-new")
	if err != nil {
		t.Fatalf("Run(run-new): %v", err)
	}
	if newChecks[0].Waited != 1500*time.Millisecond {
		t.Errorf("checks[0].Waited = %v, want 1.5s", newChecks[0].Waited)
	}
	blocked := newChecks[len(newChecks)-1]
	if blocked.Status != "blocked" || blocked.BlockedBy != "lint,test" {
		t.Errorf("blocked row = %+v, want status blocked, BlockedBy \"lint,test\"", blocked)
	}
}

// TestMigrate_V9ToV10 builds a v9 database by hand (v7 schema + the v8 and
// v9 ALTERs) and proves Open migrates it: checks gains the image column
// with pre-existing rows untouched, and a fresh write round-trips it.
func TestMigrate_V9ToV10(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	for _, stmt := range []string{
		schemaV7SQL,
		`ALTER TABLE checks ADD COLUMN command TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE checks ADD COLUMN blocked_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE checks ADD COLUMN waited_ms INTEGER NOT NULL DEFAULT 0`,
		`PRAGMA user_version = 9`,
		`INSERT INTO runs (run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
			base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
			batch_id, position, batch_size, speculated, recovered)
		 VALUES ('run-old', 'main', 'refs/heads/for/main/alice/feat', 'alice', 'feat', 'deadbeef',
			'base0', 'merge0', 1, 'landed', '', 0, 1000, 1000, '', 0, 1, 0, 0)`,
		`INSERT INTO checks (run_id, seq, name, status, duration_ms, err, output, log_path, command)
		 VALUES ('run-old', 0, 'lint', 'passed', 500, '', 'clean', '', 'true')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed v9 db: %q: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v9 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}
	_, checks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if len(checks) != 1 || checks[0].Image != "" {
		t.Errorf("pre-v10 row = %+v, want untouched with empty image", checks)
	}

	const id = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ctx := context.Background()
	rec := sampleRecord("run-new", "main", time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC))
	rec.Checks[0].Image = id
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-new", Record: rec}); err != nil {
		t.Fatalf("Emit after migrate: %v", err)
	}
	_, newChecks, err := s.Run("run-new")
	if err != nil {
		t.Fatalf("Run(run-new): %v", err)
	}
	if newChecks[0].Image != id {
		t.Errorf("checks[0].Image = %q, want the identity round-tripped", newChecks[0].Image)
	}
}

func TestStore_SatisfiesCoreChannel(t *testing.T) {
	var _ core.Channel = (*Store)(nil)
}

func TestStore_Commands_NeverYields(t *testing.T) {
	s := openTestStore(t)
	select {
	case cmd := <-s.Commands():
		t.Fatalf("Commands() yielded %+v, want never", cmd)
	default:
	}
}

// TestStore_Emit_IgnoresNonTerminalEvents also covers S14's universal
// contract: core.EventKind(999) (a future kind Store.Emit's Kind/Record
// switch has never heard of) has Record == nil like every other
// non-terminal event here, so it must fall into the same "silently
// ignored, no rows written" branch — not panic or otherwise misbehave.
func TestStore_Emit_IgnoresNonTerminalEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	nonTerminal := []core.Event{
		{Kind: core.EventQueued, Target: "main"},
		{Kind: core.EventTrialClean, Target: "main"},
		{Kind: core.EventCheckStarted, Target: "main", RunID: "run-1", CheckName: "lint"},
		{Kind: core.EventCheckFinished, Target: "main", RunID: "run-1", CheckName: "lint"},
		// The trial-merge/verified events (issue #7) are non-terminal but
		// carry a MergeSHA; they must NOT write a phantom "landed" row —
		// they carry no Record, which is what the terminal guard keys on.
		{Kind: core.EventTrialMerged, Target: "main", RunID: "run-1", MergeSHA: "mergesha"},
		{Kind: core.EventVerified, Target: "main", RunID: "run-1", MergeSHA: "mergesha"},
		{Kind: core.EventKind(999), Target: "main"}, // unrecognized kind
	}
	for _, ev := range nonTerminal {
		if err := s.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit(%v): %v", ev.Kind, err)
		}
	}

	runs, err := s.RecentRuns("main", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("RecentRuns = %d rows, want 0 (non-terminal events must be ignored)", len(runs))
	}
}

func TestStore_Emit_WritesTerminalEventAndChecks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	rec := sampleRecord("run-1", "main", started)
	ev := core.Event{
		Kind:   core.EventRejected,
		At:     rec.EndedAt,
		Target: rec.Target,
		RunID:  rec.RunID,
		Record: rec,
	}
	if err := s.Emit(ctx, ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	run, checks, err := s.Run("run-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.RunID != rec.RunID || run.Target != rec.Target {
		t.Errorf("Run() run = %+v", run)
	}
	if run.CandidateRef != rec.Candidate.Ref || run.CandidateUser != rec.Candidate.User ||
		run.CandidateTopic != rec.Candidate.Topic || run.CandidateSHA != rec.Candidate.SHA {
		t.Errorf("Run() candidate fields = %+v, want %+v", run, rec.Candidate)
	}
	if run.BaseOID != rec.BaseOID || run.MergeSHA != rec.MergeSHA {
		t.Errorf("Run() base/merge = %q/%q, want %q/%q", run.BaseOID, run.MergeSHA, rec.BaseOID, rec.MergeSHA)
	}
	if !run.TrialClean {
		t.Errorf("Run() TrialClean = false, want true")
	}
	if run.Outcome != "rejected" {
		t.Errorf("Run() Outcome = %q, want %q", run.Outcome, "rejected")
	}
	if run.Detail != rec.Detail {
		t.Errorf("Run() Detail = %q, want %q", run.Detail, rec.Detail)
	}
	if !run.StartedAt.Equal(rec.StartedAt) {
		t.Errorf("Run() StartedAt = %v, want %v", run.StartedAt, rec.StartedAt)
	}
	if !run.EndedAt.Equal(rec.EndedAt) {
		t.Errorf("Run() EndedAt = %v, want %v", run.EndedAt, rec.EndedAt)
	}
	if run.Duration != 3*time.Second {
		t.Errorf("Run() Duration = %v, want 3s", run.Duration)
	}

	if len(checks) != 2 {
		t.Fatalf("Run() checks = %d, want 2", len(checks))
	}
	if checks[0].Name != "lint" || checks[0].Status != "passed" || checks[0].Duration != 500*time.Millisecond {
		t.Errorf("Run() checks[0] = %+v", checks[0])
	}
	if checks[0].Output != "all clean" {
		t.Errorf("Run() checks[0].Output = %q, want %q (passed checks store output too)", checks[0].Output, "all clean")
	}
	if checks[1].Name != "test" || checks[1].Status != "failed" || checks[1].Duration != 2500*time.Millisecond {
		t.Errorf("Run() checks[1] = %+v", checks[1])
	}
	if checks[1].Output != "boom" {
		t.Errorf("Run() checks[1].Output = %q, want %q", checks[1].Output, "boom")
	}
}

// TestStore_Emit_PersistsSpeculatedAndRecovered confirms writeRecord persists
// core.RunRecord.Speculated/Recovered (v7+) rather than dropping them at
// landing the way the pre-v7 schema silently did: both false by default
// (sampleRecord's zero-value case), and each independently true when set.
func TestStore_Emit_PersistsSpeculatedAndRecovered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	plain := sampleRecord("run-plain", "main", started)
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "run-plain", Record: plain}); err != nil {
		t.Fatalf("Emit (plain): %v", err)
	}
	run, _, err := s.Run("run-plain")
	if err != nil {
		t.Fatalf("Run(run-plain): %v", err)
	}
	if run.Speculated || run.Recovered {
		t.Errorf("Run(run-plain).Speculated/Recovered = %v/%v, want false/false", run.Speculated, run.Recovered)
	}

	speculated := sampleRecord("run-speculated", "main", started.Add(time.Minute))
	speculated.Speculated = true
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "run-speculated", Record: speculated}); err != nil {
		t.Fatalf("Emit (speculated): %v", err)
	}
	run, _, err = s.Run("run-speculated")
	if err != nil {
		t.Fatalf("Run(run-speculated): %v", err)
	}
	if !run.Speculated || run.Recovered {
		t.Errorf("Run(run-speculated).Speculated/Recovered = %v/%v, want true/false", run.Speculated, run.Recovered)
	}

	recovered := sampleRecord("run-recovered", "main", started.Add(2*time.Minute))
	recovered.Recovered = true
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-recovered", Record: recovered}); err != nil {
		t.Fatalf("Emit (recovered): %v", err)
	}
	run, _, err = s.Run("run-recovered")
	if err != nil {
		t.Fatalf("Run(run-recovered): %v", err)
	}
	if run.Speculated || !run.Recovered {
		t.Errorf("Run(run-recovered).Speculated/Recovered = %v/%v, want false/true", run.Speculated, run.Recovered)
	}
}

// TestStore_Emit_OutputStoredVerbatimForAllStatuses confirms Output is
// persisted exactly as carried on the CheckResult for every status —
// passed, skipped, failed, and Err-set alike. Green output is diagnostics
// too ("is it actually doing the thing" gets asked about green runs), so
// history stores what was captured with no status condition and no re-cap:
// the executor's 64KiB tail cap (executor.outputCap) is the only bound.
func TestStore_Emit_OutputStoredVerbatimForAllStatuses(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	// Larger than any history-side cap this store ever had (16KiB in an
	// earlier draft of schema v2): must survive byte-for-byte.
	big := strings.Repeat("a", 64*1024-40) + "THE ACTUAL FAILURE LINE"

	rec := &core.RunRecord{
		RunID:  "run-outputs",
		Target: "main",
		Checks: []core.CheckResult{
			{Name: "passed", Status: core.CheckPassed, Output: "green diagnostics"},
			{Name: "skipped", Status: core.CheckSkipped, Output: "skip reason"},
			{Name: "failed", Status: core.CheckFailed, Output: big},
			{Name: "errored", Err: fmt.Errorf("boom"), Output: "daemon error tail"},
			{Name: "empty", Status: core.CheckPassed},
		},
		Outcome:   core.OutcomeError,
		StartedAt: started,
		EndedAt:   started.Add(time.Second),
	}
	if err := s.Emit(ctx, core.Event{Kind: core.EventError, Target: "main", RunID: "run-outputs", Record: rec}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	_, checks, err := s.Run("run-outputs")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(checks) != len(rec.Checks) {
		t.Fatalf("Run() checks = %d, want %d", len(checks), len(rec.Checks))
	}
	for i, cr := range rec.Checks {
		if checks[i].Output != cr.Output {
			t.Errorf("checks[%d] (%s).Output = %d bytes %.40q..., want %d bytes (verbatim)",
				i, cr.Name, len(checks[i].Output), checks[i].Output, len(cr.Output))
		}
	}
}

func TestStore_Emit_ReEmitIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	rec := sampleRecord("run-dup", "main", started)
	ev := core.Event{Kind: core.EventRejected, Target: rec.Target, RunID: rec.RunID, Record: rec}

	if err := s.Emit(ctx, ev); err != nil {
		t.Fatalf("Emit (1st): %v", err)
	}
	if err := s.Emit(ctx, ev); err != nil {
		t.Fatalf("Emit (2nd): %v", err)
	}

	runs, err := s.RecentRuns("main", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("RecentRuns = %d rows, want 1 (re-emit must not duplicate the run row)", len(runs))
	}

	_, checks, err := s.Run("run-dup")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("Run() checks after re-emit = %d, want 2 (re-emit must not duplicate check rows)", len(checks))
	}
}

// hookFinishedEvent builds an EventHookFinished the way internal/hooks'
// runLanding actually emits one: Record is always nil on this kind (see
// core.Event's doc), the whole payload rides on Check + CheckName.
func hookFinishedEvent(runID, target, name string, cr core.CheckResult) core.Event {
	cr.Name = name
	return core.Event{
		Kind:      core.EventHookFinished,
		Target:    target,
		RunID:     runID,
		CheckName: name,
		Check:     &cr,
	}
}

// TestStore_Emit_InsertsHookRowsKeyedOffCheck confirms Store.Emit's
// EventHookFinished branch (writeHookResult): it fires off ev.Check (never
// ev.Record, which is nil on this event kind) and assigns seq by counting
// hooks already recorded for run_id — so successive hooks for the same run,
// emitted one event at a time as internal/hooks.Runner actually does it, get
// 0, 1, 2, ... in arrival order.
func TestStore_Emit_InsertsHookRowsKeyedOffCheck(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// The run row must exist first (foreign key) — this is what
	// queue.Daemon's channel fan-out order actually guarantees in
	// production (history's own Emit runs before hooks.Runner's), and what
	// writeHookResult's doc documents as the assumption.
	started := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
	rec := sampleRecord("run-hooks-1", "main", started)
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-hooks-1", Record: rec}); err != nil {
		t.Fatalf("Emit (landed): %v", err)
	}

	deploy := hookFinishedEvent("run-hooks-1", "main", "deploy", core.CheckResult{
		Status: core.CheckPassed, Duration: 250 * time.Millisecond, Output: "deployed ok",
		LogPath: "/var/lib/gauntlet/logs/run-hooks-1/hook-1-deploy.log.zst",
	})
	if err := s.Emit(ctx, deploy); err != nil {
		t.Fatalf("Emit (deploy hook): %v", err)
	}
	notify := hookFinishedEvent("run-hooks-1", "main", "notify", core.CheckResult{
		Status: core.CheckFailed, Duration: 50 * time.Millisecond, Output: "webhook 500", Err: fmt.Errorf("boom"),
	})
	if err := s.Emit(ctx, notify); err != nil {
		t.Fatalf("Emit (notify hook): %v", err)
	}

	hooks, err := s.Hooks("run-hooks-1")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("Hooks = %d rows, want 2", len(hooks))
	}
	if hooks[0].Seq != 0 || hooks[0].Name != "deploy" || hooks[0].Status != "passed" ||
		hooks[0].Duration != 250*time.Millisecond || hooks[0].Output != "deployed ok" ||
		hooks[0].LogPath != "/var/lib/gauntlet/logs/run-hooks-1/hook-1-deploy.log.zst" {
		t.Errorf("hooks[0] = %+v, unexpected", hooks[0])
	}
	if hooks[1].Seq != 1 || hooks[1].Name != "notify" || hooks[1].Status != "failed" ||
		hooks[1].Duration != 50*time.Millisecond || hooks[1].Output != "webhook 500" || hooks[1].Err != "boom" {
		t.Errorf("hooks[1] = %+v, unexpected", hooks[1])
	}
}

// TestStore_Emit_HookFinishedIgnoredWhenCheckNil guards the Kind-plus-Check
// gate in Emit: an EventHookFinished with a nil Check (which internal/hooks
// never actually emits, but core.Channel implementations must tolerate any
// nil-Check event per core.Event's doc) must not panic or write a row.
func TestStore_Emit_HookFinishedIgnoredWhenCheckNil(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Emit(ctx, core.Event{Kind: core.EventHookFinished, Target: "main", RunID: "run-x", CheckName: "deploy"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	hooks, err := s.Hooks("run-x")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Errorf("Hooks = %+v, want none", hooks)
	}
}

// TestStore_Hooks_EmptyForRunWithNoHooks confirms the "omit the section"
// contract the dashboard/MCP tool both rely on: a perfectly ordinary run
// that never had any hooks configured for its target returns an empty,
// non-error result from Hooks.
func TestStore_Hooks_EmptyForRunWithNoHooks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	started := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
	rec := sampleRecord("run-no-hooks", "main", started)
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "run-no-hooks", Record: rec}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	hooks, err := s.Hooks("run-no-hooks")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Errorf("Hooks = %+v, want none", hooks)
	}
}

// --- v6 durability rows: retry_intents, ignored_refs, hook_runs ---

// TestStore_Emit_RetryIntent_UpsertsLatest confirms writeRetryIntent's
// upsert: a second EventRetryRequested for the same (target, ref) replaces
// the first's sha/at rather than erroring or creating a second row — only
// the LATEST retry matters to LatestTerminalPerRef's seed-park suppression.
func TestStore_Emit_RetryIntent_UpsertsLatest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ref := "refs/heads/for/main/alice/feat"

	first := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventRetryRequested, At: first, Target: "main",
		Candidate: core.Candidate{Ref: ref, SHA: "sha1"},
	}); err != nil {
		t.Fatalf("Emit (1st retry): %v", err)
	}

	second := first.Add(time.Minute)
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventRetryRequested, At: second, Target: "main",
		Candidate: core.Candidate{Ref: ref, SHA: "sha2"},
	}); err != nil {
		t.Fatalf("Emit (2nd retry): %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM retry_intents WHERE target = ? AND ref = ?`, "main", ref).Scan(&count); err != nil {
		t.Fatalf("count retry_intents: %v", err)
	}
	if count != 1 {
		t.Fatalf("retry_intents rows for (main, %s) = %d, want 1 (upsert, not append)", ref, count)
	}

	var sha string
	var atMS int64
	if err := s.db.QueryRow(`SELECT sha, at FROM retry_intents WHERE target = ? AND ref = ?`, "main", ref).Scan(&sha, &atMS); err != nil {
		t.Fatalf("read retry_intents: %v", err)
	}
	if sha != "sha2" || !time.UnixMilli(atMS).Equal(second) {
		t.Errorf("retry_intents row = sha=%q at=%v, want sha2 at %v (latest wins)", sha, time.UnixMilli(atMS), second)
	}
}

// TestStore_LatestTerminalPerRef_RetrySuppressesStalePark is the store-level
// proof that a ref's most recent terminal outcome, a rejection, is
// suppressed when an operator's retry (EventRetryRequested) landed AFTER that
// rejection's ended_at — simulating a daemon crash between the retry and the
// retried run's own terminal event. LatestTerminalPerRef must omit this ref
// entirely (not just report it differently): the stale rejection must never
// again be treated as a park candidate.
func TestStore_LatestTerminalPerRef_RetrySuppressesStalePark(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ref := "refs/heads/for/main/alice/feat"

	emitRun(t, s, "run-1", "main", ref, "sha1", core.OutcomeRejected, "first try failed", base)

	// The retry lands AFTER the rejection's ended_at (base + 1s).
	retryAt := base.Add(time.Hour)
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventRetryRequested, At: retryAt, Target: "main",
		Candidate: core.Candidate{Ref: ref, SHA: "sha1"},
	}); err != nil {
		t.Fatalf("Emit EventRetryRequested: %v", err)
	}

	verdicts, err := s.LatestTerminalPerRef("main")
	if err != nil {
		t.Fatalf("LatestTerminalPerRef: %v", err)
	}
	for _, v := range verdicts {
		if v.Ref == ref {
			t.Fatalf("LatestTerminalPerRef returned %+v for %s, want it omitted (retry newer than the terminal must suppress the stale park)", v, ref)
		}
	}

	// A later terminal (the retried run rejected again) re-parks with the
	// new reason: ended_at now postdates the retry again.
	emitRun(t, s, "run-2", "main", ref, "sha1", core.OutcomeRejected, "regressed again", retryAt.Add(time.Minute))

	verdicts, err = s.LatestTerminalPerRef("main")
	if err != nil {
		t.Fatalf("LatestTerminalPerRef (after re-reject): %v", err)
	}
	var found bool
	for _, v := range verdicts {
		if v.Ref == ref {
			found = true
			if v.Detail != "regressed again" {
				t.Errorf("re-parked verdict = %+v, want Detail=%q", v, "regressed again")
			}
		}
	}
	if !found {
		t.Fatal("a terminal newer than the retry must re-include the ref as a park candidate")
	}
}

// TestStore_HookRunSummaries_OwedGreaterThanDoneIsCrashIncomplete is the
// crash-timing acceptance criterion at the store layer: a landing whose
// hook_runs row records owed_count=3 (EventHookStarted fired for hook 0)
// but only 1 hooks row exists (the chain never got further — a simulated
// daemon crash mid-chain) must report OwedCount > DoneCount with
// Skipped=false, the exact signal HookRunSummaries exists to surface
// (crash-incomplete, discoverable without ever auto-resuming a hook).
// Contrasted with a landing that finished normally (owed == done) and one
// that was deliberately skipped (recovery landing, never crash-incomplete
// regardless of owed vs. done).
func TestStore_HookRunSummaries_OwedGreaterThanDoneIsCrashIncomplete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	// run-crashed: EventHookStarted says 3 hooks owed; only 1 ever finished
	// (writeHookResult below records exactly one hooks row) — simulating a
	// daemon crash between hook 0 finishing and hook 1 starting.
	crashedRec := sampleRecord("run-crashed", "main", base)
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-crashed", Record: crashedRec}); err != nil {
		t.Fatalf("Emit (run-crashed landed): %v", err)
	}
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventHookStarted, At: base, Target: "main",
		RunID: "run-crashed", CheckName: "deploy", HookIndex: 0, HookCount: 3,
	}); err != nil {
		t.Fatalf("Emit (run-crashed hook 0 started): %v", err)
	}
	if err := s.Emit(ctx, hookFinishedEvent("run-crashed", "main", "deploy", core.CheckResult{Status: core.CheckPassed})); err != nil {
		t.Fatalf("Emit (run-crashed hook 0 finished): %v", err)
	}
	// Hook 1 never starts: the daemon "crashed" here.

	// run-complete: owed_count == done_count, a perfectly normal landing.
	completeRec := sampleRecord("run-complete", "main", base.Add(time.Minute))
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-complete", Record: completeRec}); err != nil {
		t.Fatalf("Emit (run-complete landed): %v", err)
	}
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventHookStarted, At: base.Add(time.Minute), Target: "main",
		RunID: "run-complete", CheckName: "deploy", HookIndex: 0, HookCount: 1,
	}); err != nil {
		t.Fatalf("Emit (run-complete hook started): %v", err)
	}
	if err := s.Emit(ctx, hookFinishedEvent("run-complete", "main", "deploy", core.CheckResult{Status: core.CheckPassed})); err != nil {
		t.Fatalf("Emit (run-complete hook finished): %v", err)
	}

	// run-skipped-recovery: owed_count=2, done_count=0, but Skipped=true —
	// must never read as crash-incomplete despite owed > done.
	skippedRec := sampleRecord("run-skipped-recovery", "main", base.Add(2*time.Minute))
	skippedRec.MergeSHA = ""
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-skipped-recovery", Record: skippedRec}); err != nil {
		t.Fatalf("Emit (run-skipped-recovery landed): %v", err)
	}
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventHookSkipped, At: base.Add(2 * time.Minute), Target: "main",
		RunID: "run-skipped-recovery", Detail: "recovered landing; hooks not run", HookCount: 2,
	}); err != nil {
		t.Fatalf("Emit (run-skipped-recovery skipped): %v", err)
	}

	summaries, err := s.HookRunSummaries("main", 10)
	if err != nil {
		t.Fatalf("HookRunSummaries: %v", err)
	}
	byRunID := make(map[string]HookRunSummary, len(summaries))
	for _, h := range summaries {
		byRunID[h.RunID] = h
	}
	if len(byRunID) != 3 {
		t.Fatalf("HookRunSummaries = %d distinct runs, want 3 (got %+v)", len(byRunID), summaries)
	}

	crashed := byRunID["run-crashed"]
	if crashed.OwedCount != 3 || crashed.DoneCount != 1 || crashed.Skipped {
		t.Errorf("run-crashed summary = %+v, want OwedCount=3 DoneCount=1 Skipped=false (crash-incomplete)", crashed)
	}
	if crashed.OwedCount <= crashed.DoneCount {
		t.Errorf("run-crashed OwedCount=%d DoneCount=%d, want OwedCount > DoneCount", crashed.OwedCount, crashed.DoneCount)
	}

	complete := byRunID["run-complete"]
	if complete.OwedCount != 1 || complete.DoneCount != 1 || complete.Skipped {
		t.Errorf("run-complete summary = %+v, want OwedCount=1 DoneCount=1 Skipped=false (finished normally)", complete)
	}

	skipped := byRunID["run-skipped-recovery"]
	if skipped.OwedCount != 2 || skipped.DoneCount != 0 || !skipped.Skipped || skipped.SkipReason == "" {
		t.Errorf("run-skipped-recovery summary = %+v, want OwedCount=2 DoneCount=0 Skipped=true with a reason", skipped)
	}
}

// TestStore_IgnoredRefs_NewestFirstAndLimit confirms IgnoredRefs' read
// contract: rows for target only, newest first, capped at limit.
func TestStore_IgnoredRefs_NewestFirstAndLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	for i, ref := range []string{"refs/heads/for/typoed/a/1", "refs/heads/for/typoed/b/2", "refs/heads/for/typoed/c/3"} {
		if err := s.Emit(ctx, core.Event{
			Kind: core.EventIgnoredRef, At: base.Add(time.Duration(i) * time.Minute), Target: "typoed",
			Candidate: core.Candidate{Ref: ref, SHA: fmt.Sprintf("sha%d", i)},
			Detail:    `target "typoed" is not configured`,
		}); err != nil {
			t.Fatalf("Emit(%s): %v", ref, err)
		}
	}
	// A second unconfigured target — IgnoredRefs is deliberately global (an
	// ignored ref never belongs to a configured target, so per-target
	// filtering would hide everything; see the method doc). Emitted OLDER
	// than every "typoed" row so the newest-first limit-2 result below is
	// unchanged by it.
	if err := s.Emit(ctx, core.Event{
		Kind: core.EventIgnoredRef, At: base.Add(-time.Minute), Target: "other-typo",
		Candidate: core.Candidate{Ref: "refs/heads/for/other-typo/x/y", SHA: "shaX"},
		Detail:    `target "other-typo" is not configured`,
	}); err != nil {
		t.Fatalf("Emit(other-typo): %v", err)
	}

	refs, err := s.IgnoredRefs(2)
	if err != nil {
		t.Fatalf("IgnoredRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("IgnoredRefs limit = %d rows, want 2", len(refs))
	}
	if refs[0].Ref != "refs/heads/for/typoed/c/3" || refs[1].Ref != "refs/heads/for/typoed/b/2" {
		t.Errorf("IgnoredRefs order = [%s %s], want newest-first [c/3 b/2]", refs[0].Ref, refs[1].Ref)
	}
	if refs[0].Detail == "" {
		t.Error("IgnoredRefs[0].Detail is empty")
	}
}

func TestStore_Run_NotFound(t *testing.T) {
	s := openTestStore(t)
	_, _, err := s.Run("does-not-exist")
	if err == nil {
		t.Fatalf("Run(missing) = nil error, want a not-found error")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Run(missing) error = %v, want wrapped sql.ErrNoRows", err)
	}
}

func TestStore_RecentRuns_OrderAndLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	for i, id := range []string{"run-a", "run-b", "run-c"} {
		rec := sampleRecord(id, "main", base.Add(time.Duration(i)*time.Minute))
		if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: id, Record: rec}); err != nil {
			t.Fatalf("Emit(%s): %v", id, err)
		}
	}
	// Different target must not show up in "main" queries.
	other := sampleRecord("run-other", "release", base)
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "release", RunID: "run-other", Record: other}); err != nil {
		t.Fatalf("Emit(other): %v", err)
	}

	runs, err := s.RecentRuns("main", 2)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("RecentRuns limit = %d rows, want 2", len(runs))
	}
	// Newest (run-c) first.
	if runs[0].RunID != "run-c" || runs[1].RunID != "run-b" {
		t.Errorf("RecentRuns order = [%s %s], want [run-c run-b]", runs[0].RunID, runs[1].RunID)
	}
}

// TestStore_RecentRuns_ChecksTotalAndPassed confirms RecentRuns' joined
// check-count aggregate (RunRow.ChecksTotal/ChecksPassed, dashboard's "✓ n/m"
// ratio): a run with sampleRecord's two checks (one passed, one failed)
// reads back 2/1, and a run with no checks table rows at all reads back 0/0
// — dashboard/server.go is what turns that 0 into "omit the ratio", but the
// store-level contract is just "count what's actually there".
func TestStore_RecentRuns_ChecksTotalAndPassed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	withChecks := sampleRecord("run-with-checks", "main", base)
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "run-with-checks", Record: withChecks}); err != nil {
		t.Fatalf("Emit(run-with-checks): %v", err)
	}

	noChecks := &core.RunRecord{
		RunID:     "run-no-checks",
		Target:    "main",
		Candidate: core.Candidate{Ref: "refs/heads/for/main/eve/nocheck", Target: "main", User: "eve", Topic: "nocheck", SHA: "sha-nocheck"},
		Outcome:   core.OutcomeError,
		StartedAt: base.Add(time.Minute),
		EndedAt:   base.Add(time.Minute),
	}
	if err := s.Emit(ctx, core.Event{Kind: core.EventError, Target: "main", RunID: "run-no-checks", Record: noChecks}); err != nil {
		t.Fatalf("Emit(run-no-checks): %v", err)
	}

	runs, err := s.RecentRuns("main", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	byID := make(map[string]RunRow, len(runs))
	for _, r := range runs {
		byID[r.RunID] = r
	}
	if r := byID["run-with-checks"]; r.ChecksTotal != 2 || r.ChecksPassed != 1 {
		t.Errorf("run-with-checks ChecksTotal/ChecksPassed = %d/%d, want 2/1", r.ChecksTotal, r.ChecksPassed)
	}
	if r := byID["run-no-checks"]; r.ChecksTotal != 0 || r.ChecksPassed != 0 {
		t.Errorf("run-no-checks ChecksTotal/ChecksPassed = %d/%d, want 0/0", r.ChecksTotal, r.ChecksPassed)
	}
}

func TestStore_CheckStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	runs := []*core.RunRecord{
		{
			RunID: "run-1", Target: "main",
			Checks: []core.CheckResult{
				{Name: "lint", Status: core.CheckPassed, Duration: 1 * time.Second},
			},
			Outcome: core.OutcomeLanded, StartedAt: base, EndedAt: base.Add(time.Second),
		},
		{
			RunID: "run-2", Target: "main",
			Checks: []core.CheckResult{
				{Name: "lint", Status: core.CheckFailed, Duration: 3 * time.Second},
			},
			Outcome: core.OutcomeRejected, StartedAt: base.Add(time.Minute), EndedAt: base.Add(time.Minute + 3*time.Second),
		},
	}
	for _, rec := range runs {
		if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: rec.Target, RunID: rec.RunID, Record: rec}); err != nil {
			t.Fatalf("Emit(%s): %v", rec.RunID, err)
		}
	}

	stats, err := s.CheckStats("main", base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CheckStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("CheckStats = %d entries, want 1", len(stats))
	}
	st := stats[0]
	if st.Name != "lint" || st.Total != 2 || st.Failed != 1 {
		t.Errorf("CheckStats[0] = %+v, want Name=lint Total=2 Failed=1", st)
	}
	if st.RedRate != 0.5 {
		t.Errorf("CheckStats[0].RedRate = %v, want 0.5", st.RedRate)
	}
	if st.AvgDuration != 2*time.Second {
		t.Errorf("CheckStats[0].AvgDuration = %v, want 2s", st.AvgDuration)
	}
	if st.MaxDuration != 3*time.Second {
		t.Errorf("CheckStats[0].MaxDuration = %v, want 3s", st.MaxDuration)
	}

	// since filters out both runs.
	stats, err = s.CheckStats("main", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("CheckStats (filtered): %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("CheckStats (filtered) = %d entries, want 0", len(stats))
	}
}

// TestStore_CheckStats_DedupesBatchMembers is the statistical-honesty test:
// a batch of 3 member RunRecords sharing one BatchID, each carrying the
// *same* duplicated check results (the batch's checks ran once against the
// chain tip and were duplicated onto every member's record) must count as
// ONE suite in CheckStats,
// not three — otherwise a green batch of N deflates the red-rate and a red
// batch's duplicated failures inflate it. A fourth, unrelated serial run
// (its own distinct check result, no BatchID) must still count as its own
// suite. So across 4 run rows total, CheckStats must report Total=2 for the
// shared check name, not 4.
func TestStore_CheckStats_DedupesBatchMembers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	// The batch's checks: one passed check, duplicated verbatim onto every
	// member record, exactly as queue/reconcile.go's batch landing path does
	// (§3.3's "Checks = the batch's check results, duplicated onto every
	// member record").
	batchChecks := []core.CheckResult{
		{Name: "lint", Status: core.CheckPassed, Duration: 1 * time.Second},
	}
	for i, id := range []string{"batch-run-0", "batch-run-1", "batch-run-2"} {
		rec := &core.RunRecord{
			RunID: id, Target: "main",
			Checks:    batchChecks,
			Outcome:   core.OutcomeLanded,
			BatchID:   "batch-xyz",
			Position:  i,
			BatchSize: 3,
			StartedAt: base.Add(time.Duration(i) * time.Second),
			EndedAt:   base.Add(time.Duration(i)*time.Second + time.Second),
		}
		if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: id, Record: rec}); err != nil {
			t.Fatalf("Emit(%s): %v", id, err)
		}
	}

	// One unrelated serial run: its own distinct (failed) "lint" result, no
	// BatchID — must count as its own suite, separately from the batch.
	serialRec := &core.RunRecord{
		RunID: "serial-run", Target: "main",
		Checks:    []core.CheckResult{{Name: "lint", Status: core.CheckFailed, Duration: 3 * time.Second}},
		Outcome:   core.OutcomeRejected,
		StartedAt: base.Add(time.Minute),
		EndedAt:   base.Add(time.Minute + 3*time.Second),
	}
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "serial-run", Record: serialRec}); err != nil {
		t.Fatalf("Emit(serial-run): %v", err)
	}

	stats, err := s.CheckStats("main", base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CheckStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("CheckStats = %d entries, want 1", len(stats))
	}
	st := stats[0]
	if st.Name != "lint" {
		t.Fatalf("CheckStats[0].Name = %q, want lint", st.Name)
	}
	if st.Total != 2 {
		t.Errorf("CheckStats[0].Total = %d, want 2 (one suite for the 3-member batch, one for the serial run — not 4)", st.Total)
	}
	if st.Failed != 1 {
		t.Errorf("CheckStats[0].Failed = %d, want 1 (only the serial run failed; the batch's single counted suite passed)", st.Failed)
	}
	if st.RedRate != 0.5 {
		t.Errorf("CheckStats[0].RedRate = %v, want 0.5 (1/2, not 1/4)", st.RedRate)
	}
	// Duration comes from whichever run was picked representative for the
	// batch suite (deterministic: MIN(run_id) = "batch-run-0", also 1s) plus
	// the serial run's 3s: avg 2s, max 3s.
	if st.AvgDuration != 2*time.Second {
		t.Errorf("CheckStats[0].AvgDuration = %v, want 2s", st.AvgDuration)
	}
	if st.MaxDuration != 3*time.Second {
		t.Errorf("CheckStats[0].MaxDuration = %v, want 3s", st.MaxDuration)
	}
}

// TestStore_BatchMembers confirms BatchMembers returns exactly the rows
// sharing a batch_id, ordered by position, and that an empty/unknown batchID
// returns an empty result rather than every serial run (which all share the
// "" batch_id column default).
func TestStore_BatchMembers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	for i, id := range []string{"member-2", "member-0", "member-1"} {
		// Insert out of position order to prove the query sorts by position,
		// not insertion/started_at order.
		pos := map[string]int{"member-2": 2, "member-0": 0, "member-1": 1}[id]
		rec := &core.RunRecord{
			RunID: id, Target: "main",
			Outcome:   core.OutcomeLanded,
			BatchID:   "batch-abc",
			Position:  pos,
			BatchSize: 3,
			StartedAt: base.Add(time.Duration(i) * time.Second),
			EndedAt:   base.Add(time.Duration(i) * time.Second),
		}
		if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: id, Record: rec}); err != nil {
			t.Fatalf("Emit(%s): %v", id, err)
		}
	}
	// An unrelated serial run must never show up in a batch's members.
	other := sampleRecord("solo-run", "main", base)
	if err := s.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", RunID: "solo-run", Record: other}); err != nil {
		t.Fatalf("Emit(solo-run): %v", err)
	}

	members, err := s.BatchMembers("batch-abc")
	if err != nil {
		t.Fatalf("BatchMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("BatchMembers = %d rows, want 3", len(members))
	}
	for i, m := range members {
		if m.Position != i {
			t.Errorf("members[%d].Position = %d, want %d (position order)", i, m.Position, i)
		}
		if m.BatchID != "batch-abc" || m.BatchSize != 3 {
			t.Errorf("members[%d] = %+v, want BatchID=batch-abc BatchSize=3", i, m)
		}
	}
	if members[0].RunID != "member-0" || members[1].RunID != "member-1" || members[2].RunID != "member-2" {
		t.Errorf("members order = [%s %s %s], want [member-0 member-1 member-2]",
			members[0].RunID, members[1].RunID, members[2].RunID)
	}

	empty, err := s.BatchMembers("")
	if err != nil {
		t.Fatalf("BatchMembers(\"\"): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("BatchMembers(\"\") = %d rows, want 0 (not every serial run)", len(empty))
	}

	unknown, err := s.BatchMembers("does-not-exist")
	if err != nil {
		t.Fatalf("BatchMembers(unknown): %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("BatchMembers(unknown) = %d rows, want 0", len(unknown))
	}
}

// TestStore_BatchMembers_RedSkipped is TestStore_BatchMembers's red-batch
// counterpart (finishBatchRed's shape, internal/queue/reconcile.go): a
// batch-red run emits EventSkipped (not EventLanded/EventRejected) per
// member, Outcome Skipped on every one, the shared failing check duplicated
// onto each record (§3.3) — proving the fixed data-loss bug (batch members
// sharing one RunID, collapsed by history's run_id PRIMARY KEY under INSERT
// OR REPLACE) is fixed for the red path too, not just the green one: 3
// distinct RunIDs (the real "<batchID>"/"<batchID>-m1"/"<batchID>-m2" shape
// queue.memberRunID mints) round-trip as 3 separate rows.
func TestStore_BatchMembers_RedSkipped(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 13, 0, 0, 0, time.UTC)
	batchID := "20260705T130000Z-1-abc123def456"

	runIDs := []string{batchID, batchID + "-m1", batchID + "-m2"}
	for i, runID := range runIDs {
		rec := &core.RunRecord{
			RunID:  runID,
			Target: "main",
			Candidate: core.Candidate{
				Ref: fmt.Sprintf("refs/heads/for/main/user%d/topic%d", i, i),
			},
			Checks: []core.CheckResult{
				{Name: "test", Status: core.CheckFailed, Duration: time.Second, Output: "boom"},
			},
			Outcome:   core.OutcomeSkipped,
			Detail:    fmt.Sprintf("batch %s red on check \"test\"; serializing", batchID),
			BatchID:   batchID,
			Position:  i,
			BatchSize: 3,
			StartedAt: base.Add(time.Duration(i) * time.Second),
			EndedAt:   base.Add(time.Duration(i) * time.Second),
		}
		if err := s.Emit(ctx, core.Event{Kind: core.EventSkipped, Target: "main", RunID: runID, Record: rec}); err != nil {
			t.Fatalf("Emit(%s): %v", runID, err)
		}
	}

	members, err := s.BatchMembers(batchID)
	if err != nil {
		t.Fatalf("BatchMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("BatchMembers = %d rows, want 3 (the data-loss bug would collapse these to 1)", len(members))
	}
	for i, m := range members {
		if m.RunID != runIDs[i] {
			t.Errorf("members[%d].RunID = %q, want %q", i, m.RunID, runIDs[i])
		}
		if m.Position != i {
			t.Errorf("members[%d].Position = %d, want %d (position order)", i, m.Position, i)
		}
		if m.Outcome != "skipped" {
			t.Errorf("members[%d].Outcome = %q, want skipped", i, m.Outcome)
		}
		if m.BatchID != batchID || m.BatchSize != 3 {
			t.Errorf("members[%d] = %+v, want BatchID=%s BatchSize=3", i, m, batchID)
		}
	}
}

// TestStore_ConcurrentReadsDontBlockWrites is the sanity check for raising
// SetMaxOpenConns from 1 to 4: Emit runs inline on the reconcile goroutine
// in production, so a dashboard read must
// never serialize behind it (or vice versa). This drives a batch of Emits
// concurrently with a batch of read-side queries (RecentRuns, CheckStats —
// the JOIN query called out as the risk) against one Store, under -race, and
// simply asserts nothing errors or deadlocks: a pool capped at 1 connection
// would still pass this correctness-wise (database/sql would just queue the
// callers), so the real evidence the fix works is this test completing
// promptly under `go test -race` rather than serializing to the point of
// timing out — verified manually when this change was made.
func TestStore_ConcurrentReadsDontBlockWrites(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	// Seed some rows so the read side has something to scan.
	for i := 0; i < 20; i++ {
		rec := sampleRecord(fmt.Sprintf("seed-%d", i), "main", base.Add(time.Duration(i)*time.Second))
		if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: rec.RunID, Record: rec}); err != nil {
			t.Fatalf("seed Emit(%d): %v", i, err)
		}
	}

	const writers = 8
	const readers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				rec := sampleRecord(fmt.Sprintf("writer-%d-%d", w, i), "main", base.Add(time.Duration(w*100+i)*time.Second))
				if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: rec.RunID, Record: rec}); err != nil {
					errs <- fmt.Errorf("writer %d emit %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := s.RecentRuns("main", 10); err != nil {
					errs <- fmt.Errorf("RecentRuns: %w", err)
					return
				}
				if _, err := s.CheckStats("main", base.Add(-time.Hour)); err != nil {
					errs <- fmt.Errorf("CheckStats: %w", err)
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent reads/writes did not complete within 10s")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestStore_DepthSeries_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	samples := []struct {
		at                        time.Time
		waiting, inFlight, parked int
	}{
		{base, 1, 1, 0},
		{base.Add(10 * time.Second), 2, 1, 1},
		{base.Add(20 * time.Second), 0, 0, 3},
	}
	for _, sm := range samples {
		if err := s.RecordDepth(sm.at, "main", sm.waiting, sm.inFlight, sm.parked); err != nil {
			t.Fatalf("RecordDepth(%v): %v", sm.at, err)
		}
	}
	// Different target must not leak into "main" queries.
	if err := s.RecordDepth(base, "release", 9, 9, 9); err != nil {
		t.Fatalf("RecordDepth(release): %v", err)
	}

	points, err := s.DepthSeries("main", base.Add(5*time.Second))
	if err != nil {
		t.Fatalf("DepthSeries: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("DepthSeries = %d points, want 2", len(points))
	}
	if !points[0].At.Equal(base.Add(10 * time.Second)) {
		t.Errorf("DepthSeries[0].At = %v, want %v", points[0].At, base.Add(10*time.Second))
	}
	if points[0].Waiting != 2 || points[0].InFlight != 1 || points[0].Parked != 1 {
		t.Errorf("DepthSeries[0] = %+v", points[0])
	}
	if points[1].Waiting != 0 || points[1].InFlight != 0 || points[1].Parked != 3 {
		t.Errorf("DepthSeries[1] = %+v", points[1])
	}

	// RecordDepth at an existing (at, target) replaces the row rather than
	// duplicating it.
	if err := s.RecordDepth(base, "main", 42, 42, 42); err != nil {
		t.Fatalf("RecordDepth (replace): %v", err)
	}
	points, err = s.DepthSeries("main", base)
	if err != nil {
		t.Fatalf("DepthSeries (after replace): %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("DepthSeries (after replace) = %d points, want 3", len(points))
	}
	if points[0].Waiting != 42 {
		t.Errorf("DepthSeries[0].Waiting after replace = %d, want 42", points[0].Waiting)
	}
}

// TestStore_PruneDepth confirms PruneDepth deletes only queue_depth rows
// strictly older than its cutoff, leaving rows at or after the cutoff (and
// other targets' rows in the same window) untouched, and — the deliberate
// asymmetry the doc comment calls out — never touches runs/checks even when
// those rows are far older than any depth-series retention window.
func TestStore_PruneDepth(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	for i, at := range []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)} {
		if err := s.RecordDepth(at, "main", i, i, i); err != nil {
			t.Fatalf("RecordDepth(%v): %v", at, err)
		}
	}

	// A very old run/check row: pruning must leave it alone regardless of
	// how the depth cutoff is chosen below.
	oldRec := sampleRecord("run-ancient", "main", base.Add(-24*time.Hour))
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-ancient", Record: oldRec}); err != nil {
		t.Fatalf("Emit(run-ancient): %v", err)
	}

	if err := s.PruneDepth(base.Add(time.Hour)); err != nil {
		t.Fatalf("PruneDepth: %v", err)
	}

	points, err := s.DepthSeries("main", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("DepthSeries: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("DepthSeries after prune = %d points, want 2 (base and base+1h kept, base+1h is the boundary)", len(points))
	}
	if !points[0].At.Equal(base.Add(time.Hour)) || !points[1].At.Equal(base.Add(2*time.Hour)) {
		t.Errorf("DepthSeries after prune = %+v, want [base+1h base+2h]", points)
	}

	runs, err := s.RecentRuns("main", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-ancient" {
		t.Errorf("RecentRuns after PruneDepth = %+v, want run-ancient untouched", runs)
	}
}

// TestStore_PruneIgnoredRefs confirms PruneIgnoredRefs deletes only
// ignored_refs rows strictly older than its cutoff,
// leaving rows at or after the cutoff untouched — same boundary semantics
// as TestStore_PruneDepth — and never touches runs/checks.
func TestStore_PruneIgnoredRefs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	for i, at := range []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)} {
		if err := s.Emit(ctx, core.Event{
			Kind: core.EventIgnoredRef, At: at, Target: "typoed",
			Candidate: core.Candidate{Ref: "refs/heads/for/typoed/a/1", SHA: fmt.Sprintf("sha%d", i)},
			Detail:    `target "typoed" is not configured`,
		}); err != nil {
			t.Fatalf("Emit(%v): %v", at, err)
		}
	}

	// A very old run/check row: pruning ignored_refs must leave it alone.
	oldRec := sampleRecord("run-ancient", "main", base.Add(-24*time.Hour))
	if err := s.Emit(ctx, core.Event{Kind: core.EventLanded, Target: "main", RunID: "run-ancient", Record: oldRec}); err != nil {
		t.Fatalf("Emit(run-ancient): %v", err)
	}

	if err := s.PruneIgnoredRefs(base.Add(time.Hour)); err != nil {
		t.Fatalf("PruneIgnoredRefs: %v", err)
	}

	refs, err := s.IgnoredRefs(10)
	if err != nil {
		t.Fatalf("IgnoredRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("IgnoredRefs after prune = %d rows, want 2 (base+1h and base+2h kept, base+1h is the boundary)", len(refs))
	}
	for _, r := range refs {
		if r.At.Before(base.Add(time.Hour)) {
			t.Errorf("IgnoredRefs after prune contains a row at %v, want nothing before %v", r.At, base.Add(time.Hour))
		}
	}

	runs, err := s.RecentRuns("main", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-ancient" {
		t.Errorf("RecentRuns after PruneIgnoredRefs = %+v, want run-ancient untouched", runs)
	}
}
