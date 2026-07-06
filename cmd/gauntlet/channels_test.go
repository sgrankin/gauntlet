package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/history"
)

// fakeSummarizeGit is a no-op summarize.Git, just enough for buildSummarizer
// to type-check and construct a *summarize.Summarizer without touching real
// git — this file only tests cmd's Config -> Params wiring, not the
// summarizer's own behavior (covered by internal/summarize's own tests).
type fakeSummarizeGit struct{}

func (fakeSummarizeGit) Log(ctx context.Context, base, tip string) ([]gitx.CommitInfo, error) {
	return nil, nil
}

func (fakeSummarizeGit) DiffStat(ctx context.Context, base, tip string) (string, error) {
	return "", nil
}

func TestBuildSummarizer_DisabledWhenSectionAbsent(t *testing.T) {
	cfg := &config.Daemon{}
	s, err := buildSummarizer(cfg, fakeSummarizeGit{})
	if err != nil {
		t.Fatalf("buildSummarizer: %v", err)
	}
	if s != nil {
		t.Fatalf("buildSummarizer = %v, want nil when Summarize is unset", s)
	}
}

func TestBuildSummarizer_MissingAPIKeyIsLoudError(t *testing.T) {
	t.Setenv("GAUNTLET_TEST_SUMMARIZE_KEY", "")
	cfg := &config.Daemon{
		Summarize: &config.Summarize{
			Model:     "claude-haiku-4-5",
			APIKeyEnv: "GAUNTLET_TEST_SUMMARIZE_KEY",
		},
	}
	_, err := buildSummarizer(cfg, fakeSummarizeGit{})
	if err == nil {
		t.Fatal("buildSummarizer: want an error when the API key env var is unset")
	}
	if !strings.Contains(err.Error(), "GAUNTLET_TEST_SUMMARIZE_KEY") {
		t.Errorf("error = %q, want it to name the missing env var", err.Error())
	}
}

func TestBuildSummarizer_ConstructsWhenConfigured(t *testing.T) {
	t.Setenv("GAUNTLET_TEST_SUMMARIZE_KEY", "sk-test-key")
	cfg := &config.Daemon{
		Summarize: &config.Summarize{
			Model:     "claude-haiku-4-5",
			APIKeyEnv: "GAUNTLET_TEST_SUMMARIZE_KEY",
		},
	}
	s, err := buildSummarizer(cfg, fakeSummarizeGit{})
	if err != nil {
		t.Fatalf("buildSummarizer: %v", err)
	}
	if s == nil {
		t.Fatal("buildSummarizer = nil, want a constructed Summarizer")
	}
}

// --- parseOutcome / buildSeedParks (Feature 2: park persistence) ----------

func TestParseOutcome(t *testing.T) {
	cases := []struct {
		in   string
		want core.Outcome
		ok   bool
	}{
		{"landed", core.OutcomeLanded, true},
		{"rejected", core.OutcomeRejected, true},
		{"conflict", core.OutcomeConflict, true},
		{"skipped", core.OutcomeSkipped, true},
		{"error", core.OutcomeError, true},
		{"bogus", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseOutcome(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseOutcome(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestBuildSeedParks_MapsLatestRedVerdicts opens a real history.Store (same
// tier internal/history's own tests use), writes an interleaved history for
// two refs, and confirms buildSeedParks's closure hands queue.Config.
// SeedParks every ref's latest verdict, string outcome parsed back to
// core.Outcome — the queue side (seedParksOnce) is what actually filters to
// red-family outcomes, so this closure is expected to pass everything
// through unfiltered, landed included.
func TestBuildSeedParks_MapsLatestRedVerdicts(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer store.Close()

	writeRun := func(runID, ref string, outcome core.Outcome, detail string) {
		t.Helper()
		rec := &core.RunRecord{
			RunID:  runID,
			Target: "main",
			Candidate: core.Candidate{
				Ref: ref, Target: "main", User: "alice", Topic: "feat", SHA: "sha-" + runID,
			},
			Outcome: outcome,
			Detail:  detail,
		}
		if err := store.Emit(context.Background(), core.Event{Kind: core.EventLanded, Target: "main", RunID: runID, Record: rec}); err != nil {
			t.Fatalf("Emit(%s): %v", runID, err)
		}
	}

	refRed := "refs/heads/for/main/alice/red"
	refGreen := "refs/heads/for/main/alice/green"
	writeRun("run-1", refRed, core.OutcomeRejected, "check failed")
	writeRun("run-2", refGreen, core.OutcomeLanded, "")

	seedParks := buildSeedParks(store)
	seeds := seedParks("main")

	byRef := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		byRef[s.Ref] = true
		switch s.Ref {
		case refRed:
			if s.Outcome != core.OutcomeRejected || s.SHA != "sha-run-1" || s.Reason != "check failed" || s.RunID != "run-1" {
				t.Errorf("refRed seed = %+v, want Outcome=Rejected SHA=sha-run-1 Reason=%q RunID=run-1", s, "check failed")
			}
		case refGreen:
			if s.Outcome != core.OutcomeLanded {
				t.Errorf("refGreen seed = %+v, want Outcome=Landed (queue itself filters this out, not this closure)", s)
			}
		}
	}
	if !byRef[refRed] || !byRef[refGreen] {
		t.Fatalf("seeds = %+v, want entries for both %s and %s", seeds, refRed, refGreen)
	}

	// A target with no history at all degrades to an empty slice, not an error.
	if got := seedParks("does-not-exist"); len(got) != 0 {
		t.Errorf("seedParks(unknown target) = %+v, want empty", got)
	}
}
