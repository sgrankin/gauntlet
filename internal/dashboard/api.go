package dashboard

// This file adds a JSON API alongside the HTML dashboard in server.go:
// GET /api/v1/status mirrors the live
// queue.Snapshot, GET /api/v1/runs and /api/v1/run/{id} mirror the history
// views, and POST /api/v1/retry lets a script (or a future `gauntlet mcp`)
// inject a core.CommandRetry the same way a Slack ":recycle:" reaction does.
//
// That last route is the one mutating thing this package does — everything
// else here, like every HTML route in server.go, only reads. See Channel's
// doc for how the mutation is wired: the dashboard now optionally
// implements core.Channel on its *inbound* (Commands) side, but never
// consumes Events on its outbound side.
//
// Every response is JSON with stable lowerCamel field names and
// "Content-Type: application/json". Errors are always `{"error": "..."}`:
// 503 when a data source that's allowed to be absent (no snapshot yet, or
// history disabled) is absent, 404 when a specific resource (a run) doesn't
// exist, 400 for a malformed request, 405 for a disallowed method.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// cmdsBuffer bounds a Channel's inbound retry-command queue, mirroring
// slack.Slack's cmdsBuffer (internal/slack/slack.go): generous for a daemon
// that drains every channel's Commands() once per reconcile tick.
const cmdsBuffer = 64

// Channel is the dashboard's core.Channel: a core.Command buffer that POST
// /api/v1/retry feeds (once wired into a handler via WithChannel) and whose
// Commands() the queue drains like any other channel (Invariant 8).
//
// It is constructed independently of the http.Handler because of a wiring
// order constraint in cmd/gauntlet: the channel list passed to queue.New
// must include this Channel *before* queue.New returns, but the dashboard's
// http.Handler needs Daemon.Snapshot, which only exists *after* queue.New
// returns. NewChannel gives cmd/gauntlet something to register early;
// WithChannel wires the handler, built later, to feed it.
//
// Emit is a documented no-op: the dashboard is pull-only (every HTTP
// handler reads snapshot()/store fresh on each request) and never consumes
// events. Only the Commands() side is real.
type Channel struct {
	cmds chan core.Command
}

var _ core.Channel = (*Channel)(nil)

// NewChannel returns a fresh, unwired command buffer. Register it in the
// channel list passed to queue.New so the queue drains it, and pass it to
// WithChannel when later constructing the dashboard's http.Handler so POST
// /api/v1/retry actually feeds it.
func NewChannel() *Channel {
	return &Channel{cmds: make(chan core.Command, cmdsBuffer)}
}

// Emit is a no-op: see Channel's doc. The error return exists only to
// satisfy core.Channel.
func (c *Channel) Emit(ctx context.Context, ev core.Event) error { return nil }

// Commands yields core.Command values enqueued by POST /api/v1/retry on a
// handler built with WithChannel(c).
func (c *Channel) Commands() <-chan core.Command { return c.cmds }

// TrySend attempts to enqueue cmd onto c's inbound buffer, reporting
// whether it was accepted. It never blocks: a full buffer reports false
// rather than waiting for the queue to drain. Exported so internal/mcp's
// retry tool can feed the same channel POST /api/v1/retry does and
// distinguish "queued" from "dropped, buffer full" the way an HTTP
// response code lets api.go's caller do.
func (c *Channel) TrySend(cmd core.Command) bool {
	select {
	case c.cmds <- cmd:
		return true
	default:
		return false
	}
}

// enqueue sends cmd, dropping (and logging) rather than blocking if the
// buffer is full — never let a slow/stalled queue block an HTTP handler,
// mirroring slack.Slack.Emit's full-outbox handling.
func (c *Channel) enqueue(cmd core.Command) {
	if !c.TrySend(cmd) {
		log.Printf("dashboard: retry: cmds buffer full (%d), dropping target=%s ref=%s", cmdsBuffer, cmd.Target, cmd.Ref)
	}
}

// Option configures New.
type Option func(*dash)

// WithChannel wires ch so POST /api/v1/retry enqueues onto it. Without this
// option the route still validates the request and responds 202, but the
// resulting Command has nowhere to go and is dropped exactly like a full
// buffer — useful for exercising the request-validation path in isolation.
func WithChannel(ch *Channel) Option {
	return func(d *dash) { d.ch = ch }
}

// WithVersion sets the gauntlet version string shown in every page's
// footer (cmd/gauntlet wires this from its own main.version — see
// docs/deploy.md for how that's packaged). Without this option the footer
// omits the version line entirely, same as today.
func WithVersion(v string) Option {
	return func(d *dash) { d.version = v }
}

// WithLogRoot enables full per-check log serving (DESIGN.md "Full per-check
// log files"): root is the containment boundary every stored
// history.CheckRow.LogPath must resolve under (cmd/gauntlet wires this to
// the same directory passed as queue.Config.LogDir, so it's exactly the
// tree the executor ever writes log files into). Without this option
// GET /run/{id}/log/{check} always 404s and every logUrl field (run.html,
// GET /api/v1/run/{id}) stays empty/absent — see server.go's
// resolveLogPath for the containment check itself.
func WithLogRoot(root string) Option {
	return func(d *dash) { d.logRoot = root }
}

// WithHookCancel wires fn so POST /api/v1/hooks/cancel can cancel a target's
// currently-running post-land hook execution (hooks.Runner.CancelCurrent,
// cmd/gauntlet's nil-safe wiring when hooks are configured). Without this
// option the route always responds 503 "hooks disabled" — see
// handleAPIHookCancel.
func WithHookCancel(fn func(target string) bool) Option {
	return func(d *dash) { d.hookCancel = fn }
}

// WithDrain wires fn so POST /api/v1/drain can begin a graceful shutdown
// drain (issue #8, cmd/gauntlet's beginDrain). Without this option the
// route responds 503 "drain unavailable" — the drain is still reachable
// by signal, but no HTTP admin surface was wired for it.
func WithDrain(fn func(deadline time.Time)) Option {
	return func(d *dash) { d.drain = fn }
}

// LiveHook mirrors hooks.LiveState (internal/hooks) as a dashboard-local
// struct, so this package never needs to import internal/hooks just to read
// one target's live post-land hook progress. Target is included even
// though a caller always already knows which target it asked
// WithHookSnapshot's func for, purely so LiveHook is a complete,
// self-describing value on its own.
type LiveHook struct {
	Target       string
	Running      bool
	CurrentHook  string
	HookIndex    int
	HookCount    int
	StartedAt    time.Time
	BacklogDepth int
}

// WithHookSnapshot wires fn so GET /api/v1/status (per target) and the
// target page's "Post-land hooks" section can render a target's current
// in-flight hook progress (hooks.Runner.Snapshot, nil-safe wiring mirroring
// WithHookCancel). Without this option, both
// surfaces simply omit live-hook data (ok=false, as if no hook were ever
// running) — the durable hookRuns/HookRunSummaries data (from the history
// store, independent of this) still renders regardless.
func WithHookSnapshot(fn func(target string) (LiveHook, bool)) Option {
	return func(d *dash) { d.hookSnapshot = fn }
}

// hookRunsLimit/ignoredRefsLimit cap how many durable hook-run summaries /
// ignored-ref rows GET /api/v1/status and the target page fetch per target —
// both are meant as a recent-activity glance, not a full history browse (that
// belongs to /run/{id} and a future ignored-refs list view).
const (
	hookRunsLimit    = 10
	ignoredRefsLimit = 10
)

// ServiceStatus mirrors services.InstanceStatus (internal/services) as a
// dashboard-local struct, same "duplicated rather than imported" convention
// as LiveHook — this package never needs to import internal/services just to
// render the shared-services pool's tuning surface: every operator-visible
// fact appears on dashboard HTML, the JSON API, and MCP alike. Mode is
// already the string form (services.Mode.String())
// rather than the numeric services.Mode type: cmd/gauntlet's adapter — the
// one place a services.Mode value ever exists outside internal/services —
// converts it before crossing into this package.
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

// ServicesStatus mirrors services.PoolStatus field-for-field.
type ServicesStatus struct {
	MaxInstances int
	Pending      int
	Instances    []ServiceStatus
}

// WithServicesSnapshot wires fn so the index page's "Services" section and
// GET /api/v1/services can render the shared-services pool: operators
// sizing idle-ttl/max-instances need to SEE the pool. Without this option
// — services aren't configured for this daemon, or cmd/gauntlet never
// wired it up — both surfaces treat the pool as absent: the index page
// omits the section entirely, and GET
// /api/v1/services responds 503 "services disabled", mirroring
// WithHookCancel's own nil-safe degradation.
func WithServicesSnapshot(fn func() ServicesStatus) Option {
	return func(d *dash) { d.servicesSnapshot = fn }
}

// mountAPIRoutes registers the JSON API beside the HTML routes New already
// registers. /api/v1/retry is registered without a method verb (unlike the
// GET-only routes) because its handler needs full control over the 405
// response body (a JSON `{"error": ...}`, not net/http's default plain-text
// "Method Not Allowed").
func (d *dash) mountAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/status", d.handleAPIStatus)
	mux.HandleFunc("GET /api/v1/runs", d.handleAPIRuns)
	mux.HandleFunc("GET /api/v1/run/{id}", d.handleAPIRun)
	mux.HandleFunc("GET /api/v1/batch/{id}", d.handleAPIBatch)
	mux.HandleFunc("GET /api/v1/checks", d.handleAPIChecks)
	mux.HandleFunc("GET /api/v1/services", d.handleAPIServices)
	mux.HandleFunc("/api/v1/retry", d.handleAPIRetry)
	mux.HandleFunc("/api/v1/cancel", d.handleAPICancel)
	mux.HandleFunc("/api/v1/hooks/cancel", d.handleAPIHookCancel)
	mux.HandleFunc("/api/v1/drain", d.handleAPIDrain)
}

// --- GET /api/v1/status ------------------------------------------------------

type statusResponse struct {
	SnapshotAt string         `json:"snapshotAt"`
	Targets    []targetStatus `json:"targets"`

	// IgnoredRefs surfaces recently pushed refs that named NO configured
	// target (history.Store.IgnoredRefs). Deliberately a top-level,
	// daemon-wide field rather than per-target: an ignored ref's defining
	// property is that its target segment names no configured target —
	// reconcile emits EventIgnoredRef under that unconfigured name — so a
	// per-configured-target list could
	// never match anything. Each entry's target field carries the
	// unconfigured name for display. Omitted (not just empty) when history
	// is disabled.
	IgnoredRefs []ignoredRefStatus `json:"ignoredRefs,omitempty"`

	// IdleSince is the RFC3339 instant since which the WHOLE daemon has been
	// idle — every target's queue is idle (queue.Snapshot.IdleSince) AND no
	// target has a post-land hook running or backlogged right now: the
	// signal an external timer function polls to know when it's safe to
	// deallocate the parked builder VM. Omitted whenever the daemon isn't
	// idle at this instant — there is no "was
	// idle, no longer" value here, only "idle since T" or absent. See
	// dash.idleSince for the composition (queue idleness lives in
	// internal/queue; hook idleness lives outside it entirely, Invariant 8,
	// so only this layer — which holds both — can combine them).
	IdleSince string `json:"idleSince,omitempty"`

	// Lifecycle is the daemon's shutdown phase (issue #8):
	// running/draining/drained. ActiveRuns/ActiveChecks are the in-flight
	// drain set; DrainSince/DrainDeadline (RFC3339, omitted outside a
	// drain) frame it. Readiness integrations distinguish "draining"
	// (alive, no longer admitting new work) from an unhealthy daemon.
	Lifecycle     string `json:"lifecycle"`
	ActiveRuns    int    `json:"activeRuns"`
	ActiveChecks  int    `json:"activeChecks"`
	DrainSince    string `json:"drainSince,omitempty"`
	DrainDeadline string `json:"drainDeadline,omitempty"`
}

type targetStatus struct {
	Name     string           `json:"name"`
	Branch   string           `json:"branch"`
	Tip      string           `json:"tip"`
	InFlight *inFlightStatus  `json:"inFlight"`
	Pipeline []pipelineStatus `json:"pipeline"`
	Waiting  []waitingStatus  `json:"waiting"`
	Parked   []parkedStatus   `json:"parked"`

	// LiveHook is this target's current post-land hook progress
	// (hooks.Runner.Snapshot via WithHookSnapshot), nil when no hook is
	// running right now or WithHookSnapshot was never wired up.
	LiveHook *liveHookStatus `json:"liveHook,omitempty"`

	// HookRuns surfaces the durable hook-run ledger (history.Store.
	// HookRunSummaries): each landing's owed/done hook count, so a
	// crash-incomplete or recovery-skipped hook chain is visible without
	// digging into /run/{id}. Omitted (not just empty) when history is
	// disabled. (Ignored refs, by contrast, live at the response's top
	// level — see statusResponse.IgnoredRefs for why they can't be
	// target-scoped.)
	HookRuns []hookRunStatus `json:"hookRuns,omitempty"`
}

// liveHookStatus is GET /api/v1/status's JSON view of one target's LiveHook.
type liveHookStatus struct {
	Running      bool   `json:"running"`
	CurrentHook  string `json:"currentHook"`
	HookIndex    int    `json:"hookIndex"`
	HookCount    int    `json:"hookCount"`
	StartedAt    string `json:"startedAt"`
	BacklogDepth int    `json:"backlogDepth"`
}

// hookRunStatus is GET /api/v1/status's JSON view of one history.
// HookRunSummary row. Incomplete is computed here (OwedCount > DoneCount &&
// !Skipped) rather than left for the client to derive: it's the
// crash-incomplete signal.
type hookRunStatus struct {
	RunID      string `json:"runID"`
	OwedCount  int    `json:"owedCount"`
	DoneCount  int    `json:"doneCount"`
	StartedAt  string `json:"startedAt"`
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skipReason,omitempty"`
	Incomplete bool   `json:"incomplete"`
}

// ignoredRefStatus is GET /api/v1/status's JSON view of one history.
// IgnoredRef row. Target is the UNCONFIGURED target name the ref's segment
// named (the reason it was ignored), carried purely for display.
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

// pipelineStatus mirrors one queue.RunSnapshot within a target's pipeline:
// head first, additive alongside inFlight (which stays the head run,
// back-compat). Field names are RunSnapshot's,
// lowerCamel.
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
	// RunID is the run that parked this candidate, "" when unknown (mirrors
	// queue.ParkedEntry.RunID — see its doc). omitempty so an old boot-seed
	// park without one doesn't clutter the JSON with an empty string.
	RunID string `json:"runId,omitempty"`
}

func (d *dash) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	snap := d.snapshot()
	if snap == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no snapshot yet")
		return
	}

	resp := statusResponse{
		SnapshotAt: formatRFC3339(snap.At),
		Targets:    make([]targetStatus, 0, len(snap.Targets)),
	}
	for _, ts := range snap.Targets {
		resp.Targets = append(resp.Targets, d.buildTargetStatus(ts))
	}
	if d.store != nil {
		if refs, err := d.store.IgnoredRefs(ignoredRefsLimit); err != nil {
			log.Printf("dashboard: api: ignored refs: %v", err)
		} else {
			resp.IgnoredRefs = make([]ignoredRefStatus, 0, len(refs))
			for _, ir := range refs {
				resp.IgnoredRefs = append(resp.IgnoredRefs, ignoredRefStatus{
					At: formatRFC3339(ir.At), Target: ir.Target, Ref: ir.Ref, Detail: ir.Detail,
				})
			}
		}
	}
	if since := d.idleSince(snap); !since.IsZero() {
		resp.IdleSince = formatRFC3339(since)
	}
	resp.Lifecycle = string(snap.Lifecycle)
	if resp.Lifecycle == "" {
		resp.Lifecycle = string(queue.LifecycleRunning) // pre-#8 snapshots
	}
	resp.ActiveRuns = snap.ActiveRuns
	resp.ActiveChecks = snap.ActiveChecks
	if !snap.DrainSince.IsZero() {
		resp.DrainSince = formatRFC3339(snap.DrainSince)
	}
	if !snap.DrainDeadline.IsZero() {
		resp.DrainDeadline = formatRFC3339(snap.DrainDeadline)
	}
	writeJSON(w, http.StatusOK, resp)
}

// idleSince composes queue idleness (snap.IdleSince) with hook idleness
// into the one daemon-wide idle instant GET
// /api/v1/status, the MCP status tool (internal/mcp/server.go's own
// idleSince), and the index page's footer line all surface identically:
// zero (not idle) unless the queue has been idle since some instant AND no
// target's post-land hook is running or backlogged right now. Hooks live
// outside the queue package (Invariant 8), so this composition can only
// happen at this layer, which is the one place both a queue.Snapshot and
// d.hookSnapshot are both in hand. d.hookSnapshot nil (hooks not configured
// for this daemon) means queue idleness alone decides.
func (d *dash) idleSince(snap *queue.Snapshot) time.Time {
	if snap.IdleSince.IsZero() {
		return time.Time{}
	}
	if d.hookSnapshot != nil {
		for _, ts := range snap.Targets {
			if lh, ok := d.hookSnapshot(ts.Name); ok && (lh.Running || lh.BacklogDepth > 0) {
				return time.Time{}
			}
		}
	}
	return snap.IdleSince
}

// buildTargetStatus is a method (rather than a free function) so it can
// reach d.hookSnapshot/d.store for the live/durable hook fields —
// every other field is built exactly as before. Ignored refs
// are daemon-level, populated by handleAPIStatus itself, not here.
func (d *dash) buildTargetStatus(ts queue.TargetSnapshot) targetStatus {
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

	// Defensive re-sort by Seq, matching handleTarget's html view: this is
	// already FIFO order out of buildTargetSnapshot, but the API's ordering
	// guarantee shouldn't rest on an internal detail of the queue package.
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

	if d.hookSnapshot != nil {
		if lh, ok := d.hookSnapshot(ts.Name); ok {
			out.LiveHook = &liveHookStatus{
				Running: lh.Running, CurrentHook: lh.CurrentHook,
				HookIndex: lh.HookIndex, HookCount: lh.HookCount,
				StartedAt: formatRFC3339(lh.StartedAt), BacklogDepth: lh.BacklogDepth,
			}
		}
	}
	if d.store != nil {
		if runs, err := d.store.HookRunSummaries(ts.Name, hookRunsLimit); err != nil {
			log.Printf("dashboard: api: hook run summaries %s: %v", ts.Name, err)
		} else {
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

// buildPipelineStatus builds one pipelineStatus from a RunSnapshot, for
// GET /api/v1/status's additive "pipeline" array.
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

// --- GET /api/v1/runs?target=&limit= -----------------------------------------

const defaultRunsLimit = 20

type runsResponse struct {
	Runs []runSummaryJSON `json:"runs"`
}

type runSummaryJSON struct {
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

	// BatchID/Position/BatchSize surface batch identity as-is: all three
	// are omitted entirely for a serial or speculate run (BatchID == ""),
	// present for a batch member. Note omitempty's one blind spot: a
	// batch's member 0 also omits "position" (Go's zero value for int), so
	// a client must treat "batchId present, position absent" as position
	// 0, not "not a batch member" — batchId's presence is the actual
	// batch-membership signal.
	BatchID   string `json:"batchId,omitempty"`
	Position  int    `json:"position,omitempty"`
	BatchSize int    `json:"batchSize,omitempty"`
}

func (d *dash) handleAPIRuns(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history disabled")
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		writeJSONError(w, http.StatusBadRequest, "target is required")
		return
	}

	limit := defaultRunsLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}

	rows, err := d.store.RecentRuns(target, limit)
	if err != nil {
		log.Printf("dashboard: api: recent runs %s: %v", target, err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := runsResponse{Runs: make([]runSummaryJSON, 0, len(rows))}
	for _, row := range rows {
		resp.Runs = append(resp.Runs, runRowToJSON(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

func runRowToJSON(row history.RunRow) runSummaryJSON {
	out := runSummaryJSON{
		RunID: row.RunID, Target: row.Target, Ref: row.CandidateRef,
		User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
		Outcome: row.Outcome, Detail: row.Detail,
		StartedAt:  formatRFC3339(row.StartedAt),
		EndedAt:    formatRFC3339(row.EndedAt),
		DurationMs: row.Duration.Milliseconds(),
	}
	if row.BatchID != "" {
		out.BatchID, out.Position, out.BatchSize = row.BatchID, row.Position, row.BatchSize
	}
	return out
}

// --- GET /api/v1/run/{id} -----------------------------------------------------

type runDetailResponse struct {
	RunID      string      `json:"runID"`
	Target     string      `json:"target"`
	Ref        string      `json:"ref"`
	User       string      `json:"user"`
	Topic      string      `json:"topic"`
	SHA        string      `json:"sha"`
	BaseOID    string      `json:"baseOID"`
	MergeSHA   string      `json:"mergeSHA"`
	TrialClean bool        `json:"trialClean"`
	Outcome    string      `json:"outcome"`
	Detail     string      `json:"detail"`
	StartedAt  string      `json:"startedAt"`
	EndedAt    string      `json:"endedAt"`
	DurationMs int64       `json:"durationMs"`
	Checks     []checkJSON `json:"checks"`
	// Hooks holds this run's post-land hook results (internal/hooks), same
	// shape as Checks. Always present as an array (possibly empty, never
	// omitted) — a client that doesn't care about hooks can simply ignore an
	// empty one.
	Hooks []checkJSON `json:"hooks"`

	// BatchID/Position/BatchSize surface batch identity as-is: all three
	// are omitted entirely for a serial or speculate run (BatchID == ""),
	// present for a batch member. Note omitempty's one blind spot: a
	// batch's member 0 also omits "position" (Go's zero value for int), so
	// a client must treat "batchId present, position absent" as position
	// 0, not "not a batch member" — batchId's presence is the actual
	// batch-membership signal.
	BatchID   string `json:"batchId,omitempty"`
	Position  int    `json:"position,omitempty"`
	BatchSize int    `json:"batchSize,omitempty"`

	// Speculated and Recovered mirror history.RunRow's own fields
	// (core.RunRecord.Speculated/Recovered, v7+): purely informational
	// (RunRecord's own field docs), omitted (rather than present-and-false)
	// for the common case, matching BatchID's own omitempty convention.
	// Run-detail only — GET /api/v1/runs and /api/v1/batch/{id} don't carry
	// these.
	Speculated bool `json:"speculated,omitempty"`
	Recovered  bool `json:"recovered,omitempty"`

	// ReceiptRef/ReceiptBlob/ReceiptPublished mirror history.RunRow's own
	// fields of the same names (v12+, issue #13): the receipt-notes
	// publication provenance of a run whose note was confirmed published —
	// NOT landed-only, an orphaned publish (target CAS lost the race after
	// a successful publish) carries these too. Omitted (rather than
	// present-and-empty) when the run never published one — same
	// omitempty convention as BatchID/Speculated above.
	ReceiptRef       string `json:"receiptRef,omitempty"`
	ReceiptBlob      string `json:"receiptBlob,omitempty"`
	ReceiptPublished string `json:"receiptPublished,omitempty"`
}

type checkJSON struct {
	Seq        int    `json:"seq"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs"`
	Err        string `json:"err"`
	// Output is the check's (or hook's) captured output, stored verbatim,
	// straight from the same history.CheckRow/HookRow column the HTML page
	// and the MCP server already render — added so a JSON/CLI consumer
	// doesn't need a second round-trip through the log file (LogPath/LogURL,
	// below) just to see what a check printed.
	Output string `json:"output"`
	// LogPath is the check's full per-check log file path on disk (empty if
	// none was written), verbatim from history.CheckRow.LogPath.
	LogPath string `json:"logPath"`
	// LogURL is the relative link the dashboard serves that file at
	// (GET /run/{id}/log/{name}), present only when the dashboard is
	// actually configured to serve it (WithLogRoot) and LogPath is
	// non-empty — omitted from the JSON entirely otherwise.
	LogURL string `json:"logUrl,omitempty"`

	// PeakRSSBytes/UserCPUMs/SysCPUMs mirror history.CheckRow.PeakRSS/
	// UserCPU/SysCPU (v13+, issue #14): bounded numbers, same convention as
	// DurationMs above. omitempty, not a bare 0: the zero-means-unmeasured
	// contract (the container executor's v1 result, or a pre-v13 row) means
	// a present-but-zero field would misread as "measured zero", so an
	// unmeasured field is absent from the JSON entirely — a client checks
	// for the field's presence, not its value, to know whether it was
	// captured. Hooks never carry these (history.HookRow has no equivalent
	// columns), so a checkJSON built from a hook's HookRow always omits all
	// three, same as it always did before this field existed.
	PeakRSSBytes int64 `json:"peakRSSBytes,omitempty"`
	UserCPUMs    int64 `json:"userCPUMs,omitempty"`
	SysCPUMs     int64 `json:"sysCPUMs,omitempty"`
}

func (d *dash) handleAPIRun(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history disabled")
		return
	}

	id := r.PathValue("id")
	row, checks, err := d.store.Run(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		log.Printf("dashboard: api: run %s: %v", id, err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := runDetailResponse{
		RunID: row.RunID, Target: row.Target, Ref: row.CandidateRef,
		User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
		BaseOID: row.BaseOID, MergeSHA: row.MergeSHA, TrialClean: row.TrialClean,
		Outcome: row.Outcome, Detail: row.Detail,
		StartedAt:  formatRFC3339(row.StartedAt),
		EndedAt:    formatRFC3339(row.EndedAt),
		DurationMs: row.Duration.Milliseconds(),
		Checks:     make([]checkJSON, 0, len(checks)),
		Speculated: row.Speculated,
		Recovered:  row.Recovered,

		ReceiptRef:       row.ReceiptRef,
		ReceiptBlob:      row.ReceiptBlob,
		ReceiptPublished: row.ReceiptPublished,
	}
	if row.BatchID != "" {
		resp.BatchID, resp.Position, resp.BatchSize = row.BatchID, row.Position, row.BatchSize
	}
	for _, c := range checks {
		resp.Checks = append(resp.Checks, checkJSON{
			Seq: c.Seq, Name: c.Name, Status: c.Status,
			DurationMs: c.Duration.Milliseconds(), Err: c.Err,
			Output:  c.Output,
			LogPath: c.LogPath,
			LogURL:  d.runLogURL(row.RunID, c.Name, c.LogPath),

			PeakRSSBytes: c.PeakRSS,
			UserCPUMs:    c.UserCPU.Milliseconds(),
			SysCPUMs:     c.SysCPU.Milliseconds(),
		})
	}

	resp.Hooks = make([]checkJSON, 0)
	hooks, err := d.store.Hooks(id)
	if err != nil {
		log.Printf("dashboard: api: run %s: hooks: %v", id, err)
	}
	for _, h := range hooks {
		resp.Hooks = append(resp.Hooks, checkJSON{
			Seq: h.Seq, Name: h.Name, Status: h.Status,
			DurationMs: h.Duration.Milliseconds(), Err: h.Err,
			Output:  h.Output,
			LogPath: h.LogPath,
			LogURL:  d.runLogURL(row.RunID, h.Name, h.LogPath),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- GET /api/v1/batch/{id} ---------------------------------------------------

// batchMemberJSON is one member of GET /api/v1/batch/{id}'s response — the
// same data handleBatch (server.go) already renders as HTML, over
// history.Store.BatchMembers: no new data source, just a JSON surface
// for it.
type batchMemberJSON struct {
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

type batchResponse struct {
	BatchID string            `json:"batchId"`
	Members []batchMemberJSON `json:"members"`
}

// handleAPIBatch mirrors handleBatch (server.go) but as JSON: unknown batch
// ID (or an empty result — BatchMembers' own "empty batchID" doc) 404s, same
// as the HTML route.
func (d *dash) handleAPIBatch(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history disabled")
		return
	}

	id := r.PathValue("id")
	members, err := d.store.BatchMembers(id)
	if err != nil {
		log.Printf("dashboard: api: batch %s: %v", id, err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(members) == 0 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}

	resp := batchResponse{BatchID: id, Members: make([]batchMemberJSON, 0, len(members))}
	for _, m := range members {
		resp.Members = append(resp.Members, batchMemberJSON{
			RunID: m.RunID, Target: m.Target, Position: m.Position,
			User: m.CandidateUser, Topic: m.CandidateTopic, SHA: m.CandidateSHA,
			Outcome: m.Outcome, Detail: m.Detail,
			StartedAt:  formatRFC3339(m.StartedAt),
			EndedAt:    formatRFC3339(m.EndedAt),
			DurationMs: m.Duration.Milliseconds(),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- GET /api/v1/checks?target=&since= ----------------------------------------

// checkStatJSON is one row of GET /api/v1/checks's per-check stats table,
// mirroring history.CheckStat field-for-field.
type checkStatJSON struct {
	Name          string  `json:"name"`
	Total         int     `json:"total"`
	Failed        int     `json:"failed"`
	RedRate       float64 `json:"redRate"`
	AvgDurationMs int64   `json:"avgDurationMs"`
	MaxDurationMs int64   `json:"maxDurationMs"`

	// PeakRSSMax/MedianBytes and UserCPU/SysCPUMax/MedianMs mirror
	// history.CheckStat's resource-usage aggregates (v13+, issue #14): all
	// six omitted together, per metric, when that metric's *Measured flag
	// is false (nothing in the window measured it) — never a reported zero.
	// A client should treat "peakRSSMaxBytes absent" as "no data", the same
	// signal history.CheckStat.PeakRSSMeasured == false gives Go callers.
	PeakRSSMaxBytes    int64 `json:"peakRSSMaxBytes,omitempty"`
	PeakRSSMedianBytes int64 `json:"peakRSSMedianBytes,omitempty"`
	UserCPUMaxMs       int64 `json:"userCPUMaxMs,omitempty"`
	UserCPUMedianMs    int64 `json:"userCPUMedianMs,omitempty"`
	SysCPUMaxMs        int64 `json:"sysCPUMaxMs,omitempty"`
	SysCPUMedianMs     int64 `json:"sysCPUMedianMs,omitempty"`
}

// depthPointJSON is one sample of GET /api/v1/checks's depth series,
// mirroring history.DepthPoint field-for-field.
type depthPointJSON struct {
	At       string `json:"at"`
	Waiting  int    `json:"waiting"`
	InFlight int    `json:"inFlight"`
	Parked   int    `json:"parked"`
}

type checksAPIResponse struct {
	Target string           `json:"target"`
	Since  string           `json:"since"`
	Stats  []checkStatJSON  `json:"stats"`
	Depth  []depthPointJSON `json:"depth"`
}

// handleAPIChecks is the JSON counterpart to handleChecks (server.go)'s
// red-rate/avg-duration table and depth chart: it reuses the exact same
// CheckStats/DepthSeries queries and parseSince parsing, just serialized as
// JSON numbers/points instead of an SVG.
func (d *dash) handleAPIChecks(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "history disabled")
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		writeJSONError(w, http.StatusBadRequest, "target is required")
		return
	}

	now := time.Now()
	since := parseSince(r.URL.Query().Get("since"), now)

	stats, err := d.store.CheckStats(target, since)
	if err != nil {
		log.Printf("dashboard: api: check stats %s: %v", target, err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	depth, err := d.store.DepthSeries(target, since)
	if err != nil {
		log.Printf("dashboard: api: depth series %s: %v", target, err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := checksAPIResponse{
		Target: target, Since: formatRFC3339(since),
		Stats: make([]checkStatJSON, 0, len(stats)),
		Depth: make([]depthPointJSON, 0, len(depth)),
	}
	for _, st := range stats {
		out := checkStatJSON{
			Name: st.Name, Total: st.Total, Failed: st.Failed, RedRate: st.RedRate,
			AvgDurationMs: st.AvgDuration.Milliseconds(), MaxDurationMs: st.MaxDuration.Milliseconds(),
		}
		if st.PeakRSSMeasured {
			out.PeakRSSMaxBytes, out.PeakRSSMedianBytes = st.PeakRSSMax, st.PeakRSSMedian
		}
		if st.UserCPUMeasured {
			out.UserCPUMaxMs, out.UserCPUMedianMs = st.UserCPUMax.Milliseconds(), st.UserCPUMedian.Milliseconds()
		}
		if st.SysCPUMeasured {
			out.SysCPUMaxMs, out.SysCPUMedianMs = st.SysCPUMax.Milliseconds(), st.SysCPUMedian.Milliseconds()
		}
		resp.Stats = append(resp.Stats, out)
	}
	for _, p := range depth {
		resp.Depth = append(resp.Depth, depthPointJSON{
			At: formatRFC3339(p.At), Waiting: p.Waiting, InFlight: p.InFlight, Parked: p.Parked,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- GET /api/v1/services -----------------------------------------------------

// serviceInstanceJSON is one live shared-service instance, mirroring
// ServiceStatus field-for-field. Key is the full key (see
// docs/design/services.md, "Full key versus name" — only the full key is
// guaranteed collision-free); KeyHash12 is the same truncation the
// dashboard HTML table shows for compact display.
type serviceInstanceJSON struct {
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

// servicesResponse mirrors ServicesStatus (services.PoolStatus)
// field-for-field: the pool's own tuning knobs alongside one
// serviceInstanceJSON per live instance.
type servicesResponse struct {
	MaxInstances int                   `json:"maxInstances"`
	Pending      int                   `json:"pending"`
	Instances    []serviceInstanceJSON `json:"instances"`
}

// handleAPIServices renders the shared-services pool, mirroring
// handleAPIRuns/handleAPIRun's "disabled" degradation:
// 503 {"error":"services disabled"} when WithServicesSnapshot was never
// wired up (no daemon-level services block configured, or this build has no
// services support) — the one case this route can't do anything meaningful.
func (d *dash) handleAPIServices(w http.ResponseWriter, r *http.Request) {
	if d.servicesSnapshot == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "services disabled")
		return
	}

	ss := d.servicesSnapshot()
	resp := servicesResponse{
		MaxInstances: ss.MaxInstances, Pending: ss.Pending,
		Instances: make([]serviceInstanceJSON, 0, len(ss.Instances)),
	}
	for _, inst := range ss.Instances {
		resp.Instances = append(resp.Instances, serviceInstanceJSON{
			Service: inst.Service, Image: inst.Image, Key: inst.Key, KeyHash12: inst.KeyHash12,
			Mode: inst.Mode, Host: inst.Host, Port: inst.Port,
			CreatedAt: formatRFC3339(inst.CreatedAt), LastUsed: formatRFC3339(inst.LastUsed),
			Refcount: inst.Refcount, Hits: inst.Hits,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- POST /api/v1/retry -------------------------------------------------------

type retryRequest struct {
	Target string `json:"target"`
	Ref    string `json:"ref"`
}

func (d *dash) handleAPIRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req retryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Target == "" || req.Ref == "" {
		writeJSONError(w, http.StatusBadRequest, "target and ref are required")
		return
	}

	if d.ch != nil {
		d.ch.enqueue(core.Command{Kind: core.CommandRetry, Target: req.Target, Ref: req.Ref})
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// --- POST /api/v1/cancel ------------------------------------------------------

// cancelRequest mirrors retryRequest's shape (manual operator
// cancellation): same (target, ref) pair, a different Command.Kind.
type cancelRequest struct {
	Target string `json:"target"`
	Ref    string `json:"ref"`
}

// handleAPICancel mirrors handleAPIRetry exactly but enqueues a
// core.CommandCancel instead of a core.CommandRetry — see command.go's
// applyCancel for what the queue does with it (cancel an in-flight run and
// park its member, or park a waiting ref directly).
func (d *dash) handleAPICancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req cancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Target == "" || req.Ref == "" {
		writeJSONError(w, http.StatusBadRequest, "target and ref are required")
		return
	}

	if d.ch != nil {
		d.ch.enqueue(core.Command{Kind: core.CommandCancel, Target: req.Target, Ref: req.Ref})
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// --- POST /api/v1/hooks/cancel ------------------------------------------------

// hookCancelRequest is POST /api/v1/hooks/cancel's body: just the target,
// since hooks.Runner.CancelCurrent (what this ultimately calls, via
// WithHookCancel) only ever cancels whatever landing's hooks are running for
// that target right now — there is no ref/run to name (a hook stage has no
// notion of "which candidate", only "which target's backlog").
type hookCancelRequest struct {
	Target string `json:"target"`
}

// handleAPIHookCancel cancels target's currently-running post-land hook
// execution, if any (hooks.Runner.CancelCurrent via WithHookCancel). Unlike
// handleAPICancel/handleAPIRetry
// (which enqueue a Command for the next reconcile pass to apply), this calls
// straight through synchronously and its result is known immediately:
// "cancelled" if a running landing was found and signalled, "no-op"
// otherwise (nothing running for this target right now — a normal,
// expected outcome, not an error). 503 {"error":"hooks disabled"} when
// WithHookCancel was never wired up at all (no target configures any
// hooks, or hooks aren't compiled in for this deployment) — the one case
// this route can't do anything meaningful, mirroring handleAPIRuns/
// handleAPIRun's "history disabled" 503.
func (d *dash) handleAPIHookCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req hookCancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Target == "" {
		writeJSONError(w, http.StatusBadRequest, "target is required")
		return
	}
	if d.hookCancel == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "hooks disabled")
		return
	}

	status := "no-op"
	if d.hookCancel(req.Target) {
		status = "cancelled"
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": status})
}

// --- POST /api/v1/drain -------------------------------------------------------

// drainRequest is POST /api/v1/drain's body. deadline is an optional
// RFC3339 instant at which an unfinished drain is forced (the immediate
// kill), so orchestration can bound how long it waits; absent means no
// queue-level deadline (the operator, a second signal, or systemd's
// TimeoutStopSec still force it).
type drainRequest struct {
	Deadline string `json:"deadline,omitempty"`
}

// handleAPIDrain begins a graceful drain (issue #8): stop admitting new
// candidates, let the in-flight set finish, then exit. Idempotent — a
// repeat call never resumes admission and only ever shortens the deadline.
// Returns 202 with the drain start acknowledged; the caller polls GET
// /api/v1/status for the lifecycle transition to "drained". 503 when no
// drain callback was wired (WithDrain absent).
func (d *dash) handleAPIDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req drainRequest
	// An empty body is valid (drain with no deadline); only malformed JSON
	// is an error. io.EOF means the body was empty — which reaches here
	// for a chunked/HTTP-2 request (ContentLength == -1, so the length
	// guard alone can't catch it) — and must be accepted, not rejected.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var deadline time.Time
	if req.Deadline != "" {
		t, err := time.Parse(time.RFC3339, req.Deadline)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "deadline must be an RFC3339 timestamp")
			return
		}
		deadline = t
	}
	if d.drain == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "drain unavailable")
		return
	}
	d.drain(deadline)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "draining"})
}

// --- shared JSON helpers -----------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("dashboard: api: encode response: %v", err)
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
