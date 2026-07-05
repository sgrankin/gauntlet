// Package mcp exposes gauntlet's live queue state and run history to AI
// agents over the Model Context Protocol (chunk E5): four tools — status,
// runs, run, retry — mounted at /mcp on the daemon's existing dashboard HTTP
// port, right alongside the read-only HTML dashboard and its JSON API
// (internal/dashboard/api.go, chunk E4).
//
// This package deliberately does not import internal/dashboard: its Params
// are the same two read sources (a live queue.Snapshot and an optional
// *history.Store) plus one write path (a retry func), wired independently
// in cmd/gauntlet/dashboard.go so an agent talking MCP and a human looking
// at the dashboard both feed off the same daemon state without either
// package depending on the other. The status tool's view structs
// deliberately mirror api.go's JSON field names (lowerCamel) so an agent and
// a script hitting the HTTP API see the same vocabulary for the same data —
// see api.go's statusResponse/targetStatus family, duplicated here rather
// than imported.
//
// New returns a plain http.Handler (the SDK's Streamable HTTP transport)
// backed by one long-lived *mcp.Server; every tool call reads Params.
// Snapshot/Params.Store fresh, exactly like every dashboard HTTP handler, so
// there is no per-session state to keep in sync.
package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// serverName and serverVersion identify this server to connecting MCP
// clients (the "initialize" response's serverInfo). serverVersion is a
// small internal counter, not gauntlet's release version: bump it when a
// tool's shape changes in a way a client might care about.
const (
	serverName    = "gauntlet"
	serverVersion = "0.1.0"
)

// defaultRunsLimit mirrors dashboard/api.go's defaultRunsLimit: the runs
// tool's limit when the caller doesn't specify one.
const defaultRunsLimit = 20

// Params configures New. Snapshot must be non-nil (mirroring dashboard.New's
// snapshot param); Store and Retry may be nil, in which case the runs/run
// tools report "history disabled" and the retry tool reports an error
// rather than panicking.
type Params struct {
	// Snapshot returns the live queue state, or nil if no reconcile pass has
	// completed yet (typically Daemon.Snapshot).
	Snapshot func() *queue.Snapshot

	// Store serves run history for the runs and run tools. Nil disables
	// both, matching dashboard.New's history-disabled degradation.
	Store *history.Store

	// Retry enqueues a retry command for (Command.Target, Command.Ref) and
	// reports whether it was accepted (false = backpressure, e.g. a full
	// buffer — never blocks). Nil disables the retry tool.
	Retry func(core.Command) bool

	// LogRoot mirrors dashboard.WithLogRoot: when non-empty, the run tool's
	// checks gain a logUrl pointing at the dashboard's
	// GET /run/{id}/log/{check} route (mounted on the same HTTP server this
	// MCP handler is, per cmd/gauntlet/dashboard.go) — a relative path works
	// identically here since an MCP client reaching this server can reach
	// that route too. Empty (the default) omits logUrl entirely, same as
	// the dashboard's own JSON API when WithLogRoot isn't used.
	LogRoot string
}

// New builds the MCP-over-Streamable-HTTP handler: one *sdkmcp.Server,
// reused across every session (ExampleStreamableHTTPHandler's pattern in
// the SDK), exposing status/runs/run/retry as typed tools.
func New(p Params) http.Handler {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "status",
		Description: "Live merge-queue status: per-target in-flight run, waiting queue (FIFO order), " +
			"and parked candidates. Mirrors GET /api/v1/status. Pass target to filter to one target.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in statusIn) (*sdkmcp.CallToolResult, statusOut, error) {
		return nil, handleStatus(p, in), nil
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name:        "runs",
		Description: "Recent run history for one target, newest first. Requires run history to be enabled.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in runsIn) (*sdkmcp.CallToolResult, runsOut, error) {
		out, err := handleRuns(p, in)
		return nil, out, err
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "run",
		Description: "Full detail for one run by run ID, including every check's status, duration, and " +
			"captured output — use this to debug why a run went red. Requires run history to be enabled.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in runIn) (*sdkmcp.CallToolResult, runOut, error) {
		out, err := handleRun(p, in)
		return nil, out, err
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "retry",
		Description: "Clear the park on (target, ref) so the next reconcile pass re-tests it, the same " +
			"effect as a Slack \":recycle:\" reaction or POST /api/v1/retry.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in retryIn) (*sdkmcp.CallToolResult, retryOut, error) {
		out, err := handleRetry(p, in)
		return nil, out, err
	})

	return sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
}

// --- status -------------------------------------------------------------

type statusIn struct {
	Target string `json:"target,omitempty" jsonschema:"limit the result to one target by name; omit for every target"`
}

type statusOut struct {
	SnapshotAt string         `json:"snapshotAt"`
	Targets    []targetStatus `json:"targets"`
}

// targetStatus, inFlightStatus, waitingStatus, and parkedStatus mirror
// dashboard/api.go's statusResponse family field-for-field (same lowerCamel
// JSON names), deliberately duplicated rather than imported — see the
// package doc.
type targetStatus struct {
	Name     string          `json:"name"`
	Branch   string          `json:"branch"`
	Tip      string          `json:"tip"`
	InFlight *inFlightStatus `json:"inFlight"`
	Waiting  []waitingStatus `json:"waiting"`
	Parked   []parkedStatus  `json:"parked"`
}

type inFlightStatus struct {
	Ref          string   `json:"ref"`
	SHA          string   `json:"sha"`
	RunID        string   `json:"runID"`
	CurrentCheck string   `json:"currentCheck"`
	StartedAt    string   `json:"startedAt"`
	ChecksDone   []string `json:"checksDone"`
}

type waitingStatus struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
	Seq int64  `json:"seq"`
}

type parkedStatus struct {
	Ref     string `json:"ref"`
	SHA     string `json:"sha"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
}

// handleStatus never returns an error: an absent snapshot or an unknown
// target both render as an empty/explanatory result rather than a tool
// error, since "queue hasn't started yet" and "no such target" are normal,
// expected states an agent should be able to read without a failed call.
func handleStatus(p Params, in statusIn) statusOut {
	snap := p.Snapshot()
	if snap == nil {
		return statusOut{}
	}

	out := statusOut{SnapshotAt: formatRFC3339(snap.At), Targets: make([]targetStatus, 0, len(snap.Targets))}
	for _, ts := range snap.Targets {
		if in.Target != "" && ts.Name != in.Target {
			continue
		}
		out.Targets = append(out.Targets, buildTargetStatus(ts))
	}
	return out
}

func buildTargetStatus(ts queue.TargetSnapshot) targetStatus {
	out := targetStatus{
		Name:    ts.Name,
		Branch:  ts.Branch,
		Tip:     ts.TargetTip,
		Waiting: make([]waitingStatus, 0, len(ts.Waiting)),
		Parked:  make([]parkedStatus, 0, len(ts.Parked)),
	}
	if ts.InFlight != nil {
		out.InFlight = buildInFlightStatus(ts.InFlight)
	}

	// Defensive re-sort by Seq, matching api.go's buildTargetStatus: this is
	// already FIFO order out of buildTargetSnapshot, but shouldn't rest on
	// that internal detail.
	waiting := append([]queue.WaitingEntry(nil), ts.Waiting...)
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].Seq < waiting[j].Seq })
	for _, we := range waiting {
		out.Waiting = append(out.Waiting, waitingStatus{Ref: we.Candidate.Ref, SHA: we.Candidate.SHA, Seq: we.Seq})
	}

	for _, pe := range ts.Parked {
		out.Parked = append(out.Parked, parkedStatus{
			Ref: pe.Candidate.Ref, SHA: pe.Candidate.SHA,
			Outcome: outcomeWord(pe.Outcome), Reason: pe.Reason, At: formatRFC3339(pe.At),
		})
	}
	return out
}

func buildInFlightStatus(rs *queue.RunSnapshot) *inFlightStatus {
	v := &inFlightStatus{
		Ref:        rs.Candidate.Ref,
		SHA:        rs.Candidate.SHA,
		RunID:      rs.RunID,
		StartedAt:  formatRFC3339(rs.StartedAt),
		ChecksDone: make([]string, 0, len(rs.Done)),
	}
	for _, cr := range rs.Done {
		v.ChecksDone = append(v.ChecksDone, cr.Name)
	}
	if rs.Current != nil {
		v.CurrentCheck = rs.Current.Name
	}
	return v
}

// --- runs -----------------------------------------------------------------

type runsIn struct {
	Target string `json:"target" jsonschema:"the target name to list runs for"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max number of runs to return, newest first (default 20)"`
}

type runsOut struct {
	Runs []runSummary `json:"runs"`
}

type runSummary struct {
	RunID      string `json:"runID"`
	Target     string `json:"target"`
	Ref        string `json:"ref"`
	User       string `json:"user"`
	Topic      string `json:"topic"`
	SHA        string `json:"sha"`
	Outcome    string `json:"outcome"`
	Detail     string `json:"detail"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt"`
	DurationMs int64  `json:"durationMs"`
}

func handleRuns(p Params, in runsIn) (runsOut, error) {
	if p.Store == nil {
		return runsOut{}, errors.New("history disabled")
	}
	if in.Target == "" {
		return runsOut{}, errors.New("target is required")
	}

	limit := defaultRunsLimit
	if in.Limit > 0 {
		limit = in.Limit
	}

	rows, err := p.Store.RecentRuns(in.Target, limit)
	if err != nil {
		return runsOut{}, fmt.Errorf("recent runs %s: %w", in.Target, err)
	}

	out := runsOut{Runs: make([]runSummary, 0, len(rows))}
	for _, row := range rows {
		out.Runs = append(out.Runs, runSummary{
			RunID: row.RunID, Target: row.Target, Ref: row.CandidateRef,
			User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
			Outcome: row.Outcome, Detail: row.Detail,
			StartedAt:  formatRFC3339(row.StartedAt),
			EndedAt:    formatRFC3339(row.EndedAt),
			DurationMs: row.Duration.Milliseconds(),
		})
	}
	return out, nil
}

// --- run --------------------------------------------------------------------

type runIn struct {
	RunID string `json:"run_id" jsonschema:"the run ID to fetch full detail for"`
}

type runOut struct {
	RunID      string        `json:"runID"`
	Target     string        `json:"target"`
	Ref        string        `json:"ref"`
	User       string        `json:"user"`
	Topic      string        `json:"topic"`
	SHA        string        `json:"sha"`
	BaseOID    string        `json:"baseOID"`
	MergeSHA   string        `json:"mergeSHA"`
	TrialClean bool          `json:"trialClean"`
	Outcome    string        `json:"outcome"`
	Detail     string        `json:"detail"`
	StartedAt  string        `json:"startedAt"`
	EndedAt    string        `json:"endedAt"`
	DurationMs int64         `json:"durationMs"`
	Checks     []checkDetail `json:"checks"`
	// Hooks holds this run's post-land hook results (internal/hooks), same
	// shape as Checks (including captured Output — an agent debugging a
	// failed deploy hook needs it exactly as much as a failed check's).
	// Always present as an array (possibly empty, never omitted).
	Hooks []checkDetail `json:"hooks"`
}

// checkDetail is api.go's checkJSON plus Output: an agent debugging a red
// run needs the check's captured output, which the JSON API omits (it's
// meant for a human clicking into the dashboard's run page; the run page
// doesn't render output either — this tool is the one place that surfaces
// it).
type checkDetail struct {
	Seq        int    `json:"seq"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs"`
	Err        string `json:"err"`
	Output     string `json:"output"`
	// LogPath is the check's full per-check log file path on disk (empty if
	// none was written), verbatim from history.CheckRow.LogPath.
	LogPath string `json:"logPath"`
	// LogURL is the dashboard's relative link to that file
	// (GET /run/{id}/log/{name}), present only when Params.LogRoot is set
	// and LogPath is non-empty — see Params.LogRoot's doc.
	LogURL string `json:"logUrl,omitempty"`
}

func handleRun(p Params, in runIn) (runOut, error) {
	if p.Store == nil {
		return runOut{}, errors.New("history disabled")
	}
	if in.RunID == "" {
		return runOut{}, errors.New("run_id is required")
	}

	row, checks, err := p.Store.Run(in.RunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runOut{}, fmt.Errorf("run not found: %s", in.RunID)
		}
		return runOut{}, fmt.Errorf("run %s: %w", in.RunID, err)
	}

	out := runOut{
		RunID: row.RunID, Target: row.Target, Ref: row.CandidateRef,
		User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
		BaseOID: row.BaseOID, MergeSHA: row.MergeSHA, TrialClean: row.TrialClean,
		Outcome: row.Outcome, Detail: row.Detail,
		StartedAt:  formatRFC3339(row.StartedAt),
		EndedAt:    formatRFC3339(row.EndedAt),
		DurationMs: row.Duration.Milliseconds(),
		Checks:     make([]checkDetail, 0, len(checks)),
	}
	for _, c := range checks {
		out.Checks = append(out.Checks, checkDetail{
			Seq: c.Seq, Name: c.Name, Status: c.Status,
			DurationMs: c.Duration.Milliseconds(), Err: c.Err, Output: c.Output,
			LogPath: c.LogPath,
			LogURL:  runLogURL(p.LogRoot, in.RunID, c.Name, c.LogPath),
		})
	}

	out.Hooks = make([]checkDetail, 0)
	hooks, err := p.Store.Hooks(in.RunID)
	if err != nil {
		return runOut{}, fmt.Errorf("run %s hooks: %w", in.RunID, err)
	}
	for _, h := range hooks {
		out.Hooks = append(out.Hooks, checkDetail{
			Seq: h.Seq, Name: h.Name, Status: h.Status,
			DurationMs: h.Duration.Milliseconds(), Err: h.Err, Output: h.Output,
			LogPath: h.LogPath,
			LogURL:  runLogURL(p.LogRoot, in.RunID, h.Name, h.LogPath),
		})
	}
	return out, nil
}

// runLogURL mirrors dashboard's runLogURL: it returns the dashboard's
// relative log-serving link for one check, or "" when there's nothing
// meaningful to link (no log file was written, or LogRoot isn't
// configured, in which case the dashboard's handler would 404 anyway).
func runLogURL(logRoot, runID, checkName, logPath string) string {
	if logRoot == "" || logPath == "" {
		return ""
	}
	return "/run/" + url.PathEscape(runID) + "/log/" + url.PathEscape(checkName)
}

// --- retry --------------------------------------------------------------------

type retryIn struct {
	Target string `json:"target" jsonschema:"the target the candidate is queued against"`
	Ref    string `json:"ref" jsonschema:"the candidate's ref, e.g. refs/heads/for/main/alice/feat-a"`
}

type retryOut struct {
	Status string `json:"status"`
}

func handleRetry(p Params, in retryIn) (retryOut, error) {
	if in.Target == "" || in.Ref == "" {
		return retryOut{}, errors.New("target and ref are required")
	}
	if p.Retry == nil {
		return retryOut{}, errors.New("retry is disabled")
	}
	if !p.Retry(core.Command{Kind: core.CommandRetry, Target: in.Target, Ref: in.Ref}) {
		return retryOut{}, errors.New("retry queue is full; try again")
	}
	return retryOut{Status: "queued"}, nil
}

// --- shared -------------------------------------------------------------------

func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func outcomeWord(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "landed"
	case core.OutcomeRejected:
		return "rejected"
	case core.OutcomeConflict:
		return "conflict"
	case core.OutcomeSkipped:
		return "skipped"
	case core.OutcomeError:
		return "error"
	default:
		return "unknown"
	}
}
