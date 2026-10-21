package history

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestCommitLookupChoosesLatestAttemptAndDisambiguatesSlots(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Millisecond)
	for _, tc := range []struct{ id, target, ref, landed string }{
		{"first", "main", "review", "tested-first"},
		{"retry", "main", "review", "tested-retry"},
		{"elsewhere", "release", "other", "tested-elsewhere"},
	} {
		record := sampleRecord(tc.id, tc.target, now)
		record.Candidate.Ref, record.Candidate.SHA, record.MergeSHA = tc.ref, "source", tc.landed
		if err := s.Emit(context.Background(), core.Event{Record: record}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ sha, target, ref, want string }{
		{"source", "", "", "elsewhere"},
		{"source", "main", "", "retry"},
		{"source", "", "review", "retry"},
		{"tested-first", "main", "review", "first"},
	} {
		row, err := s.RunForCommit(tc.sha, tc.target, tc.ref)
		if err != nil || row.RunID != tc.want {
			t.Fatalf("lookup %+v: %+v %v", tc, row, err)
		}
	}
}

func TestChangeLookupTracksNewHeadsWithoutCrossingTargets(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Millisecond)
	for _, tc := range []struct{ id, target, sha string }{
		{"first", "main", "old-head"},
		{"new", "main", "new-head"},
		{"elsewhere", "release", "new-head"},
	} {
		record := sampleRecord(tc.id, tc.target, now)
		record.Candidate.Ref, record.Candidate.SHA = "review", tc.sha
		if err := s.Emit(context.Background(), core.Event{Record: record}); err != nil {
			t.Fatal(err)
		}
	}
	row, err := s.RunForChange("main", "review")
	if err != nil || row.RunID != "new" || row.CandidateSHA != "new-head" {
		t.Fatalf("latest change: %+v %v", row, err)
	}
	if _, err := s.RunForChange("main", "other"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("crossed change identities: %v", err)
	}
}

func TestVerificationProvenanceRoundTrips(t *testing.T) {
	s := openTestStore(t)
	record := sampleRecord("rebuilt", "main", time.Now())
	record.Checks[0].VerificationID = "inputs-identity"
	record.Checks[0].ExecutedRun = "original"
	record.Checks[0].ExecutedSHA = "old-commit"
	record.Checks[0].Reused = true
	if err := s.Emit(t.Context(), core.Event{Record: record}); err != nil {
		t.Fatal(err)
	}
	_, checks, err := s.Run("rebuilt")
	if err != nil || len(checks) == 0 || checks[0].VerificationID != "inputs-identity" || checks[0].ExecutedRun != "original" || checks[0].ExecutedSHA != "old-commit" || !checks[0].Reused {
		t.Fatalf("provenance: %+v %v", checks, err)
	}
}
