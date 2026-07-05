package mcp_test

// Exercises internal/mcp's Streamable HTTP handler (chunk E5) the way a real
// MCP client would: over httptest, using the SDK's own client and
// StreamableClientTransport rather than calling handleXxx directly, so a
// wire-protocol regression (a schema the SDK rejects, a tool the client
// can't discover) would actually fail these tests.

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/history"
	mcpsrv "github.com/sgrankin/gauntlet/internal/mcp"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// testSnapshot builds a small, hand-constructed queue.Snapshot: "main" has an
// in-flight run, one waiting candidate, one parked candidate; "release" is
// idle. Mirrors the shape (not the exact values) of dashboard_test.go's
// testSnapshot, since internal/mcp can't import that unexported helper.
func testSnapshot() *queue.Snapshot {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	return &queue.Snapshot{
		At: now,
		Targets: []queue.TargetSnapshot{
			{
				Name:      "main",
				Branch:    "main",
				TargetTip: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				InFlight: &queue.RunSnapshot{
					Candidate: core.Candidate{
						Ref: "refs/heads/for/main/alice/feat-a", Target: "main",
						User: "alice", Topic: "feat-a", SHA: "1111111111111111111111111111111111111111",
					},
					RunID:     "run-inflight-1",
					Done:      []core.CheckResult{{Name: "lint", Status: core.CheckPassed}},
					Current:   &queue.CurrentCheck{Name: "test", StartedAt: now.Add(-5 * time.Second)},
					StartedAt: now.Add(-30 * time.Second),
				},
				Waiting: []queue.WaitingEntry{
					{Candidate: core.Candidate{Ref: "refs/heads/for/main/bob/second", Target: "main", User: "bob", Topic: "second", SHA: "2222222222222222222222222222222222222222"}, Seq: 1},
				},
				Parked: []queue.ParkedEntry{
					{
						Candidate: core.Candidate{Ref: "refs/heads/for/main/mallory/evil", Target: "main", User: "mallory", Topic: "evil", SHA: "4444444444444444444444444444444444444444"},
						Outcome:   core.OutcomeRejected,
						Reason:    "build failed",
						At:        now.Add(-time.Hour),
					},
				},
			},
			{Name: "release", Branch: "release/v2"},
		},
	}
}

func openTestStore(t *testing.T) *history.Store {
	t.Helper()
	s, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func sampleRecord(runID, target string) *core.RunRecord {
	started := time.Date(2026, 7, 5, 11, 0, 0, 0, time.UTC)
	return &core.RunRecord{
		RunID:  runID,
		Target: target,
		Candidate: core.Candidate{
			Ref: "refs/heads/for/" + target + "/dave/histfix", Target: target,
			User: "dave", Topic: "histfix", SHA: "5555555555555555555555555555555555555555",
		},
		BaseOID:  "base1111111111111111111111111111111111",
		MergeSHA: "merge111111111111111111111111111111111",
		Trial:    core.TrialMerge{Clean: true, TreeOID: "tree1111111111111111111111111111111111"},
		Checks: []core.CheckResult{
			{Name: "lint", Status: core.CheckPassed, Duration: 500 * time.Millisecond},
			{Name: "test", Status: core.CheckFailed, Duration: 2500 * time.Millisecond, Output: "boom: assertion failed on line 42"},
		},
		Outcome:   core.OutcomeRejected,
		Detail:    "test failed",
		StartedAt: started,
		EndedAt:   started.Add(3 * time.Second),
	}
}

// connect starts an httptest server over p's MCP handler, connects an SDK
// client to it, and returns the initialized session plus a cleanup func
// (registered with t.Cleanup, but also returned for callers that want to
// close early).
func connect(t *testing.T, p mcpsrv.Params) *sdkmcp.ClientSession {
	t.Helper()
	handler := mcpsrv.New(p)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any) *sdkmcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func textOf(t *testing.T, res *sdkmcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("no content in result: %+v", res)
	}
	tc, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", res.Content[0])
	}
	return tc.Text
}

// --- initialize + tools/list ------------------------------------------------

func TestListTools_AllFourWithSchemas(t *testing.T) {
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := map[string]bool{"status": false, "runs": false, "run": false, "retry": false}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("tool %s: nil input schema", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q missing from tools/list (%d tools returned)", name, len(res.Tools))
		}
	}
}

// --- status -----------------------------------------------------------------

func TestStatus_ContentIncludesTargetName(t *testing.T) {
	snap := testSnapshot()
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return snap }})

	res := callTool(t, session, "status", map[string]any{"target": "main"})
	if res.IsError {
		t.Fatalf("status errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{`"name":"main"`, "alice/feat-a", "bob", "mallory"} {
		if !strings.Contains(text, want) {
			t.Errorf("status content missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"name":"release"`) {
		t.Errorf("status with target=main should not include release:\n%s", text)
	}
}

func TestStatus_NoTargetFilter_ReturnsAll(t *testing.T) {
	snap := testSnapshot()
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return snap }})

	res := callTool(t, session, "status", nil)
	text := textOf(t, res)
	for _, want := range []string{`"name":"main"`, `"name":"release"`} {
		if !strings.Contains(text, want) {
			t.Errorf("status content missing %q:\n%s", want, text)
		}
	}
}

func TestStatus_NilSnapshot_EmptyNotError(t *testing.T) {
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})

	res := callTool(t, session, "status", nil)
	if res.IsError {
		t.Fatalf("expected no error for nil snapshot, got: %s", textOf(t, res))
	}
}

// --- run ----------------------------------------------------------------------

func TestRun_OutputIncluded(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: sampleRecord("run-hist-1", "main")}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "run", map[string]any{"run_id": "run-hist-1"})
	if res.IsError {
		t.Fatalf("run errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{"run-hist-1", "dave/histfix", "boom: assertion failed on line 42", "failed", "passed"} {
		if !strings.Contains(text, want) {
			t.Errorf("run content missing %q:\n%s", want, text)
		}
	}
}

func TestRun_UnknownID_ErrorResult(t *testing.T) {
	store := openTestStore(t)
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "run", map[string]any{"run_id": "does-not-exist"})
	if !res.IsError {
		t.Fatalf("expected error result for unknown run_id, got: %s", textOf(t, res))
	}
}

// --- runs -----------------------------------------------------------------

func TestRuns_NilStore_ErrorResult(t *testing.T) {
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})

	res := callTool(t, session, "runs", map[string]any{"target": "main"})
	if !res.IsError {
		t.Fatalf("expected error result when history is disabled, got: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "history disabled") {
		t.Errorf("expected \"history disabled\" in error text, got: %s", textOf(t, res))
	}
}

func TestRuns_ReturnsRecentRuns(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: sampleRecord("run-hist-1", "main")}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "runs", map[string]any{"target": "main"})
	if res.IsError {
		t.Fatalf("runs errored: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "run-hist-1") {
		t.Errorf("runs content missing run-hist-1:\n%s", textOf(t, res))
	}
}

// --- retry --------------------------------------------------------------------

func TestRetry_CommandLandsOnChannel(t *testing.T) {
	ch := dashboard.NewChannel()
	session := connect(t, mcpsrv.Params{
		Snapshot: func() *queue.Snapshot { return nil },
		Retry:    ch.TrySend,
	})

	res := callTool(t, session, "retry", map[string]any{"target": "main", "ref": "refs/heads/for/main/alice/feat-a"})
	if res.IsError {
		t.Fatalf("retry errored: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "queued") {
		t.Errorf("expected \"queued\" in retry result, got: %s", textOf(t, res))
	}

	select {
	case cmd := <-ch.Commands():
		if cmd.Kind != core.CommandRetry || cmd.Target != "main" || cmd.Ref != "refs/heads/for/main/alice/feat-a" {
			t.Errorf("unexpected command: %+v", cmd)
		}
	default:
		t.Fatal("expected a command on the channel, got none")
	}
}

func TestRetry_NilRetryFunc_ErrorResult(t *testing.T) {
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})

	res := callTool(t, session, "retry", map[string]any{"target": "main", "ref": "refs/heads/for/main/alice/feat-a"})
	if !res.IsError {
		t.Fatalf("expected error result when retry is disabled, got: %s", textOf(t, res))
	}
}

func TestRetry_BufferFull_ErrorResult(t *testing.T) {
	calls := 0
	session := connect(t, mcpsrv.Params{
		Snapshot: func() *queue.Snapshot { return nil },
		Retry: func(core.Command) bool {
			calls++
			return false // simulate a full buffer
		},
	})

	res := callTool(t, session, "retry", map[string]any{"target": "main", "ref": "refs/heads/for/main/alice/feat-a"})
	if !res.IsError {
		t.Fatalf("expected error result when retry reports backpressure, got: %s", textOf(t, res))
	}
	if calls != 1 {
		t.Errorf("expected Retry to be called once, got %d", calls)
	}
}
