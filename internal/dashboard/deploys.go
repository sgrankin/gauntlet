package dashboard

// The two deploy pages (docs/design/deployment.md, "Surfaces"): /deploys,
// the per-environment overview, and /deploy/{runID}, the per-run detail page
// — plus the full per-node log route beside it.
//
// Both read from two independent sources and neither is required: the live
// deploy Snapshot (WithDeploySnapshot, absent when no `deploy` block is
// configured) and the history store (absent when history is disabled). Every
// combination degrades to a plain, honest page rather than a 500 — the same
// contract server.go's queue pages already hold, with one extra axis because
// the whole subsystem is optional.
//
// The division of labor between the two sources is the design's, not a
// convenience: the OVERVIEW is derivable from refs alone, so it renders from
// the Snapshot with history only enriching it (per-node detail for a
// finished run, the recent-deploy chips). Detail PAGES are history's alone —
// lose the database and you lose old detail pages, nothing else.

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/history"
)

// --- /deploys -----------------------------------------------------------------

func (d *dash) handleDeploys(w http.ResponseWriter, r *http.Request) {
	// No tracker at all: a 200 saying so, never a 404 — the route exists in
	// every build, and "this daemon doesn't deploy" is an answer, not an
	// error. The nav entry is already hidden (baseData.HasDeploys), so the
	// only way here is a typed URL or an old bookmark.
	if d.deploySnapshot == nil {
		render(w, deploysTmpl, deploysData{baseData: d.newBase("deploys", nil, false)})
		return
	}

	snap := d.deploySnapshot()
	if snap == nil {
		// Configured, but no reconcile pass has published yet: the same
		// Starting treatment the index page gives a nil queue snapshot,
		// refreshing so the page fills itself in when the first pass lands.
		b := d.newBase("deploys", nil, true)
		b.Starting = true
		render(w, deploysTmpl, deploysData{baseData: b, Configured: true})
		return
	}

	data := deploysData{
		baseData:    d.newBase("deploys", nil, true),
		Configured:  true,
		SnapshotAt:  formatTime(snap.At),
		SourceChain: buildSourceChainSVG(snap.Lanes),
	}
	data.StoreEnabled = d.store != nil
	for _, lane := range snap.Lanes {
		data.Lanes = append(data.Lanes, d.buildDeployCard(lane, snap.At))
	}

	// Recent deploys: live lanes first (a run in flight has no history row
	// yet — it hasn't finished — so without this the table would silently
	// omit the very deploy the page exists to watch), then the finished
	// rows underneath.
	for _, lane := range snap.Lanes {
		if lane.Running != nil {
			data.Recent = append(data.Recent, liveDeployRow(lane, snap.At))
		}
	}
	if d.store != nil {
		rows, err := d.store.RecentDeploys("", recentDeploysLimit)
		if err != nil {
			log.Printf("dashboard: deploys: recent: %v", err)
		} else {
			for _, row := range rows {
				data.Recent = append(data.Recent, d.buildDeployRowView(row, snap.At))
			}
		}
	}
	render(w, deploysTmpl, data)
}

// buildDeployCard folds one lane (plus, when history is on, its own recent
// deploys) into one environment card.
func (d *dash) buildDeployCard(lane DeployLane, at time.Time) deployCardView {
	card := deployCardView{
		Env:    lane.Env,
		State:  deployStateTag(lane),
		Source: lane.Source,
		Mode:   lane.Mode,
		InSync: lane.InSync,

		Desired:      shortSHA(lane.Desired),
		DesiredFull:  lane.Desired,
		Observed:     shortSHA(lane.Observed),
		ObservedFull: lane.Observed,

		LastError: lane.LastError,
	}
	if lane.Running != nil {
		card.RunID = lane.Running.RunID
		card.RunElapsed = formatDuration(at.Sub(lane.Running.StartedAt))
	}
	if lane.Parked != nil {
		// The retry button appears ONLY on a parked lane, because that is
		// the only state retry means anything in: a park is level-triggered
		// state about one revision, cleared by a new desired SHA or an
		// explicit retry and nothing else.
		card.ShowRetry = true
		card.ParkDetail = lane.Parked.Detail
	}

	var histNodes []history.CheckRow
	if lr := lane.LastResult; lr != nil {
		card.LastRunID = lr.RunID
		card.LastOutcome = wordTag(deployOutcomeWord(lr.Outcome))
		card.LastAgo = relAgo(lr.EndedAt, at)
		card.LastDuration = formatDuration(lr.EndedAt.Sub(lr.StartedAt))
		card.LastSummary = deployResultSummary(lr)
		if d.store != nil {
			if _, nodes, err := d.store.Deploy(lr.RunID); err == nil {
				histNodes = nodes
			} else if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("dashboard: deploys: nodes for %s: %v", lr.RunID, err)
			}
		}
	}
	card.Nodes, card.NodeSource = deployNodeRows(lane, histNodes)

	if d.store != nil {
		rows, err := d.store.RecentDeploys(lane.Env, laneDeployChips)
		if err != nil {
			log.Printf("dashboard: deploys: recent %s: %v", lane.Env, err)
		} else {
			for _, row := range rows {
				card.Recent = append(card.Recent, deployChipView{
					RunID:     row.RunID,
					ChipClass: outcomeChipClass(row.Outcome),
					Title:     deployChipTitle(row, at),
				})
			}
		}
	}
	return card
}

// deployNodeRows builds a card's per-node strip, and reports WHICH source it
// came from ("live", "history", or "" for neither) so the template can fall
// back to the last run's one-line summary when there are no rows at all.
//
// The precedence is the page's whole point — what is happening now beats
// what happened last:
//
//  1. LIVE: the lane's in-flight run publishes the node names executing
//     right this instant (deploy.LaneRun.Nodes, a gauge rather than a
//     record), rendered as pulsing running chips.
//  2. HISTORY: no run in flight, but the last finished one has stored node
//     rows — the full picture including the blocked rows that name their
//     failed edges, which is exactly what a parked lane needs shown.
//  3. NEITHER: history disabled, or the run is winding down with no node
//     executing at this instant. The template renders the summary line.
func deployNodeRows(lane DeployLane, hist []history.CheckRow) ([]deployNodeView, string) {
	if lane.Running != nil && len(lane.Running.Nodes) > 0 {
		out := make([]deployNodeView, 0, len(lane.Running.Nodes))
		for _, name := range lane.Running.Nodes {
			out = append(out, deployNodeView{Name: name, ChipClass: "running", Meta: "running"})
		}
		return out, "live"
	}
	if lane.Running != nil {
		// A run IS in flight, it just has no node executing at this
		// instant (before the first start, or while winding down). Showing
		// the PREVIOUS run's node rows here would be a lie about the run
		// the card is currently reporting on.
		return nil, ""
	}
	if len(hist) == 0 {
		return nil, ""
	}
	out := make([]deployNodeView, 0, len(hist))
	for _, n := range hist {
		out = append(out, deployNodeView{
			Name:      n.Name,
			ChipClass: deployNodeChipClass(n.Status),
			Meta:      deployNodeMeta(n),
		})
	}
	return out, "history"
}

// deployNodeChipClass maps a node's stored status onto its chip class.
// "blocked" gets its own dashed-outline chip rather than one of the filled
// colors: a blocked node has no verdict of its own — it never ran — and
// painting it like one would attribute a failure to the wrong node.
func deployNodeChipClass(status string) string {
	switch status {
	case "passed":
		return "ok"
	case "failed":
		return "bad"
	case "skipped":
		return "warn"
	case "blocked":
		return "blocked"
	default:
		return "neutral"
	}
}

// deployNodeTag is wordTag for a deploy node row, differing in exactly one
// case: a "blocked" node gets the dashed outline chip (.chip-blocked) the
// card strip already uses for it, rather than wordTag's filled neutral
// square. Same reason as deployNodeChipClass — a node that never ran has no
// verdict to paint — and it is a local override rather than a change to
// wordTag itself so /run/{id}'s blocked CHECK rows keep rendering exactly as
// they do today.
func deployNodeTag(status string) tag {
	if status == "blocked" {
		return tag{status, "blocked"}
	}
	return wordTag(status)
}

// deployNodeMeta is the muted right-hand annotation on a node row: what a
// blocked node was blocked BY (the explicit edge attribution), the verdict
// plus duration for a non-green node, and the bare duration for a green one.
func deployNodeMeta(n history.CheckRow) string {
	if n.BlockedBy != "" {
		return "blocked by " + strings.ReplaceAll(n.BlockedBy, ",", ", ")
	}
	switch n.Status {
	case "passed":
		return formatDuration(n.Duration)
	case "skipped":
		return "skipped"
	default:
		return n.Status + " · " + formatDuration(n.Duration)
	}
}

// deployStateTag folds a lane into the one-word state tag its card carries,
// using the same priority laneState (api.go) applies — parked over running
// over pending over the ref comparison — so the HTML card and the JSON
// `state` field can never say different things about the same lane.
func deployStateTag(lane DeployLane) tag {
	word := laneState(lane)
	switch word {
	case "in sync":
		return tag{word, "ok"}
	case "parked":
		return tag{word, "bad"}
	case "deploying":
		// accent, not ok: deploying is in-progress, and painting it green
		// would read as "done" at a glance — the one reading this page must
		// never get wrong.
		return tag{word, "accent"}
	case "pending", "waiting":
		return tag{word, "warn"}
	default:
		return tag{word, "neutral"}
	}
}

// deployOutcomeWord renders a deploy's outcome in DEPLOY words: a green
// graph "deployed" (it landed nothing), a red one "failed", an
// externally-concluded one "cancelled". Kept distinct from the queue's own
// outcome vocabulary because the same core.Outcome genuinely means a
// different thing on a lane.
func deployOutcomeWord(outcome string) string {
	switch outcome {
	case "landed":
		return "deployed"
	case "rejected":
		return "failed"
	case "skipped":
		return "cancelled"
	default:
		return outcome
	}
}

// deployResultSummary is the one-line fallback a card shows when it has no
// node rows to show (history disabled, or a pre-history run).
func deployResultSummary(lr *DeployResult) string {
	word := deployOutcomeWord(lr.Outcome)
	if lr.Culprit != "" {
		return word + " · " + lr.Culprit + " failed"
	}
	if lr.Detail != "" {
		return word + " · " + lr.Detail
	}
	return word
}

// deployChipTitle builds a recent-deploy chip's tooltip, mirroring
// chipTitle's shape for runs: "deployed · e5f6a7b8c9d0 · 4m ago".
func deployChipTitle(row history.DeployRow, now time.Time) string {
	return fmt.Sprintf("%s · %s · %s", deployOutcomeWord(row.Outcome), shortSHA(row.DeploySHA), relAgo(row.StartedAt, now))
}

// liveDeployRow renders an in-flight run as a Recent-deploys table row. It
// has no history row yet by construction (nothing is written until the
// terminal event), so its outcome cell is the running chip and its
// duration is elapsed-so-far rather than a final measurement.
func liveDeployRow(lane DeployLane, at time.Time) deployRowView {
	return deployRowView{
		RunID:     lane.Running.RunID,
		Env:       lane.Env,
		From:      shortSHA(lane.Running.DeployedSHA),
		To:        shortSHA(lane.Running.DeploySHA),
		ToFull:    lane.Running.DeploySHA,
		ChipClass: "running",
		Outcome:   "deploying",
		Duration:  formatDuration(at.Sub(lane.Running.StartedAt)),
		Live:      true,
	}
}

func (d *dash) buildDeployRowView(row history.DeployRow, at time.Time) deployRowView {
	return deployRowView{
		RunID:     row.RunID,
		Env:       row.Env,
		From:      shortSHA(row.DeployedSHA),
		To:        shortSHA(row.DeploySHA),
		ToFull:    row.DeploySHA,
		ChipClass: outcomeChipClass(row.Outcome),
		Outcome:   deployOutcomeWord(row.Outcome),
		Culprit:   row.Culprit,
		Duration:  formatDuration(row.Duration),
		Finished:  relAgo(row.EndedAt, at),
	}
}

// --- source-chain strip (hand-built SVG) --------------------------------------
//
// The promotion topology, drawn from the lanes' own Source strings and
// nothing else: `source "main"` is an edge from a BRANCH node, `source
// env="dev"` an edge from another environment's node. That makes the picture
// derivable rather than configured — there is no topology object anywhere in
// gauntlet to draw from, because the chain of sources IS the pipeline (the
// design's "data flow, not control flow").
//
// Server-rendered SVG, computed once per request, no charting library — the
// buildDepthSVG precedent.

const (
	chainColW  = 200 // horizontal distance between dependency columns
	chainNodeW = 150
	chainNodeH = 46
	chainRowH  = 64
	chainPadX  = 14
	chainPadY  = 10
)

// buildSourceChainSVG lays every lane out in DEPENDENCY COLUMNS: a branch
// source sits in column 0, an environment sits one column right of whatever
// feeds it. Depth is resolved by walking `env=` edges, so `main → dev →
// {prod, prod2}` renders as three columns with the two prods stacked — the
// promotion topology legible at a glance, which is the whole reason this
// strip exists.
//
// Returns "" for no lanes at all; the template renders nothing in that case.
// A `source env=` naming an environment this daemon doesn't configure (or,
// defensively, a cycle — config rejects those at load) is treated as depth
// 0, which draws a disconnected node rather than looping forever.
func buildSourceChainSVG(lanes []DeployLane) template.HTML {
	if len(lanes) == 0 {
		return ""
	}

	index := make(map[string]DeployLane, len(lanes))
	for _, l := range lanes {
		index[l.Env] = l
	}

	// depthOf resolves an environment's column by walking its source chain,
	// bounded by the lane count so a malformed cycle can't spin.
	depthOf := func(env string) int {
		depth := 0
		cur := env
		for i := 0; i <= len(lanes); i++ {
			lane, ok := index[cur]
			if !ok {
				return depth
			}
			src, isEnv := strings.CutPrefix(lane.Source, "env=")
			if !isEnv {
				// Fed by a branch: the branch itself occupies the column to
				// this environment's left.
				return depth + 1
			}
			depth++
			cur = src
		}
		return depth
	}

	type chainNode struct {
		label  string
		sub    string
		fill   string // CSS var for the status line
		branch bool
		col    int
		row    int
	}
	var nodes []chainNode
	byName := map[string]int{} // node label (env or "branch:"+name) -> index into nodes

	// Branch nodes first, one per distinct branch source, so an environment
	// always has something to point back at.
	for _, l := range lanes {
		if _, isEnv := strings.CutPrefix(l.Source, "env="); isEnv || l.Source == "" {
			continue
		}
		key := "branch:" + l.Source
		if _, seen := byName[key]; seen {
			continue
		}
		byName[key] = len(nodes)
		nodes = append(nodes, chainNode{
			label: l.Source, sub: "branch · " + orDash(shortSHA(l.SourceTip)),
			fill: "var(--muted)", branch: true, col: depthOf(l.Env) - 1,
		})
	}
	for _, l := range lanes {
		byName[l.Env] = len(nodes)
		state := laneState(l)
		nodes = append(nodes, chainNode{
			label: l.Env,
			sub:   state + " · " + orDash(shortSHA(l.Desired)),
			fill:  chainStateColor(state),
			col:   depthOf(l.Env),
		})
	}

	// Assign rows within each column, in declaration order.
	perCol := map[int]int{}
	maxCol, maxRow := 0, 0
	for i := range nodes {
		c := nodes[i].col
		if c < 0 {
			c = 0
			nodes[i].col = 0
		}
		nodes[i].row = perCol[c]
		perCol[c]++
		if c > maxCol {
			maxCol = c
		}
		if nodes[i].row > maxRow {
			maxRow = nodes[i].row
		}
	}

	xOf := func(col int) float64 { return float64(chainPadX + col*chainColW) }
	yOf := func(row int) float64 { return float64(chainPadY + row*chainRowH) }

	width := chainPadX*2 + maxCol*chainColW + chainNodeW
	height := chainPadY*2 + maxRow*chainRowH + chainNodeH

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" width="%d" height="%d" class="source-chain" role="img" aria-label="deploy source chain">`,
		width, height, width, height)
	b.WriteString(`<defs><marker id="chain-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0 L8 4 L0 8 z" fill="var(--muted)"/></marker></defs>`)

	// Edges first so node boxes paint over their endpoints.
	for _, l := range lanes {
		var srcKey string
		if src, isEnv := strings.CutPrefix(l.Source, "env="); isEnv {
			srcKey = src
		} else if l.Source != "" {
			srcKey = "branch:" + l.Source
		} else {
			continue
		}
		si, ok := byName[srcKey]
		if !ok {
			continue
		}
		di := byName[l.Env]
		x1 := xOf(nodes[si].col) + chainNodeW
		y1 := yOf(nodes[si].row) + chainNodeH/2
		x2 := xOf(nodes[di].col) - 6
		y2 := yOf(nodes[di].row) + chainNodeH/2
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="chain-edge" marker-end="url(#chain-arrow)"/>`, x1, y1, x2, y2)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="chain-edge-label">%s</text>`,
			(x1+x2)/2, (y1+y2)/2-5, template.HTMLEscapeString(l.Mode))
	}

	for _, n := range nodes {
		x, y := xOf(n.col), yOf(n.row)
		dash := ""
		if n.branch {
			// A branch is not an environment gauntlet owns any state for —
			// dashed, the same visual distinction the mockup draws.
			dash = ` stroke-dasharray="4 3"`
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%d" height="%d" rx="8" class="chain-node"%s/>`, x, y, chainNodeW, chainNodeH, dash)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="chain-label">%s</text>`,
			x+chainNodeW/2, y+19, template.HTMLEscapeString(n.label))
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="chain-sub" fill="%s">%s</text>`,
			x+chainNodeW/2, y+34, n.fill, template.HTMLEscapeString(n.sub))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// chainStateColor maps a lane's state word onto the CSS variable its status
// line is painted with — the same green/blue/red/muted vocabulary the cards'
// tags use, so the strip and the cards agree at a glance.
func chainStateColor(state string) string {
	switch state {
	case "in sync":
		return "var(--ok)"
	case "deploying":
		return "var(--accent)"
	case "parked":
		return "var(--bad)"
	default:
		return "var(--muted)"
	}
}

// --- /deploy/{runID} ----------------------------------------------------------

func (d *dash) handleDeploy(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")

	if d.store == nil {
		// Same treatment /run/{id} gives: the page renders, saying history
		// is off, rather than 404ing an ID that may well be real.
		render(w, deployTmpl, deployData{baseData: d.newBase(runID, nil, false)})
		return
	}

	row, nodes, err := d.store.Deploy(runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No history row: this may be a run still IN FLIGHT — nothing
			// is written until the terminal event — and the overview links
			// straight to it while it is deploying. Serve the live header
			// rather than 404ing the page someone just clicked.
			if data, ok := d.inFlightDeploy(runID); ok {
				render(w, deployTmpl, data)
				return
			}
			http.NotFound(w, r)
			return
		}
		log.Printf("dashboard: deploy %s: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := deployData{
		baseData:     d.newBase("deploy "+row.RunID, nil, false),
		StoreEnabled: true,
		HasRecord:    true,
		Deploy: deploySummaryFull{
			RunID: row.RunID, Env: row.Env,
			DeploySHA: row.DeploySHA, DeployedSHA: row.DeployedSHA,
			Outcome: wordTag(deployOutcomeWord(row.Outcome)), Culprit: row.Culprit,
			Detail:    row.Detail,
			StartedAt: formatTime(row.StartedAt), EndedAt: formatTime(row.EndedAt),
			Duration: formatDuration(row.Duration),
		},
	}
	for _, n := range nodes {
		data.Nodes = append(data.Nodes, checkView{
			Seq: n.Seq, Name: n.Name, Status: deployNodeTag(n.Status),
			Duration: formatDuration(n.Duration), Err: n.Err,
			Detail: checkRowDetail(n),
			Output: n.Output,
			// Open the culprit's output by default — this page exists to
			// answer "how did the deploy fail".
			Open:    n.Status == "failed" || n.Err != "",
			LogURL:  d.deployLogURL(row.RunID, n.Name, n.LogPath),
			Command: n.Command,
		})
	}

	// Live lane context: which actions are meaningful right now, and what
	// this environment currently tracks. Absent when deployment isn't
	// configured (an old history row on a daemon that has since dropped its
	// deploy block) — the page still renders, just without the actions.
	if lane, ok := d.laneFor(row.Env); ok {
		data.Source = lane.Source
		data.ShowCancel = lane.Running != nil && lane.Running.RunID == row.RunID
		data.ShowRetry = lane.Parked != nil && lane.Parked.RunID == row.RunID
		if lane.Parked != nil && lane.Parked.RunID == row.RunID {
			data.ParkNote = "parked — the lane holds until a retry or a new desired revision"
		}
	}

	// Previous deploys for the same environment, this one excluded.
	prev, err := d.store.RecentDeploys(row.Env, recentDeploysLimit)
	if err != nil {
		log.Printf("dashboard: deploy %s: previous: %v", runID, err)
	}
	now := time.Now()
	for _, p := range prev {
		if p.RunID == row.RunID {
			continue
		}
		data.Previous = append(data.Previous, d.buildDeployRowView(p, now))
	}
	render(w, deployTmpl, data)
}

// inFlightDeploy builds the minimal header for a run that is still
// executing, so the overview's "deploying" link resolves to a live page
// instead of a 404. It is the ONE deploy page that refreshes: it exists
// precisely to be watched until the terminal event writes the real row,
// after which the very next tick renders the full record.
func (d *dash) inFlightDeploy(runID string) (deployData, bool) {
	if d.deploySnapshot == nil {
		return deployData{}, false
	}
	snap := d.deploySnapshot()
	if snap == nil {
		return deployData{}, false
	}
	for _, lane := range snap.Lanes {
		if lane.Running == nil || lane.Running.RunID != runID {
			continue
		}
		data := deployData{
			baseData:     d.newBase("deploy "+runID, nil, true),
			StoreEnabled: true,
			InFlight:     true,
			Source:       lane.Source,
			ShowCancel:   true,
			Deploy: deploySummaryFull{
				RunID: runID, Env: lane.Env,
				DeploySHA: lane.Running.DeploySHA, DeployedSHA: lane.Running.DeployedSHA,
				Outcome:   tag{"deploying", "accent"},
				StartedAt: formatTime(lane.Running.StartedAt),
				Duration:  formatDuration(snap.At.Sub(lane.Running.StartedAt)),
			},
			RunningNodes: append([]string(nil), lane.Running.Nodes...),
		}
		return data, true
	}
	return deployData{}, false
}

// laneFor returns the published state of one environment's lane.
func (d *dash) laneFor(env string) (DeployLane, bool) {
	if d.deploySnapshot == nil {
		return DeployLane{}, false
	}
	snap := d.deploySnapshot()
	if snap == nil {
		return DeployLane{}, false
	}
	for _, lane := range snap.Lanes {
		if lane.Env == env {
			return lane, true
		}
	}
	return DeployLane{}, false
}

// --- /deploy/{runID}/log/{node} -----------------------------------------------

// deployLogURL mirrors runLogURL for a deploy node: "" when there is nothing
// to link (no log file was written, or no LogRoot is configured, in which
// case the route would 404 anyway).
func (d *dash) deployLogURL(runID, node, logPath string) string {
	if logPath == "" || d.logRoot == "" {
		return ""
	}
	return "/deploy/" + url.PathEscape(runID) + "/log/" + url.PathEscape(node)
}

// handleDeployLog serves one deploy node's full log file. Structurally
// identical to handleRunLog — look the stored path up in history, then hand
// it to the shared serveLogFile (server.go) for the containment check and
// the streaming — over deploy_nodes instead of checks/hooks. Every failure
// mode renders the same friendly 404, for the same reason.
func (d *dash) handleDeployLog(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		notFoundPrunedOrMissing(w)
		return
	}
	runID := r.PathValue("runID")
	node := r.PathValue("node")

	_, nodes, err := d.store.Deploy(runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFoundPrunedOrMissing(w)
			return
		}
		log.Printf("dashboard: deploy log: %s: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var storedPath string
	for _, n := range nodes {
		if n.Name == node {
			storedPath = n.LogPath
			break
		}
	}
	if storedPath == "" {
		notFoundPrunedOrMissing(w)
		return
	}
	d.serveLogFile(w, r, storedPath)
}

// --- view models --------------------------------------------------------------

// deploysData is /deploys' view model. Configured is false only when this
// daemon has no deploy tracker at all — the "no environments configured"
// page; baseData.Starting covers "configured but no pass yet".
type deploysData struct {
	baseData
	Configured   bool
	StoreEnabled bool
	SnapshotAt   template.HTML
	SourceChain  template.HTML
	Lanes        []deployCardView
	Recent       []deployRowView
}

// deployCardView is one environment card.
type deployCardView struct {
	Env    string
	State  tag
	Source string
	Mode   string

	// InSync selects which shape the .shaline takes: "running <sha>" when
	// desired == observed, or "observed → desired" when they differ.
	InSync                 bool
	Desired, DesiredFull   string
	Observed, ObservedFull string

	// RunID/RunElapsed describe the run in flight, empty when there is
	// none: the card links it and says how long it has been going.
	RunID, RunElapsed string

	// Nodes is the per-node strip; NodeSource is "live"|"history"|"" (see
	// deployNodeRows). LastSummary is the one-line fallback the template
	// renders when Nodes is empty.
	Nodes       []deployNodeView
	NodeSource  string
	LastSummary string

	LastRunID    string
	LastOutcome  tag
	LastAgo      string
	LastDuration string

	// ShowRetry is true only on a parked lane — the one state a retry acts
	// on. ParkDetail says what parked it.
	ShowRetry  bool
	ParkDetail string

	LastError string
	Recent    []deployChipView
}

// deployNodeView is one row of a card's node strip.
type deployNodeView struct {
	Name      string
	ChipClass string // ok|bad|warn|blocked|running|neutral -> chip-<class>
	Meta      string
}

// deployChipView is one recent-deploy chip on a card.
type deployChipView struct {
	RunID     string
	ChipClass string
	Title     string
}

// deployRowView is one row of a Recent/Previous deploys table. Live rows
// (a run in flight) have no history row yet, hence no outcome, culprit or
// finished-at — and their Duration is elapsed-so-far.
type deployRowView struct {
	RunID     string
	Env       string
	From, To  string
	ToFull    string
	ChipClass string
	Outcome   string
	Culprit   string
	Duration  string
	Finished  string

	// Live marks a row for a run still in flight: it has no history row by
	// construction (nothing is written until the terminal event), so its
	// outcome cell is the running chip, its duration is elapsed-so-far, and
	// it has no finished-at.
	Live bool
}

// deployData is /deploy/{runID}'s view model. StoreEnabled false renders the
// "history disabled" body; HasRecord false with InFlight true renders the
// minimal live header (the only refreshing deploy detail page).
type deployData struct {
	baseData
	StoreEnabled bool
	HasRecord    bool
	InFlight     bool

	Deploy   deploySummaryFull
	Nodes    []checkView
	Previous []deployRowView

	// Source is the environment's configured source ("main", "env=dev"),
	// present only while the lane still exists in the live snapshot.
	Source string

	// ShowRetry/ShowCancel gate the two action buttons on what is actually
	// actionable right now: retry only when THIS run is the one that parked
	// the lane, cancel only when THIS run is the one in flight.
	ShowRetry  bool
	ShowCancel bool
	ParkNote   string

	// RunningNodes is the in-flight node list, live-header only.
	RunningNodes []string
}

type deploySummaryFull struct {
	RunID, Env             string
	DeploySHA, DeployedSHA string
	Outcome                tag
	Culprit, Detail        string
	StartedAt, EndedAt     template.HTML
	Duration               string
}
