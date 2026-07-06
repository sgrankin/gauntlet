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
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

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
	mux.HandleFunc("GET /run/{runID}/log/{checkName}", d.handleRunLog)
	mux.HandleFunc("GET /batch/{batchID}", d.handleBatch)
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

	// version is empty unless New was called with WithVersion; the footer
	// omits its version line in that case (api.go's WithVersion doc).
	version string

	// logRoot is empty unless New was called with WithLogRoot; both
	// GET /run/{id}/log/{check} and every logUrl field this package emits
	// (run.html, GET /api/v1/run/{id}) stay disabled/absent in that case —
	// see WithLogRoot's doc (api.go) and handleRunLog below.
	logRoot string

	// hookCancel is nil unless New was called with WithHookCancel: POST
	// /api/v1/hooks/cancel responds 503 "hooks disabled" in that case (api.go).
	hookCancel func(target string) bool

	// hookSnapshot is nil unless New was called with WithHookSnapshot: both
	// GET /api/v1/status's per-target liveHook field and the target page's
	// "Post-land hooks" live section simply omit live-hook data in that case
	// (S5-surface, api.go's WithHookSnapshot doc).
	hookSnapshot func(target string) (LiveHook, bool)
}

// --- / --------------------------------------------------------------------

func (d *dash) handleIndex(w http.ResponseWriter, r *http.Request) {
	snap := d.snapshot()
	data := indexData{baseData: d.newBase("gauntlet", snap, true)}
	if snap == nil {
		data.Starting = true
		render(w, indexTmpl, data)
		return
	}

	for _, ts := range snap.Targets {
		card := targetCard{
			Name:          ts.Name,
			Branch:        ts.Branch,
			TargetTip:     orDash(ts.TargetTip),
			WaitingCount:  len(ts.Waiting),
			ParkedCount:   len(ts.Parked),
			PipelineDepth: len(ts.Pipeline),
		}
		if ts.InFlight != nil {
			card.InFlight = buildInFlight(ts.InFlight, snap.At)
		}
		card.RecentRuns, card.StoreEnabled = d.recentRuns(ts.Name, 6, snap.At)
		data.Targets = append(data.Targets, card)
	}

	// Recently ignored refs (S7c): a DAEMON-level section, not per-target —
	// an ignored ref's defining property is that its target segment names no
	// configured target (that's why it was ignored), so it can't belong to
	// any target page. A query error degrades to "no section" (logged),
	// same convention as recentRuns.
	if d.store != nil {
		if refs, err := d.store.IgnoredRefs(ignoredRefsLimit); err != nil {
			log.Printf("dashboard: index: ignored refs: %v", err)
		} else {
			for _, ir := range refs {
				data.IgnoredRefs = append(data.IgnoredRefs, ignoredRefView{Ref: ir.Ref, Detail: ir.Detail, At: formatTime(ir.At)})
			}
		}
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
		b := d.newBase(name, snap, true)
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
		baseData:  d.newBase(ts.Name, snap, true),
		Name:      ts.Name,
		Branch:    ts.Branch,
		TargetTip: orDash(ts.TargetTip),
	}
	if ts.InFlight != nil {
		data.InFlight = buildInFlight(ts.InFlight, snap.At)
	}

	// Render the stacked pipeline view (docs/plans/phase5.md §3.4, §10
	// amendment 5's "P5-H" chunk) only when there's something a single
	// InFlight card can't show: more than one run in flight (speculation)
	// or the head run has more than one member (batch). A single-run,
	// single-member lane — today's only shape and every existing serial
	// test's fixture — leaves data.Pipeline nil, so the template falls
	// through to the unchanged .InFlight branch: no visual regression.
	if len(ts.Pipeline) > 1 || (len(ts.Pipeline) == 1 && len(ts.Pipeline[0].Members) > 1) {
		for i := range ts.Pipeline {
			data.Pipeline = append(data.Pipeline, buildPipelineRun(&ts.Pipeline[i], snap.At))
		}
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
			RunID: pe.RunID,
		})
	}

	data.RecentRuns, data.StoreEnabled = d.recentRuns(ts.Name, 25, snap.At)

	// Post-land hooks (S5-surface): live state comes from the hookSnapshot
	// closure (nil-safe, WithHookSnapshot), durable state from the history
	// store (nil-safe; a query error degrades to "log and omit" rather than
	// failing the whole page — same convention as recentRuns/Hooks above).
	// Ignored refs are deliberately NOT here: they're daemon-level (their
	// target segment names no configured target), rendered on the index page
	// — see handleIndex.
	if d.hookSnapshot != nil {
		if lh, ok := d.hookSnapshot(ts.Name); ok {
			data.LiveHook = buildLiveHookView(lh, snap.At)
		}
	}
	if d.store != nil {
		if runs, err := d.store.HookRunSummaries(ts.Name, hookRunsLimit); err != nil {
			log.Printf("dashboard: target %s: hook run summaries: %v", ts.Name, err)
		} else {
			for _, hr := range runs {
				data.HookRuns = append(data.HookRuns, buildHookRunView(hr))
			}
		}
	}

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
		render(w, runTmpl, runData{baseData: d.newBase(runID, nil, false)})
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
		baseData:     d.newBase(row.RunID, nil, false),
		StoreEnabled: true,
		Run: runSummaryFull{
			RunID: row.RunID, Target: row.Target,
			Ref: row.CandidateRef, User: row.CandidateUser, Topic: row.CandidateTopic, SHA: row.CandidateSHA,
			BaseOID: row.BaseOID, MergeSHA: row.MergeSHA, TrialClean: row.TrialClean,
			Outcome: wordTag(row.Outcome), Detail: row.Detail,
			StartedAt: formatTime(row.StartedAt), EndedAt: formatTime(row.EndedAt), Duration: formatDuration(row.Duration),
			BatchID: row.BatchID,
		},
	}
	if row.BatchID != "" {
		// "k of n": Position is 0-based, displayed 1-based.
		data.Run.BatchPosition = fmt.Sprintf("%d of %d", row.Position+1, row.BatchSize)
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
			// LogURL is only populated when there's both a stored log file
			// (c.LogPath != "") and a configured containment root to serve it
			// from (d.logRoot != "", WithLogRoot) — see runLogURL.
			LogURL: d.runLogURL(row.RunID, c.Name, c.LogPath),
		})
	}

	// Hooks (internal/hooks; log/history parity, DESIGN.md's decision
	// ledger "Deployments as post-land hooks"): a query error degrades to
	// "no hooks" rather than failing the whole page — the run row and its
	// checks already rendered successfully above, and a hooks-table hiccup
	// shouldn't take that down with it. run.html only renders the Hooks
	// section at all when data.Hooks is non-empty (chunk P5-B requirement:
	// "rendered only when rows exist").
	hooks, err := d.store.Hooks(runID)
	if err != nil {
		log.Printf("dashboard: run %s: hooks: %v", runID, err)
	}
	for _, h := range hooks {
		data.Hooks = append(data.Hooks, checkView{
			Seq: h.Seq, Name: h.Name, Status: wordTag(h.Status), Duration: formatDuration(h.Duration), Err: h.Err,
			Output: h.Output,
			Open:   h.Status == "failed" || h.Err != "",
			// runLogURL is name-agnostic (checks vs. hooks): handleRunLog
			// below looks a checkName up in checks first, then hooks, so the
			// same relative link shape works for either.
			LogURL: d.runLogURL(row.RunID, h.Name, h.LogPath),
		})
	}

	render(w, runTmpl, data)
}

// --- /run/{runID}/log/{checkName} --------------------------------------------

// runLogURL builds the relative link to a check's full log
// (GET /run/{runID}/log/{checkName}), or "" when full-log serving isn't
// meaningful: no log file was ever written for this check (logPath == "")
// or the dashboard has no LogRoot configured (WithLogRoot never called),
// in which case handleRunLog would always 404 anyway. checkName is
// path-escaped since check names are free-form (a repo-owned check spec),
// not guaranteed to be URL-safe as a single path segment.
func (d *dash) runLogURL(runID, checkName, logPath string) string {
	if logPath == "" || d.logRoot == "" {
		return ""
	}
	return "/run/" + url.PathEscape(runID) + "/log/" + url.PathEscape(checkName)
}

// handleRunLog serves one check's full per-check log file (DESIGN.md "Full
// per-check log files"): GET /run/{runID}/log/{checkName}. The run/check
// must exist in history and have a non-empty stored LogPath, and — the
// containment check — that path, once resolved, must live under d.logRoot
// (WithLogRoot). Every failure mode (nil store, unknown run, unknown check,
// no LogPath, containment violation, or the file missing/pruned) renders
// the same friendly 404 rather than distinguishing attacker-relevant detail
// in the response.
func (d *dash) handleRunLog(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		notFoundPrunedOrMissing(w)
		return
	}

	runID := r.PathValue("runID")
	checkName := r.PathValue("checkName")

	_, checks, err := d.store.Run(runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFoundPrunedOrMissing(w)
			return
		}
		log.Printf("dashboard: run log: run %s: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var storedPath string
	for _, c := range checks {
		if c.Name == checkName {
			storedPath = c.LogPath
			break
		}
	}
	// Not a check by that name (or it has no LogPath) — try the run's hooks
	// next (internal/hooks; log/history parity): the route is shared between
	// checks and hooks, so a hook's full log is served through the exact
	// same GET /run/{id}/log/{name} URL a check's is.
	if storedPath == "" {
		hooks, err := d.store.Hooks(runID)
		if err != nil {
			log.Printf("dashboard: run log: hooks %s: %v", runID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, h := range hooks {
			if h.Name == checkName {
				storedPath = h.LogPath
				break
			}
		}
	}
	if storedPath == "" {
		notFoundPrunedOrMissing(w)
		return
	}

	resolved, ok := resolveLogPath(d.logRoot, storedPath)
	if !ok {
		// Either LogRoot isn't configured, or the stored path (cleaned and
		// symlink-resolved) doesn't live under it. Same 404 either way —
		// see handleRunLog's doc.
		notFoundPrunedOrMissing(w)
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		// Most commonly the file was pruned (retention swept its run-log
		// directory) since the row was written; any other Open failure
		// degrades to the same friendly message rather than a 500, since
		// there's nothing an operator-facing 500 would let a viewer act on.
		notFoundPrunedOrMissing(w)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		notFoundPrunedOrMissing(w)
		return
	}
	if info.IsDir() {
		// A stored path that resolves to a directory (e.g. LogRoot itself,
		// or a wrong-depth path from a buggy writer) is never a servable
		// log; ServeContent on a directory handle would produce a confusing
		// read error rather than a clean 404.
		notFoundPrunedOrMissing(w)
		return
	}

	// text/plain + nosniff: this is a raw command-output log, never
	// executed or rendered as HTML by the browser regardless of what a
	// check's output happens to contain.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// ".log.zst" (internal/queue/reconcile.go's job.LogPath assignment,
	// internal/executor/logfile.go's openCheckLog) is a single zstd stream:
	// decompress it to the response as we go. http.ServeContent needs a
	// ReadSeeker to support Range requests and If-Modified-Since, which a
	// decompressing reader can't provide, so this path is a plain io.Copy
	// with the headers set explicitly above instead. Legacy plain ".log"
	// rows (written before this change) still go through ServeContent
	// below unchanged.
	if strings.HasSuffix(resolved, ".log.zst") {
		dec, err := zstd.NewReader(f)
		if err != nil {
			notFoundPrunedOrMissing(w)
			return
		}
		defer dec.Close()
		if _, err := io.Copy(w, dec); err != nil {
			// A truncated final frame (the daemon/check killed mid-write —
			// openCheckLog's doc: losing/truncating the log must never
			// fail the check itself) decompresses partially and then
			// errors here. The response has already been partially
			// written by this point, so there's no clean way to turn this
			// into an error status for the client; log it server-side and
			// let the client see whatever decompressed cleanly, which is
			// strictly more useful than hanging or serving nothing for a
			// supplementary, best-effort log view.
			log.Printf("dashboard: run log: %s: decompress: %v", resolved, err)
		}
		return
	}

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

// notFoundPrunedOrMissing is handleRunLog's uniform failure response: a
// plain-text 404 body distinguishing "the log is gone" from an ordinary
// route-not-found 404, for every failure mode handleRunLog has (nil store,
// unknown run/check, no LogPath, containment violation, or a stat/open
// failure) — see handleRunLog's doc for why these don't get distinguished
// further.
func notFoundPrunedOrMissing(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "log pruned or missing\n")
}

// resolveLogPath validates storedPath against logRoot (WithLogRoot) and
// returns the path handleRunLog should actually open. Containment is
// checked twice, each time comparing two paths run through the *same*
// resolution so the comparison is apples-to-apples:
//
//  1. cleaned-but-unresolved root vs. cleaned-but-unresolved storedPath —
//     catches a stored path that simply isn't textually under logRoot.
//  2. symlink-resolved root vs. symlink-resolved storedPath — catches a
//     path that only escapes logRoot via a symlink component.
//
// per DESIGN.md/the work order, "reject any stored path that, after
// filepath.Clean/EvalSymlinks, does not live under LogRoot". Resolving only
// one side (e.g. comparing a symlink-resolved root against an unresolved
// storedPath) would misfire on any host where the root itself sits behind a
// symlink — notably macOS, where t.TempDir()/os.TempDir() live under
// /tmp, itself a symlink to /private/tmp: resolving root but not storedPath
// would make every legitimately-contained path fail containment.
//
// A storedPath whose file has already been pruned is expected and common
// (retention deletes the run-log directory, not the history row): in that
// case EvalSymlinks fails with a not-exist error, and this returns the
// cleaned (unresolved) path with ok=true, deferring the actual
// missing-file 404 to the caller's os.Open/Stat — "pruned" and "escaped
// containment" are different failure modes even though handleRunLog
// currently renders them identically.
func resolveLogPath(logRoot, storedPath string) (path string, ok bool) {
	if logRoot == "" || storedPath == "" {
		return "", false
	}

	root, err := filepath.Abs(logRoot)
	if err != nil {
		return "", false
	}
	root = filepath.Clean(root)

	clean := filepath.Clean(storedPath)
	if !filepath.IsAbs(clean) {
		return "", false
	}
	// Step 1: unresolved-vs-unresolved, both in the same (possibly
	// symlinked) coordinate system.
	if !pathUnder(root, clean) {
		return "", false
	}

	resolvedRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		resolvedRoot = r
	}
	// A logRoot that doesn't exist yet (fresh state dir, nothing pruned or
	// served yet) leaves resolvedRoot at the cleaned-but-unresolved absolute
	// path — EvalSymlinks failing here is not itself a containment
	// violation, only a reason not to trust symlink resolution of anything
	// under it either.

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Most commonly: pruned. Return the cleaned, contained path — the
		// caller's os.Open reports the real "missing" failure.
		return clean, true
	}
	// Step 2: resolved-vs-resolved, both run through EvalSymlinks.
	if !pathUnder(resolvedRoot, resolved) {
		return "", false
	}
	return resolved, true
}

// pathUnder reports whether path is root itself or lives strictly beneath
// it, using filepath.Rel rather than a string-prefix check so "/a/bb" is
// never mistaken for being under "/a/b".
func pathUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// --- /batch/{batchID} ---------------------------------------------------------

// handleBatch renders the members of one batch run (docs/plans/phase5.md
// §10 amendment 1): the link from /run/{id}'s "landed in batch <id> (k of
// n)" line. Each member is its own history row (§3.3: per-member RunRecords
// sharing a BatchID), so this is a small listing over history.BatchMembers,
// not a new data source.
func (d *dash) handleBatch(w http.ResponseWriter, r *http.Request) {
	batchID := r.PathValue("batchID")
	if d.store == nil {
		render(w, batchTmpl, batchData{baseData: d.newBase("batch", nil, false), BatchID: batchID})
		return
	}

	members, err := d.store.BatchMembers(batchID)
	if err != nil {
		log.Printf("dashboard: batch %s: %v", batchID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(members) == 0 {
		http.NotFound(w, r)
		return
	}

	data := batchData{
		baseData:     d.newBase("batch "+batchID, nil, false),
		BatchID:      batchID,
		StoreEnabled: true,
	}
	for _, m := range members {
		data.Members = append(data.Members, batchMemberView{
			RunID: m.RunID, Position: m.Position, Target: m.Target,
			User: m.CandidateUser, Topic: m.CandidateTopic, SHA: m.CandidateSHA,
			Outcome: wordTag(m.Outcome), Detail: m.Detail,
			StartedAt: formatTime(m.StartedAt), Duration: formatDuration(m.Duration),
		})
	}
	render(w, batchTmpl, data)
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
		baseData: d.newBase("checks", nil, false),
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
			Batched: row.BatchID != "",
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

// buildPipelineRun builds one pipelineRunView from a RunSnapshot for the
// target page's stacked pipeline list (docs/plans/phase5.md §3.4): every
// member (topic/user, short SHA), the predicted/batch badges the template
// renders from Predicted/len(Members), and the same per-run check-progress
// fields buildInFlight already computes for the single-run case.
func buildPipelineRun(rs *queue.RunSnapshot, at time.Time) pipelineRunView {
	v := pipelineRunView{
		RunID:      rs.RunID,
		BaseOID:    rs.BaseOID,
		ChainTip:   rs.ChainTip,
		Predicted:  rs.Predicted,
		BatchID:    rs.BatchID,
		RunElapsed: formatDuration(at.Sub(rs.StartedAt)),
	}
	for _, m := range rs.Members {
		v.Members = append(v.Members, memberView{User: m.User, Topic: m.Topic, SHA: m.SHA})
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

// buildLiveHookView builds one liveHookView from a LiveHook snapshot
// (WithHookSnapshot), computing its elapsed-since-start string the same way
// buildInFlight/buildPipelineRun do for a check's current-elapsed field.
func buildLiveHookView(lh LiveHook, at time.Time) *liveHookView {
	v := &liveHookView{
		Running: lh.Running, CurrentHook: lh.CurrentHook,
		HookIndex: lh.HookIndex, HookCount: lh.HookCount,
		BacklogDepth: lh.BacklogDepth,
	}
	if lh.Running && !lh.StartedAt.IsZero() {
		v.Elapsed = formatDuration(at.Sub(lh.StartedAt))
	}
	return v
}

// buildHookRunView builds one hookRunView from a history.HookRunSummary row,
// computing Incomplete the same way api.go's hookRunStatus does (OwedCount >
// DoneCount && !Skipped — a crash-incomplete hook chain, S1-C).
func buildHookRunView(hr history.HookRunSummary) hookRunView {
	return hookRunView{
		RunID: hr.RunID, Owed: hr.OwedCount, Done: hr.DoneCount,
		StartedAt: formatTime(hr.StartedAt), Skipped: hr.Skipped, SkipReason: hr.SkipReason,
		Incomplete: hr.OwedCount > hr.DoneCount && !hr.Skipped,
	}
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

// formatTime renders t as a <time> element carrying both a machine-readable
// RFC3339 UTC instant (its datetime attribute, for base.html's tooltip
// script) and the same "2006-01-02 15:04:05 UTC" text this used to return
// bare — the visible text is unchanged, only the wrapper is new. Both parts
// come straight from t itself, never from user input, so building this with
// Sprintf rather than the html/template escaper is safe; returning
// template.HTML is exactly what tells html/template to trust that and emit
// it unescaped. A zero Time (no value recorded) renders as plain "-" text,
// not a <time> with nothing to date.
func formatTime(t time.Time) template.HTML {
	if t.IsZero() {
		return "-"
	}
	u := t.UTC()
	return template.HTML(fmt.Sprintf(`<time datetime="%s">%s</time>`, u.Format(time.RFC3339), u.Format("2006-01-02 15:04:05 UTC")))
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
	GeneratedAt template.HTML
	Starting    bool
	Version     string
}

// newBase is a method (rather than a free function) only so it can reach
// d.version — the gauntlet version string set via WithVersion (api.go),
// shown in every page's footer. Empty unless the option was used.
func (d *dash) newBase(title string, snap *queue.Snapshot, refresh bool) baseData {
	at := time.Now()
	if snap != nil {
		at = snap.At
	}
	return baseData{Title: title, Refresh: refresh, GeneratedAt: formatTime(at), Version: d.version}
}

type indexData struct {
	baseData
	Targets []targetCard

	// IgnoredRefs is recently pushed refs naming no configured target
	// (history.Store.IgnoredRefs, S7c), newest first, daemon-wide — a
	// daemon-level section on the index page, not per-target, since an
	// ignored ref by definition belongs to no configured target. Nil when
	// history is disabled or the query errors (logged, degraded to "no
	// section").
	IgnoredRefs []ignoredRefView
}

type targetCard struct {
	Name, Branch, TargetTip   string
	InFlight                  *inFlightView
	WaitingCount, ParkedCount int
	StoreEnabled              bool
	RecentRuns                []runSummary

	// PipelineDepth is len(TargetSnapshot.Pipeline): 0 when idle, 1 for
	// today's ordinary single in-flight run, >1 once speculation lands
	// (docs/plans/phase5.md §3.4). index.html only changes its rendering
	// (the in-flight cell becomes "N in flight") when this is >1 — a
	// single in-flight run renders exactly as before.
	PipelineDepth int
}

type inFlightView struct {
	Candidate                   core.Candidate
	RunID, BaseOID, MergeSHA    string
	Done                        []checkView
	CurrentName, CurrentElapsed string
	RunElapsed                  string
}

// pipelineRunView is one run within a target's stacked pipeline list
// (docs/plans/phase5.md §3.4): target.html renders one of these per element
// of targetData.Pipeline, head first. Predicted/len(Members) drive the
// "on predicted base" / "batch of N" badges directly in the template.
type pipelineRunView struct {
	RunID, BaseOID, ChainTip string
	Predicted                bool
	BatchID                  string
	Members                  []memberView

	Done                        []checkView
	CurrentName, CurrentElapsed string
	RunElapsed                  string
}

// memberView is one candidate within a pipelineRunView's Members list: just
// enough to render "user/topic (shortSHA)" per member.
type memberView struct {
	User, Topic, SHA string
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

	// LogURL is the relative link to this check's full per-check log
	// (GET /run/{id}/log/{name}), or "" when there's nothing to link:
	// either no log file was ever written (history.CheckRow.LogPath == "")
	// or the dashboard has no LogRoot configured (WithLogRoot). Only
	// handleRun populates this — buildInFlight's in-flight "Done" checks
	// never have a history row yet, so their LogURL is always "".
	LogURL string
}

// runSummary is one row of a target's recent-run history: the compact
// outcome-chip strip on / and /t/{target} (ChipClass/Title), not the old
// bordered text pill.
type runSummary struct {
	RunID, User, Topic string
	ChipClass          string // ok|bad|warn|conflict|error|neutral -> chip-<class>
	Title              string // tooltip: "landed · topic · 2m ago"
	Detail             string
	StartedAt          template.HTML
	Duration           string

	// Batched is whether this run landed as part of a batch (RunRow.BatchID
	// != "") — the recent-runs chip strip (index.html, target.html) adds a
	// distinguishing ring around a batched chip (CSS .chip-batched) so a
	// batch's members read as visually grouped without redesigning the
	// strip. Purely cosmetic; the underlying data is BatchID itself
	// (exposed via the API/MCP fields, not this view struct).
	Batched bool
}

type targetData struct {
	baseData
	Name, Branch, TargetTip string
	InFlight                *inFlightView
	Waiting                 []waitingView
	Parked                  []parkedView
	StoreEnabled            bool
	RecentRuns              []runSummary

	// Pipeline is non-nil only when there's more than one in-flight run or
	// the head run has more than one member (docs/plans/phase5.md §3.4):
	// see handleTarget's doc. target.html renders this stacked list instead
	// of the plain .InFlight card when it's set; a single-run single-member
	// lane leaves this nil and renders exactly as before.
	Pipeline []pipelineRunView

	// LiveHook is this target's current post-land hook progress
	// (WithHookSnapshot), nil when no hook is running right now or
	// WithHookSnapshot was never wired up (S5-surface).
	LiveHook *liveHookView

	// HookRuns is the durable hook-run ledger (history.Store.
	// HookRunSummaries, S1-C/S5), newest first, nil when history is disabled
	// or the query errors (logged, degraded to "none" — matching
	// RecentRuns/Hooks's own error-handling convention). Ignored refs are
	// NOT here: they're a daemon-level index-page section (indexData.
	// IgnoredRefs), since an ignored ref belongs to no configured target.
	HookRuns []hookRunView
}

// liveHookView is target.html's view of one target's LiveHook.
type liveHookView struct {
	Running              bool
	CurrentHook          string
	HookIndex, HookCount int
	Elapsed              string
	BacklogDepth         int
}

// hookRunView is target.html's view of one history.HookRunSummary row.
type hookRunView struct {
	RunID      string
	Owed, Done int
	StartedAt  template.HTML
	Skipped    bool
	SkipReason string
	Incomplete bool
}

// ignoredRefView is index.html's view of one history.IgnoredRef row (the
// daemon-level "recently ignored refs" section).
type ignoredRefView struct {
	Ref, Detail string
	At          template.HTML
}

type waitingView struct {
	Seq                   int64
	Ref, User, Topic, SHA string
}

// parkedView is target.html's view of one queue.ParkedEntry: RunID (the
// terminal run that parked this candidate) can be "" when the queue never
// had one to plumb through — history disabled at boot-seed time, or a park
// entry seeded from a pre-RunID history row — in which case target.html
// renders the outcome tag unlinked rather than pointing /run/ at an ID that
// doesn't exist. But RunID alone isn't enough to link safely: a LIVE park
// (queue.Daemon.park, independent of whether THIS dashboard has a store)
// always sets it, even when the dashboard itself was built with store == nil
// — handleRun 404s unconditionally in that case (no store to look the run up
// in), so target.html's link must also check targetData.StoreEnabled, not
// RunID alone.
type parkedView struct {
	User, Topic, SHA string
	Outcome          tag
	Reason           string
	At               template.HTML
	RunID            string
}

type runData struct {
	baseData
	StoreEnabled bool
	Run          runSummaryFull
	Checks       []checkView
	// Hooks holds this run's post-land hook results (internal/hooks), same
	// view shape as Checks. Empty (nil) whenever the run landed no hooks —
	// no target hooks configured, or the run never reached hooks at all —
	// in which case run.html omits the Hooks section entirely rather than
	// rendering an empty one.
	Hooks []checkView
}

type runSummaryFull struct {
	RunID, Target         string
	Ref, User, Topic, SHA string
	BaseOID, MergeSHA     string
	TrialClean            bool
	Outcome               tag
	Detail                string
	StartedAt, EndedAt    template.HTML
	Duration              string

	// BatchID is this run's batch, empty for serial/speculate — run.html
	// only renders the "landed in batch <id>" line when it's non-empty.
	// BatchPosition is the pre-formatted "k of n" string (Position+1 of
	// BatchSize), computed by handleRun rather than in the template.
	BatchID       string
	BatchPosition string
}

// batchData is /batch/{id}'s view model: the members of one batch run
// (docs/plans/phase5.md §10 amendment 1), linked from /run/{id}.
type batchData struct {
	baseData
	BatchID      string
	StoreEnabled bool
	Members      []batchMemberView
}

type batchMemberView struct {
	RunID, Target    string
	Position         int
	User, Topic, SHA string
	Outcome          tag
	Detail           string
	StartedAt        template.HTML
	Duration         string
}

type checksData struct {
	baseData
	StoreEnabled     bool
	Target           string
	Since            template.HTML
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
