package flaky

import (
	"context"
	"github.com/sgrankin/gauntlet/internal/core"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRunRetryBudgetAndObservedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failure.db")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			ok, err := h.Reserve(context.Background(), "run", 3)
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("reserved %d retries", accepted.Load())
	}
	err = h.Record(context.Background(), core.CheckJob{RunID: "run", Name: "test", MergeSHA: "tested"}, "failure", Decision{Action: "retry", Confidence: .9, Reason: "timeout"}, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	h, err = OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if ok, err := h.Reserve(context.Background(), "run", 3); err != nil || ok {
		t.Fatalf("budget forgotten: %t %v", ok, err)
	}
	rows, err := h.Recent(context.Background(), "test")
	if err != nil || len(rows) != 1 || !rows[0].Retried || !rows[0].Passed {
		t.Fatalf("history=%+v %v", rows, err)
	}
}
