package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

type persistedReview struct {
	candidate core.Candidate
	eligible  bool
}

func (r *persistedReview) Candidates(context.Context) ([]core.Candidate, error) {
	if !r.eligible {
		return nil, nil
	}
	return []core.Candidate{r.candidate}, nil
}
func (*persistedReview) Validate(context.Context, core.Candidate) error       { return nil }
func (*persistedReview) Landed(context.Context, core.Candidate, string) error { return nil }

func TestReviewParkSurvivesRestartAndMetadataEditUnparks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{".gauntlet.kdl": `check "test" { command "true"; }`})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "author", "change", map[string]string{"change": "one"})
	source := remote.Ref(ref)
	remote.SetRef("refs/heads/feature", source)
	remote.DeleteCandidate(ref)
	review := &persistedReview{candidate: core.Candidate{Ref: "refs/heads/for/main/github/pr-0000000001", Target: "main", SHA: source, Source: "github", SourceBase: base, Message: "Change", Version: "original"}}
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	record := &core.RunRecord{RunID: "prior", Target: "main", Candidate: review.candidate, Outcome: core.OutcomeRejected, Detail: "prior failure", StartedAt: now, EndedAt: now.Add(time.Second)}
	if err := store.Emit(ctx, core.Event{Kind: core.EventRejected, Target: "main", Record: record}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	row, _, err := store.Run("prior")
	if err != nil {
		t.Fatal(err)
	}
	if row.CandidateVersion != "original" {
		t.Fatalf("stored version=%q", row.CandidateVersion)
	}
	repo, err := gitx.New(ctx, remote.BareClone(), remote.Dir)
	if err != nil {
		t.Fatal(err)
	}
	gated := executor.NewGatedExecutor()
	daemon, err := queue.New(repo, gated, nil, queue.Config{Targets: []config.Target{{Name: "main", Branch: "main", Landing: "squash"}}, CheckSpec: ".gauntlet.kdl", Committer: core.Identity{Name: "Queue", Email: "queue@example.com"}, Reviews: review, SeedParks: buildSeedParks(store)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap := daemon.Snapshot().Targets[0]
	if len(snap.Parked) != 0 || len(snap.Pipeline) != 0 {
		t.Fatalf("ineligible review appeared active: %+v", snap)
	}
	review.eligible = true
	if err := daemon.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap = daemon.Snapshot().Targets[0]
	if len(snap.Parked) != 1 || len(snap.Pipeline) != 0 || snap.Parked[0].RunID != "prior" {
		t.Fatalf("restart lost park: %+v", snap)
	}
	review.eligible = false
	if err := daemon.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(daemon.Snapshot().Targets[0].Parked) != 0 {
		t.Fatal("inactive review remained visible")
	}
	review.eligible = true
	if err := daemon.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if snap := daemon.Snapshot().Targets[0]; len(snap.Parked) != 1 || len(snap.Pipeline) != 0 {
		t.Fatal("temporary readiness loss cleared failure verdict")
	}
	review.candidate.Version = "edited-message"
	if err := daemon.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap = daemon.Snapshot().Targets[0]
	if len(snap.Parked) != 0 || len(snap.Pipeline) != 1 {
		t.Fatalf("metadata edit did not unpark: %+v", snap)
	}
}
