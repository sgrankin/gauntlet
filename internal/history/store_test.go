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

func TestStore_Emit_IgnoresNonTerminalEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	nonTerminal := []core.Event{
		{Kind: core.EventQueued, Target: "main"},
		{Kind: core.EventTrialClean, Target: "main"},
		{Kind: core.EventCheckStarted, Target: "main", RunID: "run-1", CheckName: "lint"},
		{Kind: core.EventCheckFinished, Target: "main", RunID: "run-1", CheckName: "lint"},
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

// TestStore_ConcurrentReadsDontBlockWrites is the sanity check for raising
// SetMaxOpenConns from 1 to 4 (docs/plans/phase23.md review): Emit runs
// inline on the reconcile goroutine in production, so a dashboard read must
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
