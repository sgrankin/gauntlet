package dashboard_test

// Tests for the JSON API added in internal/dashboard/api.go. Reuses
// testSnapshot/openTestStore/emitRun/sampleRecord/get from
// dashboard_test.go (same package).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/queue"
)

func decodeJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode JSON: %v\nbody: %s", err, body)
	}
	return m
}

func assertJSONContentType(t *testing.T, resp *http.Response) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// --- GET /api/v1/status -------------------------------------------------------

func TestAPIStatus_Shape(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	resp, body := get(t, h, "/api/v1/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertJSONContentType(t, resp)

	m := decodeJSON(t, body)
	if _, ok := m["snapshotAt"]; !ok {
		t.Errorf("missing snapshotAt: %s", body)
	}
	targets, ok := m["targets"].([]any)
	if !ok || len(targets) != 2 {
		t.Fatalf("targets = %v", m["targets"])
	}

	var main map[string]any
	for _, tv := range targets {
		tm := tv.(map[string]any)
		if tm["name"] == "main" {
			main = tm
		}
	}
	if main == nil {
		t.Fatalf("main target missing: %s", body)
	}
	for _, key := range []string{"name", "branch", "tip", "inFlight", "waiting", "parked"} {
		if _, ok := main[key]; !ok {
			t.Errorf("main missing key %q: %v", key, main)
		}
	}

	inFlight, ok := main["inFlight"].(map[string]any)
	if !ok {
		t.Fatalf("inFlight = %v", main["inFlight"])
	}
	for _, key := range []string{"ref", "sha", "runID", "currentCheck", "startedAt", "checksDone"} {
		if _, ok := inFlight[key]; !ok {
			t.Errorf("inFlight missing key %q", key)
		}
	}
	if inFlight["ref"] != "refs/heads/for/main/alice/feat-a" {
		t.Errorf("inFlight.ref = %v", inFlight["ref"])
	}

	waiting, ok := main["waiting"].([]any)
	if !ok || len(waiting) != 2 {
		t.Fatalf("waiting = %v", main["waiting"])
	}
	first := waiting[0].(map[string]any)
	if first["ref"] != "refs/heads/for/main/carol/first" {
		t.Errorf("waiting not seq-ordered (carol seq=2 should come first): %v", waiting)
	}

	parked, ok := main["parked"].([]any)
	if !ok || len(parked) != 1 {
		t.Fatalf("parked = %v", main["parked"])
	}
	pm := parked[0].(map[string]any)
	for _, key := range []string{"ref", "sha", "outcome", "reason", "at"} {
		if _, ok := pm[key]; !ok {
			t.Errorf("parked missing key %q", key)
		}
	}
	if pm["outcome"] != "rejected" {
		t.Errorf("parked.outcome = %v", pm["outcome"])
	}
}

func TestAPIStatus_IdleTargetHasNullInFlightAndEmptyLists(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/api/v1/status")
	m := decodeJSON(t, body)
	targets := m["targets"].([]any)

	var release map[string]any
	for _, tv := range targets {
		tm := tv.(map[string]any)
		if tm["name"] == "release" {
			release = tm
		}
	}
	if release == nil {
		t.Fatalf("release target missing: %s", body)
	}
	if release["inFlight"] != nil {
		t.Errorf("release.inFlight = %v, want nil", release["inFlight"])
	}
	if w, ok := release["waiting"].([]any); !ok || len(w) != 0 {
		t.Errorf("release.waiting = %v, want empty array", release["waiting"])
	}
}

func TestAPIStatus_NoSnapshot503(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := get(t, h, "/api/v1/status")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertJSONContentType(t, resp)
	m := decodeJSON(t, body)
	if m["error"] != "no snapshot yet" {
		t.Errorf("error = %v", m["error"])
	}
}

// --- GET /api/v1/runs ---------------------------------------------------------

func TestAPIRuns_Shape(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main"))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/api/v1/runs?target=main&limit=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertJSONContentType(t, resp)

	m := decodeJSON(t, body)
	runs, ok := m["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("runs = %v", m["runs"])
	}
	run := runs[0].(map[string]any)
	for _, key := range []string{"runID", "target", "ref", "user", "topic", "sha", "outcome", "detail", "startedAt", "endedAt", "durationMs"} {
		if _, ok := run[key]; !ok {
			t.Errorf("run missing key %q", key)
		}
	}
	if run["runID"] != "run-hist-1" {
		t.Errorf("runID = %v", run["runID"])
	}
	if run["outcome"] != "rejected" {
		t.Errorf("outcome = %v", run["outcome"])
	}
}

func TestAPIRuns_UnknownTargetEmpty(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main"))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/api/v1/runs?target=does-not-exist")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	m := decodeJSON(t, body)
	runs, ok := m["runs"].([]any)
	if !ok || len(runs) != 0 {
		t.Errorf("runs = %v, want empty array", m["runs"])
	}
}

func TestAPIRuns_MissingTarget400(t *testing.T) {
	store := openTestStore(t)
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	resp, body := get(t, h, "/api/v1/runs")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertJSONContentType(t, resp)
	m := decodeJSON(t, body)
	if m["error"] == nil {
		t.Errorf("expected error field: %s", body)
	}
}

func TestAPIRuns_NoStore503(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := get(t, h, "/api/v1/runs?target=main")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	m := decodeJSON(t, body)
	if m["error"] != "history disabled" {
		t.Errorf("error = %v", m["error"])
	}
}

// --- GET /api/v1/run/{id} -----------------------------------------------------

func TestAPIRun_Shape(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main"))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/api/v1/run/run-hist-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertJSONContentType(t, resp)

	m := decodeJSON(t, body)
	for _, key := range []string{
		"runID", "target", "ref", "user", "topic", "sha",
		"baseOID", "mergeSHA", "trialClean", "outcome", "detail",
		"startedAt", "endedAt", "durationMs", "checks",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("run missing key %q", key)
		}
	}

	checks, ok := m["checks"].([]any)
	if !ok || len(checks) != 2 {
		t.Fatalf("checks = %v", m["checks"])
	}
	c0 := checks[0].(map[string]any)
	for _, key := range []string{"seq", "name", "status", "durationMs", "err", "logPath"} {
		if _, ok := c0[key]; !ok {
			t.Errorf("check missing key %q", key)
		}
	}
	if c0["name"] != "lint" || c0["status"] != "passed" {
		t.Errorf("checks[0] = %v", c0)
	}
}

// TestAPIRun_ChecksIncludeLogPathAndLogURLWhenConfigured confirms
// GET /api/v1/run/{id} exposes logPath (always, when non-empty) and logUrl
// (only when the dashboard is configured to actually serve it, WithLogRoot)
// on each check — chunk F-b's API field additions.
func TestAPIRun_ChecksIncludeLogPathAndLogURLWhenConfigured(t *testing.T) {
	store := openTestStore(t)
	const logPath = "/var/lib/gauntlet/logs/run-log-api/test.log"
	emitRun(t, store, logRecord("run-log-api", "test", logPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store, dashboard.WithLogRoot("/var/lib/gauntlet/logs"))
	resp, body := get(t, h, "/api/v1/run/run-log-api")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}

	m := decodeJSON(t, body)
	checks := m["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("checks = %v", checks)
	}
	c := checks[0].(map[string]any)
	if c["logPath"] != logPath {
		t.Errorf("logPath = %v, want %q", c["logPath"], logPath)
	}
	wantURL := "/run/run-log-api/log/test"
	if c["logUrl"] != wantURL {
		t.Errorf("logUrl = %v, want %q", c["logUrl"], wantURL)
	}
}

// TestAPIRun_ChecksOmitLogURLWithoutLogRoot confirms logUrl is absent
// (omitempty) when the dashboard has no LogRoot configured, even though
// logPath is still reported — logUrl should never point at a route that
// always 404s.
func TestAPIRun_ChecksOmitLogURLWithoutLogRoot(t *testing.T) {
	store := openTestStore(t)
	const logPath = "/var/lib/gauntlet/logs/run-log-api-2/test.log"
	emitRun(t, store, logRecord("run-log-api-2", "test", logPath))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	_, body := get(t, h, "/api/v1/run/run-log-api-2")

	m := decodeJSON(t, body)
	checks := m["checks"].([]any)
	c := checks[0].(map[string]any)
	if c["logPath"] != logPath {
		t.Errorf("logPath = %v, want %q", c["logPath"], logPath)
	}
	if _, ok := c["logUrl"]; ok {
		t.Errorf("logUrl = %v, want absent (omitempty) without WithLogRoot", c["logUrl"])
	}
}

// TestAPIRun_HooksFieldPresentAndPopulated confirms GET /api/v1/run/{id}
// gains a "hooks" array alongside "checks" (chunk P5-B, log/history
// parity): present (as an array, never omitted) even when empty, and
// populated with the same field shape as a check when the run actually has
// hook rows.
func TestAPIRun_HooksFieldPresentAndPopulated(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hooks-api-1", "main"))
	emitHook(t, store, "run-hooks-api-1", "deploy", core.CheckResult{Status: core.CheckPassed, Duration: 250 * time.Millisecond})

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/api/v1/run/run-hooks-api-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}

	m := decodeJSON(t, body)
	hooksField, ok := m["hooks"]
	if !ok {
		t.Fatalf("run missing key %q\nbody:\n%s", "hooks", body)
	}
	hooks, ok := hooksField.([]any)
	if !ok || len(hooks) != 1 {
		t.Fatalf("hooks = %v, want a 1-element array", hooksField)
	}
	hk := hooks[0].(map[string]any)
	for _, key := range []string{"seq", "name", "status", "durationMs", "err", "logPath"} {
		if _, ok := hk[key]; !ok {
			t.Errorf("hook missing key %q", key)
		}
	}
	if hk["name"] != "deploy" || hk["status"] != "passed" {
		t.Errorf("hooks[0] = %v", hk)
	}
}

// TestAPIRun_HooksFieldEmptyArrayWhenNone confirms "hooks" is present as an
// empty array (never omitted, never null) for a run with no hook rows —
// mirroring "checks" always being an array too.
func TestAPIRun_HooksFieldEmptyArrayWhenNone(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hooks-api-2", "main"))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	_, body := get(t, h, "/api/v1/run/run-hooks-api-2")

	m := decodeJSON(t, body)
	hooksField, ok := m["hooks"]
	if !ok {
		t.Fatalf("run missing key %q\nbody:\n%s", "hooks", body)
	}
	hooks, ok := hooksField.([]any)
	if !ok || len(hooks) != 0 {
		t.Fatalf("hooks = %v, want an empty array", hooksField)
	}
}

func TestAPIRun_NotFound404(t *testing.T) {
	store := openTestStore(t)
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	resp, body := get(t, h, "/api/v1/run/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	m := decodeJSON(t, body)
	if m["error"] != "not found" {
		t.Errorf("error = %v", m["error"])
	}
}

func TestAPIRun_NoStore503(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := get(t, h, "/api/v1/run/whatever")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	m := decodeJSON(t, body)
	if m["error"] != "history disabled" {
		t.Errorf("error = %v", m["error"])
	}
}

// --- POST /api/v1/retry -------------------------------------------------------

func postJSON(t *testing.T, h http.Handler, path, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

func TestAPIRetry_RoundTrip(t *testing.T) {
	ch := dashboard.NewChannel()
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil, dashboard.WithChannel(ch))

	resp, body := postJSON(t, h, "/api/v1/retry", `{"target":"main","ref":"refs/heads/for/main/alice/feat-a"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertJSONContentType(t, resp)
	m := decodeJSON(t, body)
	if m["status"] != "queued" {
		t.Errorf("status field = %v", m["status"])
	}

	select {
	case cmd := <-ch.Commands():
		if cmd.Kind != core.CommandRetry || cmd.Target != "main" || cmd.Ref != "refs/heads/for/main/alice/feat-a" {
			t.Errorf("cmd = %+v", cmd)
		}
	default:
		t.Fatalf("no command enqueued on ch.Commands()")
	}
}

func TestAPIRetry_NoChannelStillAccepted(t *testing.T) {
	// Without WithChannel, /retry has nowhere to send the command but the
	// request itself is still well-formed, so it's still accepted (the
	// command is silently dropped, same as a full buffer would be).
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := postJSON(t, h, "/api/v1/retry", `{"target":"main","ref":"refs/heads/for/main/alice/feat-a"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
}

func TestAPIRetry_MissingFields400(t *testing.T) {
	ch := dashboard.NewChannel()
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil, dashboard.WithChannel(ch))

	for _, body := range []string{
		`{"target":"main"}`,
		`{"ref":"refs/heads/for/main/alice/feat-a"}`,
		`{}`,
	} {
		resp, respBody := postJSON(t, h, "/api/v1/retry", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, resp:\n%s", body, resp.StatusCode, respBody)
		}
	}

	select {
	case cmd := <-ch.Commands():
		t.Errorf("expected no command enqueued, got %+v", cmd)
	default:
	}
}

func TestAPIRetry_InvalidJSON400(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := postJSON(t, h, "/api/v1/retry", `not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
}

func TestAPIRetry_MethodNotAllowed405(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := get(t, h, "/api/v1/retry")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	assertJSONContentType(t, resp)
	m := decodeJSON(t, body)
	if m["error"] == nil {
		t.Errorf("expected error field: %s", body)
	}
}
