// Package mcp exposes gauntlet's live queue state and run history to AI
// agents over the Model Context Protocol: nine tools — status, runs, run,
// retry, cancel, hook_cancel, batch, checks, services — mounted at /mcp on
// the daemon's existing dashboard HTTP port, right alongside the read-only
// HTML dashboard and its JSON API (internal/dashboard/api.go).
//
// This package deliberately does not import internal/dashboard: its Params
// are the same two read sources (a live queue.Snapshot and an optional
// *history.Store) plus three write paths (retry/cancel/hook_cancel funcs),
// wired independently in cmd/gauntlet/dashboard.go so an agent talking MCP
// and a human looking at the dashboard both feed off the same daemon state
// without either package depending on the other. The status tool's view structs
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

	// Cancel enqueues a cancel command for (Command.Target, Command.Ref)
	// (manual operator cancellation) and reports whether it was accepted,
	// same backpressure contract as Retry. Nil disables the cancel tool.
	Cancel func(core.Command) bool

	// HookCancel cancels a target's currently-running post-land hook
	// execution, if any (hooks.Runner.CancelCurrent, wired in nil-safely by
	// cmd/gauntlet when hooks are configured), and reports whether a
	// running landing was actually found and signalled. Nil disables the
	// hook_cancel tool.
	HookCancel func(target string) bool

	// Drain begins a graceful shutdown drain (issue #8, cmd/gauntlet's
	// beginDrain): stop admitting new candidates, finish the in-flight
	// set, then exit. The deadline argument (zero for none) forces the
	// immediate kill at that instant. Nil disables the drain tool.
	Drain func(deadline time.Time)

	// LogRoot mirrors dashboard.WithLogRoot: when non-empty, the run tool's
	// checks gain a logUrl pointing at the dashboard's
	// GET /run/{id}/log/{check} route (mounted on the same HTTP server this
	// MCP handler is, per cmd/gauntlet/dashboard.go) — a relative path works
	// identically here since an MCP client reaching this server can reach
	// that route too. Empty (the default) omits logUrl entirely, same as
	// the dashboard's own JSON API when WithLogRoot isn't used.
	LogRoot string

	// HookSnapshot mirrors dashboard.WithHookSnapshot (hooks.Runner.Snapshot,
	// wired nil-safely by cmd/gauntlet exactly like HookCancel): the status
	// tool's per-target liveHook field is simply omitted (ok=false) when this
	// is nil. See dashboard/api.go's LiveHook doc for why this package
	// defines its own local LiveHook rather than importing internal/hooks or
	// internal/dashboard (same "duplicated rather than imported" convention
	// as targetStatus and friends — see the package doc).
	HookSnapshot func(target string) (LiveHook, bool)

	// ServicesSnapshot mirrors dashboard.WithServicesSnapshot
	// (services.Pool.Snapshot, wired nil-safely by cmd/gauntlet exactly like
	// HookSnapshot): the services tool reports "services disabled" when this
	// is nil. See dashboard/api.go's ServiceStatus doc for why this package
	// defines its own local ServiceStatus/ServicesStatus rather than
	// importing internal/services or internal/dashboard.
	ServicesSnapshot func() ServicesStatus
}

// LiveHook mirrors hooks.LiveState (internal/hooks) / dashboard.LiveHook
// field-for-field, deliberately duplicated rather than imported (package
// doc's "status tool's view structs deliberately mirror api.go's").
type LiveHook struct {
	Target       string
	Running      bool
	CurrentHook  string
	HookIndex    int
	HookCount    int
	StartedAt    time.Time
	BacklogDepth int
}

// ServiceStatus mirrors services.InstanceStatus (internal/services) /
// dashboard.ServiceStatus field-for-field, deliberately duplicated rather
// than imported (package doc). Mode is already the string form
// (services.Mode.String()), same as dashboard.ServiceStatus — cmd/gauntlet's
// adapter converts it before crossing into either package.
type ServiceStatus struct {
	Service    string
	Image      string
	Key        string
	KeyHash12  string
	Mode       string
	Host, Port string
	CreatedAt  time.Time
	LastUsed   time.Time
	Refcount   int
	Hits       int
}

// ServicesStatus mirrors services.PoolStatus / dashboard.ServicesStatus
// field-for-field.
type ServicesStatus struct {
	MaxInstances int
	Pending      int
	Instances    []ServiceStatus
}

// New builds the MCP-over-Streamable-HTTP handler: one *sdkmcp.Server,
// reused across every session (ExampleStreamableHTTPHandler's pattern in
// the SDK), exposing status/runs/run/retry as typed tools.
func New(p Params) http.Handler {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "status",
		Description: "Live merge-queue status: per-target in-flight run, waiting queue (FIFO order), " +
			"and parked candidates, plus idleSince when the whole daemon (queue and hooks, every " +
			"target) has been idle since some instant. Mirrors GET /api/v1/status. Pass target to " +
			"filter which targets are displayed (idleSince always covers every target regardless).",
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

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "cancel",
		Description: "Cancel (target, ref): stop its in-flight run (parking the candidate at its current " +
			"SHA) or, if it's only queued, park it before it's ever picked up. Same effect as a Slack \":x:\" " +
			"reaction or POST /api/v1/cancel. A retry later clears the park.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in cancelIn) (*sdkmcp.CallToolResult, cancelOut, error) {
		out, err := handleCancel(p, in)
		return nil, out, err
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "hook_cancel",
		Description: "Cancel target's currently-running post-land hook execution, if any. Same effect as " +
			"POST /api/v1/hooks/cancel. Reports \"no-op\" (not an error) when nothing is running for that " +
			"target right now.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in hookCancelIn) (*sdkmcp.CallToolResult, hookCancelOut, error) {
		out, err := handleHookCancel(p, in)
		return nil, out, err
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "drain",
		Description: "Begin a graceful shutdown drain: stop admitting new candidates and extending " +
			"speculation, let the in-flight set finish (checks + one landing each), then the daemon exits. " +
			"Same effect as POST /api/v1/drain. Idempotent. Optional deadline (RFC3339) forces the " +
			"immediate kill at that instant. Poll the status tool's lifecycle field for the transition " +
			"to \"drained\".",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in drainIn) (*sdkmcp.CallToolResult, drainOut, error) {
		out, err := handleDrain(p, in)
		return nil, out, err
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "batch",
		Description: "Batch membership for a batch ID: every member run (runID, ref, position, outcome, sha). " +
			"Mirrors GET /api/v1/batch/{id}. Requires run history to be enabled.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in batchIn) (*sdkmcp.CallToolResult, batchOut, error) {
		out, err := handleBatch(p, in)
		return nil, out, err
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "checks",
		Description: "Per-check red-rate/duration stats plus the queue-depth series for one target over a " +
			"time window. Mirrors GET /api/v1/checks. Requires run history to be enabled.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in checksIn) (*sdkmcp.CallToolResult, checksOut, error) {
		out, err := handleChecks(p, in)
		return nil, out, err
	})

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "services",
		Description: "The shared-services pool: every live warm instance (name, image, endpoint, age, " +
			"last-used, refcount, cumulative reuse hits) plus the pool's own tuning knobs (max instances, " +
			"pending creates). Mirrors GET /api/v1/services. Use this to size idle-ttl/max-instances.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in servicesIn) (*sdkmcp.CallToolResult, servicesOut, error) {
		out, err := handleServices(p, in)
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

	// IgnoredRefs mirrors dashboard/api.go's statusResponse.IgnoredRefs: a
	// TOP-LEVEL, daemon-wide list, not per-target — an ignored ref's
	// defining property is that its target segment names no configured
	// target, so a per-configured-target list could never match anything.
	// Each entry's target field carries the unconfigured name for display.
	// Omitted when history is disabled.
	IgnoredRefs []ignoredRefStatus `json:"ignoredRefs,omitempty"`

	// IdleSince mirrors dashboard/api.go's statusResponse.IdleSince
	// field-for-field: the RFC3339 instant since which the WHOLE daemon has
	// been idle — every target's queue AND every target's post-land hooks.
	// Always computed over EVERY target regardless of in.Target's filter,
	// since the signal is daemon-wide by definition (an agent asking about
	// one target still gets the whole daemon's idle state, same as
	// dashboard.statusResponse.IdleSince isn't scoped by any target filter
	// either — this endpoint has none). Omitted whenever the daemon isn't
	// idle right now.
	IdleSince string `json:"idleSince,omitempty"`

	// Lifecycle mirrors dashboard/api.go's statusResponse lifecycle fields
	// (issue #8): running/draining/drained, plus the in-flight drain set
	// counts and the drain window. Lets an agent watch a graceful shutdown
	// progress.
	Lifecycle     string `json:"lifecycle"`
	ActiveRuns    int    `json:"activeRuns"`
	ActiveChecks  int    `json:"activeChecks"`
	DrainSince    string `json:"drainSince,omitempty"`
	DrainDeadline string `json:"drainDeadline,omitempty"`
}

// targetStatus, inFlightStatus, waitingStatus, and parkedStatus mirror
// dashboard/api.go's statusResponse family field-for-field (same lowerCamel
// JSON names), deliberately duplicated rather than imported — see the
// package doc.
type targetStatus struct {
	Name     string           `json:"name"`
	Branch   string           `json:"branch"`
	Tip      string           `json:"tip"`
	InFlight *inFlightStatus  `json:"inFlight"`
	Pipeline []pipelineStatus `json:"pipeline"`
	Waiting  []waitingStatus  `json:"waiting"`
	Parked   []parkedStatus   `json:"parked"`

	// LiveHook and HookRuns mirror dashboard/api.go's own targetStatus
	// additions field-for-field: live post-land hook progress
	// (Params.HookSnapshot) and the durable hook-run ledger (Store.
	// HookRunSummaries) — both omitted (not present, not just empty) when
	// their respective data source is unavailable. Ignored refs live at
	// statusOut's top level, not here — see that field's doc.
	LiveHook *liveHookStatus `json:"liveHook,omitempty"`
	HookRuns []hookRunStatus `json:"hookRuns,omitempty"`
}

type liveHookStatus struct {
	Running      bool   `json:"running"`
	CurrentHook  string `json:"currentHook"`
	HookIndex    int    `json:"hookIndex"`
	HookCount    int    `json:"hookCount"`
	StartedAt    string `json:"startedAt"`
	BacklogDepth int    `json:"backlogDepth"`
}

type hookRunStatus struct {
	RunID      string `json:"runID"`
	OwedCount  int    `json:"owedCount"`
	DoneCount  int    `json:"doneCount"`
	StartedAt  string `json:"startedAt"`
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skipReason,omitempty"`
	Incomplete bool   `json:"incomplete"`
}

// ignoredRefStatus mirrors dashboard/api.go's own: Target is the
// UNCONFIGURED target name the ref's segment named (the reason it was
// ignored), carried purely for display.
type ignoredRefStatus struct {
	At     string `json:"at"`
	Target string `json:"target"`
	Ref    string `json:"ref"`
	Detail string `json:"detail"`
}

type inFlightStatus struct {
	Ref          string `json:"ref"`
	SHA          string `json:"sha"`
	RunID        string `json:"runID"`
	CurrentCheck string `json:"currentCheck"`
	// RunningChecks is every check in flight right now, in spec order —
	// more than one only when the candidate's spec set max-parallel > 1.
	// CurrentCheck stays the spec-first entry for back-compat.
	RunningChecks []string `json:"runningChecks,omitempty"`
	StartedAt     string   `json:"startedAt"`
	ChecksDone    []string `json:"checksDone"`
}

// pipelineStatus, pipelineMemberStatus mirror dashboard/api.go's own
// pipelineStatus family field-for-field (same lowerCamel JSON names),
// deliberately duplicated rather than imported — see the package doc.
type pipelineStatus struct {
	Members      []pipelineMemberStatus `json:"members"`
	ChainTip     string                 `json:"chainTip"`
	Predicted    bool                   `json:"predicted"`
	BatchID      string                 `json:"batchId"`
	ChecksDone   []string               `json:"checksDone"`
	CurrentCheck string                 `json:"currentCheck"`
	// RunningChecks mirrors inFlightStatus's field of the same name.
	RunningChecks []string `json:"runningChecks,omitempty"`
	StartedAt     string   `json:"startedAt"`
}

type pipelineMemberStatus struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
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
	// RunID mirrors dashboard/api.go's own parkedStatus.RunID addition: the
	// run that parked this candidate, "" (omitted) when unknown.
	RunID string `json:"runId,omitempty"`
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
		out.Targets = append(out.Targets, buildTargetStatus(p, ts))
	}
	if p.Store != nil {
		if refs, err := p.Store.IgnoredRefs(ignoredRefsLimit); err == nil {
			out.IgnoredRefs = make([]ignoredRefStatus, 0, len(refs))
			for _, ir := range refs {
				out.IgnoredRefs = append(out.IgnoredRefs, ignoredRefStatus{
					At: formatRFC3339(ir.At), Target: ir.Target, Ref: ir.Ref, Detail: ir.Detail,
				})
			}
		}
	}
	if since := idleSince(p, snap); !since.IsZero() {
		out.IdleSince = formatRFC3339(since)
	}
	out.Lifecycle = string(snap.Lifecycle)
	if out.Lifecycle == "" {
		out.Lifecycle = string(queue.LifecycleRunning)
	}
	out.ActiveRuns = snap.ActiveRuns
	out.ActiveChecks = snap.ActiveChecks
	if !snap.DrainSince.IsZero() {
		out.DrainSince = formatRFC3339(snap.DrainSince)
	}
	if !snap.DrainDeadline.IsZero() {
		out.DrainDeadline = formatRFC3339(snap.DrainDeadline)
	}
	return out
}

// idleSince mirrors dashboard/api.go's dash.idleSince: composes queue
// idleness (snap.IdleSince) with hook idleness (p.HookSnapshot) into the
// one daemon-wide idle instant. Always evaluated
// over every target in snap, never a caller-filtered subset — handleStatus's
// in.Target only narrows what's DISPLAYED in out.Targets, not what decides
// daemon-wide idleness. p.HookSnapshot nil (hooks not configured) means
// queue idleness alone decides.
func idleSince(p Params, snap *queue.Snapshot) time.Time {
	if snap.IdleSince.IsZero() {
		return time.Time{}
	}
	if p.HookSnapshot != nil {
		for _, ts := range snap.Targets {
			if lh, ok := p.HookSnapshot(ts.Name); ok && (lh.Running || lh.BacklogDepth > 0) {
				return time.Time{}
			}
		}
	}
	return snap.IdleSince
}

// hookRunsLimit/ignoredRefsLimit mirror dashboard/api.go's own limits: a
// recent-activity glance per target, not a full history browse.
const (
	hookRunsLimit    = 10
	ignoredRefsLimit = 10
)

func buildTargetStatus(p Params, ts queue.TargetSnapshot) targetStatus {
	out := targetStatus{
		Name:     ts.Name,
		Branch:   ts.Branch,
		Tip:      ts.TargetTip,
		Pipeline: make([]pipelineStatus, 0, len(ts.Pipeline)),
		Waiting:  make([]waitingStatus, 0, len(ts.Waiting)),
		Parked:   make([]parkedStatus, 0, len(ts.Parked)),
	}
	if ts.InFlight != nil {
		out.InFlight = buildInFlightStatus(ts.InFlight)
	}
	for _, rs := range ts.Pipeline {
		out.Pipeline = append(out.Pipeline, buildPipelineStatus(rs))
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
			RunID: pe.RunID,
		})
	}

	if p.HookSnapshot != nil {
		if lh, ok := p.HookSnapshot(ts.Name); ok {
			out.LiveHook = &liveHookStatus{
				Running: lh.Running, CurrentHook: lh.CurrentHook,
				HookIndex: lh.HookIndex, HookCount: lh.HookCount,
				StartedAt: formatRFC3339(lh.StartedAt), BacklogDepth: lh.BacklogDepth,
			}
		}
	}
	if p.Store != nil {
		if runs, err := p.Store.HookRunSummaries(ts.Name, hookRunsLimit); err == nil {
			out.HookRuns = make([]hookRunStatus, 0, len(runs))
			for _, hr := range runs {
				out.HookRuns = append(out.HookRuns, hookRunStatus{
					RunID: hr.RunID, OwedCount: hr.OwedCount, DoneCount: hr.DoneCount,
					StartedAt: formatRFC3339(hr.StartedAt), Skipped: hr.Skipped, SkipReason: hr.SkipReason,
					Incomplete: hr.OwedCount > hr.DoneCount && !hr.Skipped,
				})
			}
		}
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
	for _, c := range rs.Running {
		v.RunningChecks = append(v.RunningChecks, c.Name)
	}
	return v
}

// buildPipelineStatus mirrors dashboard/api.go's buildPipelineStatus: one
// RunSnapshot in the status tool's additive "pipeline" array.
func buildPipelineStatus(rs queue.RunSnapshot) pipelineStatus {
	v := pipelineStatus{
		Members:    make([]pipelineMemberStatus, 0, len(rs.Members)),
		ChainTip:   rs.ChainTip,
		Predicted:  rs.Predicted,
		BatchID:    rs.BatchID,
		ChecksDone: make([]string, 0, len(rs.Done)),
		StartedAt:  formatRFC3339(rs.StartedAt),
	}
	for _, m := range rs.Members {
		v.Members = append(v.Members, pipelineMemberStatus{Ref: m.Ref, SHA: m.SHA})
	}
	for _, cr := range rs.Done {
		v.ChecksDone = append(v.ChecksDone, cr.Name)
	}
	if rs.Current != nil {
		v.CurrentCheck = rs.Current.Name
	}
	for _, c := range rs.Running {
		v.RunningChecks = append(v.RunningChecks, c.Name)
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

	// BatchID/Position/BatchSize surface batch identity as-is, mirroring
	// dashboard/api.go's runSummaryJSON: omitted entirely for a serial or
	// speculate run (BatchID == ""), present for a batch member. See that
	// type's doc for the omitempty/position-0 caveat (batchId's presence
	// is the real batch-membership signal).
	BatchID   string `json:"batchId,omitempty"`
	Position  int    `json:"position,omitempty"`
	BatchSize int    `json:"batchSize,omitempty"`
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
		rs := runSummary{
			RunID: row.RunID, Target: row.Target, Ref: row.CandidateRef,
			User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
			Outcome: row.Outcome, Detail: row.Detail,
			StartedAt:  formatRFC3339(row.StartedAt),
			EndedAt:    formatRFC3339(row.EndedAt),
			DurationMs: row.Duration.Milliseconds(),
		}
		if row.BatchID != "" {
			rs.BatchID, rs.Position, rs.BatchSize = row.BatchID, row.Position, row.BatchSize
		}
		out.Runs = append(out.Runs, rs)
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

	// BatchID/Position/BatchSize surface batch identity as-is, mirroring
	// dashboard/api.go's runDetailResponse: omitted entirely for a serial
	// or speculate run (BatchID == ""), present for a batch member. See
	// runSummary's doc for the omitempty/position-0 caveat.
	BatchID   string `json:"batchId,omitempty"`
	Position  int    `json:"position,omitempty"`
	BatchSize int    `json:"batchSize,omitempty"`

	// Speculated and Recovered mirror dashboard/api.go's runDetailResponse
	// additions field-for-field (core.RunRecord.Speculated/Recovered, v7+):
	// purely informational, omitted when false. Run-detail only — the runs
	// and batch tools don't carry these.
	Speculated bool `json:"speculated,omitempty"`
	Recovered  bool `json:"recovered,omitempty"`

	// ReceiptRef/ReceiptBlob/ReceiptPublished mirror dashboard/api.go's
	// runDetailResponse additions field-for-field (v12+, issue #13): the
	// receipt-notes publication provenance of a run whose note was
	// confirmed published (not landed-only — see that field's doc),
	// omitted when the run never published one.
	ReceiptRef       string `json:"receiptRef,omitempty"`
	ReceiptBlob      string `json:"receiptBlob,omitempty"`
	ReceiptPublished string `json:"receiptPublished,omitempty"`
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
		Speculated: row.Speculated,
		Recovered:  row.Recovered,

		ReceiptRef:       row.ReceiptRef,
		ReceiptBlob:      row.ReceiptBlob,
		ReceiptPublished: row.ReceiptPublished,
	}
	if row.BatchID != "" {
		out.BatchID, out.Position, out.BatchSize = row.BatchID, row.Position, row.BatchSize
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

// --- cancel -------------------------------------------------------------------

type cancelIn struct {
	Target string `json:"target" jsonschema:"the target the candidate is queued against"`
	Ref    string `json:"ref" jsonschema:"the candidate's ref, e.g. refs/heads/for/main/alice/topic"`
}

type cancelOut struct {
	Status string `json:"status"`
}

func handleCancel(p Params, in cancelIn) (cancelOut, error) {
	if in.Target == "" || in.Ref == "" {
		return cancelOut{}, errors.New("target and ref are required")
	}
	if p.Cancel == nil {
		return cancelOut{}, errors.New("cancel is disabled")
	}
	if !p.Cancel(core.Command{Kind: core.CommandCancel, Target: in.Target, Ref: in.Ref}) {
		return cancelOut{}, errors.New("cancel queue is full; try again")
	}
	return cancelOut{Status: "queued"}, nil
}

// --- hook_cancel ----------------------------------------------------------------

type hookCancelIn struct {
	Target string `json:"target" jsonschema:"the target whose currently-running post-land hooks should be cancelled"`
}

type hookCancelOut struct {
	Status string `json:"status"` // "cancelled" or "no-op"
}

func handleHookCancel(p Params, in hookCancelIn) (hookCancelOut, error) {
	if in.Target == "" {
		return hookCancelOut{}, errors.New("target is required")
	}
	if p.HookCancel == nil {
		return hookCancelOut{}, errors.New("hook cancel is disabled")
	}
	if p.HookCancel(in.Target) {
		return hookCancelOut{Status: "cancelled"}, nil
	}
	return hookCancelOut{Status: "no-op"}, nil
}

// --- drain ------------------------------------------------------------------

type drainIn struct {
	Deadline string `json:"deadline,omitempty" jsonschema:"optional RFC3339 instant to force the immediate kill if the drain has not finished by then"`
}

type drainOut struct {
	Status string `json:"status"` // "draining"
}

func handleDrain(p Params, in drainIn) (drainOut, error) {
	if p.Drain == nil {
		return drainOut{}, errors.New("drain is disabled")
	}
	var deadline time.Time
	if in.Deadline != "" {
		t, err := time.Parse(time.RFC3339, in.Deadline)
		if err != nil {
			return drainOut{}, errors.New("deadline must be an RFC3339 timestamp")
		}
		deadline = t
	}
	p.Drain(deadline)
	return drainOut{Status: "draining"}, nil
}

// --- batch ------------------------------------------------------------------

type batchIn struct {
	BatchID string `json:"batch_id" jsonschema:"the batch ID to list members for (see a run's batchId field)"`
}

// batchMember mirrors dashboard/api.go's batchMemberJSON field-for-field.
type batchMember struct {
	RunID      string `json:"runID"`
	Target     string `json:"target"`
	Position   int    `json:"position"`
	User       string `json:"user"`
	Topic      string `json:"topic"`
	SHA        string `json:"sha"`
	Outcome    string `json:"outcome"`
	Detail     string `json:"detail"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt"`
	DurationMs int64  `json:"durationMs"`
}

type batchOut struct {
	BatchID string        `json:"batchId"`
	Members []batchMember `json:"members"`
}

func handleBatch(p Params, in batchIn) (batchOut, error) {
	if p.Store == nil {
		return batchOut{}, errors.New("history disabled")
	}
	if in.BatchID == "" {
		return batchOut{}, errors.New("batch_id is required")
	}

	members, err := p.Store.BatchMembers(in.BatchID)
	if err != nil {
		return batchOut{}, fmt.Errorf("batch %s: %w", in.BatchID, err)
	}
	if len(members) == 0 {
		return batchOut{}, fmt.Errorf("batch not found: %s", in.BatchID)
	}

	out := batchOut{BatchID: in.BatchID, Members: make([]batchMember, 0, len(members))}
	for _, m := range members {
		out.Members = append(out.Members, batchMember{
			RunID: m.RunID, Target: m.Target, Position: m.Position,
			User: m.CandidateUser, Topic: m.CandidateTopic, SHA: m.CandidateSHA,
			Outcome: m.Outcome, Detail: m.Detail,
			StartedAt:  formatRFC3339(m.StartedAt),
			EndedAt:    formatRFC3339(m.EndedAt),
			DurationMs: m.Duration.Milliseconds(),
		})
	}
	return out, nil
}

// --- checks -------------------------------------------------------------------

type checksIn struct {
	Target string `json:"target" jsonschema:"the target name to compute check stats/depth for"`
	Since  string `json:"since,omitempty" jsonschema:"a Go duration (e.g. '24h') or an RFC3339 timestamp; defaults to 24h"`
}

// checkStat mirrors dashboard/api.go's checkStatJSON field-for-field.
type checkStat struct {
	Name          string  `json:"name"`
	Total         int     `json:"total"`
	Failed        int     `json:"failed"`
	RedRate       float64 `json:"redRate"`
	AvgDurationMs int64   `json:"avgDurationMs"`
	MaxDurationMs int64   `json:"maxDurationMs"`
}

// depthPoint mirrors dashboard/api.go's depthPointJSON field-for-field.
type depthPoint struct {
	At       string `json:"at"`
	Waiting  int    `json:"waiting"`
	InFlight int    `json:"inFlight"`
	Parked   int    `json:"parked"`
}

type checksOut struct {
	Target string       `json:"target"`
	Since  string       `json:"since"`
	Stats  []checkStat  `json:"stats"`
	Depth  []depthPoint `json:"depth"`
}

func handleChecks(p Params, in checksIn) (checksOut, error) {
	if p.Store == nil {
		return checksOut{}, errors.New("history disabled")
	}
	if in.Target == "" {
		return checksOut{}, errors.New("target is required")
	}

	now := time.Now()
	since := parseSince(in.Since, now)

	stats, err := p.Store.CheckStats(in.Target, since)
	if err != nil {
		return checksOut{}, fmt.Errorf("check stats %s: %w", in.Target, err)
	}
	depth, err := p.Store.DepthSeries(in.Target, since)
	if err != nil {
		return checksOut{}, fmt.Errorf("depth series %s: %w", in.Target, err)
	}

	out := checksOut{
		Target: in.Target, Since: formatRFC3339(since),
		Stats: make([]checkStat, 0, len(stats)),
		Depth: make([]depthPoint, 0, len(depth)),
	}
	for _, st := range stats {
		out.Stats = append(out.Stats, checkStat{
			Name: st.Name, Total: st.Total, Failed: st.Failed, RedRate: st.RedRate,
			AvgDurationMs: st.AvgDuration.Milliseconds(), MaxDurationMs: st.MaxDuration.Milliseconds(),
		})
	}
	for _, dp := range depth {
		out.Depth = append(out.Depth, depthPoint{At: formatRFC3339(dp.At), Waiting: dp.Waiting, InFlight: dp.InFlight, Parked: dp.Parked})
	}
	return out, nil
}

// --- services -------------------------------------------------------------------

type servicesIn struct{}

// serviceInstance mirrors dashboard/api.go's serviceInstanceJSON
// field-for-field. Key carries the full key (see docs/design/services.md,
// "Full key versus name" — only the full key is guaranteed collision-free);
// KeyHash12 is the same truncation the dashboard HTML table shows for
// compact display.
type serviceInstance struct {
	Service   string `json:"service"`
	Image     string `json:"image"`
	Key       string `json:"key"`
	KeyHash12 string `json:"keyHash12"`
	Mode      string `json:"mode"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsed"`
	Refcount  int    `json:"refcount"`
	Hits      int    `json:"hits"`
}

type servicesOut struct {
	MaxInstances int               `json:"maxInstances"`
	Pending      int               `json:"pending"`
	Instances    []serviceInstance `json:"instances"`
}

// handleServices mirrors dashboard/api.go's handleAPIServices: an error
// ("services disabled") only when Params.ServicesSnapshot was never wired up
// (no daemon-level services block configured) — an empty pool is a normal
// result, not an error, same as every other tool's "disabled" convention.
func handleServices(p Params, _ servicesIn) (servicesOut, error) {
	if p.ServicesSnapshot == nil {
		return servicesOut{}, errors.New("services disabled")
	}

	ss := p.ServicesSnapshot()
	out := servicesOut{
		MaxInstances: ss.MaxInstances, Pending: ss.Pending,
		Instances: make([]serviceInstance, 0, len(ss.Instances)),
	}
	for _, inst := range ss.Instances {
		out.Instances = append(out.Instances, serviceInstance{
			Service: inst.Service, Image: inst.Image, Key: inst.Key, KeyHash12: inst.KeyHash12,
			Mode: inst.Mode, Host: inst.Host, Port: inst.Port,
			CreatedAt: formatRFC3339(inst.CreatedAt), LastUsed: formatRFC3339(inst.LastUsed),
			Refcount: inst.Refcount, Hits: inst.Hits,
		})
	}
	return out, nil
}

// parseSince mirrors dashboard/api.go's parseSince (server.go): a Go
// duration ("24h") is relative to now; an RFC3339 timestamp is absolute;
// anything else (including empty) falls back to a 24h window. Duplicated
// rather than imported, matching this package's own convention.
func parseSince(s string, now time.Time) time.Time {
	def := now.Add(-24 * time.Hour)
	if s == "" {
		return def
	}
	if dur, err := time.ParseDuration(s); err == nil {
		return now.Add(-dur)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
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
