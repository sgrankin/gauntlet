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

// pipelineSnapshot builds a queue.Snapshot for target "spec" with a
// depth-2 pipeline (docs/plans/phase5.md §3.4, chunk P5-H): run 0 is the
// head (Predicted false), run 1 is a downstream window member built on a
// predicted base. Mirrors internal/dashboard's own pipelineSnapshot test
// helper in shape, not value, since this package can't import that
// unexported helper.
func pipelineSnapshot() *queue.Snapshot {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	mkRun := func(i int, predicted bool) queue.RunSnapshot {
		cand := core.Candidate{
			Ref: "refs/heads/for/spec/user/topic" + string(rune('0'+i)), Target: "spec",
			User: "user", Topic: "topic" + string(rune('0'+i)), SHA: "sha" + string(rune('0'+i)),
		}
		return queue.RunSnapshot{
			Candidate: cand,
			Members:   []core.Candidate{cand},
			RunID:     "run-spec-" + string(rune('0'+i)),
			ChainTip:  "chain" + string(rune('0'+i)),
			Predicted: predicted,
			StartedAt: now.Add(-time.Duration(i+1) * time.Minute),
			Current:   &queue.CurrentCheck{Name: "check" + string(rune('0'+i)), StartedAt: now},
		}
	}
	pipeline := []queue.RunSnapshot{mkRun(0, false), mkRun(1, true)}
	head := pipeline[0]
	return &queue.Snapshot{
		At: now,
		Targets: []queue.TargetSnapshot{
			{Name: "spec", Branch: "spec", TargetTip: "tip", InFlight: &head, Pipeline: pipeline},
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

// TestStatus_PipelineFieldMirrorsAPI confirms the status tool's "pipeline"
// array (docs/plans/phase5.md §3.4, chunk P5-H) mirrors dashboard/api.go's
// field-for-field: members, chainTip, predicted, batchId, checksDone,
// currentCheck, startedAt, present per run, head first, while inFlight stays
// the head run for back-compat.
func TestStatus_PipelineFieldMirrorsAPI(t *testing.T) {
	snap := pipelineSnapshot()
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return snap }})

	res := callTool(t, session, "status", map[string]any{"target": "spec"})
	if res.IsError {
		t.Fatalf("status errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{
		`"runID":"run-spec-0"`, // inFlight stays the head run
		`"pipeline"`, `"members"`, `"chainTip"`, `"predicted"`, `"batchId"`,
		`"checksDone"`, `"currentCheck"`, `"startedAt"`,
		`"predicted":false`, `"predicted":true`,
		"check0", "check1",
	} {
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

// batchMemberRecord builds one member's RunRecord of a 3-member batch
// sharing batchID, mirroring queue/reconcile.go's per-member RunRecords
// (§3.3): same shape as sampleRecord, but with BatchID/Position/BatchSize
// set (docs/plans/phase5.md §10 amendment 1).
func batchMemberRecord(runID, batchID string, position int) *core.RunRecord {
	rec := sampleRecord(runID, "main")
	rec.BatchID = batchID
	rec.Position = position
	rec.BatchSize = 3
	return rec
}

// TestRun_BatchFieldsIncludedForBatchMember confirms the run tool mirrors
// batchId/position/batchSize (docs/plans/phase5.md §10 amendment 1) for a
// batch member.
func TestRun_BatchFieldsIncludedForBatchMember(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: batchMemberRecord("batch-run-2", "batch-xyz", 2)}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "run", map[string]any{"run_id": "batch-run-2"})
	if res.IsError {
		t.Fatalf("run errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{`"batchId":"batch-xyz"`, `"position":2`, `"batchSize":3`} {
		if !strings.Contains(text, want) {
			t.Errorf("run content missing %q:\n%s", want, text)
		}
	}
}

// TestRun_BatchFieldsOmittedForSerialRun confirms an ordinary serial run
// omits all three batch fields entirely (omitempty).
func TestRun_BatchFieldsOmittedForSerialRun(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: sampleRecord("run-hist-1", "main")}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "run", map[string]any{"run_id": "run-hist-1"})
	text := textOf(t, res)
	for _, absent := range []string{`"batchId"`, `"position"`, `"batchSize"`} {
		if strings.Contains(text, absent) {
			t.Errorf("serial run must omit %q (omitempty):\n%s", absent, text)
		}
	}
}

// TestRuns_BatchFieldsIncludedForBatchMember confirms the runs tool mirrors
// the same batch fields alongside a serial run in the same result set.
func TestRuns_BatchFieldsIncludedForBatchMember(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: batchMemberRecord("batch-run-1", "batch-xyz", 1)}); err != nil {
		t.Fatalf("Emit (batch): %v", err)
	}
	if err := store.Emit(context.Background(), core.Event{Record: sampleRecord("run-hist-1", "main")}); err != nil {
		t.Fatalf("Emit (serial): %v", err)
	}

	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "runs", map[string]any{"target": "main"})
	if res.IsError {
		t.Fatalf("runs errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{`"batchId":"batch-xyz"`, `"position":1`, `"batchSize":3`} {
		if !strings.Contains(text, want) {
			t.Errorf("runs content missing %q:\n%s", want, text)
		}
	}
}

// logRecord builds a run record with one check carrying a non-empty
// LogPath, for exercising the run tool's logPath/logUrl fields (chunk F-b).
func logRecord(runID string) *core.RunRecord {
	started := time.Date(2026, 7, 5, 11, 0, 0, 0, time.UTC)
	return &core.RunRecord{
		RunID:  runID,
		Target: "main",
		Candidate: core.Candidate{
			Ref: "refs/heads/for/main/dave/histfix", Target: "main",
			User: "dave", Topic: "histfix", SHA: "5555555555555555555555555555555555555555",
		},
		Checks: []core.CheckResult{
			{Name: "test", Status: core.CheckFailed, Duration: time.Second, Output: "boom", LogPath: "/var/lib/gauntlet/logs/" + runID + "/test.log"},
		},
		Outcome:   core.OutcomeRejected,
		StartedAt: started,
		EndedAt:   started.Add(time.Second),
	}
}

// TestRun_LogFieldsIncludedWhenLogRootConfigured confirms the run tool's
// checks carry logPath (verbatim from history) and logUrl (only when
// Params.LogRoot is set) — mirroring dashboard/api.go's GET /api/v1/run/{id}
// field additions (chunk F-b).
func TestRun_LogFieldsIncludedWhenLogRootConfigured(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: logRecord("run-log-mcp")}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	session := connect(t, mcpsrv.Params{
		Snapshot: func() *queue.Snapshot { return nil },
		Store:    store,
		LogRoot:  "/var/lib/gauntlet/logs",
	})

	res := callTool(t, session, "run", map[string]any{"run_id": "run-log-mcp"})
	if res.IsError {
		t.Fatalf("run errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{
		`"logPath":"/var/lib/gauntlet/logs/run-log-mcp/test.log"`,
		`"logUrl":"/run/run-log-mcp/log/test"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("run content missing %q:\n%s", want, text)
		}
	}
}

// TestRun_LogURLOmittedWithoutLogRoot confirms logUrl is absent when
// Params.LogRoot isn't set, even though logPath is still present — an agent
// should never be handed a link that always 404s.
func TestRun_LogURLOmittedWithoutLogRoot(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: logRecord("run-log-mcp-2")}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "run", map[string]any{"run_id": "run-log-mcp-2"})
	text := textOf(t, res)
	if !strings.Contains(text, `"logPath":"/var/lib/gauntlet/logs/run-log-mcp-2/test.log"`) {
		t.Errorf("run content missing logPath:\n%s", text)
	}
	if strings.Contains(text, `"logUrl"`) {
		t.Errorf("run content included logUrl without LogRoot configured:\n%s", text)
	}
}

// emitHook writes one post-land hook result (internal/hooks;
// core.EventHookFinished) into s, the way internal/hooks.Runner actually
// emits one: Record is always nil on this event kind, the whole payload
// rides on Check + CheckName.
func emitHook(t *testing.T, s *history.Store, runID, name string, cr core.CheckResult) {
	t.Helper()
	cr.Name = name
	ev := core.Event{Kind: core.EventHookFinished, Target: "main", RunID: runID, CheckName: name, Check: &cr}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit (hook %s): %v", name, err)
	}
}

// TestRun_HooksFieldIncluded confirms the run tool gains a "hooks" array
// alongside "checks" (chunk P5-B, log/history parity): present (even if
// empty) and populated with the same field shape a check gets, including
// captured Output for an agent debugging a failed hook.
func TestRun_HooksFieldIncluded(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: sampleRecord("run-hooks-mcp-1", "main")}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	emitHook(t, store, "run-hooks-mcp-1", "deploy", core.CheckResult{Status: core.CheckFailed, Duration: 250 * time.Millisecond, Output: "deploy exploded"})

	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "run", map[string]any{"run_id": "run-hooks-mcp-1"})
	if res.IsError {
		t.Fatalf("run errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{`"hooks"`, `"name":"deploy"`, `"status":"failed"`, "deploy exploded"} {
		if !strings.Contains(text, want) {
			t.Errorf("run content missing %q:\n%s", want, text)
		}
	}
}

// TestRun_HooksFieldEmptyArrayWhenNone confirms "hooks" is present as an
// empty array (never omitted) for a run with no hook rows.
func TestRun_HooksFieldEmptyArrayWhenNone(t *testing.T) {
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{Record: sampleRecord("run-hooks-mcp-2", "main")}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }, Store: store})

	res := callTool(t, session, "run", map[string]any{"run_id": "run-hooks-mcp-2"})
	if res.IsError {
		t.Fatalf("run errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	if !strings.Contains(text, `"hooks":[]`) {
		t.Errorf("run content missing empty hooks array:\n%s", text)
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

// --- cancel (Feature 1: manual operator cancellation) ----------------------
// Mirrors the retry suite above exactly: same wiring, same request/response
// shape, differing only in core.Command.Kind.

func TestCancel_CommandLandsOnChannel(t *testing.T) {
	ch := dashboard.NewChannel()
	session := connect(t, mcpsrv.Params{
		Snapshot: func() *queue.Snapshot { return nil },
		Cancel:   ch.TrySend,
	})

	res := callTool(t, session, "cancel", map[string]any{"target": "main", "ref": "refs/heads/for/main/alice/feat-a"})
	if res.IsError {
		t.Fatalf("cancel errored: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "queued") {
		t.Errorf("expected \"queued\" in cancel result, got: %s", textOf(t, res))
	}

	select {
	case cmd := <-ch.Commands():
		if cmd.Kind != core.CommandCancel || cmd.Target != "main" || cmd.Ref != "refs/heads/for/main/alice/feat-a" {
			t.Errorf("unexpected command: %+v", cmd)
		}
	default:
		t.Fatal("expected a command on the channel, got none")
	}
}

func TestCancel_NilCancelFunc_ErrorResult(t *testing.T) {
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})

	res := callTool(t, session, "cancel", map[string]any{"target": "main", "ref": "refs/heads/for/main/alice/feat-a"})
	if !res.IsError {
		t.Fatalf("expected error result when cancel is disabled, got: %s", textOf(t, res))
	}
}

func TestCancel_BufferFull_ErrorResult(t *testing.T) {
	calls := 0
	session := connect(t, mcpsrv.Params{
		Snapshot: func() *queue.Snapshot { return nil },
		Cancel: func(core.Command) bool {
			calls++
			return false // simulate a full buffer
		},
	})

	res := callTool(t, session, "cancel", map[string]any{"target": "main", "ref": "refs/heads/for/main/alice/feat-a"})
	if !res.IsError {
		t.Fatalf("expected error result when cancel reports backpressure, got: %s", textOf(t, res))
	}
	if calls != 1 {
		t.Errorf("expected Cancel to be called once, got %d", calls)
	}
}

// --- hook_cancel ------------------------------------------------------------

func TestHookCancel_Cancelled(t *testing.T) {
	var gotTarget string
	session := connect(t, mcpsrv.Params{
		Snapshot: func() *queue.Snapshot { return nil },
		HookCancel: func(target string) bool {
			gotTarget = target
			return true
		},
	})

	res := callTool(t, session, "hook_cancel", map[string]any{"target": "main"})
	if res.IsError {
		t.Fatalf("hook_cancel errored: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "cancelled") {
		t.Errorf("expected \"cancelled\" in hook_cancel result, got: %s", textOf(t, res))
	}
	if gotTarget != "main" {
		t.Errorf("HookCancel called with target = %q, want main", gotTarget)
	}
}

func TestHookCancel_NoOpWhenNothingRunning(t *testing.T) {
	session := connect(t, mcpsrv.Params{
		Snapshot:   func() *queue.Snapshot { return nil },
		HookCancel: func(string) bool { return false },
	})

	res := callTool(t, session, "hook_cancel", map[string]any{"target": "main"})
	if res.IsError {
		t.Fatalf("hook_cancel errored: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "no-op") {
		t.Errorf("expected \"no-op\" in hook_cancel result, got: %s", textOf(t, res))
	}
}

func TestHookCancel_NilHookCancelFunc_ErrorResult(t *testing.T) {
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})

	res := callTool(t, session, "hook_cancel", map[string]any{"target": "main"})
	if !res.IsError {
		t.Fatalf("expected error result when hook cancel is disabled, got: %s", textOf(t, res))
	}
}
