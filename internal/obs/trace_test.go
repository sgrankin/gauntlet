package obs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Under the default no-op provider (no SDK registered), every span
// produced by this package must be non-recording, and every helper must
// tolerate the nil/zero inputs the queue could realistically pass it
// (e.g. a run that ends without ever producing a RunRecord) without
// panicking.

func TestNoopTracerProducesNonRecordingSpans(t *testing.T) {
	tr := Tracer()
	if tr == nil {
		t.Fatal("Tracer() returned nil")
	}

	ctx, root := StartRun(context.Background(), tr, "run-1", "main", core.Candidate{
		Ref: "refs/heads/for/main/alice/topic", SHA: "cand-sha",
	}, "merge-sha")
	if root.IsRecording() {
		t.Error("root span IsRecording() = true, want false under no-op provider")
	}

	_, trial := StartTrialMerge(ctx, tr)
	if trial.IsRecording() {
		t.Error("trial-merge span IsRecording() = true, want false")
	}
	EndSpan(trial, nil)
	EndSpan(trial, errors.New("conflict"))

	checkCtx, check := StartCheck(ctx, tr, "test")
	if check.IsRecording() {
		t.Error("check span IsRecording() = true, want false")
	}
	_ = checkCtx
	EndCheck(check, core.CheckResult{Name: "test", Status: core.CheckPassed, Duration: time.Second})

	_, land := StartLand(ctx, tr)
	if land.IsRecording() {
		t.Error("land span IsRecording() = true, want false")
	}
	EndSpan(land, nil)

	EndRun(root, &core.RunRecord{RunID: "run-1", Outcome: core.OutcomeLanded})
}

func TestCrossGoroutineParenting(t *testing.T) {
	tr := Tracer()
	ctx, root := StartRun(context.Background(), tr, "run-1", "main", core.Candidate{}, "")

	// The queue derives a fresh context per-goroutine from the held root
	// span; verify that path compiles and runs cleanly (no-op, so nothing
	// to assert on the resulting span beyond "didn't panic").
	done := make(chan struct{})
	go func() {
		defer close(done)
		goCtx := trace.ContextWithSpan(context.Background(), root)
		_, check := StartCheck(goCtx, tr, "build")
		EndCheck(check, core.CheckResult{Name: "build", Status: core.CheckSkipped})
	}()
	<-done

	_ = ctx
	EndRun(root, nil)
}

func TestEndCheckHandlesAllStatusesAndErr(t *testing.T) {
	tr := Tracer()
	_, root := StartRun(context.Background(), tr, "r", "main", core.Candidate{}, "")
	ctx := context.Background()
	_ = ctx

	cases := []core.CheckResult{
		{Name: "a", Status: core.CheckPassed, Duration: time.Millisecond},
		{Name: "b", Status: core.CheckFailed, Duration: time.Millisecond},
		{Name: "c", Status: core.CheckSkipped, Duration: time.Millisecond},
		{Name: "d", Status: core.CheckPassed, Err: errors.New("boom")},
		{}, // zero value: realistic for a check that never really ran
	}
	for _, c := range cases {
		_, span := StartCheck(context.Background(), tr, c.Name)
		EndCheck(span, c) // must not panic for any of the above
	}
	EndRun(root, nil)
}

func TestEndRunNilRecordDoesNotPanic(t *testing.T) {
	tr := Tracer()
	_, root := StartRun(context.Background(), tr, "", "", core.Candidate{}, "")
	EndRun(root, nil) // a run can end (e.g. crash-recovered land) without a record
}

// --- attribute-building logic, verified without any SDK ---

func attr(t *testing.T, kvs []attribute.KeyValue, key string) attribute.KeyValue {
	t.Helper()
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv
		}
	}
	t.Fatalf("attribute %q not present in %v", key, kvs)
	return attribute.KeyValue{}
}

func TestCheckAttributes(t *testing.T) {
	r := core.CheckResult{Name: "lint", Status: core.CheckFailed, Duration: 1500 * time.Millisecond}
	kvs := checkAttributes(r)

	if got := attr(t, kvs, AttrCheckName).Value.AsString(); got != "lint" {
		t.Errorf("name = %q, want %q", got, "lint")
	}
	if got := attr(t, kvs, AttrCheckStatus).Value.AsString(); got != "failed" {
		t.Errorf("status = %q, want %q", got, "failed")
	}
	if got := attr(t, kvs, AttrCheckDuration).Value.AsInt64(); got != 1500 {
		t.Errorf("duration = %d ms, want 1500", got)
	}
}

func TestCheckStatusString(t *testing.T) {
	cases := map[core.CheckStatus]string{
		core.CheckPassed:  "passed",
		core.CheckFailed:  "failed",
		core.CheckSkipped: "skipped",
	}
	for status, want := range cases {
		if got := checkStatusString(status); got != want {
			t.Errorf("checkStatusString(%v) = %q, want %q", status, got, want)
		}
	}
}

func TestRunAttributes(t *testing.T) {
	rec := &core.RunRecord{
		RunID:  "run-42",
		Target: "main",
		Candidate: core.Candidate{
			Ref: "refs/heads/for/main/alice/topic",
			SHA: "cand-sha",
		},
		BaseOID:  "base-oid",
		MergeSHA: "merge-sha",
		Checks: []core.CheckResult{
			{Name: "lint", Status: core.CheckPassed},
			{Name: "test", Status: core.CheckPassed},
		},
		Outcome: core.OutcomeLanded,
		Detail:  "",
	}
	kvs := runAttributes(rec)

	want := map[string]string{
		AttrRunID:        "run-42",
		AttrTarget:       "main",
		AttrCandidateRef: "refs/heads/for/main/alice/topic",
		AttrCandidateSHA: "cand-sha",
		AttrBaseOID:      "base-oid",
		AttrMergeSHA:     "merge-sha",
		AttrOutcome:      "landed",
	}
	for key, wantVal := range want {
		if got := attr(t, kvs, key).Value.AsString(); got != wantVal {
			t.Errorf("%s = %q, want %q", key, got, wantVal)
		}
	}
	if got := attr(t, kvs, AttrChecksTotal).Value.AsInt64(); got != 2 {
		t.Errorf("checks.total = %d, want 2", got)
	}
}

func TestOutcomeString(t *testing.T) {
	cases := map[core.Outcome]string{
		core.OutcomeLanded:   "landed",
		core.OutcomeRejected: "rejected",
		core.OutcomeConflict: "conflict",
		core.OutcomeSkipped:  "skipped",
		core.OutcomeError:    "error",
	}
	for outcome, want := range cases {
		if got := outcomeString(outcome); got != want {
			t.Errorf("outcomeString(%v) = %q, want %q", outcome, got, want)
		}
	}
}

func TestOutcomeStatus(t *testing.T) {
	cases := []struct {
		outcome core.Outcome
		detail  string
		wantC   codes.Code
		wantD   string
	}{
		{core.OutcomeLanded, "ignored", codes.Ok, ""},
		{core.OutcomeSkipped, "ignored", codes.Unset, ""},
		{core.OutcomeRejected, "check X failed", codes.Error, "check X failed"},
		{core.OutcomeConflict, "conflicted paths", codes.Error, "conflicted paths"},
		{core.OutcomeError, "infra failure", codes.Error, "infra failure"},
	}
	for _, c := range cases {
		gotC, gotD := outcomeStatus(c.outcome, c.detail)
		if gotC != c.wantC || gotD != c.wantD {
			t.Errorf("outcomeStatus(%v, %q) = (%v, %q), want (%v, %q)", c.outcome, c.detail, gotC, gotD, c.wantC, c.wantD)
		}
	}
}
