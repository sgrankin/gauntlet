package history

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
			{Name: "lint", Status: core.CheckPassed, Duration: 500 * time.Millisecond},
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
	if checks[1].Name != "test" || checks[1].Status != "failed" || checks[1].Duration != 2500*time.Millisecond {
		t.Errorf("Run() checks[1] = %+v", checks[1])
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
