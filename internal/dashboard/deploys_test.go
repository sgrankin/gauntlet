package dashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// Deploy surfaces (/deploys, /deploy/{id}, their JSON mirrors, and the two
// mutating routes).
//
// Every timestamp below is seeded as an offset from time.Now() — never a
// fixed calendar date. These pages render RELATIVE ages ("4m ago") and order
// rows by time, both of which a pinned date silently breaks as the calendar
// moves away from it.

// deploySnapshot builds the scenario the mockup shows: dev in sync, prod
// mid-graph, prod2 parked on a failed migration.
func deploySnapshot(now time.Time) *dashboard.DeployStatus {
	return &dashboard.DeployStatus{
		At: now,
		Lanes: []dashboard.DeployLane{
			{
				Env: "dev", Mode: "track", Source: "main",
				SourceTip: "e5f6a7b8c9d0e1f2", Desired: "e5f6a7b8c9d0e1f2", Observed: "e5f6a7b8c9d0e1f2",
				InSync:      true,
				LastAdvance: now.Add(-4 * time.Minute),
				LastResult: &dashboard.DeployResult{
					RunID: "deploy-dev-1", DeploySHA: "e5f6a7b8c9d0e1f2", DeployedSHA: "9c0d1e2f3a4b5c6d",
					Outcome: "landed", StartedAt: now.Add(-5 * time.Minute), EndedAt: now.Add(-4 * time.Minute),
				},
			},
			{
				Env: "prod", Mode: "track", Source: "env=dev",
				SourceTip: "e5f6a7b8c9d0e1f2", Desired: "e5f6a7b8c9d0e1f2", Observed: "9c0d1e2f3a4b5c6d",
				Running: &dashboard.DeployRun{
					RunID: "deploy-prod-2", DeploySHA: "e5f6a7b8c9d0e1f2", DeployedSHA: "9c0d1e2f3a4b5c6d",
					StartedAt: now.Add(-2 * time.Minute), Nodes: []string{"app2"},
				},
			},
			{
				Env: "prod2", Mode: "track", Source: "env=dev",
				SourceTip: "e5f6a7b8c9d0e1f2", Desired: "e5f6a7b8c9d0e1f2", Observed: "9c0d1e2f3a4b5c6d",
				Parked: &dashboard.DeployPark{
					SHA: "e5f6a7b8c9d0e1f2", RunID: "deploy-prod2-3", Outcome: "rejected",
					Detail: "node migrate failed", At: now.Add(-3 * time.Minute),
				},
				LastResult: &dashboard.DeployResult{
					RunID: "deploy-prod2-3", DeploySHA: "e5f6a7b8c9d0e1f2", DeployedSHA: "9c0d1e2f3a4b5c6d",
					Outcome: "rejected", Culprit: "migrate", Detail: "node migrate failed",
					StartedAt: now.Add(-4 * time.Minute), EndedAt: now.Add(-3 * time.Minute),
				},
			},
		},
	}
}

// emitDeploy writes one finished deploy graph run into store.
func emitDeploy(t *testing.T, s *history.Store, rec *core.DeployRecord) {
	t.Helper()
	err := s.Emit(context.Background(), core.Event{
		Kind: core.EventDeployFinished, At: rec.EndedAt, RunID: rec.RunID,
		DeployEnv: rec.Env, DeploySHA: rec.DeploySHA, DeployedSHA: rec.DeployedSHA,
		Deploy: rec,
	})
	if err != nil {
		t.Fatalf("Emit deploy %s: %v", rec.RunID, err)
	}
}

// seedDeployHistory writes the finished runs the snapshot above refers to.
func seedDeployHistory(t *testing.T, s *history.Store, now time.Time) {
	t.Helper()
	emitDeploy(t, s, &core.DeployRecord{
		RunID: "deploy-dev-1", Env: "dev",
		DeploySHA: "e5f6a7b8c9d0e1f2", DeployedSHA: "9c0d1e2f3a4b5c6d",
		Nodes: []core.CheckResult{
			{Name: "migrate", Seq: 1, Status: core.CheckPassed, Duration: 22 * time.Second},
			{Name: "app1", Seq: 2, Status: core.CheckPassed, Duration: 58 * time.Second},
			{Name: "app2", Seq: 3, Status: core.CheckSkipped},
		},
		Outcome: core.OutcomeLanded, StartedAt: now.Add(-5 * time.Minute), EndedAt: now.Add(-4 * time.Minute),
	})
	emitDeploy(t, s, &core.DeployRecord{
		RunID: "deploy-prod2-3", Env: "prod2",
		DeploySHA: "e5f6a7b8c9d0e1f2", DeployedSHA: "9c0d1e2f3a4b5c6d",
		Nodes: []core.CheckResult{
			{
				Name: "migrate", Seq: 1, Status: core.CheckFailed, Duration: 24 * time.Second,
				Output: "lock wait timeout", Command: []string{"./scripts/migrate"},
				LogPath: "/nonexistent/deploy-prod2-3/1-migrate.log.zst",
			},
			{Name: "app1", Seq: 2, Status: core.CheckBlocked, BlockedBy: []string{"migrate"}},
		},
		Outcome: core.OutcomeRejected, Culprit: "migrate", Detail: "node migrate failed",
		StartedAt: now.Add(-4 * time.Minute), EndedAt: now.Add(-3 * time.Minute),
	})
}

// deployHandler builds a dashboard wired with a deploy snapshot and,
// optionally, retry/cancel closures.
func deployHandler(store *history.Store, snap *dashboard.DeployStatus, opts ...dashboard.Option) http.Handler {
	all := append([]dashboard.Option{
		dashboard.WithDeploySnapshot(func() *dashboard.DeployStatus { return snap }),
	}, opts...)
	return dashboard.New(func() *queue.Snapshot { return nil }, store, all...)
}

// --- /deploys -----------------------------------------------------------------

func TestDeploys_RendersLanesAndRecentTable(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)
	h := deployHandler(store, deploySnapshot(now))

	resp, body := get(t, h, "/deploys")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"dev", "prod", "prod2", // one card per lane
		"in sync", "deploying", "parked", // their state tags
		"tracks <code>main</code>", "tracks <code>env=dev</code>",
		"source chain",               // the topology strip
		"chip-running",               // prod's executing node
		"chip-blocked",               // prod2's blocked node, from history
		"blocked by migrate",         // its edge attribution
		"Recent deploys",             // the daemon-wide table
		"deploy-prod-2",              // the LIVE run, which has no history row yet
		"deploy-dev-1",               // a finished one
		`data-deploy-action="retry"`, // the parked lane's button
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/deploys body missing %q", want)
		}
	}
	// The in-flight run must be linked, not just named: that link is what
	// the detail page's live fallback exists to answer.
	if !strings.Contains(body, `href="/deploy/deploy-prod-2"`) {
		t.Error("/deploys does not link the in-flight deploy")
	}
	// Retry appears ONLY on the parked lane.
	if got := strings.Count(body, `data-deploy-action="retry"`); got != 1 {
		t.Errorf("retry buttons = %d, want exactly 1 (the parked lane)", got)
	}
	if !strings.Contains(body, `href="/deploys"`) {
		t.Error("nav is missing the deploys entry on a deploy-configured daemon")
	}
}

// TestDeploys_NoTracker: no `deploy` block configured — a 200 saying so, the
// nav entry hidden, and the JSON route 503.
func TestDeploys_NoTracker(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := get(t, h, "/deploys")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an unconfigured subsystem is an answer, not an error)", resp.StatusCode)
	}
	if !strings.Contains(body, "no environments configured") {
		t.Errorf("body missing the unconfigured message:\n%s", body)
	}
	if strings.Contains(body, `href="/deploys"`) {
		t.Error("nav shows a deploys entry on a daemon with no deploy tracker")
	}

	resp, body = get(t, h, "/api/v1/deploys")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "deploy not configured") {
		t.Errorf("GET /api/v1/deploys = %d %s, want 503 \"deploy not configured\"", resp.StatusCode, body)
	}
}

// TestDeploys_NoSnapshotYet: the tracker exists but no pass has published.
func TestDeploys_NoSnapshotYet(t *testing.T) {
	h := deployHandler(nil, nil)

	resp, body := get(t, h, "/deploys")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "starting up") {
		t.Errorf("body missing the starting-up treatment:\n%s", body)
	}

	resp, body = get(t, h, "/api/v1/deploys")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "no snapshot yet") {
		t.Errorf("GET /api/v1/deploys = %d %s, want 503 \"no snapshot yet\"", resp.StatusCode, body)
	}
}

// TestDeploys_HistoryDisabled: cards render from the Snapshot alone, and the
// recent table says why it's empty rather than pretending nothing happened.
func TestDeploys_HistoryDisabled(t *testing.T) {
	now := time.Now()
	h := deployHandler(nil, deploySnapshot(now))

	resp, body := get(t, h, "/deploys")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"dev", "prod2", "parked", "history disabled"} {
		if !strings.Contains(body, want) {
			t.Errorf("/deploys with history disabled missing %q", want)
		}
	}
	// No stored node rows to draw, so the parked lane falls back to its
	// last result's one-line summary.
	if !strings.Contains(body, "migrate failed") {
		t.Errorf("history-disabled card missing the last-result summary:\n%s", body)
	}
}

// --- the live-vs-history node helper (one test per branch) --------------------

// TestDeployCard_NodeSourcePrecedence: a lane with a run in flight shows the
// LIVE node list; a settled lane shows its last run's stored rows; a lane
// with neither shows no strip at all. The three branches are the deploys
// page's whole "what is happening now beats what happened last" rule.
func TestDeployCard_NodeSourcePrecedence(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)

	t.Run("live wins", func(t *testing.T) {
		snap := deploySnapshot(now)
		// Give prod a finished last result too: the live nodes must still
		// win over its stored rows.
		snap.Lanes[1].LastResult = &dashboard.DeployResult{
			RunID: "deploy-dev-1", DeploySHA: "x", Outcome: "landed",
			StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-time.Hour),
		}
		h := deployHandler(store, snap)
		_, body := get(t, h, "/deploys")
		card := cardFor(t, body, "prod")
		if !strings.Contains(card, "chip-running") || !strings.Contains(card, "app2") {
			t.Errorf("prod card did not render its live node strip:\n%s", card)
		}
		if strings.Contains(card, "chip-ok") && strings.Contains(card, "migrate") {
			t.Errorf("prod card rendered the PREVIOUS run's stored nodes while a run is in flight:\n%s", card)
		}
	})

	t.Run("history when settled", func(t *testing.T) {
		h := deployHandler(store, deploySnapshot(now))
		_, body := get(t, h, "/deploys")
		card := cardFor(t, body, "prod2")
		for _, want := range []string{"chip-bad", "migrate", "chip-blocked", "blocked by migrate"} {
			if !strings.Contains(card, want) {
				t.Errorf("prod2 card missing %q from its stored node rows:\n%s", want, card)
			}
		}
	})

	t.Run("neither", func(t *testing.T) {
		now := time.Now()
		snap := &dashboard.DeployStatus{At: now, Lanes: []dashboard.DeployLane{{
			Env: "fresh", Mode: "track", Source: "main",
			SourceTip: "abc123def456", Desired: "abc123def456",
		}}}
		h := deployHandler(openTestStore(t), snap)
		_, body := get(t, h, "/deploys")
		card := cardFor(t, body, "fresh")
		if strings.Contains(card, `class="nodes"`) {
			t.Errorf("a lane with no live run and no history rendered a node strip:\n%s", card)
		}
	})
}

// cardFor extracts the rendered card block for one environment, so a
// per-lane assertion can't accidentally be satisfied by another lane's
// markup on the same page.
func cardFor(t *testing.T, body, env string) string {
	t.Helper()
	marker := "<h2>" + env + "</h2>"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no card for %q in body:\n%s", env, body)
	}
	rest := body[i:]
	if j := strings.Index(rest[len(marker):], "<h2>"); j >= 0 {
		rest = rest[:len(marker)+j]
	}
	return rest
}

// --- /deploy/{id} -------------------------------------------------------------

func TestDeploy_DetailPage(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)
	h := deployHandler(store, deploySnapshot(now),
		dashboard.WithDeployRetry(func(string) bool { return true }))

	resp, body := get(t, h, "/deploy/deploy-prod2-3")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"prod2", "tracks env=dev", // the .kv header
		"failed",             // the outcome word, in deploy vocabulary
		"culprit",            // the attribution row
		"9c0d1e2f3a4b",       // deployed_sha ...
		"e5f6a7b8c9d0",       // ... → deploy_sha
		"lock wait timeout",  // the culprit's captured output, inline
		"./scripts/migrate",  // its command echo
		"blocked by migrate", // the blocked node's edge attribution
		"parked — the lane holds",
		`data-deploy-action="retry"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/deploy/{id} body missing %q", want)
		}
	}
	// A settled deploy's detail page does NOT refresh: it is a record, and
	// nothing about it can change.
	if strings.Contains(body, "Idiomorph.morph") {
		t.Error("a finished deploy's detail page auto-refreshes; only the in-flight header should")
	}
	// No trigger/graph rows: neither fact is in core.DeployRecord, and
	// inventing them is exactly what the template comment forbids.
	if strings.Contains(body, "<dt>trigger</dt>") || strings.Contains(body, "<dt>graph</dt>") {
		t.Error("/deploy/{id} rendered a trigger or graph row; neither is in the record")
	}
}

// TestDeploy_InFlightFallback: the overview links to a run the instant it
// starts, and nothing is written to history until it FINISHES — so the
// detail page must serve a live header rather than 404ing the link someone
// just clicked. That page is the one deploy page that refreshes.
func TestDeploy_InFlightFallback(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)
	h := deployHandler(store, deploySnapshot(now),
		dashboard.WithDeployCancel(func(string) bool { return true }))

	resp, body := get(t, h, "/deploy/deploy-prod-2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the live header, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"deploying", "prod", "app2", `data-deploy-action="cancel"`} {
		if !strings.Contains(body, want) {
			t.Errorf("in-flight detail page missing %q", want)
		}
	}
	if !strings.Contains(body, "Idiomorph.morph") {
		t.Error("the in-flight detail page does not auto-refresh; it exists to be watched")
	}
}

// TestDeploy_UnknownAndHistoryDisabled: an ID matching neither a stored row
// nor a live run is a 404; with history off the page renders the run-page's
// own "history disabled" body rather than 404ing an ID that may be real.
func TestDeploy_UnknownAndHistoryDisabled(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)

	h := deployHandler(store, deploySnapshot(now))
	if resp, _ := get(t, h, "/deploy/deploy-nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown deploy id = %d, want 404", resp.StatusCode)
	}

	noStore := deployHandler(nil, deploySnapshot(now))
	resp, body := get(t, noStore, "/deploy/deploy-prod2-3")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "history disabled") {
		t.Errorf("/deploy/{id} with history off = %d, want 200 \"history disabled\", body:\n%s", resp.StatusCode, body)
	}
}

// TestDeployLog_ServesAndDegrades: the node log route reuses handleRunLog's
// containment machinery over deploy_nodes. Without WithLogRoot there is no
// link to follow and the route 404s with the same friendly body.
func TestDeployLog_MissingFile(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)
	h := deployHandler(store, deploySnapshot(now))

	resp, body := get(t, h, "/deploy/deploy-prod2-3/log/migrate")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "log pruned or missing") {
		t.Errorf("deploy log without a log root = %d %q, want the 404 body", resp.StatusCode, body)
	}
	if resp, _ := get(t, h, "/deploy/deploy-prod2-3/log/nosuchnode"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown node log = %d, want 404", resp.StatusCode)
	}
}

// --- JSON API -----------------------------------------------------------------

func TestAPIDeploys_Shape(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)
	h := deployHandler(store, deploySnapshot(now))

	resp, body := get(t, h, "/api/v1/deploys")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	var got struct {
		SnapshotAt string `json:"snapshotAt"`
		Lanes      []struct {
			Env     string `json:"env"`
			State   string `json:"state"`
			Pending bool   `json:"pending"`
			InSync  bool   `json:"inSync"`
			Running *struct {
				RunID string   `json:"runID"`
				Nodes []string `json:"nodes"`
			} `json:"running"`
			Parked *struct {
				SHA     string `json:"sha"`
				Outcome string `json:"outcome"`
			} `json:"parked"`
		} `json:"lanes"`
		Recent []struct {
			RunID   string `json:"runID"`
			Env     string `json:"env"`
			Outcome string `json:"outcome"`
			Culprit string `json:"culprit"`
		} `json:"recent"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(got.Lanes) != 3 {
		t.Fatalf("lanes = %d, want 3", len(got.Lanes))
	}
	if got.Lanes[0].Env != "dev" || got.Lanes[0].State != "in sync" || !got.Lanes[0].InSync {
		t.Errorf("dev lane = %+v", got.Lanes[0])
	}
	if got.Lanes[1].State != "deploying" || got.Lanes[1].Running == nil || got.Lanes[1].Running.RunID != "deploy-prod-2" {
		t.Errorf("prod lane = %+v", got.Lanes[1])
	}
	if got.Lanes[2].State != "parked" || got.Lanes[2].Parked == nil || got.Lanes[2].Parked.Outcome != "rejected" {
		t.Errorf("prod2 lane = %+v", got.Lanes[2])
	}
	if len(got.Recent) != 2 {
		t.Fatalf("recent = %d, want the 2 finished deploys", len(got.Recent))
	}
	if got.Recent[0].RunID != "deploy-prod2-3" || got.Recent[0].Culprit != "migrate" {
		t.Errorf("newest recent deploy = %+v, want prod2's failed run first", got.Recent[0])
	}
}

func TestAPIDeploy_DetailAndDegradation(t *testing.T) {
	now := time.Now()
	store := openTestStore(t)
	seedDeployHistory(t, store, now)
	h := deployHandler(store, deploySnapshot(now))

	resp, body := get(t, h, "/api/v1/deploy/deploy-prod2-3")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	var got struct {
		RunID   string `json:"runID"`
		Env     string `json:"env"`
		Outcome string `json:"outcome"`
		Culprit string `json:"culprit"`
		Nodes   []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Output string `json:"output"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if got.RunID != "deploy-prod2-3" || got.Env != "prod2" || got.Outcome != "rejected" || got.Culprit != "migrate" {
		t.Errorf("detail = %+v", got)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].Name != "migrate" || got.Nodes[1].Status != "blocked" {
		t.Errorf("nodes = %+v, want migrate(failed) then app1(blocked)", got.Nodes)
	}

	if resp, _ := get(t, h, "/api/v1/deploy/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown deploy = %d, want 404", resp.StatusCode)
	}
	noStore := deployHandler(nil, deploySnapshot(now))
	resp, body = get(t, noStore, "/api/v1/deploy/deploy-prod2-3")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "history disabled") {
		t.Errorf("detail with history off = %d %s, want 503 \"history disabled\"", resp.StatusCode, body)
	}
}

// TestAPIDeployRetryCancel is the mutating pair's whole contract: the
// hook-cancel shape (202 with a status word, "no-op" when there was nothing
// to do), 400 on a missing env, 503 when deployment isn't configured, 405 on
// a wrong method.
func TestAPIDeployRetryCancel(t *testing.T) {
	now := time.Now()
	var retried, cancelled []string
	h := deployHandler(nil, deploySnapshot(now),
		dashboard.WithDeployRetry(func(env string) bool {
			retried = append(retried, env)
			return env == "prod2"
		}),
		dashboard.WithDeployCancel(func(env string) bool {
			cancelled = append(cancelled, env)
			return env == "prod"
		}),
	)

	cases := []struct {
		path, body string
		wantStatus int
		wantWord   string
	}{
		{"/api/v1/deploy/retry", `{"env":"prod2"}`, http.StatusAccepted, "retried"},
		{"/api/v1/deploy/retry", `{"env":"dev"}`, http.StatusAccepted, "no-op"},
		{"/api/v1/deploy/cancel", `{"env":"prod"}`, http.StatusAccepted, "cancelled"},
		{"/api/v1/deploy/cancel", `{"env":"dev"}`, http.StatusAccepted, "no-op"},
	}
	for _, c := range cases {
		resp, body := postJSON(t, h, c.path, c.body)
		if resp.StatusCode != c.wantStatus {
			t.Errorf("POST %s %s = %d, want %d (%s)", c.path, c.body, resp.StatusCode, c.wantStatus, body)
			continue
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if got["status"] != c.wantWord {
			t.Errorf("POST %s %s status = %q, want %q", c.path, c.body, got["status"], c.wantWord)
		}
	}
	if len(retried) != 2 || retried[0] != "prod2" {
		t.Errorf("retry closure calls = %v", retried)
	}
	if len(cancelled) != 2 || cancelled[0] != "prod" {
		t.Errorf("cancel closure calls = %v", cancelled)
	}

	// Missing env.
	for _, path := range []string{"/api/v1/deploy/retry", "/api/v1/deploy/cancel"} {
		resp, body := postJSON(t, h, path, `{}`)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "env is required") {
			t.Errorf("POST %s {} = %d %s, want 400 \"env is required\"", path, resp.StatusCode, body)
		}
		// Wrong method.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, rec.Code)
		}
	}

	// Not configured at all.
	bare := dashboard.New(func() *queue.Snapshot { return nil }, nil)
	for _, path := range []string{"/api/v1/deploy/retry", "/api/v1/deploy/cancel"} {
		resp, body := postJSON(t, bare, path, `{"env":"prod"}`)
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "deploy not configured") {
			t.Errorf("POST %s on an unconfigured daemon = %d %s, want 503 \"deploy not configured\"", path, resp.StatusCode, body)
		}
	}
}

// TestAPIStatus_IdleSinceComposesDeployLanes: the scale-to-zero signal must
// not report idle while a lane is deploying OR about to. Both halves are
// checked, plus the positive case where every lane is settled.
func TestAPIStatus_IdleSinceComposesDeployLanes(t *testing.T) {
	now := time.Now()
	idleQueue := &queue.Snapshot{
		At: now, IdleSince: now.Add(-10 * time.Minute),
		Targets: []queue.TargetSnapshot{{Name: "main", Branch: "main"}},
	}

	idleSince := func(t *testing.T, lanes []dashboard.DeployLane) string {
		t.Helper()
		snap := &dashboard.DeployStatus{At: now, Lanes: lanes}
		h := dashboard.New(func() *queue.Snapshot { return idleQueue }, nil,
			dashboard.WithDeploySnapshot(func() *dashboard.DeployStatus { return snap }))
		_, body := get(t, h, "/api/v1/status")
		var got struct {
			IdleSince string `json:"idleSince"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode: %v\nbody: %s", err, body)
		}
		return got.IdleSince
	}

	settled := []dashboard.DeployLane{{Env: "dev", InSync: true, Desired: "a", Observed: "a"}}
	if idleSince(t, settled) == "" {
		t.Error("a settled lane suppressed the idle signal")
	}

	running := []dashboard.DeployLane{{Env: "dev", Running: &dashboard.DeployRun{RunID: "d", StartedAt: now}}}
	if got := idleSince(t, running); got != "" {
		t.Errorf("idleSince = %q while a lane is deploying, want empty", got)
	}

	pending := []dashboard.DeployLane{{Env: "dev", Desired: "b", Observed: "a", Pending: true}}
	if got := idleSince(t, pending); got != "" {
		t.Errorf("idleSince = %q while a lane would start a run on the next pass, want empty", got)
	}
}
