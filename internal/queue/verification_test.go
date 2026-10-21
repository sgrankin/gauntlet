package queue

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/verification"
)

type inputGatedExecutor struct {
	*executor.GatedExecutor
	mu      sync.Mutex
	started []core.CheckJob
}

func (e *inputGatedExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	if job.InputsCapture {
		contents, err := os.ReadFile(filepath.Join(job.Dir, job.Candidate.Topic+".txt"))
		shared, _ := os.ReadFile(filepath.Join(job.Dir, "shared.txt"))
		return core.CheckResult{Name: job.Name, Status: core.CheckPassed, InputsKey: verification.Key(string(contents), string(shared)), Err: err}
	}
	e.mu.Lock()
	e.started = append(e.started, job)
	e.mu.Unlock()
	return e.GatedExecutor.RunCheck(ctx, job)
}

func TestRejectedPredecessorRetainsIndependentChecks(t *testing.T) {
	for _, scenario := range []string{"running", "completed", "shared-input-changed"} {
		t.Run(scenario, func(t *testing.T) {
			complete := scenario != "running"
			sharedChanged := scenario == "shared-input-changed"
			ex := &inputGatedExecutor{GatedExecutor: executor.NewGatedExecutor()}
			h := newIntegrationHarness(t, nil, ex, config.Target{Name: "main", Branch: "main", Landing: "squash", Mode: "speculate", Window: 3})
			evidence, err := verification.Open(t.Context(), filepath.Join(t.TempDir(), "verification.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer evidence.Close()
			h.d.cfg.Verification = evidence
			h.remote.Seed("main", map[string]string{testCheckSpecPath: `workspace "isolated"
check "test" {
 command "test"
 inputs "identity"
}
`})
			aFiles := map[string]string{"a.txt": "web"}
			if sharedChanged {
				aFiles["shared.txt"] = "new library"
			}
			h.remote.PushCandidate("main", "author", "a", aFiles)
			h.remote.PushCandidate("main", "author", "b", map[string]string{"b.txt": "billing"})
			h.remote.PushCandidate("main", "author", "c", map[string]string{"c.txt": "search"})
			h.reconcile()
			original := append([]*run(nil), h.d.lanes["main"].runs...)
			for _, r := range original {
				h.awaitStarted(ex.GatedExecutor, r.runID, "test")
			}
			if complete {
				for _, r := range original[1:] {
					h.releaseGated(ex.GatedExecutor, r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
				}
			}
			h.releaseGated(ex.GatedExecutor, original[0].runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed})
			h.reconcile()
			if !complete {
				for _, r := range original[1:] {
					ex.Release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
				}
			}
			released := map[string]bool{}
			deadline := time.Now().Add(15 * time.Second)
			for {
				h.reconcile()
				if sharedChanged {
					ex.mu.Lock()
					jobs := append([]core.CheckJob(nil), ex.started...)
					ex.mu.Unlock()
					for _, job := range jobs[3:] {
						if !released[job.RunID] {
							ex.Release(job.RunID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
							released[job.RunID] = true
						}
					}
				}
				landed := 0
				for _, record := range h.ch.Records() {
					if record.Outcome == core.OutcomeLanded {
						landed++
						if record.Checks[0].Reused == sharedChanged || (record.Checks[0].ExecutedSHA == record.MergeSHA) != sharedChanged {
							t.Fatalf("missing reused provenance: %+v", record.Checks)
						}
					}
				}
				if landed == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("successors failed to land: %+v", h.d.Snapshot())
				}
			}
			ex.mu.Lock()
			defer ex.mu.Unlock()
			want := 3
			if sharedChanged {
				want = 5
			}
			if len(ex.started) != want {
				t.Fatalf("independent checks reran: %+v", ex.started)
			}
		})
	}
}

func TestQueueOrderRestoresBeforeDiscoveringNewArrivals(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
	h.d.controls.Pauses["main"] = Pause{}
	h.git.seed("main", checkSpecFile("test"))
	b, c, a := candidateRef("main", "author", "b"), candidateRef("main", "author", "c"), candidateRef("main", "author", "a")
	h.git.pushCandidate(b, "", map[string]string{"b.txt": "b"})
	h.git.pushCandidate(c, "", map[string]string{"c.txt": "c"})
	h.reconcile()
	h.git.pushCandidate(a, "", map[string]string{"a.txt": "a"})
	replacement, err := New(h.git, h.d.exec, nil, h.d.cfg, h.now)
	if err != nil {
		t.Fatal(err)
	}
	replacement.controls.Pauses["main"] = Pause{}
	if err := replacement.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	order := replacement.order["main"]
	if !(order[b] < order[c] && order[c] < order[a]) {
		t.Fatalf("recovery reordered existing work: %+v", order)
	}
}

func TestCheckpointRetainsParksAndInfrastructureRetryBudget(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
	h.git.seed("main", checkSpecFile("test"))
	ref := candidateRef("main", "author", "failed")
	sha := h.git.pushCandidate(ref, "", map[string]string{"failed.txt": "change"})
	h.d.done["main"] = map[string]parkEntry{ref: {SHA: sha, Outcome: core.OutcomeError, Reason: "worker lost"}}
	h.d.autoRetried["main"] = map[string]string{ref: sha}
	if !h.d.persistScheduler() {
		t.Fatal(h.d.controls.LastError)
	}
	replacement, err := New(h.git, h.d.exec, nil, h.d.cfg, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.done["main"][ref].SHA != sha || replacement.autoRetried["main"][ref] != sha {
		t.Fatal("recovery forgot park or spent retry budget")
	}
}

func TestCheckpointWriteFailureStopsAdmissionAndRecoversLive(t *testing.T) {
	h := newHarness(t)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	h.d.cfg.ControlPath = filepath.Join(blocked, "controls.json")
	h.git.seed("main", checkSpecFile("test"))
	h.git.pushCandidate(candidateRef("main", "author", "change"), "", map[string]string{"a": "change"})
	if err := h.d.ReconcileOnce(t.Context()); err != nil || !h.d.controls.Uncertain || h.d.headRun("main") != nil {
		t.Fatalf("failed persistence admitted work: %v", err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.d.ReconcileOnce(t.Context()); err != nil || h.d.controls.Uncertain || h.d.headRun("main") == nil {
		t.Fatalf("checkpoint did not recover live: %v", err)
	}
}
