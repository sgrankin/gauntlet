package mcp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/history"
	mcpsrv "github.com/sgrankin/gauntlet/internal/mcp"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// The four deploy tools. Timestamps are relative offsets from time.Now(),
// never fixed dates — these tools order by time and the fixtures must not
// rot as the calendar moves.

func deployStatus(now time.Time) *mcpsrv.DeployStatus {
	return &mcpsrv.DeployStatus{
		At: now,
		Lanes: []mcpsrv.DeployLane{
			{
				Env: "dev", Mode: "track", Source: "main",
				SourceTip: "e5f6a7b8c9d0", Desired: "e5f6a7b8c9d0", Observed: "e5f6a7b8c9d0",
				InSync: true,
			},
			{
				Env: "prod", Mode: "track", Source: "env=dev",
				SourceTip: "e5f6a7b8c9d0", Desired: "e5f6a7b8c9d0", Observed: "9c0d1e2f3a4b",
				Running: &mcpsrv.DeployRun{
					RunID: "deploy-prod-2", DeploySHA: "e5f6a7b8c9d0", DeployedSHA: "9c0d1e2f3a4b",
					StartedAt: now.Add(-2 * time.Minute), Nodes: []string{"app2"},
				},
			},
			{
				Env: "prod2", Mode: "manual", Source: "env=dev",
				SourceTip: "e5f6a7b8c9d0", Desired: "e5f6a7b8c9d0", Observed: "9c0d1e2f3a4b",
				Parked: &mcpsrv.DeployPark{
					SHA: "e5f6a7b8c9d0", RunID: "deploy-prod2-3", Outcome: "rejected",
					Detail: "node migrate failed", At: now.Add(-3 * time.Minute),
				},
			},
		},
	}
}

func seedDeploy(t *testing.T, s *history.Store, now time.Time) {
	t.Helper()
	rec := &core.DeployRecord{
		RunID: "deploy-prod2-3", Env: "prod2",
		DeploySHA: "e5f6a7b8c9d0", DeployedSHA: "9c0d1e2f3a4b",
		Nodes: []core.CheckResult{
			{Name: "migrate", Seq: 1, Status: core.CheckFailed, Duration: 24 * time.Second, Output: "lock wait timeout"},
			{Name: "app1", Seq: 2, Status: core.CheckBlocked, BlockedBy: []string{"migrate"}},
		},
		Outcome: core.OutcomeRejected, Culprit: "migrate", Detail: "node migrate failed",
		StartedAt: now.Add(-4 * time.Minute), EndedAt: now.Add(-3 * time.Minute),
	}
	err := s.Emit(context.Background(), core.Event{
		Kind: core.EventDeployFinished, At: rec.EndedAt, RunID: rec.RunID,
		DeployEnv: rec.Env, DeploySHA: rec.DeploySHA, DeployedSHA: rec.DeployedSHA,
		Deploy: rec,
	})
	if err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
}

func TestDeployTools_Listed(t *testing.T) {
	session := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{"deploys": false, "deploy": false, "deploy_retry": false, "deploy_cancel": false}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
			if tool.InputSchema == nil {
				t.Errorf("tool %s: nil input schema", tool.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q missing from tools/list", name)
		}
	}
}

func TestDeploysTool_LanesAndRecent(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeploy(t, store, now)
	session := connect(t, mcpsrv.Params{
		Snapshot:       func() *queue.Snapshot { return nil },
		Store:          store,
		DeploySnapshot: func() *mcpsrv.DeployStatus { return deployStatus(now) },
	})

	res := callTool(t, session, "deploys", map[string]any{})
	if res.IsError {
		t.Fatalf("deploys errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{
		`"env":"dev"`, `"state":"in sync"`,
		`"env":"prod"`, `"state":"deploying"`, `"runID":"deploy-prod-2"`, `"nodes":["app2"]`,
		`"env":"prod2"`, `"state":"parked"`, `"outcome":"rejected"`,
		`"recent"`, `"culprit":"migrate"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deploys output missing %q\n%s", want, text)
		}
	}

	// env narrows both the lanes and the recent list.
	res = callTool(t, session, "deploys", map[string]any{"env": "prod2"})
	text = textOf(t, res)
	if strings.Contains(text, `"env":"dev"`) || strings.Contains(text, `"env":"prod"`+",") {
		t.Errorf("env filter did not narrow the lanes:\n%s", text)
	}
	if !strings.Contains(text, `"env":"prod2"`) {
		t.Errorf("env filter dropped the requested lane:\n%s", text)
	}
}

// TestDeploysTool_NotConfiguredAndNoSnapshot: the same two "absent" answers
// GET /api/v1/deploys gives as 503s, worded identically.
func TestDeploysTool_NotConfiguredAndNoSnapshot(t *testing.T) {
	bare := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})
	res := callTool(t, bare, "deploys", map[string]any{})
	if !res.IsError || !strings.Contains(textOf(t, res), "deploy not configured") {
		t.Errorf("deploys with no tracker = %q, want an error saying \"deploy not configured\"", textOf(t, res))
	}

	pending := connect(t, mcpsrv.Params{
		Snapshot:       func() *queue.Snapshot { return nil },
		DeploySnapshot: func() *mcpsrv.DeployStatus { return nil },
	})
	res = callTool(t, pending, "deploys", map[string]any{})
	if !res.IsError || !strings.Contains(textOf(t, res), "no snapshot yet") {
		t.Errorf("deploys before the first pass = %q, want \"no snapshot yet\"", textOf(t, res))
	}
}

func TestDeployTool_DetailAndErrors(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeploy(t, store, now)
	session := connect(t, mcpsrv.Params{
		Snapshot:       func() *queue.Snapshot { return nil },
		Store:          store,
		DeploySnapshot: func() *mcpsrv.DeployStatus { return deployStatus(now) },
	})

	res := callTool(t, session, "deploy", map[string]any{"run_id": "deploy-prod2-3"})
	if res.IsError {
		t.Fatalf("deploy errored: %s", textOf(t, res))
	}
	text := textOf(t, res)
	for _, want := range []string{
		`"env":"prod2"`, `"culprit":"migrate"`,
		`"name":"migrate"`, `"status":"failed"`, "lock wait timeout",
		`"name":"app1"`, `"status":"blocked"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deploy output missing %q\n%s", want, text)
		}
	}

	res = callTool(t, session, "deploy", map[string]any{"run_id": "nope"})
	if !res.IsError || !strings.Contains(textOf(t, res), "deploy not found") {
		t.Errorf("unknown deploy = %q, want a not-found error", textOf(t, res))
	}

	noStore := connect(t, mcpsrv.Params{
		Snapshot:       func() *queue.Snapshot { return nil },
		DeploySnapshot: func() *mcpsrv.DeployStatus { return deployStatus(now) },
	})
	res = callTool(t, noStore, "deploy", map[string]any{"run_id": "deploy-prod2-3"})
	if !res.IsError || !strings.Contains(textOf(t, res), "history disabled") {
		t.Errorf("deploy with history off = %q, want \"history disabled\"", textOf(t, res))
	}
}

// TestDeployRetryCancelTools mirrors the API's semantics exactly: a status
// word on success, "no-op" when there was nothing to act on, "env is
// required" for a missing argument, "deploy not configured" when unwired.
func TestDeployRetryCancelTools(t *testing.T) {
	session := connect(t, mcpsrv.Params{
		Snapshot:     func() *queue.Snapshot { return nil },
		DeployRetry:  func(env string) bool { return env == "prod2" },
		DeployCancel: func(env string) bool { return env == "prod" },
	})

	cases := []struct {
		tool, env, want string
	}{
		{"deploy_retry", "prod2", `"status":"retried"`},
		{"deploy_retry", "dev", `"status":"no-op"`},
		{"deploy_cancel", "prod", `"status":"cancelled"`},
		{"deploy_cancel", "dev", `"status":"no-op"`},
	}
	for _, c := range cases {
		res := callTool(t, session, c.tool, map[string]any{"env": c.env})
		if res.IsError {
			t.Errorf("%s(%s) errored: %s", c.tool, c.env, textOf(t, res))
			continue
		}
		if got := textOf(t, res); !strings.Contains(got, c.want) {
			t.Errorf("%s(%s) = %q, want %q", c.tool, c.env, got, c.want)
		}
	}

	// An EMPTY env, not an absent one: the SDK's own schema validation
	// rejects a missing required property before the handler ever runs, so
	// the handler's own guard is what an empty string exercises.
	for _, tool := range []string{"deploy_retry", "deploy_cancel"} {
		res := callTool(t, session, tool, map[string]any{"env": ""})
		if !res.IsError || !strings.Contains(textOf(t, res), "env is required") {
			t.Errorf("%s with an empty env = %q, want \"env is required\"", tool, textOf(t, res))
		}
	}

	bare := connect(t, mcpsrv.Params{Snapshot: func() *queue.Snapshot { return nil }})
	for _, tool := range []string{"deploy_retry", "deploy_cancel"} {
		res := callTool(t, bare, tool, map[string]any{"env": "prod"})
		if !res.IsError || !strings.Contains(textOf(t, res), "deploy not configured") {
			t.Errorf("%s on an unconfigured daemon = %q, want \"deploy not configured\"", tool, textOf(t, res))
		}
	}
}

// TestStatusTool_IdleSinceComposesDeployLanes: the daemon-wide idle signal
// must account for deploy lanes, both the running and the about-to-run half.
func TestStatusTool_IdleSinceComposesDeployLanes(t *testing.T) {
	now := time.Now()
	idleQueue := &queue.Snapshot{
		At: now, IdleSince: now.Add(-10 * time.Minute),
		Targets: []queue.TargetSnapshot{{Name: "main", Branch: "main"}},
	}
	idle := func(t *testing.T, lanes []mcpsrv.DeployLane) bool {
		t.Helper()
		session := connect(t, mcpsrv.Params{
			Snapshot:       func() *queue.Snapshot { return idleQueue },
			DeploySnapshot: func() *mcpsrv.DeployStatus { return &mcpsrv.DeployStatus{At: now, Lanes: lanes} },
		})
		return strings.Contains(textOf(t, callTool(t, session, "status", map[string]any{})), `"idleSince"`)
	}

	if !idle(t, []mcpsrv.DeployLane{{Env: "dev", InSync: true, Desired: "a", Observed: "a"}}) {
		t.Error("a settled lane suppressed the idle signal")
	}
	if idle(t, []mcpsrv.DeployLane{{Env: "dev", Running: &mcpsrv.DeployRun{RunID: "d", StartedAt: now}}}) {
		t.Error("idleSince present while a lane is deploying")
	}
	if idle(t, []mcpsrv.DeployLane{{Env: "dev", Desired: "b", Observed: "a", Pending: true}}) {
		t.Error("idleSince present while a lane would start a run on the next pass")
	}
}
