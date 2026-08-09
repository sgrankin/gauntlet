package mcp

// The deploy tools: deploys (the lane overview), deploy (one graph run's
// detail), deploy_retry and deploy_cancel — the MCP mirrors of
// GET /api/v1/deploys, GET /api/v1/deploy/{id}, and the two env-addressed
// POST routes (docs/design/deployment.md, "API/MCP").
//
// Same semantics as the HTTP surface, deliberately down to the wording:
// "deploy not configured" when this daemon has no deploy tracker, "env is
// required" for a missing argument, and a {"status": ...} answer whose word
// is "retried"/"cancelled" or "no-op". An agent and a curl user must never
// get different answers to the same question.
//
// The view structs mirror dashboard's field-for-field and are duplicated
// rather than imported, the package's standing convention (see Params'
// LiveHook/ServiceStatus docs): internal/mcp never imports internal/deploy
// or internal/dashboard, and cmd/gauntlet's adapter is the one place a
// deploy.Snapshot is converted into either.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/sgrankin/gauntlet/internal/history"
)

// DeployStatus mirrors deploy.Snapshot; DeployLane/DeployRun/DeployPark/
// DeployResult mirror its LaneState/LaneRun/LanePark/LaneResult. Enum-ish
// fields (Mode, Outcome) are already the string form — cmd/gauntlet's
// adapter converts them.
type DeployStatus struct {
	At    time.Time
	Lanes []DeployLane
}

type DeployLane struct {
	Env    string
	Mode   string
	Source string

	SourceTip string
	Desired   string
	Observed  string

	InSync bool
	Drift  bool

	// Pending: this lane would start a graph run on the next pass (the
	// runner's own admission predicate). Composed with Running by idleSince.
	Pending bool

	LastAdvance time.Time
	LastError   string

	Running    *DeployRun
	Parked     *DeployPark
	LastResult *DeployResult
}

type DeployRun struct {
	RunID       string
	DeploySHA   string
	DeployedSHA string
	StartedAt   time.Time
	Nodes       []string
}

type DeployPark struct {
	SHA     string
	RunID   string
	Outcome string
	Detail  string
	At      time.Time
}

type DeployResult struct {
	RunID       string
	DeploySHA   string
	DeployedSHA string
	Outcome     string
	Culprit     string
	Detail      string
	StartedAt   time.Time
	EndedAt     time.Time
}

// recentDeploysLimit mirrors dashboard/api.go's own limit.
const recentDeploysLimit = 20

// --- deploys ------------------------------------------------------------------

type deploysIn struct {
	Env string `json:"env,omitempty" jsonschema:"limit the result to one environment by name; omit for every environment"`
}

type deploysOut struct {
	SnapshotAt string          `json:"snapshotAt"`
	Lanes      []deployLaneOut `json:"lanes"`
	Recent     []deploySummary `json:"recent,omitempty"`
}

type deployLaneOut struct {
	Env       string `json:"env"`
	Mode      string `json:"mode"`
	Source    string `json:"source"`
	SourceTip string `json:"sourceTip,omitempty"`
	Desired   string `json:"desired,omitempty"`
	Observed  string `json:"observed,omitempty"`
	InSync    bool   `json:"inSync"`
	Drift     bool   `json:"drift"`
	Pending   bool   `json:"pending"`
	// State folds the flags above into one word — parked, deploying,
	// pending, in sync, waiting, never deployed — in that priority.
	State       string           `json:"state"`
	LastAdvance string           `json:"lastAdvance,omitempty"`
	LastError   string           `json:"lastError,omitempty"`
	Running     *deployRunOut    `json:"running,omitempty"`
	Parked      *deployParkOut   `json:"parked,omitempty"`
	LastResult  *deployResultOut `json:"lastResult,omitempty"`
}

type deployRunOut struct {
	RunID       string   `json:"runID"`
	DeploySHA   string   `json:"deploySHA"`
	DeployedSHA string   `json:"deployedSHA,omitempty"`
	StartedAt   string   `json:"startedAt"`
	Nodes       []string `json:"nodes"`
}

type deployParkOut struct {
	SHA     string `json:"sha"`
	RunID   string `json:"runID,omitempty"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	At      string `json:"at"`
}

type deployResultOut struct {
	RunID       string `json:"runID"`
	DeploySHA   string `json:"deploySHA"`
	DeployedSHA string `json:"deployedSHA,omitempty"`
	Outcome     string `json:"outcome"`
	Culprit     string `json:"culprit,omitempty"`
	Detail      string `json:"detail,omitempty"`
	StartedAt   string `json:"startedAt"`
	EndedAt     string `json:"endedAt"`
}

type deploySummary struct {
	RunID       string `json:"runID"`
	Env         string `json:"env"`
	DeploySHA   string `json:"deploySHA"`
	DeployedSHA string `json:"deployedSHA,omitempty"`
	Outcome     string `json:"outcome"`
	Culprit     string `json:"culprit,omitempty"`
	Detail      string `json:"detail,omitempty"`
	StartedAt   string `json:"startedAt"`
	EndedAt     string `json:"endedAt"`
	DurationMs  int64  `json:"durationMs"`
}

func handleDeploys(p Params, in deploysIn) (deploysOut, error) {
	snap, err := deploySnapshot(p)
	if err != nil {
		return deploysOut{}, err
	}

	out := deploysOut{SnapshotAt: formatRFC3339(snap.At), Lanes: []deployLaneOut{}}
	for _, lane := range snap.Lanes {
		if in.Env != "" && lane.Env != in.Env {
			continue
		}
		out.Lanes = append(out.Lanes, buildDeployLaneOut(lane))
	}
	if p.Store != nil {
		// in.Env narrows the recent list too, unlike handleStatus's target
		// filter: RecentDeploys is already env-indexed, so filtering here
		// costs nothing and answers the question the caller actually asked.
		rows, err := p.Store.RecentDeploys(in.Env, recentDeploysLimit)
		if err != nil {
			return deploysOut{}, fmt.Errorf("recent deploys: %w", err)
		}
		out.Recent = make([]deploySummary, 0, len(rows))
		for _, row := range rows {
			out.Recent = append(out.Recent, deployRowToSummary(row))
		}
	}
	return out, nil
}

// deploySnapshot resolves the live deploy snapshot, returning the same two
// "absent" errors the HTTP surface's 503s carry.
func deploySnapshot(p Params) (*DeployStatus, error) {
	if p.DeploySnapshot == nil {
		return nil, errors.New("deploy not configured")
	}
	snap := p.DeploySnapshot()
	if snap == nil {
		return nil, errors.New("no snapshot yet")
	}
	return snap, nil
}

func buildDeployLaneOut(lane DeployLane) deployLaneOut {
	out := deployLaneOut{
		Env: lane.Env, Mode: lane.Mode, Source: lane.Source,
		SourceTip: lane.SourceTip, Desired: lane.Desired, Observed: lane.Observed,
		InSync: lane.InSync, Drift: lane.Drift, Pending: lane.Pending,
		State:     deployLaneState(lane),
		LastError: lane.LastError,
	}
	if !lane.LastAdvance.IsZero() {
		out.LastAdvance = formatRFC3339(lane.LastAdvance)
	}
	if r := lane.Running; r != nil {
		out.Running = &deployRunOut{
			RunID: r.RunID, DeploySHA: r.DeploySHA, DeployedSHA: r.DeployedSHA,
			StartedAt: formatRFC3339(r.StartedAt), Nodes: append([]string{}, r.Nodes...),
		}
	}
	if pk := lane.Parked; pk != nil {
		out.Parked = &deployParkOut{
			SHA: pk.SHA, RunID: pk.RunID, Outcome: pk.Outcome, Detail: pk.Detail,
			At: formatRFC3339(pk.At),
		}
	}
	if lr := lane.LastResult; lr != nil {
		out.LastResult = &deployResultOut{
			RunID: lr.RunID, DeploySHA: lr.DeploySHA, DeployedSHA: lr.DeployedSHA,
			Outcome: lr.Outcome, Culprit: lr.Culprit, Detail: lr.Detail,
			StartedAt: formatRFC3339(lr.StartedAt), EndedAt: formatRFC3339(lr.EndedAt),
		}
	}
	return out
}

// deployLaneState mirrors dashboard/api.go's laneState word-for-word and in
// the same priority order, so the two surfaces can never disagree about what
// state a lane is in.
func deployLaneState(lane DeployLane) string {
	switch {
	case lane.Parked != nil:
		return "parked"
	case lane.Running != nil:
		return "deploying"
	case lane.Pending:
		return "pending"
	case lane.InSync:
		return "in sync"
	case lane.Desired == "":
		return "never deployed"
	default:
		return "waiting"
	}
}

func deployRowToSummary(row history.DeployRow) deploySummary {
	return deploySummary{
		RunID: row.RunID, Env: row.Env,
		DeploySHA: row.DeploySHA, DeployedSHA: row.DeployedSHA,
		Outcome: row.Outcome, Culprit: row.Culprit, Detail: row.Detail,
		StartedAt: formatRFC3339(row.StartedAt), EndedAt: formatRFC3339(row.EndedAt),
		DurationMs: row.Duration.Milliseconds(),
	}
}

// --- deploy -------------------------------------------------------------------

type deployIn struct {
	RunID string `json:"run_id" jsonschema:"the deploy run ID to fetch full detail for"`
}

type deployOut struct {
	RunID       string        `json:"runID"`
	Env         string        `json:"env"`
	DeploySHA   string        `json:"deploySHA"`
	DeployedSHA string        `json:"deployedSHA,omitempty"`
	Outcome     string        `json:"outcome"`
	Culprit     string        `json:"culprit,omitempty"`
	Detail      string        `json:"detail,omitempty"`
	StartedAt   string        `json:"startedAt"`
	EndedAt     string        `json:"endedAt"`
	DurationMs  int64         `json:"durationMs"`
	Nodes       []checkDetail `json:"nodes"`
}

// handleDeploy is history-backed only, exactly like the JSON API's
// /api/v1/deploy/{id}: a run still in flight has no record yet, and the
// deploys tool's own `running` block is the live view of it.
func handleDeploy(p Params, in deployIn) (deployOut, error) {
	if p.Store == nil {
		return deployOut{}, errors.New("history disabled")
	}
	if in.RunID == "" {
		return deployOut{}, errors.New("run_id is required")
	}

	row, nodes, err := p.Store.Deploy(in.RunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return deployOut{}, fmt.Errorf("deploy not found: %s", in.RunID)
		}
		return deployOut{}, fmt.Errorf("deploy %s: %w", in.RunID, err)
	}

	out := deployOut{
		RunID: row.RunID, Env: row.Env,
		DeploySHA: row.DeploySHA, DeployedSHA: row.DeployedSHA,
		Outcome: row.Outcome, Culprit: row.Culprit, Detail: row.Detail,
		StartedAt:  formatRFC3339(row.StartedAt),
		EndedAt:    formatRFC3339(row.EndedAt),
		DurationMs: row.Duration.Milliseconds(),
		Nodes:      make([]checkDetail, 0, len(nodes)),
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, checkDetail{
			Seq: n.Seq, Name: n.Name, Status: n.Status,
			DurationMs: n.Duration.Milliseconds(), Err: n.Err,
			Output:  n.Output,
			LogPath: n.LogPath,
			LogURL:  deployLogURL(p.LogRoot, row.RunID, n.Name, n.LogPath),

			PeakRSSBytes: n.PeakRSS,
			UserCPUMs:    n.UserCPU.Milliseconds(),
			SysCPUMs:     n.SysCPU.Milliseconds(),
		})
	}
	return out, nil
}

// deployLogURL mirrors runLogURL for a deploy node: the dashboard's own
// route for the full per-node log, served from the same HTTP server this
// handler is mounted on.
func deployLogURL(logRoot, runID, node, logPath string) string {
	if logRoot == "" || logPath == "" {
		return ""
	}
	return "/deploy/" + url.PathEscape(runID) + "/log/" + url.PathEscape(node)
}

// --- deploy_retry / deploy_cancel ---------------------------------------------

type deployActionIn struct {
	Env string `json:"env" jsonschema:"the environment whose lane to act on, e.g. prod"`
}

type deployActionOut struct {
	Status string `json:"status"` // "retried"/"cancelled", or "no-op"
}

func handleDeployRetry(p Params, in deployActionIn) (deployActionOut, error) {
	return deployAction(in.Env, p.DeployRetry, "retried")
}

func handleDeployCancel(p Params, in deployActionIn) (deployActionOut, error) {
	return deployAction(in.Env, p.DeployCancel, "cancelled")
}

// deployAction is the shared body of the two mutating deploy tools, matching
// handleDeployAction (dashboard/api.go) argument-check for argument-check.
func deployAction(env string, fn func(string) bool, done string) (deployActionOut, error) {
	if env == "" {
		return deployActionOut{}, errors.New("env is required")
	}
	if fn == nil {
		return deployActionOut{}, errors.New("deploy not configured")
	}
	if fn(env) {
		return deployActionOut{Status: done}, nil
	}
	return deployActionOut{Status: "no-op"}, nil
}
