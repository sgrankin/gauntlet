package verification

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestExecutionSurvivesWaiterAndRestoresEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	m, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	job := core.CheckJob{Name: "billing", RunID: "original", MergeSHA: "old-sha", Candidates: []core.Candidate{{Ref: "web"}, {Ref: "billing"}}}
	ctx, cancel := context.WithCancel(t.Context())
	started, finish := make(chan struct{}), make(chan struct{})
	detached := make(chan core.CheckResult, 1)
	var cleaned atomic.Int32
	go func() {
		detached <- m.Run(ctx, "identity", job, func(ctx context.Context) core.CheckResult {
			close(started)
			select {
			case <-finish:
				return core.CheckResult{Name: job.Name, Status: core.CheckPassed, LogPath: "original.log", Output: "tested", SuspectedRefs: []string{"web", "billing"}}
			case <-ctx.Done():
				return core.CheckResult{Err: ctx.Err()}
			}
		}, func() { cleaned.Add(1) })
	}()
	<-started
	cancel()
	if res := <-detached; res.Err == nil {
		t.Fatal("cancelled waiter succeeded")
	}
	if cleaned.Load() != 0 {
		t.Fatal("detaching destroyed execution resources")
	}
	job.RunID, job.MergeSHA = "rebuilt", "new-sha"
	job.Candidates = []core.Candidate{{Ref: "billing"}}
	adopted := make(chan core.CheckResult, 1)
	attached := make(chan struct{})
	go func() {
		adopted <- m.Run(t.Context(), "identity", job, func(context.Context) core.CheckResult { t.Error("matching work ran twice"); return core.CheckResult{} }, func() { close(attached) })
	}()
	<-attached
	close(finish)
	res := <-adopted
	if res.Err != nil || !res.Reused || res.ExecutedSHA != "old-sha" || res.LogPath != "original.log" || cleaned.Load() != 1 || len(res.SuspectedRefs) != 1 || res.SuspectedRefs[0] != "billing" {
		t.Fatalf("adoption: %+v cleanup=%d", res, cleaned.Load())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	res = m.Run(t.Context(), "identity", job, func(context.Context) core.CheckResult {
		t.Error("restart reran completed work")
		return core.CheckResult{}
	}, func() {})
	if !res.Reused || res.ExecutedRun != "original" {
		t.Fatalf("recovery: %+v", res)
	}
}

func TestFailedAndExpiredEvidenceCannotPass(t *testing.T) {
	m, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	job := core.CheckJob{Name: "test", RunID: "run", MergeSHA: "sha"}
	calls := 0
	run := func(context.Context) core.CheckResult {
		calls++
		return core.CheckResult{Name: "test", Status: core.CheckFailed}
	}
	for range 2 {
		if res := m.Run(t.Context(), "failure", job, run, func() {}); res.Status != core.CheckFailed {
			t.Fatal(res)
		}
	}
	if calls != 2 {
		t.Fatal("failed evidence was reused")
	}
	passed := func(context.Context) core.CheckResult {
		calls++
		return core.CheckResult{Name: "test", Status: core.CheckPassed}
	}
	m.Run(t.Context(), "passing", job, passed, func() {})
	if err := m.Maintain(time.Now().Add(Retention + time.Minute)); err != nil {
		t.Fatal(err)
	}
	m.Run(t.Context(), "passing", job, passed, func() {})
	if calls != 4 {
		t.Fatal("expired evidence survived")
	}
}

func TestUnclaimedExecutionIsCancelledAfterGrace(t *testing.T) {
	m, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(t.Context())
	started, stopped, detached := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		m.Run(ctx, "abandoned", core.CheckJob{}, func(ctx context.Context) core.CheckResult {
			close(started)
			<-ctx.Done()
			close(stopped)
			return core.CheckResult{Err: ctx.Err()}
		}, func() {})
		close(detached)
	}()
	<-started
	cancel()
	<-detached
	if err := m.Maintain(time.Now().Add(OrphanGrace + time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("abandoned work kept running")
	}
}
