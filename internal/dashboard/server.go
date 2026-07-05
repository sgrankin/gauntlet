// Package dashboard implements gauntlet's read-only web dashboard
// (docs/plans/phase23.md §4.2): stdlib net/http + html/template over two
// data sources — a live queue.Snapshot published by the reconcile loop, and
// a *history.Store for run history. Both sources are optional at the type
// level: snapshot may return nil (no reconcile pass has completed yet) and
// store may be nil (history disabled); every route degrades to a friendly
// message rather than panicking or 500ing in either case.
//
// Every HTML route in this file only reads. api.go (work chunk E4) adds a
// JSON API beside it, mounted on the same handler, with one mutating route
// (POST /api/v1/retry) that injects a core.Command the same way a Slack
// ":recycle:" reaction does — see api.go's Channel doc for how that's
// wired. Auth is out of scope (documented in DESIGN.md); bind to a trusted
// interface.
package dashboard

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// New builds the dashboard's http.Handler. snapshot is called on every
// request that needs live queue state (typically Daemon.Snapshot); store
// may be nil, in which case every history-backed section renders "history
// disabled" instead of querying it. opts configures the JSON API added in
// api.go — see WithChannel.
func New(snapshot func() *queue.Snapshot, store *history.Store, opts ...Option) http.Handler {
	d := &dash{snapshot: snapshot, store: store}
	for _, opt := range opts {
		opt(d)
	}
	return d.mux()
}

// mux assembles every route: the HTML dashboard (this file) plus the JSON
// API (api.go, mountAPIRoutes).
func (d *dash) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.handleIndex)
	mux.HandleFunc("GET /t/{target}", d.handleTarget)
	mux.HandleFunc("GET /run/{runID}", d.handleRun)
	mux.HandleFunc("GET /checks", d.handleChecks)
	d.mountAPIRoutes(mux)
	return mux
}

type dash struct {
	snapshot func() *queue.Snapshot
	store    *history.Store

	// ch is nil unless New was called with WithChannel: POST /api/v1/retry
	// only has somewhere to send a retry Command when it is set (api.go).
	ch *Channel
}

// --- / --------------------------------------------------------------------

func (d *dash) handleIndex(w http.ResponseWriter, r *http.Request) {
	snap := d.snapshot()
	data := indexData{baseData: newBase("gauntlet", snap, true)}
	if snap == nil {
		data.Starting = true
		render(w, indexTmpl, data)
		return
	}

	for _, ts := range snap.Targets {
		card := targetCard{
			Name:         ts.Name,
			Branch:       ts.Branch,
			TargetTip:    orDash(ts.TargetTip),
			WaitingCount: len(ts.Waiting),
			ParkedCount:  len(ts.Parked),
		}
		if ts.InFlight != nil {
			card.InFlight = buildInFlight(ts.InFlight, snap.At)
		}
		card.RecentRuns, card.StoreEnabled = d.recentRuns(ts.Name, 6, snap.At)
		data.Targets = append(data.Targets, card)
	}
	render(w, indexTmpl, data)
}

// --- /t/{target} ------------------------------------------------------------

func (d *dash) handleTarget(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("target")
	snap := d.snapshot()
	if snap == nil {
		// No pass has completed, so we can't tell a real target name from a
		// typo yet — show the friendly starting-up state rather than 404.
		b := newBase(name, snap, true)
		b.Starting = true
		render(w, targetTmpl, targetData{baseData: b, Name: name})
		return
	}

	ts, ok := findTarget(snap, name)
	if !ok {
		http.NotFound(w, r)
		return
	}

	data := targetData{
		baseData:  newBase(ts.Name, snap, true),
		Name:      ts.Name,
		Branch:    ts.Branch,
		TargetTip: orDash(ts.TargetTip),
	}
	if ts.InFlight != nil {
		data.InFlight = buildInFlight(ts.InFlight, snap.At)
	}

	waiting := append([]queue.WaitingEntry(nil), ts.Waiting...)
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].Seq < waiting[j].Seq })
	for _, we := range waiting {
		data.Waiting = append(data.Waiting, waitingView{
			Seq: we.Seq, Ref: we.Candidate.Ref, User: we.Candidate.User, Topic: we.Candidate.Topic, SHA: we.Candidate.SHA,
		})
	}

	for _, pe := range ts.Parked {
		data.Parked = append(data.Parked, parkedView{
			User: pe.Candidate.User, Topic: pe.Candidate.Topic, SHA: pe.Candidate.SHA,
			Outcome: wordTag(outcomeWord(pe.Outcome)), Reason: pe.Reason, At: formatTime(pe.At),
		})
	}

	data.RecentRuns, data.StoreEnabled = d.recentRuns(ts.Name, 25, snap.At)
	render(w, targetTmpl, data)
}

func findTarget(snap *queue.Snapshot, name string) (queue.TargetSnapshot, bool) {
	for _, t := range snap.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return queue.TargetSnapshot{}, false
}

// --- /run/{runID} -----------------------------------------------------------

func (d *dash) handleRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if d.store == nil {
		render(w, runTmpl, runData{baseData: newBase(runID, nil, false)})
		return
	}

	row, checks, err := d.store.Run(runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("dashboard: run %s: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := runData{
		baseData:     newBase(row.RunID, nil, false),
		StoreEnabled: true,
		Run: runSummaryFull{
			RunID: row.RunID, Target: row.Target,
			Ref: row.CandidateRef, User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
			BaseOID: row.BaseOID, MergeSHA: row.MergeSHA, TrialClean: row.TrialClean,
			Outcome: wordTag(row.Outcome), Detail: row.Detail,
			StartedAt: formatTime(row.StartedAt), EndedAt: formatTime(row.EndedAt), Duration: formatDuration(row.Duration),
		},
	}
	for _, c := range checks {
		data.Checks = append(data.Checks, checkView{
			Seq: c.Seq, Name: c.Name, Status: wordTag(c.Status), Duration: formatDuration(c.Duration), Err: c.Err,
			Output: c.Output,
			// Open the failed/errored check's output by default — this page
			// exists to answer "how did it fail", so the failed check's
			// output should be impossible to miss. Passed/skipped checks
			// start collapsed.
			Open: c.Status == "failed" || c.Err != "",
		})
	}
	render(w, runTmpl, data)
}

// --- /checks?target=&since= --------------------------------------------------

// maxStatsRows caps the per-check stats table: it's grouped by check name,
// which is normally a small set, but nothing stops a misbehaving client from
// naming a fresh check on every run, so cap it at a sane row count rather
// than trusting that.
const maxStatsRows = 200

func (d *dash) handleChecks(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	target := r.URL.Query().Get("target")
	since := parseSince(r.URL.Query().Get("since"), now)

	data := checksData{
		baseData: newBase("checks", nil, false),
		Target:   target,
		Since:    formatTime(since),
	}
	if snap := d.snapshot(); snap != nil {
		for _, t := range snap.Targets {
			data.AvailableTargets = append(data.AvailableTargets, t.Name)
		}
	}
	if d.store == nil {
		render(w, checksTmpl, data)
		return
	}
	data.StoreEnabled = true

	stats, err := d.store.CheckStats(target, since)
	if err != nil {
		log.Printf("dashboard: check stats: %v", err)
	}
	if len(stats) > maxStatsRows {
		stats = stats[:maxStatsRows]
	}
	for _, st := range stats {
		data.Stats = append(data.Stats, statView{
			Name: st.Name, Total: st.Total, Failed: st.Failed,
			RedRate: fmt.Sprintf("%.0f%%", st.RedRate*100),
			Avg:     formatDuration(st.AvgDuration), Max: formatDuration(st.MaxDuration),
		})
	}

	depth, err := d.store.DepthSeries(target, since)
	if err != nil {
		log.Printf("dashboard: depth series: %v", err)
	}
	data.HasDepth = len(depth) > 0
	if data.HasDepth {
		data.DepthChart = buildDepthSVG(depth, since, now)
	}
	render(w, checksTmpl, data)
}

// parseSince interprets the "since" query param relative to now: a Go
// duration ("24h") is relative to now; an RFC3339 timestamp is absolute;
// anything else (including empty) falls back to a 24h window — the depth
// chart's default per requirement, tighter than the 7-day default this used
// to have back when the only consumer was the stats table.
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

// --- depth chart (hand-built SVG) --------------------------------------------
//
// buildDepthSVG renders history.DepthPoint (queue.depth's change-only,
// 10m-heartbeat sampled series — see history/queries.go's DepthPoint doc) as
// a step chart: one line each for waiting/in-flight/parked. No JS charting
// lib — this is plain server-rendered SVG, computed once per request.
//
// Step interpolation with carry-forward: between two stored samples the
// true value never changed (a row is only written on change, or every 10m
// as a heartbeat proving liveness) — so the polyline holds each sample's
// value flat until the next sample's timestamp, then jumps. That is exactly
// what "carry-forward across gaps" means here: a gap between samples is
// never interpolated toward some other value, and never renders as missing
// data — it renders as the last known value, held.

const (
	depthChartW = 700
	depthChartH = 120
	depthPadL   = 32
	depthPadR   = 8
	depthPadT   = 8
	depthPadB   = 20
)

// buildDepthSVG renders points (ascending time order, per DepthSeries) into
// an inline SVG covering [start, end] on the x-axis, where start is the
// earlier of since/points[0].At and end is the later of now/the last
// point's At (so the current value visibly holds through to "now" even if
// no fresher sample has landed yet). Returns "" for an empty series — the
// checks.html template renders "no data yet" in that case instead of calling
// this.
func buildDepthSVG(points []history.DepthPoint, since, now time.Time) template.HTML {
	if len(points) == 0 {
		return ""
	}

	start := since
	if points[0].At.Before(start) {
		start = points[0].At
	}
	end := now
	if last := points[len(points)-1].At; last.After(end) {
		end = last
	}
	span := end.Sub(start).Seconds()
	if span <= 0 {
		span = 1
	}

	maxV := 1
	for _, p := range points {
		for _, v := range [3]int{p.Waiting, p.InFlight, p.Parked} {
			if v > maxV {
				maxV = v
			}
		}
	}

	plotW := float64(depthChartW - depthPadL - depthPadR)
	plotH := float64(depthChartH - depthPadT - depthPadB)
	xAt := func(t time.Time) float64 {
		return float64(depthPadL) + t.Sub(start).Seconds()/span*plotW
	}
	yAt := func(v int) float64 {
		return float64(depthPadT) + plotH - float64(v)/float64(maxV)*plotH
	}

	// stepPoints builds a step-after polyline (see doc above) for one
	// series, extracted from points via sel, extended flat to end.
	stepPoints := func(sel func(history.DepthPoint) int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "%.1f,%.1f", xAt(points[0].At), yAt(sel(points[0])))
		for i := 1; i < len(points); i++ {
			fmt.Fprintf(&b, " %.1f,%.1f", xAt(points[i].At), yAt(sel(points[i-1])))
			fmt.Fprintf(&b, " %.1f,%.1f", xAt(points[i].At), yAt(sel(points[i])))
		}
		fmt.Fprintf(&b, " %.1f,%.1f", xAt(end), yAt(sel(points[len(points)-1])))
		return b.String()
	}
	waiting := stepPoints(func(p history.DepthPoint) int { return p.Waiting })
	inFlight := stepPoints(func(p history.DepthPoint) int { return p.InFlight })
	parked := stepPoints(func(p history.DepthPoint) int { return p.Parked })

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" width="%d" height="%d" class="depth-chart" role="img" aria-label="queue depth over time">`,
		depthChartW, depthChartH, depthChartW, depthChartH)
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="depth-axis"/>`,
		depthPadL, yAt(0), depthChartW-depthPadR, yAt(0))
	fmt.Fprintf(&b, `<text x="2" y="%.1f" class="depth-label">%d</text>`, yAt(maxV)+8, maxV)
	fmt.Fprintf(&b, `<text x="2" y="%.1f" class="depth-label">0</text>`, yAt(0)+4)
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="depth-label">%s</text>`,
		depthPadL, depthChartH-4, template.HTMLEscapeString(start.UTC().Format("01-02 15:04")))
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="depth-label" text-anchor="end">%s</text>`,
		depthChartW-depthPadR, depthChartH-4, template.HTMLEscapeString(end.UTC().Format("01-02 15:04")))
	fmt.Fprintf(&b, `<polyline points="%s" class="depth-parked"/>`, parked)
	fmt.Fprintf(&b, `<polyline points="%s" class="depth-inflight"/>`, inFlight)
	fmt.Fprintf(&b, `<polyline points="%s" class="depth-waiting"/>`, waiting)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// --- shared view-building helpers -------------------------------------------

// recentRuns fetches target's recent runs for display, rendered as the
// compact outcome-chip strip on / and /t/{target} (chipClass/chipTitle
// below) rather than the old bordered text pill. store == nil (or a query
// error, logged and treated as empty) both render as an ordinary
// empty/disabled section — never an error page. at is the snapshot's "now"
// (snap.At), used to compute each chip's relative-time tooltip.
func (d *dash) recentRuns(target string, limit int, at time.Time) ([]runSummary, bool) {
	if d.store == nil {
		return nil, false
	}
	rows, err := d.store.RecentRuns(target, limit)
	if err != nil {
		log.Printf("dashboard: recent runs %s: %v", target, err)
		return nil, true
	}
	out := make([]runSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, runSummary{
			RunID: row.RunID, User: row.CandidateUser, Topic: row.CandidateTopic,
			ChipClass: outcomeChipClass(row.Outcome),
			Title:     chipTitle(row.Outcome, row.CandidateTopic, row.StartedAt, at),
			Detail:    row.Detail,
			StartedAt: formatTime(row.StartedAt), Duration: formatDuration(row.Duration),
		})
	}
	return out, true
}

func buildInFlight(rs *queue.RunSnapshot, at time.Time) *inFlightView {
	v := &inFlightView{
		Candidate:  rs.Candidate,
		RunID:      rs.RunID,
		BaseOID:    rs.BaseOID,
		MergeSHA:   rs.MergeSHA,
		RunElapsed: formatDuration(at.Sub(rs.StartedAt)),
	}
	for i, cr := range rs.Done {
		v.Done = append(v.Done, checkView{
			Seq: i, Name: cr.Name, Status: checkTag(cr.Status), Duration: formatDuration(cr.Duration), Err: errText(cr.Err),
		})
	}
	if rs.Current != nil {
		v.CurrentName = rs.Current.Name
		v.CurrentElapsed = formatDuration(at.Sub(rs.Current.StartedAt))
	}
	return v
}

// render executes t's "base" template into a buffer first, so a template
// error never leaves a half-written 200 response on the wire.
func render(w http.ResponseWriter, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		log.Printf("dashboard: render: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(10 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// tag is a display word paired with a CSS class (ok/bad/warn/neutral).
type tag struct {
	Word  string
	Class string
}

func wordTag(word string) tag {
	switch word {
	case "landed", "passed":
		return tag{word, "ok"}
	case "rejected", "conflict", "failed", "error":
		return tag{word, "bad"}
	case "skipped":
		return tag{word, "warn"}
	default:
		return tag{word, "neutral"}
	}
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

func checkWord(s core.CheckStatus) string {
	switch s {
	case core.CheckPassed:
		return "passed"
	case core.CheckFailed:
		return "failed"
	case core.CheckSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

func checkTag(s core.CheckStatus) tag { return wordTag(checkWord(s)) }

// outcomeChipClass maps a run outcome word to one of five distinct chip
// colors — unlike wordTag, which collapses rejected/conflict/error into a
// single "bad" for the bordered text pill, the compact outcome-chip strip
// (recentRuns) keeps all five apart: landed=green, rejected=red,
// skipped=yellow, conflict=orange, error=purple.
func outcomeChipClass(word string) string {
	switch word {
	case "landed":
		return "ok"
	case "rejected":
		return "bad"
	case "skipped":
		return "warn"
	case "conflict":
		return "conflict"
	case "error":
		return "error"
	default:
		return "neutral"
	}
}

// chipTitle builds a recent-run chip's title tooltip, e.g.
// "landed · safety-rating · 2m ago".
func chipTitle(outcomeWord, topic string, startedAt, now time.Time) string {
	if topic == "" {
		return fmt.Sprintf("%s · %s", outcomeWord, relAgo(startedAt, now))
	}
	return fmt.Sprintf("%s · %s · %s", outcomeWord, topic, relAgo(startedAt, now))
}

// relAgo renders t relative to now as a short "Nx ago" string.
func relAgo(t, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// shortSHA returns s's first 12 characters (or all of it, if shorter) —
// enough to disambiguate in practice while fitting a card without
// overflowing it (a full 40-char SHA does not). Templates pair this with the
// full value in a title tooltip, e.g. `title="{{.SHA}}"` next to
// `{{shortSHA .SHA}}`.
func shortSHA(s string) string {
	const n = 12
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// compactRef strips a queue-slot ref's "refs/heads/for/" prefix for display
// in space-constrained contexts (tables, cards); the full ref belongs in a
// title tooltip, same pairing as shortSHA.
func compactRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/for/")
}

// --- template view models ----------------------------------------------------

// baseData holds the fields every page template's "base" wrapper needs.
type baseData struct {
	Title       string
	Refresh     bool
	GeneratedAt string
	Starting    bool
}

func newBase(title string, snap *queue.Snapshot, refresh bool) baseData {
	at := time.Now()
	if snap != nil {
		at = snap.At
	}
	return baseData{Title: title, Refresh: refresh, GeneratedAt: formatTime(at)}
}

type indexData struct {
	baseData
	Targets []targetCard
}

type targetCard struct {
	Name, Branch, TargetTip   string
	InFlight                  *inFlightView
	WaitingCount, ParkedCount int
	StoreEnabled              bool
	RecentRuns                []runSummary
}

type inFlightView struct {
	Candidate                   core.Candidate
	RunID, BaseOID, MergeSHA    string
	Done                        []checkView
	CurrentName, CurrentElapsed string
	RunElapsed                  string
}

type checkView struct {
	Seq           int
	Name          string
	Status        tag
	Duration, Err string

	// Output is the check's captured output (history.CheckRow.Output);
	// empty for in-flight "Done" checks (buildInFlight doesn't populate it —
	// only /run/{id} renders per-check output). Run.html renders it in a
	// <details> body when non-empty, nothing when empty.
	Output string
	// Open is whether that <details> should start expanded: true for
	// failed/errored checks, so the answer to "how did it fail" is visible
	// without a click.
	Open bool
}

// runSummary is one row of a target's recent-run history: the compact
// outcome-chip strip on / and /t/{target} (ChipClass/Title), not the old
// bordered text pill.
type runSummary struct {
	RunID, User, Topic  string
	ChipClass           string // ok|bad|warn|conflict|error|neutral -> chip-<class>
	Title               string // tooltip: "landed · topic · 2m ago"
	Detail              string
	StartedAt, Duration string
}

type targetData struct {
	baseData
	Name, Branch, TargetTip string
	InFlight                *inFlightView
	Waiting                 []waitingView
	Parked                  []parkedView
	StoreEnabled            bool
	RecentRuns              []runSummary
}

type waitingView struct {
	Seq                   int64
	Ref, User, Topic, SHA string
}

type parkedView struct {
	User, Topic, SHA string
	Outcome          tag
	Reason, At       string
}

type runData struct {
	baseData
	StoreEnabled bool
	Run          runSummaryFull
	Checks       []checkView
}

type runSummaryFull struct {
	RunID, Target                string
	Ref, User, Topic, SHA        string
	BaseOID, MergeSHA            string
	TrialClean                   bool
	Outcome                      tag
	Detail                       string
	StartedAt, EndedAt, Duration string
}

type checksData struct {
	baseData
	StoreEnabled     bool
	Target           string
	Since            string
	AvailableTargets []string
	Stats            []statView
	// HasDepth is len(depth series) > 0; DepthChart is only meaningful (a
	// non-empty rendered <svg>) when this is true — otherwise the template
	// renders "no data yet" instead.
	HasDepth   bool
	DepthChart template.HTML
}

type statView struct {
	Name          string
	Total, Failed int
	RedRate       string
	Avg, Max      string
}
