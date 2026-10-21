package workers

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func workerFixture() Worker {
	return Worker{ID: "linux-1", Session: "session-1", Profiles: []string{"sql"}, Labels: []string{"linux"}, Capacity: Resources{Slots: 2, CPU: 4000, Memory: 8 << 30}}
}
func jobFixture() core.CheckJob {
	return core.CheckJob{Name: "test", Executor: "sql", Command: []string{"./ci/test"}, MergeSHA: "exact-sha", RunID: "original"}
}
func requirementsFixture() Requirements {
	return Requirements{Labels: []string{"linux"}, Resources: Resources{Slots: 1, CPU: 2000, Memory: 2 << 30}}
}

func TestPullMatchingPriorityReservationAndResourceAccounting(t *testing.T) {
	broker, err := Open(filepath.Join(t.TempDir(), "workers.db"), time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	ctx := t.Context()
	job := jobFixture()
	needs := requirementsFixture()
	normal, err := broker.Submit(ctx, "normal", job, needs, 1)
	if err != nil {
		t.Fatal(err)
	}
	worker := workerFixture()
	worker.UrgentReserve = 1
	first, err := broker.Claim(ctx, worker)
	if err != nil || first == nil || first.ID != normal {
		t.Fatalf("ordinary claim: %+v %v", first, err)
	}
	_, err = broker.Submit(ctx, "normal-two", job, needs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if next, err := broker.Claim(ctx, worker); err != nil || next != nil {
		t.Fatalf("ordinary consumed reserve: %+v %v", next, err)
	}
	urgent, err := broker.Submit(ctx, "urgent", job, needs, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := broker.Claim(ctx, worker)
	if err != nil || second == nil || second.ID != urgent {
		t.Fatalf("urgent claim: %+v %v", second, err)
	}
	if next, err := broker.Claim(ctx, worker); err != nil || next != nil {
		t.Fatalf("overcommitted worker: %+v %v", next, err)
	}
	if err := broker.Complete(ctx, first.ID, first.Token, EncodeResult(core.CheckResult{Status: core.CheckPassed})); err != nil {
		t.Fatal(err)
	}
	worker.UrgentReserve = 0
	worker.Profiles = []string{"other"}
	if next, err := broker.Claim(ctx, worker); err != nil || next != nil {
		t.Fatalf("wrong profile matched: %+v %v", next, err)
	}
	worker.Profiles = []string{"sql"}
	worker.Labels = []string{"windows"}
	if next, err := broker.Claim(ctx, worker); err != nil || next != nil {
		t.Fatalf("wrong OS matched: %+v %v", next, err)
	}
}

func TestLeaseReassignmentRejectsStaleResultAndAcceptsLateUnreassignedResult(t *testing.T) {
	for _, reassign := range []bool{false, true} {
		t.Run(map[bool]string{false: "late-result", true: "replacement"}[reassign], func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			path := filepath.Join(t.TempDir(), "workers.db")
			broker, err := Open(path, time.Minute, 2*time.Minute, clock)
			if err != nil {
				t.Fatal(err)
			}
			id, err := broker.Submit(t.Context(), "stable", jobFixture(), requirementsFixture(), 1)
			if err != nil {
				t.Fatal(err)
			}
			first, err := broker.Claim(t.Context(), workerFixture())
			if err != nil || first == nil {
				t.Fatalf("claim: %+v %v", first, err)
			}
			if err := broker.Close(); err != nil {
				t.Fatal(err)
			}
			now = now.Add(10 * time.Minute)
			broker, err = Open(path, time.Minute, 2*time.Minute, clock)
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			if _, err := broker.Submit(t.Context(), "stable", jobFixture(), requirementsFixture(), 1); err != nil {
				t.Fatal(err)
			}
			replacement := workerFixture()
			replacement.ID = "linux-2"
			replacement.Session = "session-2"
			if attempt, err := broker.Claim(t.Context(), replacement); err != nil || attempt != nil {
				t.Fatalf("restart grace did not protect reconnection: %+v %v", attempt, err)
			}
			if reassign {
				now = now.Add(3 * time.Minute)
				next, err := broker.Claim(t.Context(), replacement)
				if err != nil || next == nil || next.ID != id || next.Token == first.Token {
					t.Fatalf("replacement: %+v %v", next, err)
				}
				if err := broker.Renew(t.Context(), id, first.Token); !errors.Is(err, ErrStale) {
					t.Fatalf("stale renew: %v", err)
				}
				if err := broker.Complete(t.Context(), id, first.Token, EncodeResult(core.CheckResult{Status: core.CheckPassed})); !errors.Is(err, ErrStale) {
					t.Fatalf("stale completion: %v", err)
				}
				first = next
			}
			if err := broker.Complete(t.Context(), id, first.Token, EncodeResult(core.CheckResult{Status: core.CheckPassed, Output: "actual result"})); err != nil {
				t.Fatal(err)
			}
			result, err := broker.Wait(t.Context(), id)
			if err != nil || result.Check.Output != "actual result" {
				t.Fatalf("recovered result: %+v %v", result, err)
			}
		})
	}
}

func TestCancellationAndWorkerIdentityCollision(t *testing.T) {
	broker, err := Open(filepath.Join(t.TempDir(), "workers.db"), time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	id, err := broker.Submit(t.Context(), "cancel", jobFixture(), requirementsFixture(), 1)
	if err != nil {
		t.Fatal(err)
	}
	worker := workerFixture()
	attempt, err := broker.Claim(t.Context(), worker)
	if err != nil || attempt == nil {
		t.Fatalf("claim: %+v %v", attempt, err)
	}
	worker.Session = "another process"
	if _, err := broker.Claim(t.Context(), worker); err == nil {
		t.Fatal("duplicate active worker identity accepted")
	}
	if err := broker.Cancel(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := broker.Renew(t.Context(), id, attempt.Token); !errors.Is(err, ErrStale) {
		t.Fatalf("cancelled attempt retained authority: %v", err)
	}
	if _, err := broker.Wait(context.Background(), id); !errors.Is(err, ErrStale) {
		t.Fatalf("cancelled wait: %v", err)
	}
}

func TestRetryLineageRecoversCoverageAndOriginalAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.db")
	broker, err := Open(path, time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	job := jobFixture()
	job.TestResults = "json"
	job.RetryCommand = []string{"./ci/retry"}
	initial, err := broker.Submit(t.Context(), "root", job, requirementsFixture(), 1)
	if err != nil {
		t.Fatal(err)
	}
	first, err := broker.Claim(t.Context(), workerFixture())
	if err != nil || first == nil {
		t.Fatalf("claim: %+v %v", first, err)
	}
	if err = broker.Complete(t.Context(), initial, first.Token, EncodeResult(core.CheckResult{Status: core.CheckFailed, Tests: []core.TestCase{{ID: "green", Status: "passed"}, {ID: "red", Status: "failed"}}})); err != nil {
		t.Fatal(err)
	}
	job.PreviousExecution = initial
	job.Command = job.RetryCommand
	job.RetryTests = []string{"red"}
	retry, err := broker.Submit(t.Context(), "root", job, requirementsFixture(), 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := broker.Claim(t.Context(), workerFixture())
	if err != nil || second == nil || second.ID != retry {
		t.Fatalf("retry claim: %+v %v", second, err)
	}
	if err = broker.Complete(t.Context(), retry, second.Token, EncodeResult(core.CheckResult{Status: core.CheckPassed, Tests: []core.TestCase{{ID: "red", Status: "passed"}}})); err != nil {
		t.Fatal(err)
	}
	broker.Close()
	broker, err = Open(path, time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	job = jobFixture()
	job.TestResults = "json"
	adopted, err := broker.Submit(t.Context(), "root", job, requirementsFixture(), 1)
	if err != nil || adopted != retry {
		t.Fatalf("recovery restarted original failed command: %s %v", adopted, err)
	}
	result, err := broker.Wait(t.Context(), adopted)
	if err != nil || result.Check.Status != core.CheckPassed || len(result.Check.Tests) != 2 || len(result.Check.TestAttempts) != 2 || result.Check.TestAttempts[0].Tests[1].Status != "failed" {
		t.Fatalf("lost coverage or retry history: %+v %v", result, err)
	}
}

func TestWorkerReportedReservationsProtectStoppingStaleWork(t *testing.T) {
	broker, err := Open(filepath.Join(t.TempDir(), "workers.db"), time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	if _, err = broker.Submit(t.Context(), "pending", jobFixture(), requirementsFixture(), 1); err != nil {
		t.Fatal(err)
	}
	worker := workerFixture()
	worker.Usage = Resources{Slots: 1, CPU: worker.Capacity.CPU, Memory: 2 << 30}
	if attempt, err := broker.Claim(t.Context(), worker); err != nil || attempt != nil {
		t.Fatalf("overcommitted a stopping old attempt: %+v %v", attempt, err)
	}
	worker.Usage = Resources{}
	if attempt, err := broker.Claim(t.Context(), worker); err != nil || attempt == nil {
		t.Fatalf("released reservation not reclaimed: %+v %v", attempt, err)
	}
}
