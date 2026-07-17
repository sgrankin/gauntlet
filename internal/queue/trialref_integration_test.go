package queue

import (
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_TrialRefPublishedAndLanded proves the queue↔gitx
// trial-ref path against REAL git: a run publishes its tested merge under
// the custom namespace on the remote (visible via ls-remote, which the
// daemon's fetch refspec never mirrors), then the landing CAS-deletes it.
func TestIntegration_TrialRefPublishedAndLanded(t *testing.T) {
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated)
	h.d.cfg.TrialRefs = true
	h.d.cfg.TrialRefPrefix = "refs/gauntlet/trials"
	h.d.cfg.TrialRefRetention = time.Hour
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})
	remote.PushCandidate("main", "alice", "widget", checkSpecFile("test"))

	h.reconcile() // trial clean, merge published, check started
	runID := h.currentRunID()
	trialRef := "refs/gauntlet/trials/" + runID

	published := remote.Ref(trialRef)
	if published == "" {
		t.Fatalf("trial ref %s not on the remote after publish", trialRef)
	}
	// It names the tested merge carried by EventTrialMerged.
	var mergedSHA string
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventTrialMerged && e.Record != nil {
			mergedSHA = e.Record.MergeSHA
		}
	}
	if mergedSHA != published {
		t.Fatalf("EventTrialMerged MergeSHA %q != published ref %q", mergedSHA, published)
	}

	h.releaseGated(gated, runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: time.Second})

	if got := remote.Ref("refs/heads/main"); got != published {
		t.Fatalf("target = %q, want the published (and now landed) merge %q", got, published)
	}
	if remote.Ref(trialRef) != "" {
		t.Fatalf("trial ref %s survived the landing", trialRef)
	}
}
