package executor

import (
	"context"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestGatedExecutor_ReleaseAfterStart(t *testing.T) {
	g := NewGatedExecutor()
	job := core.CheckJob{RunID: "run1", Name: "lint"}

	done := make(chan core.CheckResult, 1)
	go func() {
		done <- g.RunCheck(context.Background(), job)
	}()

	select {
	case <-g.Started("run1", "lint"):
	case <-time.After(5 * time.Second):
		t.Fatal("check never registered as started")
	}

	want := core.CheckResult{Name: "lint", Status: core.CheckPassed}
	g.Release("run1", "lint", want)

	select {
	case got := <-done:
		if got.Status != want.Status || got.Name != want.Name {
			t.Fatalf("RunCheck returned %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCheck did not return after Release")
	}
}

func TestGatedExecutor_ReleaseBeforeStart(t *testing.T) {
	// Release ordering shouldn't matter: a test that releases before the
	// queue has actually called RunCheck must not deadlock or drop the
	// result.
	g := NewGatedExecutor()
	job := core.CheckJob{RunID: "run1", Name: "test"}
	want := core.CheckResult{Name: "test", Status: core.CheckFailed}

	g.Release("run1", "test", want)

	got := g.RunCheck(context.Background(), job)
	if got.Status != want.Status {
		t.Fatalf("Status = %v, want %v", got.Status, want.Status)
	}
}

func TestGatedExecutor_CtxCancel(t *testing.T) {
	g := NewGatedExecutor()
	job := core.CheckJob{RunID: "run1", Name: "build"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan core.CheckResult, 1)
	go func() {
		done <- g.RunCheck(ctx, job)
	}()

	select {
	case <-g.Started("run1", "build"):
	case <-time.After(5 * time.Second):
		t.Fatal("check never registered as started")
	}

	cancel()

	select {
	case got := <-done:
		if got.Err == nil {
			t.Fatalf("Err = nil, want ctx cancellation error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCheck did not return after ctx cancel")
	}
}

func TestGatedExecutor_MultipleChecksIndependent(t *testing.T) {
	g := NewGatedExecutor()
	job1 := core.CheckJob{RunID: "run1", Name: "lint"}
	job2 := core.CheckJob{RunID: "run1", Name: "test"}

	done1 := make(chan core.CheckResult, 1)
	done2 := make(chan core.CheckResult, 1)
	go func() { done1 <- g.RunCheck(context.Background(), job1) }()
	go func() { done2 <- g.RunCheck(context.Background(), job2) }()

	<-g.Started("run1", "lint")
	<-g.Started("run1", "test")

	g.Release("run1", "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	select {
	case <-done1:
		t.Fatal("lint should not have been released yet")
	case got := <-done2:
		if got.Status != core.CheckPassed {
			t.Fatalf("test Status = %v, want CheckPassed", got.Status)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("test check did not return")
	}

	g.Release("run1", "lint", core.CheckResult{Name: "lint", Status: core.CheckFailed})
	select {
	case got := <-done1:
		if got.Status != core.CheckFailed {
			t.Fatalf("lint Status = %v, want CheckFailed", got.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lint check did not return")
	}
}
