package dashboard_test

// Tests for GET /run/{runID}/log/{checkName} (DESIGN.md "Full per-check log
// files", chunk F-b): serving a check's full, uncapped log file from disk,
// containment-checked under dashboard.WithLogRoot.

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// writeZstFile zstd-compresses content (fastest level, matching
// internal/executor/logfile.go's openCheckLog) into a single stream and
// writes it to path.
func writeZstFile(t *testing.T, path, content string) {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write([]byte(content)); err != nil {
		t.Fatalf("enc.Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("enc.Close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// logRecord builds a run record with one check whose CheckResult.LogPath is
// logPath (may be empty, or point anywhere — including outside any
// particular root, to exercise containment).
func logRecord(runID, checkName, logPath string) *core.RunRecord {
	started := time.Date(2026, 7, 5, 11, 0, 0, 0, time.UTC)
	return &core.RunRecord{
		RunID:  runID,
		Target: "main",
		Candidate: core.Candidate{
			Ref: "refs/heads/for/main/dave/histfix", Target: "main",
			User: "dave", Topic: "histfix", SHA: "5555555555555555555555555555555555555555",
		},
		BaseOID:  "base1111111111111111111111111111111111",
		MergeSHA: "merge111111111111111111111111111111111",
		Trial:    core.TrialMerge{Clean: true},
		Checks: []core.CheckResult{
			{Name: checkName, Status: core.CheckFailed, Duration: time.Second, Output: "tail only", LogPath: logPath},
		},
		Outcome:   core.OutcomeRejected,
		Detail:    "test failed",
		StartedAt: started,
		EndedAt:   started.Add(time.Second),
	}
}

func TestRunLog_ServesFileUnderLogRoot(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()

	runDir := filepath.Join(logRoot, "run-log-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(runDir, "test.log")
	const fullContent = "line 1\nline 2\nthe complete uncapped log\n"
	if err := os.WriteFile(logPath, []byte(fullContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	emitRun(t, store, logRecord("run-log-1", "test", logPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-1/log/test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body:\n%s", resp.StatusCode, body)
	}
	if body != fullContent {
		t.Errorf("body = %q, want %q", body, fullContent)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestRunLog_ServesDecompressedZstFile exercises the ".log.zst" path
// (internal/queue/reconcile.go's job.LogPath assignment,
// internal/executor/logfile.go's openCheckLog): handleRunLog must
// decompress the stored zstd stream and serve the plain-text content, not
// the compressed bytes.
func TestRunLog_ServesDecompressedZstFile(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()

	runDir := filepath.Join(logRoot, "run-log-zst")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(runDir, "1-test.log.zst")
	const fullContent = "line 1\nline 2\nthe complete uncapped log\n"
	writeZstFile(t, logPath, fullContent)

	emitRun(t, store, logRecord("run-log-zst", "test", logPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-zst/log/test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body:\n%s", resp.StatusCode, body)
	}
	if body != fullContent {
		t.Errorf("body = %q, want %q (decompressed content)", body, fullContent)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestRunLog_TruncatedZstFrameServesPartialContentNoPanic covers the
// "daemon/check killed mid-write" degradation: openCheckLog's contract is
// that losing/truncating the full log must never fail the check, so a
// truncated final zstd frame is an expected, not exceptional, on-disk
// state. handleRunLog must decode whatever it can and stop cleanly — no
// panic, no hang — rather than require a complete frame to serve anything.
func TestRunLog_TruncatedZstFrameServesPartialContentNoPanic(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()

	runDir := filepath.Join(logRoot, "run-log-zst-truncated")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(runDir, "1-test.log.zst")

	// Content large enough to span multiple zstd blocks (default block
	// size is 128KiB of *source* bytes), so truncating the compressed
	// stream partway through cuts after at least one block has already
	// been fully decodable, giving a genuine non-empty partial decode
	// rather than truncating inside the only block there is.
	var content strings.Builder
	for i := 0; i < 40000; i++ {
		fmt.Fprintf(&content, "LINE-%06d the complete uncapped log line\n", i)
	}
	fullContent := content.String()

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write([]byte(fullContent)); err != nil {
		t.Fatalf("enc.Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("enc.Close: %v", err)
	}

	// Cut the compressed stream well before its end — simulates the file
	// handle being closed (or the process killed) mid-write, before the
	// final frame/checksum landed.
	truncated := buf.Bytes()[:buf.Len()*2/3]
	if err := os.WriteFile(logPath, truncated, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	emitRun(t, store, logRecord("run-log-zst-truncated", "test", logPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-zst-truncated/log/test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers are written before decompression starts), body:\n%s", resp.StatusCode, body)
	}
	if body == "" {
		t.Fatal("body is empty, want at least one fully-decoded block's worth of partial content")
	}
	if body == fullContent {
		t.Error("body equals the full content despite truncation; truncation had no effect")
	}
	if !strings.HasPrefix(fullContent, body) {
		t.Errorf("body is not a prefix of the original content (got %d bytes)", len(body))
	}
}

func TestRunLog_PathOutsideRoot404s(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()

	// A file that genuinely exists, but the stored LogPath is a sibling
	// directory of logRoot, not under it — the DB row is either corrupt or
	// (more realistically) a stale absolute path from a differently
	// configured LogDir. Either way containment must reject it.
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret.log")
	if err := os.WriteFile(outsidePath, []byte("should never be served"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	emitRun(t, store, logRecord("run-log-escape", "test", outsidePath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-escape/log/test")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body:\n%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "should never be served") {
		t.Errorf("response leaked the outside-root file's content:\n%s", body)
	}
}

// TestRunLog_PathTraversalEscape404s exercises containment specifically via
// a "../" component in the stored path — filepath.Clean must not be skipped
// on its own, only paired with the root-relative check.
func TestRunLog_PathTraversalEscape404s(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret.log")
	if err := os.WriteFile(outsidePath, []byte("nope"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Craft a path that starts under logRoot but climbs out via "..".
	traversal := filepath.Join(logRoot, "run-x", "..", "..", filepath.Base(outsideDir), "secret.log")
	emitRun(t, store, logRecord("run-log-traversal", "test", traversal))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, _ := get(t, h, "/run/run-log-traversal/log/test")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRunLog_MissingFileRendersPrunedMessage(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()

	// A stored path that's genuinely under logRoot but whose file was
	// removed (the common case: retention pruned the run-log directory
	// after the row was written).
	prunedPath := filepath.Join(logRoot, "run-log-pruned", "test.log")
	emitRun(t, store, logRecord("run-log-pruned", "test", prunedPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-pruned/log/test")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "pruned") {
		t.Errorf("body = %q, want a friendly pruned/missing message", body)
	}
}

func TestRunLog_NoLogRootConfigured404s(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()
	logPath := filepath.Join(logRoot, "run-log-noroot", "test.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	emitRun(t, store, logRecord("run-log-noroot", "test", logPath))

	// No WithLogRoot option: serving must be disabled even though the file
	// exists and the DB row has a LogPath.
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, _ := get(t, h, "/run/run-log-noroot/log/test")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (LogRoot not configured)", resp.StatusCode)
	}
}

func TestRunLog_NilStore404s(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil, dashboard.WithLogRoot(t.TempDir()))
	resp, _ := get(t, h, "/run/whatever/log/test")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (nil store)", resp.StatusCode)
	}
}

// TestRunLog_DirectoryPath404s confirms a stored path that resolves to a
// directory (here: a run-log directory rather than a file in it) serves the
// friendly 404 instead of letting ServeContent choke on a directory handle.
func TestRunLog_DirectoryPath404s(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()
	runDir := filepath.Join(logRoot, "run-log-dir")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	emitRun(t, store, logRecord("run-log-dir", "test", runDir))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-dir/log/test")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (stored path is a directory), body:\n%s", resp.StatusCode, body)
	}
}

func TestRunLog_UnknownCheckName404s(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()
	logPath := filepath.Join(logRoot, "run-log-unknown", "test.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	emitRun(t, store, logRecord("run-log-unknown", "test", logPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, _ := get(t, h, "/run/run-log-unknown/log/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (unknown check name)", resp.StatusCode)
	}
}

// --- run page link presence -------------------------------------------------

func TestRunPage_RendersFullLogLinkWhenLogRootConfigured(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()
	logPath := filepath.Join(logRoot, "run-log-link", "test.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	emitRun(t, store, logRecord("run-log-link", "test", logPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-link")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	wantHref := `href="/run/run-log-link/log/test"`
	if !strings.Contains(body, wantHref) {
		t.Errorf("run page missing full-log link %q\nbody:\n%s", wantHref, body)
	}
	if !strings.Contains(body, "full log") {
		t.Errorf("run page missing \"full log\" link text\nbody:\n%s", body)
	}
}

func TestRunPage_OmitsFullLogLinkWithoutLogRoot(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, logRecord("run-log-nolink", "test", "/some/path/test.log"))

	// No WithLogRoot: even though the check has a stored LogPath, the run
	// page must not link to a route that would always 404.
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/run/run-log-nolink")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "/run/run-log-nolink/log/") {
		t.Errorf("run page rendered a full-log link with no LogRoot configured:\n%s", body)
	}
}

func TestRunPage_OmitsFullLogLinkWhenNoLogPathStored(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()
	// LogPath == "": no file was ever written for this check.
	emitRun(t, store, logRecord("run-log-empty", "test", ""))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-log-empty")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "full log") {
		t.Errorf("run page rendered a full-log link despite no stored LogPath:\n%s", body)
	}
}

// --- hook log serving (log/history parity, chunk P5-B) ----------------------

// TestRunLog_ServesHookLog confirms GET /run/{id}/log/{name} also matches
// hook rows, not just checks: handleRunLog looks the name up in checks
// first (miss, since this run has no check by that name) and falls through
// to hooks — the same containment-checked serving path a check's log gets.
func TestRunLog_ServesHookLog(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()

	runDir := filepath.Join(logRoot, "run-hook-log-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(runDir, "hook-1-deploy.log")
	const fullContent = "deploying...\ndeployed.\n"
	if err := os.WriteFile(logPath, []byte(fullContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	emitRun(t, store, sampleRecord("run-hook-log-1", "main")) // checks: lint, test — no "deploy"
	emitHook(t, store, "run-hook-log-1", "deploy", core.CheckResult{
		Status: core.CheckPassed, Duration: time.Second, Output: "tail only", LogPath: logPath,
	})

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, body := get(t, h, "/run/run-hook-log-1/log/deploy")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body:\n%s", resp.StatusCode, body)
	}
	if body != fullContent {
		t.Errorf("body = %q, want %q", body, fullContent)
	}
}

// TestRunLog_HookUnknownNameStill404s confirms a name that matches neither
// a check nor a hook for the run still 404s, even once the lookup covers
// both tables.
func TestRunLog_HookUnknownNameStill404s(t *testing.T) {
	store := openTestStore(t)
	logRoot := t.TempDir()
	emitRun(t, store, sampleRecord("run-hook-log-2", "main"))
	emitHook(t, store, "run-hook-log-2", "deploy", core.CheckResult{Status: core.CheckPassed})

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot(logRoot))
	resp, _ := get(t, h, "/run/run-hook-log-2/log/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
