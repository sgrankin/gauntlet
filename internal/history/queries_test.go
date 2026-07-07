package history

import (
	"context"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// emitRun writes one minimal terminal run row directly (bypassing
// sampleRecord's fixed one-ref shape, store_test.go), so LatestTerminalPerRef
// tests can control Candidate.Ref/SHA/Outcome per row precisely — several
// rows sharing one ref, interleaving outcomes, is exactly what this query
// needs to prove correct.
func emitRun(t *testing.T, s *Store, runID, target, ref, sha string, outcome core.Outcome, detail string, started time.Time) {
	t.Helper()
	rec := &core.RunRecord{
		RunID:  runID,
		Target: target,
		Candidate: core.Candidate{
			Ref: ref, Target: target, User: "someone", Topic: "feat", SHA: sha,
		},
		Outcome:   outcome,
		Detail:    detail,
		StartedAt: started,
		EndedAt:   started.Add(time.Second),
	}
	if err := s.Emit(context.Background(), core.Event{Kind: core.EventLanded, Target: target, RunID: runID, Record: rec}); err != nil {
		t.Fatalf("Emit(%s): %v", runID, err)
	}
}

// TestStore_LatestTerminalPerRef_Interleaved is the interleaved-history test
// that park persistence across restarts depends on: LatestTerminalPerRef
// must return exactly one row per ref — the MOST RECENT by started_at — no
// matter what shape that ref's history takes.
func TestStore_LatestTerminalPerRef_Interleaved(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	refA := "refs/heads/for/main/alice/a" // red then landed: latest is landed, no park
	refB := "refs/heads/for/main/bob/b"   // landed then red: latest is red, a park candidate
	refC := "refs/heads/for/main/carol/c" // multiple reds in a row: only the LATEST wins

	emitRun(t, s, "a-1", "main", refA, "sha-a1", core.OutcomeRejected, "first try failed", base)
	emitRun(t, s, "a-2", "main", refA, "sha-a2", core.OutcomeLanded, "", base.Add(time.Minute))

	emitRun(t, s, "b-1", "main", refB, "sha-b1", core.OutcomeLanded, "", base)
	emitRun(t, s, "b-2", "main", refB, "sha-b2", core.OutcomeRejected, "regressed", base.Add(time.Minute))

	emitRun(t, s, "c-1", "main", refC, "sha-c1", core.OutcomeRejected, "red #1", base)
	emitRun(t, s, "c-2", "main", refC, "sha-c2", core.OutcomeConflict, "red #2", base.Add(time.Minute))
	emitRun(t, s, "c-3", "main", refC, "sha-c3", core.OutcomeError, "red #3 (latest)", base.Add(2*time.Minute))

	// A different target's history must never leak into "main"'s result.
	emitRun(t, s, "other-1", "release", "refs/heads/for/release/dave/d", "sha-other", core.OutcomeRejected, "", base)

	verdicts, err := s.LatestTerminalPerRef("main")
	if err != nil {
		t.Fatalf("LatestTerminalPerRef: %v", err)
	}
	byRef := make(map[string]RefVerdict, len(verdicts))
	for _, v := range verdicts {
		byRef[v.Ref] = v
	}
	if len(byRef) != 3 {
		t.Fatalf("LatestTerminalPerRef = %d distinct refs, want 3 (got %+v)", len(byRef), verdicts)
	}

	if v := byRef[refA]; v.SHA != "sha-a2" || v.Outcome != "landed" {
		t.Errorf("refA (red-then-landed) latest = %+v, want SHA=sha-a2 Outcome=landed (no park)", v)
	}
	if v := byRef[refB]; v.SHA != "sha-b2" || v.Outcome != "rejected" || v.Detail != "regressed" || v.RunID != "b-2" {
		t.Errorf("refB (landed-then-red) latest = %+v, want SHA=sha-b2 Outcome=rejected Detail=regressed RunID=b-2 (a park)", v)
	}
	if v := byRef[refC]; v.SHA != "sha-c3" || v.Outcome != "error" || v.Detail != "red #3 (latest)" {
		t.Errorf("refC (multiple reds) latest = %+v, want SHA=sha-c3 Outcome=error (only the LATEST wins)", v)
	}
}

// TestStore_LatestTerminalPerRef_EmptyForUnknownTarget covers a target with
// no run history at all (or a typo'd name): an empty result, not an error.
func TestStore_LatestTerminalPerRef_EmptyForUnknownTarget(t *testing.T) {
	s := openTestStore(t)
	emitRun(t, s, "a-1", "main", "refs/heads/for/main/alice/a", "sha-a1", core.OutcomeRejected, "", time.Now())

	verdicts, err := s.LatestTerminalPerRef("does-not-exist")
	if err != nil {
		t.Fatalf("LatestTerminalPerRef: %v", err)
	}
	if len(verdicts) != 0 {
		t.Fatalf("LatestTerminalPerRef(unknown target) = %+v, want empty", verdicts)
	}
}
