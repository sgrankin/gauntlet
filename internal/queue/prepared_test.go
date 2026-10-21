package queue

import (
	"path/filepath"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

func TestPreparedCommitAndRunRecoverAcrossRestart(t *testing.T) {
	d, repo, _, remote := newChainHarness(t, nil, nil)
	remote.Seed("main", map[string]string{"base": "base"})
	ref := remote.PushCandidate("main", "alice", "fix", map[string]string{"fix": "fix"})
	if err := repo.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	refs, err := repo.ListRefs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cand := core.Candidate{Ref: ref, Target: "main", SHA: refs[ref]}
	d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
	d.cfg.VerificationContext = "same-config"
	first, _, err := d.buildChainLink(t.Context(), t.Context(), "main", refs["refs/heads/main"], cand, func(core.TrialMerge, string) string { return "original" })
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(repo, executor.NewGatedExecutor(), nil, d.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	observed := ""
	second, _, err := restored.buildChainLink(t.Context(), t.Context(), "main", refs["refs/heads/main"], cand, func(_ core.TrialMerge, prior string) string { observed = prior; return prior })
	if err != nil || observed != "original" || second.mergeOID != first.mergeOID {
		t.Fatalf("recreated prepared commit: %+v %q %v", second, observed, err)
	}
	restored.forgetPrepared("original")
	if !restored.persistScheduler() {
		t.Fatal(restored.controls.LastError)
	}
	third, _, err := restored.buildChainLink(t.Context(), t.Context(), "main", refs["refs/heads/main"], cand, func(_ core.TrialMerge, prior string) string {
		if prior != "" {
			t.Fatal("terminal attempt reused on explicit retry")
		}
		return "retry"
	})
	if err != nil || third.mergeOID == first.mergeOID {
		t.Fatalf("retry did not get a fresh trial: %+v %v", third, err)
	}
	restored.cfg.VerificationContext = "changed-config"
	_, _, err = restored.buildChainLink(t.Context(), t.Context(), "main", refs["refs/heads/main"], cand, func(_ core.TrialMerge, prior string) string {
		if prior != "" {
			t.Fatal("config change reused old signing/construction context")
		}
		return "changed"
	})
	if err != nil {
		t.Fatal(err)
	}
}
