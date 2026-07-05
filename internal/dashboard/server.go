// Package dashboard implements gauntlet's read-only web dashboard
// (docs/plans/phase23.md §4.2): stdlib net/http + html/template over two
// data sources — a live queue.Snapshot published by the reconcile loop, and
// a *history.Store for run history. Both sources are optional at the type
// level: snapshot may return nil (no reconcile pass has completed yet) and
// store may be nil (history disabled); every route degrades to a friendly
// message rather than panicking or 500ing in either case.
//
// There are no forms and no mutating routes — every handler here only
// reads. Auth is out of scope (documented in DESIGN.md); bind to a trusted
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
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// New builds the dashboard's http.Handler. snapshot is called on every
// request that needs live queue state (typically Daemon.Snapshot); store
// may be nil, in which case every history-backed section renders "history
// disabled" instead of querying it.
func New(snapshot func() *queue.Snapshot, store *history.Store) http.Handler {
	d := &dash{snapshot: snapshot, store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.handleIndex)
	mux.HandleFunc("GET /t/{target}", d.handleTarget)
	mux.HandleFunc("GET /run/{runID}", d.handleRun)
	mux.HandleFunc("GET /checks", d.handleChecks)
	return mux
}

type dash struct {
	snapshot func() *queue.Snapshot
	store    *history.Store
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
		card.RecentRuns, card.StoreEnabled = d.recentRuns(ts.Name, 6)
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

	data.RecentRuns, data.StoreEnabled = d.recentRuns(ts.Name, 25)
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
		})
	}
	render(w, runTmpl, data)
}

// --- /checks?target=&since= --------------------------------------------------

func (d *dash) handleChecks(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	since := parseSince(r.URL.Query().Get("since"))

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
	for _, p := range depth {
		data.Depth = append(data.Depth, depthView{
			At: formatTime(p.At), Waiting: p.Waiting, InFlight: p.InFlight, Parked: p.Parked,
		})
	}
	render(w, checksTmpl, data)
}

// parseSince interprets the "since" query param: a Go duration ("24h") is
// relative to now; an RFC3339 timestamp is absolute; anything else
// (including empty) falls back to a 7-day window.
func parseSince(s string) time.Time {
	def := time.Now().Add(-7 * 24 * time.Hour)
	if s == "" {
		return def
	}
	if dur, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-dur)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
}

// --- shared view-building helpers -------------------------------------------

// recentRuns fetches target's recent runs for display. store == nil (or a
// query error, logged and treated as empty) both render as an ordinary
// empty/disabled section — never an error page.
func (d *dash) recentRuns(target string, limit int) ([]runSummary, bool) {
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
			Outcome: wordTag(row.Outcome), Detail: row.Detail,
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
}

type runSummary struct {
	RunID, User, Topic  string
	Outcome             tag
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
	Depth            []depthView
}

type statView struct {
	Name          string
	Total, Failed int
	RedRate       string
	Avg, Max      string
}

type depthView struct {
	At                        string
	Waiting, InFlight, Parked int
}
