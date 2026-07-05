package dashboard

// This file adds a JSON API alongside the HTML dashboard in server.go (work
// chunk E4, docs/plans/phase23.md): GET /api/v1/status mirrors the live
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
// rather than waiting for the queue to drain. Exported (chunk E5,
// internal/mcp) so the MCP retry tool can feed the same channel POST
// /api/v1/retry does and distinguish "queued" from "dropped, buffer
// full" the way an HTTP response code lets api.go's caller do.
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

// mountAPIRoutes registers the JSON API beside the HTML routes New already
// registers. /api/v1/retry is registered without a method verb (unlike the
// GET-only routes) because its handler needs full control over the 405
// response body (a JSON `{"error": ...}`, not net/http's default plain-text
// "Method Not Allowed").
func (d *dash) mountAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/status", d.handleAPIStatus)
	mux.HandleFunc("GET /api/v1/runs", d.handleAPIRuns)
	mux.HandleFunc("GET /api/v1/run/{id}", d.handleAPIRun)
	mux.HandleFunc("/api/v1/retry", d.handleAPIRetry)
}

// --- GET /api/v1/status ------------------------------------------------------

type statusResponse struct {
	SnapshotAt string         `json:"snapshotAt"`
	Targets    []targetStatus `json:"targets"`
}

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
		resp.Targets = append(resp.Targets, buildTargetStatus(ts))
	}
	writeJSON(w, http.StatusOK, resp)
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
	return runSummaryJSON{
		RunID: row.RunID, Target: row.Target, Ref: row.CandidateRef,
		User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
		Outcome: row.Outcome, Detail: row.Detail,
		StartedAt:  formatRFC3339(row.StartedAt),
		EndedAt:    formatRFC3339(row.EndedAt),
		DurationMs: row.Duration.Milliseconds(),
	}
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
}

type checkJSON struct {
	Seq        int    `json:"seq"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs"`
	Err        string `json:"err"`
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
	}
	for _, c := range checks {
		resp.Checks = append(resp.Checks, checkJSON{
			Seq: c.Seq, Name: c.Name, Status: c.Status,
			DurationMs: c.Duration.Milliseconds(), Err: c.Err,
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
