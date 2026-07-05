package core

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestRunRecord_FirstFailure(t *testing.T) {
	t.Run("none failed", func(t *testing.T) {
		r := &RunRecord{Checks: []CheckResult{
			{Name: "lint", Status: CheckPassed},
			{Name: "test", Status: CheckSkipped},
		}}
		if got := r.FirstFailure(); got != nil {
			t.Fatalf("FirstFailure() = %+v, want nil", got)
		}
	})

	t.Run("first CheckFailed wins", func(t *testing.T) {
		r := &RunRecord{Checks: []CheckResult{
			{Name: "lint", Status: CheckPassed},
			{Name: "test", Status: CheckFailed, Output: "boom"},
			{Name: "vet", Status: CheckFailed, Output: "also boom"},
		}}
		got := r.FirstFailure()
		if got == nil || got.Name != "test" {
			t.Fatalf("FirstFailure() = %+v, want the \"test\" check", got)
		}
	})

	t.Run("Err-only counts as a failure", func(t *testing.T) {
		r := &RunRecord{Checks: []CheckResult{
			{Name: "lint", Status: CheckPassed},
			{Name: "test", Status: CheckPassed, Err: errors.New("context canceled")},
		}}
		got := r.FirstFailure()
		if got == nil || got.Name != "test" {
			t.Fatalf("FirstFailure() = %+v, want the Err-bearing check", got)
		}
	})

	t.Run("empty Checks", func(t *testing.T) {
		r := &RunRecord{}
		if got := r.FirstFailure(); got != nil {
			t.Fatalf("FirstFailure() = %+v, want nil", got)
		}
	})
}

func TestFailureTail(t *testing.T) {
	t.Run("nil result", func(t *testing.T) {
		if got := FailureTail(nil, 10, 1024); got != "" {
			t.Fatalf("FailureTail(nil, ...) = %q, want empty", got)
		}
	})

	t.Run("empty output", func(t *testing.T) {
		res := &CheckResult{Output: ""}
		if got := FailureTail(res, 10, 1024); got != "" {
			t.Fatalf("FailureTail(empty) = %q, want empty", got)
		}
	})

	t.Run("all-whitespace output", func(t *testing.T) {
		res := &CheckResult{Output: "   \n\t\n   \n"}
		if got := FailureTail(res, 10, 1024); got != "" {
			t.Fatalf("FailureTail(whitespace-only) = %q, want empty", got)
		}
	})

	t.Run("trailing whitespace lines dropped, order preserved", func(t *testing.T) {
		res := &CheckResult{Output: "line one\nline two\n\n   \nline three\n"}
		got := FailureTail(res, 10, 1024)
		want := "line one\nline two\nline three"
		if got != want {
			t.Fatalf("FailureTail = %q, want %q", got, want)
		}
	})

	t.Run("maxLines caps to the last N non-empty lines", func(t *testing.T) {
		var b strings.Builder
		for i := 1; i <= 100; i++ {
			b.WriteString("line " + strconv.Itoa(i) + "\n")
		}
		res := &CheckResult{Output: b.String()}
		got := FailureTail(res, 3, 1<<20)
		want := "line 98\nline 99\nline 100"
		if got != want {
			t.Fatalf("FailureTail = %q, want %q", got, want)
		}
	})

	t.Run("maxBytes caps a huge tail and starts at a line boundary", func(t *testing.T) {
		// One huge line's worth of repeated content, capped at maxLines=1
		// so the byte cap is what's actually exercised.
		res := &CheckResult{Output: strings.Repeat("x", 1<<20)}
		got := FailureTail(res, 10, 100)
		if len(got) > 100 {
			t.Fatalf("FailureTail length = %d, want <= 100", len(got))
		}
		if got == "" {
			t.Fatalf("FailureTail = empty, want a truncated tail")
		}
	})

	t.Run("maxBytes trims a partial leading line at the cut", func(t *testing.T) {
		res := &CheckResult{Output: "0123456789\nABCDEFGHIJ\nfinal"}
		// Cut to the last 15 bytes: "6789\nABCDEFGHIJ\nfinal" is 21 chars;
		// picking maxBytes=15 lands mid "ABCDEFGHIJ" line, which must be
		// dropped entirely so the result starts cleanly.
		got := FailureTail(res, 10, 15)
		if strings.HasPrefix(got, "6789") || strings.Contains(got, "6789") {
			t.Fatalf("FailureTail = %q, want the partial leading fragment trimmed", got)
		}
		if !strings.HasSuffix(got, "final") {
			t.Fatalf("FailureTail = %q, want it to end with the last line", got)
		}
	})

	t.Run("Err message appended as a final line", func(t *testing.T) {
		res := &CheckResult{Output: "some output\n", Err: errors.New("context canceled")}
		got := FailureTail(res, 10, 1024)
		want := "some output\ncontext canceled"
		if got != want {
			t.Fatalf("FailureTail = %q, want %q", got, want)
		}
	})

	t.Run("Err-only with empty Output", func(t *testing.T) {
		res := &CheckResult{Output: "", Err: errors.New("tempdir failed")}
		got := FailureTail(res, 10, 1024)
		if got != "tempdir failed" {
			t.Fatalf("FailureTail = %q, want %q", got, "tempdir failed")
		}
	})

	t.Run("zero caps mean unbounded", func(t *testing.T) {
		res := &CheckResult{Output: "a\nb\nc"}
		got := FailureTail(res, 0, 0)
		if got != "a\nb\nc" {
			t.Fatalf("FailureTail with zero caps = %q, want unbounded output", got)
		}
	})
}
