package flaky

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/policy"
)

type classifyFunc func(context.Context, core.CheckJob, core.CheckResult) (Decision, error)

func (f classifyFunc) Classify(ctx context.Context, j core.CheckJob, r core.CheckResult) (Decision, error) {
	return f(ctx, j, r)
}

func TestRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision Decision
		err      error
		status   core.CheckStatus
		want     int
	}{
		{"flake", Decision{Action: "retry", Confidence: .95, Reason: "transient connection reset"}, nil, core.CheckPassed, 2},
		{"low confidence", Decision{Action: "retry", Confidence: .6, Reason: "uncertain"}, nil, core.CheckPassed, 1},
		{"abort", Decision{Action: "abort", Confidence: .95, Reason: "assertion changed"}, nil, core.CheckPassed, 1},
		{"abstain", Decision{Action: "abstain", Confidence: .9, Reason: "no evidence"}, nil, core.CheckPassed, 1},
		{"unavailable", Decision{}, errors.New("offline"), core.CheckPassed, 1},
		{"invalid", Decision{Action: "approve", Confidence: 1, Reason: "bad action"}, nil, core.CheckPassed, 1},
		{"exhausted", Decision{Action: "retry", Confidence: 1, Reason: "temporary"}, nil, core.CheckFailed, 3},
		{"skipped rerun", Decision{Action: "retry", Confidence: 1, Reason: "temporary"}, nil, core.CheckSkipped, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			classifications := 0
			r := &Retrier{Checks: []string{"test"}, MaxRetries: 2, MinConfidence: .8, Timeout: time.Second, Classifier: classifyFunc(func(context.Context, core.CheckJob, core.CheckResult) (Decision, error) {
				classifications++
				return tc.decision, tc.err
			})}
			job := core.CheckJob{Name: "test", RunID: "one-run", MergeSHA: "tested", Dir: "workspace", ServiceEnv: []string{"DB_PORT=123"}}
			res := r.Run(context.Background(), job, func(_ context.Context, j core.CheckJob) core.CheckResult {
				if j.RunID != job.RunID || j.MergeSHA != job.MergeSHA || j.Dir != job.Dir || j.ServiceEnv[0] != job.ServiceEnv[0] {
					t.Fatal("retry changed execution identity")
				}
				count++
				status := core.CheckFailed
				if count > 1 {
					status = tc.status
				}
				return core.CheckResult{Status: status, Output: "attempt output", UserCPU: time.Second, PeakRSS: int64(count)}
			})
			if count != tc.want {
				t.Fatalf("executions=%d want %d", count, tc.want)
			}
			if tc.want > 1 && tc.status == core.CheckPassed && res.Status != core.CheckPassed {
				t.Fatal("rerun did not pass")
			}
			if (tc.want == 1 || (tc.status == core.CheckFailed || tc.status == core.CheckSkipped)) && res.Status != core.CheckFailed {
				t.Fatal("model converted failure to green")
			}
			if res.UserCPU != time.Duration(count)*time.Second || res.PeakRSS != int64(count) {
				t.Fatal("lost attempt resource usage")
			}
			if !strings.Contains(res.Output, "attempt 1: failed") {
				t.Fatal("lost original failure")
			}
			if classifications > 2 {
				t.Fatal("unbounded decisions")
			}
		})
	}
}

func TestNonFailuresAndExcludedJobsNeverClassified(t *testing.T) {
	r := &Retrier{Checks: []string{"test"}, MaxRetries: 1, Timeout: time.Second, Classifier: classifyFunc(func(context.Context, core.CheckJob, core.CheckResult) (Decision, error) {
		t.Fatal("unexpected classification")
		return Decision{}, nil
	})}
	for _, tc := range []struct {
		job core.CheckJob
		res core.CheckResult
	}{
		{core.CheckJob{Name: "other"}, core.CheckResult{}},
		{core.CheckJob{Name: "test", OperatorOwned: true}, core.CheckResult{}},
		{core.CheckJob{Name: "test", ImageBuild: true}, core.CheckResult{}},
		{core.CheckJob{Name: "test", ReceiptCapture: true}, core.CheckResult{}},
		{core.CheckJob{Name: "test"}, core.CheckResult{Status: core.CheckPassed}},
		{core.CheckJob{Name: "test"}, core.CheckResult{Status: core.CheckSkipped}},
		{core.CheckJob{Name: "test"}, core.CheckResult{Err: errors.New("service died")}},
	} {
		r.Run(context.Background(), tc.job, func(context.Context, core.CheckJob) core.CheckResult { return tc.res })
	}
}

func TestClassificationDeadlinePreservesFailure(t *testing.T) {
	r := &Retrier{Checks: []string{"test"}, MaxRetries: 1, MinConfidence: .8, Timeout: time.Millisecond, Classifier: classifyFunc(func(ctx context.Context, _ core.CheckJob, _ core.CheckResult) (Decision, error) {
		<-ctx.Done()
		return Decision{Action: "retry", Confidence: 1, Reason: "late"}, nil
	})}
	executions := 0
	result := r.Run(context.Background(), core.CheckJob{Name: "test"}, func(context.Context, core.CheckJob) core.CheckResult {
		executions++
		return core.CheckResult{Output: "failed"}
	})
	if executions != 1 || result.Status != core.CheckFailed || result.Err != nil {
		t.Fatal("timeout altered original verdict")
	}
}

func TestRetryPreservesFullLog(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "test.log.zst")
	r := &Retrier{Checks: []string{"test"}, MaxRetries: 1, MinConfidence: .8, Timeout: time.Second, Classifier: classifyFunc(func(context.Context, core.CheckJob, core.CheckResult) (Decision, error) {
		return Decision{Action: "retry", Confidence: 1, Reason: "transient"}, nil
	})}
	job := core.CheckJob{Name: "test", Dir: dir, LogPath: original, Command: []string{"sh", "-c", `if test -f marker; then echo 'successful attempt'; else touch marker; echo 'initial failure'; exit 1; fi`}}
	result := r.Run(context.Background(), job, executor.LocalExecutor{}.RunCheck)
	if result.Status != core.CheckPassed || result.Err != nil || result.LogPath != original {
		t.Fatalf("result=%+v", result)
	}
	file, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	dec, err := zstd.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	data, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"initial failure", "confidence=1.000", "retry attempt 2", "successful attempt"} {
		if !strings.Contains(string(data), part) {
			t.Fatalf("log missing %q: %s", part, data)
		}
	}
}

func TestCancellationStopsDecisionAndRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	deciding := make(chan struct{})
	finished := make(chan core.CheckResult, 1)
	r := &Retrier{Checks: []string{"test"}, MaxRetries: 1, MinConfidence: .8, Timeout: time.Hour, Classifier: classifyFunc(func(ctx context.Context, _ core.CheckJob, _ core.CheckResult) (Decision, error) {
		close(deciding)
		<-ctx.Done()
		return Decision{Action: "retry", Confidence: 1, Reason: "too late"}, nil
	})}
	executions := 0
	go func() {
		finished <- r.Run(ctx, core.CheckJob{Name: "test"}, func(context.Context, core.CheckJob) core.CheckResult {
			executions++
			return core.CheckResult{Output: "failed"}
		})
	}()
	<-deciding
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("classification ignored cancellation")
	}
	if executions != 1 {
		t.Fatal("cancelled decision started another test")
	}
}

func TestCustomRetryPolicyCannotExceedBudget(t *testing.T) {
	engine, err := policy.Compile(context.Background(), `package gauntlet
retry := {"allow":true,"requirements":[]}`, time.Second, policy.Options{Replace: []string{"retry"}})
	if err != nil {
		t.Fatal(err)
	}
	r := &Retrier{Policy: engine, Checks: []string{"test"}, MaxRetries: 1, MinConfidence: .9, Timeout: time.Second, Classifier: classifyFunc(func(context.Context, core.CheckJob, core.CheckResult) (Decision, error) {
		return Decision{Action: "retry", Confidence: .1, Reason: "infra"}, nil
	})}
	count := 0
	result := r.Run(context.Background(), core.CheckJob{Name: "test"}, func(context.Context, core.CheckJob) core.CheckResult {
		count++
		return core.CheckResult{Status: core.CheckFailed}
	})
	if count != 2 || result.Status != core.CheckFailed {
		t.Fatalf("policy bypassed budget or actual verdict: %d %+v", count, result)
	}
}
