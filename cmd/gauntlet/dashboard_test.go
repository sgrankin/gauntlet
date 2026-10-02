package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/deploy"
	gauntletmcp "github.com/sgrankin/gauntlet/internal/mcp"
	"github.com/sgrankin/gauntlet/internal/queue"
	"github.com/sgrankin/gauntlet/internal/services"
)

// TestShouldRecord exercises the pure decision behind the depth sampler's
// change-only recording (chunk E1): record on a tuple change, or when the
// heartbeat interval has elapsed since the last recording, and never
// otherwise.
func TestShouldRecord(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		last, current depthTuple
		lastAt, now   time.Time
		want          bool
	}{
		{
			name:    "first sample ever (lastAt zero) always records",
			last:    depthTuple{},
			current: depthTuple{},
			lastAt:  time.Time{},
			now:     base,
			want:    true,
		},
		{
			name:    "changed tuple records immediately",
			last:    depthTuple{Waiting: 1, InFlight: 0, Parked: 0},
			current: depthTuple{Waiting: 2, InFlight: 0, Parked: 0},
			lastAt:  base,
			now:     base.Add(time.Second),
			want:    true,
		},
		{
			name:    "unchanged tuple, well within heartbeat, does not record",
			last:    depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			current: depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			lastAt:  base,
			now:     base.Add(time.Minute),
			want:    false,
		},
		{
			name:    "unchanged tuple, exactly at heartbeat, records",
			last:    depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			current: depthTuple{Waiting: 1, InFlight: 1, Parked: 0},
			lastAt:  base,
			now:     base.Add(depthHeartbeat),
			want:    true,
		},
		{
			name:    "unchanged tuple, past heartbeat, records",
			last:    depthTuple{Waiting: 3, InFlight: 0, Parked: 2},
			current: depthTuple{Waiting: 3, InFlight: 0, Parked: 2},
			lastAt:  base,
			now:     base.Add(depthHeartbeat + time.Hour),
			want:    true,
		},
		{
			name:    "only parked differs still counts as changed",
			last:    depthTuple{Waiting: 0, InFlight: 0, Parked: 1},
			current: depthTuple{Waiting: 0, InFlight: 0, Parked: 2},
			lastAt:  base,
			now:     base.Add(time.Second),
			want:    true,
		},
		{
			name:    "unchanged zero tuple just under heartbeat does not record",
			last:    depthTuple{},
			current: depthTuple{},
			lastAt:  base,
			now:     base.Add(depthHeartbeat - time.Nanosecond),
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRecord(tc.last, tc.current, tc.lastAt, tc.now)
			if got != tc.want {
				t.Errorf("shouldRecord(%+v, %+v, lastAt=%v, now=%v) = %v, want %v",
					tc.last, tc.current, tc.lastAt, tc.now, got, tc.want)
			}
		})
	}
}

// TestBuildDepthTuple_InFlightIsPipelineDepth exercises the depth sampler's
// tuple extraction (see docs/design/queue-modes.md, "Snapshot and pipeline
// view"): the InFlight component is len(TargetSnapshot.Pipeline), not a 0/1
// InFlight!=nil flag. For a serial-mode target Pipeline never exceeds 1
// element, so the idle (0) and serial-busy (1) cases must come out
// byte-identical to the sampler's old InFlight!=nil values — no series
// discontinuity — while a hand-built depth-3 pipeline (standing in for what
// a batch or speculate target publishes) proves the tuple reflects actual
// pipeline occupancy rather than a boolean.
func TestBuildDepthTuple_InFlightIsPipelineDepth(t *testing.T) {
	cases := []struct {
		name string
		ts   queue.TargetSnapshot
		want depthTuple
	}{
		{
			name: "idle: no pipeline, no waiting/parked",
			ts:   queue.TargetSnapshot{},
			want: depthTuple{Waiting: 0, InFlight: 0, Parked: 0},
		},
		{
			name: "serial-busy: one run in the pipeline, unchanged from today's InFlight!=nil=1",
			ts: queue.TargetSnapshot{
				Pipeline: []queue.RunSnapshot{{RunID: "run-1"}},
				Waiting:  []queue.WaitingEntry{{}, {}},
			},
			want: depthTuple{Waiting: 2, InFlight: 1, Parked: 0},
		},
		{
			name: "pipeline depth 3 (speculation): InFlight reflects lane depth, not a 0/1 flag",
			ts: queue.TargetSnapshot{
				Pipeline: []queue.RunSnapshot{{RunID: "run-1"}, {RunID: "run-2"}, {RunID: "run-3"}},
				Parked:   []queue.ParkedEntry{{}},
			},
			want: depthTuple{Waiting: 0, InFlight: 3, Parked: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDepthTuple(tc.ts)
			if got != tc.want {
				t.Errorf("buildDepthTuple(%+v) = %+v, want %+v", tc.ts, got, tc.want)
			}
		})
	}
}

// testPoolStatus builds a small services.PoolStatus fixture shared by the
// dashboardServicesStatus/mcpServicesStatus adapter tests below.
func testPoolStatus() services.PoolStatus {
	return services.PoolStatus{
		MaxInstances: 4,
		Pending:      1,
		Instances: []services.InstanceStatus{
			{
				Service: "pg", Image: "postgres:16",
				Key: "abcdef0123456789fullkey", KeyHash12: "abcdef012345",
				Mode: services.ModeNetwork, Host: "abcdef012345", Port: "5432",
				CreatedAt: time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC),
				LastUsed:  time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC),
				Refcount:  2, Hits: 7,
			},
		},
	}
}

// TestDashboardServicesStatus_ConvertsFieldByField confirms
// dashboardServicesStatus carries every field across, including converting
// services.Mode to its string form (Mode.String()) — the one field this
// adapter can't do as a plain type conversion (dashboard.go's doc).
func TestDashboardServicesStatus_ConvertsFieldByField(t *testing.T) {
	ps := testPoolStatus()
	got := dashboardServicesStatus(ps)

	if got.MaxInstances != 4 || got.Pending != 1 {
		t.Fatalf("MaxInstances/Pending = %d/%d, want 4/1", got.MaxInstances, got.Pending)
	}
	if len(got.Instances) != 1 {
		t.Fatalf("Instances = %v, want exactly 1", got.Instances)
	}
	want := dashboard.ServiceStatus{
		Service: "pg", Image: "postgres:16",
		Key: "abcdef0123456789fullkey", KeyHash12: "abcdef012345",
		Mode: "network", Host: "abcdef012345", Port: "5432",
		CreatedAt: ps.Instances[0].CreatedAt, LastUsed: ps.Instances[0].LastUsed,
		Refcount: 2, Hits: 7,
	}
	if got.Instances[0] != want {
		t.Errorf("Instances[0] = %+v, want %+v", got.Instances[0], want)
	}
}

// TestMCPServicesStatus_ConvertsFieldByField mirrors
// TestDashboardServicesStatus_ConvertsFieldByField for mcpServicesStatus.
func TestMCPServicesStatus_ConvertsFieldByField(t *testing.T) {
	ps := testPoolStatus()
	got := mcpServicesStatus(ps)

	if got.MaxInstances != 4 || got.Pending != 1 {
		t.Fatalf("MaxInstances/Pending = %d/%d, want 4/1", got.MaxInstances, got.Pending)
	}
	if len(got.Instances) != 1 {
		t.Fatalf("Instances = %v, want exactly 1", got.Instances)
	}
	want := gauntletmcp.ServiceStatus{
		Service: "pg", Image: "postgres:16",
		Key: "abcdef0123456789fullkey", KeyHash12: "abcdef012345",
		Mode: "network", Host: "abcdef012345", Port: "5432",
		CreatedAt: ps.Instances[0].CreatedAt, LastUsed: ps.Instances[0].LastUsed,
		Refcount: 2, Hits: 7,
	}
	if got.Instances[0] != want {
		t.Errorf("Instances[0] = %+v, want %+v", got.Instances[0], want)
	}
}

// TestServicesStatus_EmptyPoolHasNoInstances confirms both adapters return an
// empty (non-nil) Instances slice for a pool with none live — mirroring the
// dashboard/MCP JSON handlers' own "empty array, not omitted" convention.
func TestServicesStatus_EmptyPoolHasNoInstances(t *testing.T) {
	ps := services.PoolStatus{MaxInstances: 8}

	if got := dashboardServicesStatus(ps); len(got.Instances) != 0 {
		t.Errorf("dashboardServicesStatus(empty).Instances = %v, want empty", got.Instances)
	}
	if got := mcpServicesStatus(ps); len(got.Instances) != 0 {
		t.Errorf("mcpServicesStatus(empty).Instances = %v, want empty", got.Instances)
	}
}

// --- deploy wiring (D3) --------------------------------------------------------

// testDeploySnapshot is one published deploy.Snapshot covering every field
// the two adapters have to carry: a running lane, a parked one, and a
// finished result — the three optional blocks, plus the typed Mode/Outcome
// values that are exactly why these adapters can't be plain conversions.
func testDeploySnapshot() *deploy.Snapshot {
	now := time.Now()
	return &deploy.Snapshot{
		At: now,
		Lanes: []deploy.LaneState{
			{
				Env: "prod", Mode: deploy.ModeTrack, Source: "env=dev",
				SourceTip: "e5f6a7b8", Desired: "e5f6a7b8", Observed: "9c0d1e2f",
				Drift: true, Pending: true,
				LastAdvance: now.Add(-2 * time.Minute), LastError: "lost CAS",
				Running: &deploy.LaneRun{
					RunID: "deploy-1", DeploySHA: "e5f6a7b8", DeployedSHA: "9c0d1e2f",
					StartedAt: now.Add(-time.Minute), Nodes: []string{"app1", "app2"},
				},
				Parked: &deploy.LanePark{
					SHA: "e5f6a7b8", RunID: "deploy-0", Outcome: core.OutcomeRejected,
					Detail: "node migrate failed", At: now.Add(-3 * time.Minute),
				},
				LastResult: &deploy.LaneResult{
					RunID: "deploy-0", DeploySHA: "e5f6a7b8", DeployedSHA: "9c0d1e2f",
					Outcome: core.OutcomeError, Culprit: "migrate", Detail: "executor unreachable",
					StartedAt: now.Add(-4 * time.Minute), EndedAt: now.Add(-3 * time.Minute),
				},
			},
			{Env: "dev", Mode: deploy.ModeManual, Source: "main", InSync: true, Desired: "a", Observed: "a"},
		},
	}
}

// TestDashboardDeployStatus_ConvertsFieldByField: the adapter carries every
// field across, converting the two typed values (deploy.Mode, core.Outcome)
// to the string forms the dashboard's mirror structs hold — the reason this
// isn't a plain type conversion, exactly as for services.
func TestDashboardDeployStatus_ConvertsFieldByField(t *testing.T) {
	snap := testDeploySnapshot()
	got := dashboardDeployStatus(snap)

	if got == nil || len(got.Lanes) != 2 || !got.At.Equal(snap.At) {
		t.Fatalf("converted snapshot = %+v", got)
	}
	lane := got.Lanes[0]
	if lane.Env != "prod" || lane.Mode != "track" || lane.Source != "env=dev" {
		t.Errorf("lane identity = %+v", lane)
	}
	if !lane.Drift || !lane.Pending || lane.InSync {
		t.Errorf("lane flags = drift %v pending %v inSync %v, want true/true/false", lane.Drift, lane.Pending, lane.InSync)
	}
	if lane.LastError != "lost CAS" || !lane.LastAdvance.Equal(snap.Lanes[0].LastAdvance) {
		t.Errorf("lane advance/error = %v / %q", lane.LastAdvance, lane.LastError)
	}
	if lane.Running == nil || lane.Running.RunID != "deploy-1" || len(lane.Running.Nodes) != 2 {
		t.Errorf("running = %+v", lane.Running)
	}
	if lane.Parked == nil || lane.Parked.Outcome != "rejected" || lane.Parked.RunID != "deploy-0" {
		t.Errorf("parked = %+v, want outcome rejected", lane.Parked)
	}
	if lane.LastResult == nil || lane.LastResult.Outcome != "error" || lane.LastResult.Culprit != "migrate" {
		t.Errorf("lastResult = %+v, want outcome error with culprit migrate", lane.LastResult)
	}
	if got.Lanes[1].Mode != "manual" || !got.Lanes[1].InSync {
		t.Errorf("second lane = %+v, want the manual in-sync lane", got.Lanes[1])
	}

	// The running node slice must be a COPY: the published Snapshot's own
	// slices are never mutated after publication, but an adapter that
	// aliased them would tie the surface's lifetime to the tracker's.
	lane.Running.Nodes[0] = "clobbered"
	if snap.Lanes[0].Running.Nodes[0] != "app1" {
		t.Error("the adapter aliased the tracker's node slice instead of copying it")
	}

	if dashboardDeployStatus(nil) != nil {
		t.Error("a nil snapshot (no pass published yet) must convert to nil")
	}
}

// TestMCPDeployStatus_ConvertsFieldByField mirrors the above for the MCP
// adapter — the two must not drift apart.
func TestMCPDeployStatus_ConvertsFieldByField(t *testing.T) {
	snap := testDeploySnapshot()
	got := mcpDeployStatus(snap)

	if got == nil || len(got.Lanes) != 2 {
		t.Fatalf("converted snapshot = %+v", got)
	}
	lane := got.Lanes[0]
	if lane.Env != "prod" || lane.Mode != "track" || !lane.Pending {
		t.Errorf("lane = %+v", lane)
	}
	if lane.Running == nil || lane.Running.RunID != "deploy-1" {
		t.Errorf("running = %+v", lane.Running)
	}
	if lane.Parked == nil || lane.Parked.Outcome != "rejected" {
		t.Errorf("parked = %+v", lane.Parked)
	}
	if lane.LastResult == nil || lane.LastResult.Outcome != "error" {
		t.Errorf("lastResult = %+v", lane.LastResult)
	}
	if mcpDeployStatus(nil) != nil {
		t.Error("a nil snapshot must convert to nil")
	}
}

// TestStartDashboard_NoDeployTracker is the wiring assertion the adapters
// alone can't make: a daemon with no `deploy` block still SERVES the deploy
// routes — they exist in every build — and every one of them answers "not
// configured" rather than 404ing or panicking. This is the empty-state
// contract end to end, through the real startDashboard.
func TestStartDashboard_NoDeployTracker(t *testing.T) {
	addr := reserveAddr(t)
	cfg := &config.Daemon{Dashboard: config.Dashboard{Bind: addr}}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	// Order matters: cancel first, THEN wait — the server goroutines only
	// exit once ctx is done, so waiting before cancelling deadlocks.
	t.Cleanup(func() { cancel(); wg.Wait() })
	startDashboard(ctx, cfg, func() *queue.Snapshot { return nil }, nil, nil, t.TempDir(),
		nil, nil, nil, deployWiring{}, nil, &wg, nil)

	base := "http://" + addr
	waitForServer(t, base+"/deploys")

	// HTML: a 200 that says so, and no nav entry to it.
	status, body := httpGet(t, base+"/deploys")
	if status != http.StatusOK || !strings.Contains(body, "no environments configured") {
		t.Errorf("GET /deploys = %d, body:\n%s", status, body)
	}
	if strings.Contains(body, `href="/deploys"`) {
		t.Error("nav links /deploys on a daemon with no deploy tracker")
	}

	// JSON: 503, with the binding wording.
	status, body = httpGet(t, base+"/api/v1/deploys")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "deploy not configured") {
		t.Errorf("GET /api/v1/deploys = %d %s, want 503 \"deploy not configured\"", status, body)
	}
	for _, path := range []string{"/api/v1/deploy/retry", "/api/v1/deploy/cancel"} {
		status, body := httpPost(t, base+path, `{"env":"prod"}`)
		if status != http.StatusServiceUnavailable || !strings.Contains(body, "deploy not configured") {
			t.Errorf("POST %s = %d %s, want 503 \"deploy not configured\"", path, status, body)
		}
	}
}

// reserveAddr picks a free loopback address by binding and immediately
// releasing it — the same technique doctor_test uses for its port probes.
func reserveAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return addr
}

// waitForServer blocks until url answers, so the test never races
// startDashboard's own ListenAndServe goroutine.
func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dashboard never came up at %s", url)
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

func httpPost(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(out)
}
