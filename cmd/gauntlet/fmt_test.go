package main

// Tests for gauntlet fmt: CLI-mode behavior only (flag composition, exit
// codes, per-file isolation, the mtime-preserved-when-unchanged contract).
// internal/kdlfmt's own tests own the normalizer's correctness/safety
// properties; these tests treat kdlfmt.Format as a black box and check
// only what fmt.go adds on top of it. No exec, no wall-clock sleeps,
// t.TempDir() fixtures only — matches validate_test.go's style.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const badIndentKDL = "a {\n  b 1   \n}\n"
const wellFormattedKDL = "a {\n    b 1\n}\n"
const unterminatedStringKDL = "a \"no close\n"

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunFmt_DefaultPrintsFormattedContent covers gofmt's no-flags mode:
// every file's formatted content concatenated to stdout, in argument
// order, source files left untouched.
func TestRunFmt_DefaultPrintsFormattedContent(t *testing.T) {
	dir := t.TempDir()
	a := writeFixture(t, dir, "a.kdl", badIndentKDL)
	b := writeFixture(t, dir, "b.kdl", wellFormattedKDL)

	var stdout, stderr bytes.Buffer
	err := runFmtTo(&stdout, &stderr, []string{a, b})
	if err != nil {
		t.Fatalf("runFmtTo: %v (stderr: %s)", err, stderr.String())
	}
	want := wellFormattedKDL + wellFormattedKDL // a reformatted + b unchanged, concatenated
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	// Default mode never writes back to disk.
	got, _ := os.ReadFile(a)
	if string(got) != badIndentKDL {
		t.Fatalf("source file %s was modified in default (no -w) mode", a)
	}
}

// TestRunFmt_ListMode checks -l: only differing files are named, one per
// line, on stdout — and lists it as the CI mode's failure signal (errFmtFailed,
// exit 1), even though every file was formatted without an actual error.
func TestRunFmt_ListMode(t *testing.T) {
	dir := t.TempDir()
	a := writeFixture(t, dir, "a.kdl", badIndentKDL)
	b := writeFixture(t, dir, "b.kdl", wellFormattedKDL)

	var stdout, stderr bytes.Buffer
	err := runFmtTo(&stdout, &stderr, []string{"-l", a, b})
	if !errors.Is(err, errFmtFailed) {
		t.Fatalf("runFmtTo(-l) error = %v, want errFmtFailed (a differs)", err)
	}
	if stdout.String() != a+"\n" {
		t.Fatalf("stdout = %q, want just %q", stdout.String(), a+"\n")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	// -l alone never writes.
	got, _ := os.ReadFile(a)
	if string(got) != badIndentKDL {
		t.Fatalf("source file %s was modified by -l alone", a)
	}
}

// TestRunFmt_ListMode_AllFormatted checks the CI-mode success case: every
// file already formatted, -l lists nothing, exit 0.
func TestRunFmt_ListMode_AllFormatted(t *testing.T) {
	dir := t.TempDir()
	b := writeFixture(t, dir, "b.kdl", wellFormattedKDL)

	var stdout, stderr bytes.Buffer
	if err := runFmtTo(&stdout, &stderr, []string{"-l", b}); err != nil {
		t.Fatalf("runFmtTo(-l), all formatted: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty (nothing differs)", stdout.String())
	}
}

// TestRunFmt_DiffMode checks -d prints a unified diff for a changed file,
// nothing for an unchanged one, and does NOT by itself force exit 1.
func TestRunFmt_DiffMode(t *testing.T) {
	dir := t.TempDir()
	a := writeFixture(t, dir, "a.kdl", badIndentKDL)
	b := writeFixture(t, dir, "b.kdl", wellFormattedKDL)

	var stdout, stderr bytes.Buffer
	if err := runFmtTo(&stdout, &stderr, []string{"-d", a, b}); err != nil {
		t.Fatalf("runFmtTo(-d): %v (stderr: %s)", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "--- "+a+".orig") || !strings.Contains(out, "+++ "+a) {
		t.Fatalf("stdout missing diff headers for %s:\n%s", a, out)
	}
	if !strings.Contains(out, "-  b 1   ") || !strings.Contains(out, "+    b 1") {
		t.Fatalf("stdout missing expected diff lines for %s:\n%s", a, out)
	}
	if strings.Contains(out, b) {
		t.Fatalf("stdout mentions unchanged file %s, want it omitted:\n%s", b, out)
	}
}

// TestRunFmt_WriteMode checks -w: a changed file is rewritten in place
// (and reads back formatted); an already-formatted file is never opened
// for write at all, so its mtime is untouched — the package doc's
// "preserve mtime when already formatted" contract.
func TestRunFmt_WriteMode(t *testing.T) {
	dir := t.TempDir()
	a := writeFixture(t, dir, "a.kdl", badIndentKDL)
	b := writeFixture(t, dir, "b.kdl", wellFormattedKDL)

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(b, old, old); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := runFmtTo(&stdout, &stderr, []string{"-w", a, b}); err != nil {
		t.Fatalf("runFmtTo(-w): %v (stderr: %s)", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty (pure -w prints nothing)", stdout.String())
	}

	gotA, _ := os.ReadFile(a)
	if string(gotA) != wellFormattedKDL {
		t.Fatalf("a.kdl = %q after -w, want %q", gotA, wellFormattedKDL)
	}

	info, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("b.kdl (already formatted) mtime changed: got %v, want %v", info.ModTime(), old)
	}
}

// TestRunFmt_WriteAndListCombine checks gofmt's own composition rule
// ("-l and -w may combine"): both effects happen for the same run.
func TestRunFmt_WriteAndListCombine(t *testing.T) {
	dir := t.TempDir()
	a := writeFixture(t, dir, "a.kdl", badIndentKDL)

	var stdout, stderr bytes.Buffer
	err := runFmtTo(&stdout, &stderr, []string{"-w", "-l", a})
	if !errors.Is(err, errFmtFailed) {
		t.Fatalf("runFmtTo(-w -l) error = %v, want errFmtFailed", err)
	}
	if stdout.String() != a+"\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), a+"\n")
	}
	got, _ := os.ReadFile(a)
	if string(got) != wellFormattedKDL {
		t.Fatalf("a.kdl = %q, want it rewritten to %q", got, wellFormattedKDL)
	}
}

// TestRunFmt_DiffAndListCombine checks "-d combines with -l": both a
// listing line and a diff appear for the same changed file.
func TestRunFmt_DiffAndListCombine(t *testing.T) {
	dir := t.TempDir()
	a := writeFixture(t, dir, "a.kdl", badIndentKDL)

	var stdout, stderr bytes.Buffer
	err := runFmtTo(&stdout, &stderr, []string{"-d", "-l", a})
	if !errors.Is(err, errFmtFailed) {
		t.Fatalf("runFmtTo(-d -l) error = %v, want errFmtFailed", err)
	}
	out := stdout.String()
	if !strings.HasPrefix(out, a+"\n") {
		t.Fatalf("stdout doesn't start with the -l listing line:\n%s", out)
	}
	if !strings.Contains(out, "@@ ") {
		t.Fatalf("stdout missing the -d hunk header:\n%s", out)
	}
}

// TestRunFmt_MalformedRefusesAndLeavesFileUntouched is the refuse-on-
// malformed contract end to end: the broken file is neither printed nor
// written, gets an error on stderr, and — critically — a SIBLING valid
// file in the same invocation is still processed normally (per-file
// isolation, "errors per file don't stop other files").
func TestRunFmt_MalformedRefusesAndLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	broken := writeFixture(t, dir, "broken.kdl", unterminatedStringKDL)
	ok := writeFixture(t, dir, "ok.kdl", badIndentKDL)

	var stdout, stderr bytes.Buffer
	err := runFmtTo(&stdout, &stderr, []string{"-w", broken, ok})
	if !errors.Is(err, errFmtFailed) {
		t.Fatalf("runFmtTo error = %v, want errFmtFailed", err)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want a refusal message naming broken.kdl")
	}
	if !strings.Contains(stderr.String(), broken) {
		t.Fatalf("stderr = %q, want it to name %s", stderr.String(), broken)
	}

	gotBroken, _ := os.ReadFile(broken)
	if string(gotBroken) != unterminatedStringKDL {
		t.Fatalf("broken.kdl was modified despite being refused: %q", gotBroken)
	}
	// The sibling valid file, given in the SAME -w invocation, must still
	// have been formatted and written — one file's refusal is isolated.
	gotOK, _ := os.ReadFile(ok)
	if string(gotOK) != wellFormattedKDL {
		t.Fatalf("ok.kdl = %q, want it still formatted despite broken.kdl's error", gotOK)
	}
}

// TestRunFmt_NoFiles checks the explicit "no stdin mode" error, distinct
// from errFmtFailed — this is a genuine usage error, not a per-file
// failure, so main's dispatch prints it with the "gauntlet fmt: " prefix.
func TestRunFmt_NoFiles(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runFmtTo(&stdout, &stderr, nil)
	if err == nil || errors.Is(err, errFmtFailed) {
		t.Fatalf("runFmtTo(nil) error = %v, want a plain usage error", err)
	}
	if !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("error = %q, want it to explain there's no stdin mode", err)
	}
}

// TestRunFmt_ReadErrorIsIsolated checks an unreadable path (doesn't exist)
// behaves like a malformed file: reported on stderr, doesn't stop the rest
// of the invocation, exit 1.
func TestRunFmt_ReadErrorIsIsolated(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.kdl")
	ok := writeFixture(t, dir, "ok.kdl", wellFormattedKDL)

	var stdout, stderr bytes.Buffer
	err := runFmtTo(&stdout, &stderr, []string{missing, ok})
	if !errors.Is(err, errFmtFailed) {
		t.Fatalf("runFmtTo error = %v, want errFmtFailed", err)
	}
	if !strings.Contains(stderr.String(), missing) {
		t.Fatalf("stderr = %q, want it to name %s", stderr.String(), missing)
	}
	// Default mode: ok.kdl's content should still reach stdout despite
	// missing.kdl's read error earlier in the argument list.
	if stdout.String() != wellFormattedKDL {
		t.Fatalf("stdout = %q, want ok.kdl's formatted content despite the earlier read error", stdout.String())
	}
}
