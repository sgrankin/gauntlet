package dashboard_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

const hostileTopic = "<script>alert(1)</script>"

// hostileOutput stands in for a check that echoes something dangerous into
// its captured output (e.g. a linter quoting attacker-controlled source) —
// run.html must render it escaped inside <pre>, never as live markup.
const hostileOutput = "<script>alert(2)</script>"

// openTestStore opens a real SQLite-backed history.Store in a temp dir (no
// mocks: the dashboard's history queries are exercised against the same
// store type production uses).
func openTestStore(t *testing.T) *history.Store {
	t.Helper()
	s, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// emitRun writes one terminal run record (and its checks) into store.
func emitRun(t *testing.T, s *history.Store, rec *core.RunRecord) {
	t.Helper()
	if err := s.Emit(context.Background(), core.Event{Record: rec}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

// testSnapshot builds a hand-constructed queue.Snapshot: "main" has an
// in-flight run (mid-check), two waiting candidates given out of FIFO
// order (the dashboard must sort by Seq), and one parked candidate whose
// topic and park reason are hostile strings (must render HTML-escaped).
// "release" is idle with no target ref yet.
func testSnapshot() *queue.Snapshot {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	runStart := now.Add(-30 * time.Second)
	checkStart := now.Add(-5 * time.Second)

	return &queue.Snapshot{
		At: now,
		Targets: []queue.TargetSnapshot{
			{
				Name:      "main",
				Branch:    "main",
				TargetTip: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				InFlight: &queue.RunSnapshot{
					Candidate: core.Candidate{
						Ref: "refs/heads/for/main/alice/feat-a", Target: "main",
						User: "alice", Topic: "feat-a", SHA: "1111111111111111111111111111111111111111",
					},
					RunID:    "run-inflight-1",
					BaseOID:  "base0000000000000000000000000000000000",
					MergeSHA: "merge000000000000000000000000000000000",
					Done: []core.CheckResult{
						{Name: "lint", Status: core.CheckPassed, Duration: 2 * time.Second},
					},
					Current:   &queue.CurrentCheck{Name: "test", StartedAt: checkStart},
					StartedAt: runStart,
				},
				Waiting: []queue.WaitingEntry{
					{Candidate: core.Candidate{Ref: "refs/heads/for/main/bob/second", Target: "main", User: "bob", Topic: "second", SHA: "2222222222222222222222222222222222222222"}, Seq: 5},
					{Candidate: core.Candidate{Ref: "refs/heads/for/main/carol/first", Target: "main", User: "carol", Topic: "first", SHA: "3333333333333333333333333333333333333333"}, Seq: 2},
				},
				Parked: []queue.ParkedEntry{
					{
						Candidate: core.Candidate{
							Ref: "refs/heads/for/main/mallory/evil", Target: "main",
							User: "mallory", Topic: hostileTopic, SHA: "4444444444444444444444444444444444444444",
						},
						Outcome: core.OutcomeRejected,
						Reason:  "build failed: " + hostileTopic,
						At:      now.Add(-1 * time.Hour),
						RunID:   "run-mallory-rejected",
					},
				},
			},
			{
				Name:      "release",
				Branch:    "release/v2",
				TargetTip: "",
			},
		},
	}
}

// pipelineSnapshot builds a queue.Snapshot for target "spec" with a
// depth-3 speculative pipeline (docs/plans/phase5.md §3.4, §10 amendment
// 5): run 0 is the head (Predicted false — its base is the real target
// tip), runs 1 and 2 are downstream window members built on a predicted
// (unpushed) base. Each run has exactly one member with a distinct topic,
// so pipeline order is independently verifiable in a rendered page.
func pipelineSnapshot() *queue.Snapshot {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	mkRun := func(i int, predicted bool) queue.RunSnapshot {
		cand := core.Candidate{
			Ref: fmt.Sprintf("refs/heads/for/spec/user%d/topic%d", i, i), Target: "spec",
			User: fmt.Sprintf("user%d", i), Topic: fmt.Sprintf("topic%d", i), SHA: fmt.Sprintf("sha%d", i),
		}
		return queue.RunSnapshot{
			Candidate: cand,
			Members:   []core.Candidate{cand},
			RunID:     fmt.Sprintf("run-spec-%d", i),
			BaseOID:   fmt.Sprintf("base%d", i),
			ChainTip:  fmt.Sprintf("chain%d", i),
			MergeSHA:  fmt.Sprintf("chain%d", i),
			Predicted: predicted,
			StartedAt: now.Add(-time.Duration(i+1) * time.Minute),
			Current:   &queue.CurrentCheck{Name: fmt.Sprintf("check%d", i), StartedAt: now.Add(-time.Duration(i) * time.Second)},
		}
	}
	pipeline := []queue.RunSnapshot{mkRun(0, false), mkRun(1, true), mkRun(2, true)}
	head := pipeline[0]
	return &queue.Snapshot{
		At: now,
		Targets: []queue.TargetSnapshot{
			{
				Name: "spec", Branch: "spec", TargetTip: "tiptiptiptiptiptiptiptiptiptiptiptiptip",
				InFlight: &head, Pipeline: pipeline,
			},
		},
	}
}

// batchSnapshot builds a queue.Snapshot for target "rel" with a single
// batch run of 3 members (docs/plans/phase5.md §3.4, §10 amendment 5).
func batchSnapshot() *queue.Snapshot {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	members := []core.Candidate{
		{Ref: "refs/heads/for/rel/user0/topic0", Target: "rel", User: "user0", Topic: "topic0", SHA: "sha0"},
		{Ref: "refs/heads/for/rel/user1/topic1", Target: "rel", User: "user1", Topic: "topic1", SHA: "sha1"},
		{Ref: "refs/heads/for/rel/user2/topic2", Target: "rel", User: "user2", Topic: "topic2", SHA: "sha2"},
	}
	run := queue.RunSnapshot{
		Candidate: members[0],
		Members:   members,
		RunID:     "run-batch-1",
		BaseOID:   "base0",
		ChainTip:  "chain0",
		MergeSHA:  "chain0",
		BatchID:   "batch-1",
		StartedAt: now.Add(-time.Minute),
	}
	return &queue.Snapshot{
		At: now,
		Targets: []queue.TargetSnapshot{
			{Name: "rel", Branch: "rel", TargetTip: "tiptip", InFlight: &run, Pipeline: []queue.RunSnapshot{run}},
		},
	}
}

// TestTarget_RendersDepth3Pipeline confirms the target page renders a
// multi-run pipeline as a stacked list, head first, with the predicted
// badge only on downstream (non-head) runs (chunk P5-H, docs/plans/
// phase5.md §3.4).
func TestTarget_RendersDepth3Pipeline(t *testing.T) {
	snap := pipelineSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/t/spec")

	i0 := strings.Index(body, "topic0")
	i1 := strings.Index(body, "topic1")
	i2 := strings.Index(body, "topic2")
	if i0 == -1 || i1 == -1 || i2 == -1 {
		t.Fatalf("expected all three topics in body:\n%s", body)
	}
	if !(i0 < i1 && i1 < i2) {
		t.Errorf("pipeline not rendered head-first: topic0@%d topic1@%d topic2@%d", i0, i1, i2)
	}

	// Only the non-head runs (indices 1, 2) carry the predicted badge.
	if n := strings.Count(body, "on predicted base"); n != 2 {
		t.Errorf("expected 2 \"on predicted base\" badges (runs 1 and 2 only), got %d:\n%s", n, body)
	}

	for _, want := range []string{"run-spec-0", "run-spec-1", "run-spec-2", "check0", "check1", "check2"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
	// A depth-3 pipeline must never render the "batch of N" badge — no run
	// here has more than one member.
	if strings.Contains(body, "batch of") {
		t.Errorf("unexpected batch badge in a pure-speculation pipeline:\n%s", body)
	}
}

// TestTarget_RendersBatchRun confirms a single run with multiple members
// renders every member (topic/user, short SHA) plus a "batch of N" badge,
// even though the pipeline itself has depth 1 (chunk P5-H).
func TestTarget_RendersBatchRun(t *testing.T) {
	snap := batchSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/t/rel")

	if !strings.Contains(body, "batch of 3") {
		t.Errorf("expected \"batch of 3\" badge:\n%s", body)
	}
	for _, want := range []string{"user0/topic0", "user1/topic1", "user2/topic2"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing member %q in body:\n%s", want, body)
		}
	}
	if strings.Contains(body, "on predicted base") {
		t.Errorf("a batch run's own base is the real target tip, never predicted:\n%s", body)
	}
}

// TestIndex_PipelineDepthShowsNInFlight confirms the index card's in-flight
// cell becomes "N in flight" (rather than the head candidate's user/topic)
// once pipeline depth exceeds 1 (chunk P5-H).
func TestIndex_PipelineDepthShowsNInFlight(t *testing.T) {
	snap := pipelineSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/")

	if !strings.Contains(body, "3 in flight") {
		t.Errorf("expected \"3 in flight\" on the index card:\n%s", body)
	}
	if strings.Contains(body, "user0/topic0") {
		t.Errorf("expected the index card to omit the head candidate's user/topic once pipeline depth > 1:\n%s", body)
	}
	if !strings.Contains(body, "check0") {
		t.Errorf("expected the head run's current check to still render:\n%s", body)
	}
}

func sampleRecord(runID, target string) *core.RunRecord {
	started := time.Date(2026, 7, 5, 11, 0, 0, 0, time.UTC)
	return &core.RunRecord{
		RunID:  runID,
		Target: target,
		Candidate: core.Candidate{
			Ref: "refs/heads/for/" + target + "/dave/histfix", Target: target,
			User: "dave", Topic: "histfix", SHA: "5555555555555555555555555555555555555555",
		},
		BaseOID:  "base1111111111111111111111111111111111",
		MergeSHA: "merge111111111111111111111111111111111",
		Trial:    core.TrialMerge{Clean: true, TreeOID: "tree1111111111111111111111111111111111"},
		Checks: []core.CheckResult{
			{Name: "lint", Status: core.CheckPassed, Duration: 500 * time.Millisecond},
			{Name: "test", Status: core.CheckFailed, Duration: 2500 * time.Millisecond, Output: "boom"},
		},
		Outcome:   core.OutcomeRejected,
		Detail:    "test failed",
		StartedAt: started,
		EndedAt:   started.Add(3 * time.Second),
	}
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

func TestIndex_RendersTargetsInFlightAndElapsed(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main"))

	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, store)

	resp, body := get(t, h, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"main", "release", // both targets listed
		"alice/feat-a", // in-flight candidate
		"test",         // current check name
		"5s",           // elapsed = snap.At.Sub(Current.StartedAt) = 5s
		"2 waiting",
		"1 parked",
		"run-hist-1", // recent run from history
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestTarget_WaitingOrderAndParkedReasons(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/t/main")

	carolIdx := strings.Index(body, "carol")
	bobIdx := strings.Index(body, "bob")
	if carolIdx == -1 || bobIdx == -1 {
		t.Fatalf("expected both waiting candidates in body:\n%s", body)
	}
	if carolIdx > bobIdx {
		t.Errorf("waiting order wrong: carol (seq 2) should render before bob (seq 5); carol@%d bob@%d", carolIdx, bobIdx)
	}

	if !strings.Contains(body, "mallory") {
		t.Errorf("parked entry's user missing from body")
	}
	if !strings.Contains(body, "rejected") {
		t.Errorf("parked outcome missing from body")
	}
	if !strings.Contains(body, "build failed:") {
		t.Errorf("parked reason missing from body")
	}
	// This dashboard was built with a nil store (history disabled): even
	// though testSnapshot's parked entry carries a RunID (a live park always
	// sets one, independent of whether THIS dashboard has a store to look
	// runs up in — see parkedView's doc), the outcome tag must render
	// unlinked, not as a link to a /run/ page handleRun would 404 on.
	if strings.Contains(body, `<a href="/run/run-mallory-rejected"`) {
		t.Errorf("parked outcome tag linked to /run/ with history disabled (would 404):\n%s", body)
	}
	if !strings.Contains(body, `<span class="tag bad">rejected</span>`) {
		t.Errorf("parked outcome tag should render as a plain unlinked span with history disabled:\n%s", body)
	}
}

func TestTarget_EscapesHostileTopicAndReason(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/t/main")

	if strings.Contains(body, hostileTopic) {
		t.Errorf("hostile string rendered unescaped in body:\n%s", body)
	}
	const escaped = "&lt;script&gt;alert(1)&lt;/script&gt;"
	if !strings.Contains(body, escaped) {
		t.Errorf("expected escaped hostile string %q in body:\n%s", escaped, body)
	}
}

func TestRun_RendersChecksFromStore(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main"))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	resp, body := get(t, h, "/run/run-hist-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"lint", "test", "passed", "failed", "dave/histfix", "rejected"} {
		if !strings.Contains(body, want) {
			t.Errorf("run body missing %q\nbody:\n%s", want, body)
		}
	}
}

// emitHook writes one post-land hook result (internal/hooks;
// core.EventHookFinished) into s, the way internal/hooks.Runner actually
// emits one: Record is always nil on this event kind, the whole payload
// rides on Check + CheckName.
func emitHook(t *testing.T, s *history.Store, runID, name string, cr core.CheckResult) {
	t.Helper()
	cr.Name = name
	ev := core.Event{Kind: core.EventHookFinished, Target: "main", RunID: runID, CheckName: name, Check: &cr}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit (hook %s): %v", name, err)
	}
}

// TestRun_RendersHooksSection confirms /run/{id} gains a "Hooks" section
// (chunk P5-B, log/history parity with checks) whenever the run has hook
// rows: a passed hook and a failed one, same status/duration/output
// treatment as Checks — the failed hook's output must render already
// expanded.
func TestRun_RendersHooksSection(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hooks-1", "main"))
	emitHook(t, store, "run-hooks-1", "deploy", core.CheckResult{Status: core.CheckPassed, Duration: 250 * time.Millisecond, Output: "deployed ok"})
	emitHook(t, store, "run-hooks-1", "notify", core.CheckResult{Status: core.CheckFailed, Duration: 50 * time.Millisecond, Output: "webhook 500"})

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/run/run-hooks-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"Hooks", "deploy", "notify", "deployed ok", "webhook 500"} {
		if !strings.Contains(body, want) {
			t.Errorf("run body missing %q\nbody:\n%s", want, body)
		}
	}
	// The failed hook's <details> must start open (Open: true) so its
	// output is visible without a click, same convention as failed checks.
	if idx := strings.Index(body, "webhook 500"); idx >= 0 {
		// Walk backward to the nearest "<details" and confirm it carries
		// "open" — a light structural check, not a full HTML parse.
		detailsIdx := strings.LastIndex(body[:idx], "<details")
		if detailsIdx < 0 || !strings.Contains(body[detailsIdx:idx], "open") {
			t.Errorf("failed hook's <details> is not open by default:\n%s", body)
		}
	}
}

// TestRun_OmitsHooksSectionWhenNone confirms an ordinary run with no hook
// rows at all (no target hooks configured, or the run never reached hooks)
// renders no "Hooks" heading — the section requirement is "only when rows
// exist" (chunk P5-B), not "always, empty".
func TestRun_OmitsHooksSectionWhenNone(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-no-hooks", "main"))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	_, body := get(t, h, "/run/run-no-hooks")
	if strings.Contains(body, "<h2>Hooks</h2>") {
		t.Errorf("run body has a Hooks section for a run with no hook rows:\n%s", body)
	}
}

func TestNilStore_Degrades(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	for _, path := range []string{"/", "/t/main", "/run/whatever", "/checks"} {
		resp, body := get(t, h, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d", path, resp.StatusCode)
		}
		if !strings.Contains(body, "history disabled") {
			t.Errorf("%s: expected \"history disabled\", got:\n%s", path, body)
		}
	}
}

func TestNilSnapshot_Degrades(t *testing.T) {
	store := openTestStore(t)
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	for _, path := range []string{"/", "/t/main"} {
		resp, body := get(t, h, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d", path, resp.StatusCode)
		}
		if !strings.Contains(body, "Starting up") {
			t.Errorf("%s: expected starting-up state, got:\n%s", path, body)
		}
	}
}

func TestUnknownTarget404(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	resp, body := get(t, h, "/t/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body:\n%s", resp.StatusCode, body)
	}
}

func TestUnknownRun404(t *testing.T) {
	store := openTestStore(t)
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	resp, body := get(t, h, "/run/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body:\n%s", resp.StatusCode, body)
	}
}

func TestChecks_RendersStatsWhenStoreEnabled(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main"))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	resp, body := get(t, h, "/checks?target=main&since=720h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"lint", "test", "100%", "0%"} {
		if !strings.Contains(body, want) {
			t.Errorf("checks body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestChecks_DepthEmptyMessage(t *testing.T) {
	store := openTestStore(t)
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	_, body := get(t, h, "/checks?target=main")
	if !strings.Contains(body, "no data yet") {
		t.Errorf("expected \"no data yet\" for an empty depth series:\n%s", body)
	}
	if strings.Contains(body, `class="depth-chart"`) {
		t.Errorf("expected no chart to be rendered for an empty series:\n%s", body)
	}
}

func TestChecks_DepthChartRendersWhenSamplesExist(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC()
	if err := store.RecordDepth(now.Add(-2*time.Hour), "main", 3, 1, 0); err != nil {
		t.Fatalf("RecordDepth: %v", err)
	}
	if err := store.RecordDepth(now.Add(-1*time.Hour), "main", 1, 0, 2); err != nil {
		t.Fatalf("RecordDepth: %v", err)
	}

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	_, body := get(t, h, "/checks?target=main&since=24h")
	if !strings.Contains(body, `class="depth-chart"`) {
		t.Errorf("expected an inline SVG depth chart:\n%s", body)
	}
	if strings.Contains(body, "no data yet") {
		t.Errorf("chart present but body still says \"no data yet\":\n%s", body)
	}
}

// --- recent-outcome chips (bullet 1: bordered text pills -> compact squares) --

func TestIndex_RecentRunsRenderAsChips(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main")) // OutcomeRejected, topic "histfix"

	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, store)

	_, body := get(t, h, "/")

	if !strings.Contains(body, `class="chip chip-bad"`) {
		t.Errorf("expected a rejected run to render as a chip-bad square:\n%s", body)
	}
	if !strings.Contains(body, `href="/run/run-hist-1"`) {
		t.Errorf("expected the chip to link to /run/run-hist-1:\n%s", body)
	}
	if !strings.Contains(body, `title="rejected · histfix ·`) {
		t.Errorf("expected a title tooltip with outcome and topic:\n%s", body)
	}
	if strings.Contains(body, `class="tag bad">rejected</a>`) {
		t.Errorf("old bordered text pill still present:\n%s", body)
	}
}

func TestTarget_RecentRunsRenderAsChips(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main"))

	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, store)

	_, body := get(t, h, "/t/main")

	if !strings.Contains(body, `class="chip chip-bad"`) {
		t.Errorf("expected a rejected run to render as a chip-bad square on the target page:\n%s", body)
	}
	if !strings.Contains(body, `href="/run/run-hist-1" class="chip chip-bad"`) {
		t.Errorf("expected the chip anchor itself to carry the run link:\n%s", body)
	}
	// Unlike TestTarget_WaitingOrderAndParkedReasons (nil store), this
	// dashboard has a real store, so StoreEnabled is true and the parked
	// outcome tag must link to the run that parked it.
	if !strings.Contains(body, `<a href="/run/run-mallory-rejected" class="tag bad">rejected</a>`) {
		t.Errorf("expected the parked outcome tag to link to its run when history is enabled:\n%s", body)
	}
}

// --- short SHA rendering (bullet 2: fix SHA overflow) -------------------------

func TestIndex_ShortSHAWithFullTitle(t *testing.T) {
	snap := testSnapshot() // main's TargetTip is 40 'a's
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/")

	full := strings.Repeat("a", 40)
	short := full[:12]
	if !strings.Contains(body, `title="`+full+`"`) {
		t.Errorf("expected the full 40-char sha in a title attribute:\n%s", body)
	}
	if strings.Contains(body, ">"+full+"<") {
		t.Errorf("full 40-char sha rendered as visible text (should be short form):\n%s", body)
	}
	if !strings.Contains(body, ">"+short+"<") {
		t.Errorf("expected short sha %q as visible text:\n%s", short, body)
	}
}

// --- per-check output (/run/{id}) ---------------------------------------------

func TestRun_ChecksExpandWithOutput(t *testing.T) {
	store := openTestStore(t)
	rec := sampleRecord("run-hist-2", "main")
	rec.Checks[1].Output = hostileOutput // "test" (failed) gets hostile output
	emitRun(t, store, rec)

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/run/run-hist-2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}

	// lint passed with no captured output -> no <details> at all, just the
	// flat row; test failed with output -> the only <details> on the page.
	if n := strings.Count(body, "<details"); n != 1 {
		t.Errorf("expected exactly one <details> (lint has no output, test does), got %d:\n%s", n, body)
	}
	if !strings.Contains(body, `<details class="check-row" open>`) {
		t.Errorf("expected the failed check's <details> to start open:\n%s", body)
	}
	const escaped = "&lt;script&gt;alert(2)&lt;/script&gt;"
	if !strings.Contains(body, escaped) {
		t.Errorf("expected check output to be HTML-escaped inside <pre>:\n%s", body)
	}
	if strings.Contains(body, hostileOutput) {
		t.Errorf("check output rendered unescaped (XSS risk):\n%s", body)
	}
}

func TestRun_PassedCheckWithNoOutputStaysFlat(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-3", "main")) // lint: passed, Output == ""

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	_, body := get(t, h, "/run/run-hist-3")

	// "lint" has no output in sampleRecord, so it must not be wrapped in a
	// <details> at all — just the plain row.
	lintIdx := strings.Index(body, "lint")
	detailsIdx := strings.Index(body, "<details")
	if lintIdx == -1 {
		t.Fatalf("lint check missing from body:\n%s", body)
	}
	if detailsIdx != -1 && detailsIdx < lintIdx && strings.Index(body[detailsIdx:lintIdx], "</details>") == -1 {
		t.Errorf("lint (no output) appears to be wrapped in a <details>, want a flat row:\n%s", body)
	}
}

// --- batch identity (docs/plans/phase5.md §10 amendment 1) --------------------

// batchMemberRecord builds one member's RunRecord of a 3-member batch sharing
// batchID, mirroring queue/reconcile.go's per-member RunRecords (§3.3): same
// shape as sampleRecord, but with BatchID/Position/BatchSize set.
func batchMemberRecord(runID, target, batchID string, position int) *core.RunRecord {
	rec := sampleRecord(runID, target)
	rec.BatchID = batchID
	rec.Position = position
	rec.BatchSize = 3
	return rec
}

func TestRun_ShowsBatchIdentityAndLink(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, batchMemberRecord("batch-run-0", "main", "batch-xyz", 0))
	emitRun(t, store, batchMemberRecord("batch-run-1", "main", "batch-xyz", 1))
	emitRun(t, store, batchMemberRecord("batch-run-2", "main", "batch-xyz", 2))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/run/batch-run-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "landed in batch") {
		t.Errorf("expected batch identity line, body:\n%s", body)
	}
	if !strings.Contains(body, "2 of 3") {
		t.Errorf("expected 1-based position \"2 of 3\" (Position=1, BatchSize=3), body:\n%s", body)
	}
	if !strings.Contains(body, `href="/batch/batch-xyz"`) {
		t.Errorf("expected a link to /batch/batch-xyz, body:\n%s", body)
	}
}

func TestRun_OmitsBatchIdentityForSerialRun(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, sampleRecord("run-hist-1", "main")) // no BatchID

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	_, body := get(t, h, "/run/run-hist-1")
	if strings.Contains(body, "landed in batch") {
		t.Errorf("serial run must not show a batch identity line:\n%s", body)
	}
}

func TestBatch_ListsMembersInPositionOrder(t *testing.T) {
	store := openTestStore(t)
	// Emitted out of position order to prove the page sorts by position.
	emitRun(t, store, batchMemberRecord("batch-run-2", "main", "batch-xyz", 2))
	emitRun(t, store, batchMemberRecord("batch-run-0", "main", "batch-xyz", 0))
	emitRun(t, store, batchMemberRecord("batch-run-1", "main", "batch-xyz", 1))

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, body := get(t, h, "/batch/batch-xyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	i0 := strings.Index(body, "batch-run-0")
	i1 := strings.Index(body, "batch-run-1")
	i2 := strings.Index(body, "batch-run-2")
	if i0 == -1 || i1 == -1 || i2 == -1 {
		t.Fatalf("expected all 3 members listed, body:\n%s", body)
	}
	if !(i0 < i1 && i1 < i2) {
		t.Errorf("expected members in position order (0, 1, 2), got offsets %d, %d, %d:\n%s", i0, i1, i2, body)
	}
}

// TestBatch_HandlesRealSuffixedMemberRunIDs uses the actual RunID shape
// queue.memberRunID mints post-fix ("<batchID>" for member 0, "<batchID>-mN"
// for members 1..N-1) rather than the other batch tests' synthetic
// "batch-run-N" IDs — proving GET /run/{runID} and GET /batch/{batchID} both
// treat a suffixed RunID as the plain opaque path-string it is (no
// dashboard-side parsing assumes a particular RunID shape), and that the
// head member's RunID doubling as BatchID resolves both routes correctly.
func TestBatch_HandlesRealSuffixedMemberRunIDs(t *testing.T) {
	store := openTestStore(t)
	batchID := "20260705T130000Z-1-abc123def456"
	runIDs := []string{batchID, batchID + "-m1", batchID + "-m2"}
	for i, runID := range runIDs {
		emitRun(t, store, batchMemberRecord(runID, "main", batchID, i))
	}

	h := dashboard.New(func() *queue.Snapshot { return nil }, store)

	// The batch page, keyed on the bare batch id, lists all 3 suffixed
	// members in position order.
	resp, body := get(t, h, "/batch/"+batchID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /batch/%s status = %d, body:\n%s", batchID, resp.StatusCode, body)
	}
	var offsets []int
	for _, runID := range runIDs {
		i := strings.Index(body, runID)
		if i == -1 {
			t.Fatalf("expected member %q listed on the batch page, body:\n%s", runID, body)
		}
		offsets = append(offsets, i)
	}
	if !(offsets[0] < offsets[1] && offsets[1] < offsets[2]) {
		t.Errorf("expected members in position order, got offsets %v:\n%s", offsets, body)
	}

	// Each member's own run page resolves too, including the two suffixed
	// (non-head) RunIDs.
	for i, runID := range runIDs {
		resp, body := get(t, h, "/run/"+runID)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /run/%s status = %d, body:\n%s", runID, resp.StatusCode, body)
		}
		if !strings.Contains(body, "landed in batch") {
			t.Errorf("member %d (%s): expected batch identity line, body:\n%s", i, runID, body)
		}
		if !strings.Contains(body, fmt.Sprintf(`href="/batch/%s"`, batchID)) {
			t.Errorf("member %d (%s): expected a link back to /batch/%s, body:\n%s", i, runID, batchID, body)
		}
	}
}

func TestBatch_UnknownID404(t *testing.T) {
	store := openTestStore(t)
	h := dashboard.New(func() *queue.Snapshot { return nil }, store)
	resp, _ := get(t, h, "/batch/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- post-land hooks + ignored refs (S5-surface, S7c) -------------------------

// TestTarget_RendersLiveHookSection confirms the target page's "Post-land
// hooks" section renders a running hook's progress when WithHookSnapshot
// reports one for this target.
func TestTarget_RendersLiveHookSection(t *testing.T) {
	snap := testSnapshot()
	hookSnapshot := func(target string) (dashboard.LiveHook, bool) {
		if target != "main" {
			return dashboard.LiveHook{}, false
		}
		return dashboard.LiveHook{Target: "main", Running: true, CurrentHook: "deploy", HookIndex: 1, HookCount: 2}, true
	}
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil, dashboard.WithHookSnapshot(hookSnapshot))

	_, body := get(t, h, "/t/main")
	for _, want := range []string{"Post-land hooks", "deploy", "1", "2"} {
		if !strings.Contains(body, want) {
			t.Errorf("target body missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestTarget_NoHookSnapshotShowsIdle confirms the target page still renders
// the "Post-land hooks" section even without WithHookSnapshot wired up — it
// just omits live progress.
func TestTarget_NoHookSnapshotShowsIdle(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	_, body := get(t, h, "/t/main")
	if !strings.Contains(body, "Post-land hooks") {
		t.Errorf("target body missing Post-land hooks section:\n%s", body)
	}
}

// TestTarget_RendersDurableHookRuns confirms the target page renders the
// durable hook-run ledger (history.Store.HookRunSummaries, S1-C/S5) when a
// store is configured, seeded through the store's real Emit path: a terminal
// run (the runs row hook_runs FK-references), one EventHookStarted with
// HookCount=2 (owed=2), and one EventHookFinished (done=1) — owed>done and
// not skipped, so the row must render as crash-incomplete.
func TestTarget_RendersDurableHookRuns(t *testing.T) {
	snap := testSnapshot()
	store := openTestStore(t)
	at := time.Date(2026, 7, 5, 11, 30, 0, 0, time.UTC)

	emitRun(t, store, sampleRecord("run-hooks-page", "main"))
	if err := store.Emit(context.Background(), core.Event{
		Kind: core.EventHookStarted, Target: "main", RunID: "run-hooks-page",
		CheckName: "deploy", HookIndex: 0, HookCount: 2, At: at,
	}); err != nil {
		t.Fatalf("Emit(EventHookStarted): %v", err)
	}
	emitHook(t, store, "run-hooks-page", "deploy", core.CheckResult{Status: core.CheckPassed, Duration: 100 * time.Millisecond})

	h := dashboard.New(func() *queue.Snapshot { return snap }, store)

	_, body := get(t, h, "/t/main")
	for _, want := range []string{"Post-land hooks", "run-hooks-page", "crash-incomplete"} {
		if !strings.Contains(body, want) {
			t.Errorf("target body missing %q\nbody:\n%s", want, body)
		}
	}
	// Ignored refs are a daemon-level index-page section, never per-target
	// (their target segment names no configured target).
	if strings.Contains(body, "Recently ignored refs") {
		t.Errorf("target page must not render the ignored-refs section (daemon-level, index page only):\n%s", body)
	}
}

// TestIndex_RendersIgnoredRefs confirms the index page renders the
// daemon-level "Recently ignored refs" section (history.Store.IgnoredRefs,
// S7c) when a store holds ignored-ref rows — seeded through the store's real
// Emit path with the UNCONFIGURED target name ("nope"), exactly as
// reconcile's checkIgnoredRefs emits it. The section is daemon-level
// because an ignored ref's defining property is that its target segment
// names no configured target.
func TestIndex_RendersIgnoredRefs(t *testing.T) {
	snap := testSnapshot()
	store := openTestStore(t)
	if err := store.Emit(context.Background(), core.Event{
		Kind: core.EventIgnoredRef, Target: "nope",
		Candidate: core.Candidate{Ref: "refs/heads/for/nope/kim/typo"},
		Detail:    `target "nope" is not configured`,
		At:        time.Date(2026, 7, 5, 11, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Emit(EventIgnoredRef): %v", err)
	}

	h := dashboard.New(func() *queue.Snapshot { return snap }, store)

	_, body := get(t, h, "/")
	for _, want := range []string{
		"Recently ignored refs",
		"no configured target",
		"nope/kim/typo",
		"target &#34;nope&#34; is not configured",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestIndex_OmitsIgnoredRefsSectionWhenNone confirms the index page renders
// no ignored-refs section at all when the store holds no rows — the section
// is an attention flag, not a permanent fixture.
func TestIndex_OmitsIgnoredRefsSectionWhenNone(t *testing.T) {
	snap := testSnapshot()
	store := openTestStore(t)
	h := dashboard.New(func() *queue.Snapshot { return snap }, store)

	_, body := get(t, h, "/")
	if strings.Contains(body, "Recently ignored refs") {
		t.Errorf("index body has an ignored-refs section with no rows:\n%s", body)
	}
}

// TestIndex_BatchedRunGetsChipBatchedClass confirms the recent-runs chip
// strip's trivial visual grouping (docs/plans/phase5.md §10 amendment 1,
// "shared left-border/badge is enough"): a batch member's chip gets the
// chip-batched CSS class layered on top of its outcome class, a serial run's
// does not.
func TestIndex_BatchedRunGetsChipBatchedClass(t *testing.T) {
	store := openTestStore(t)
	emitRun(t, store, batchMemberRecord("batch-run-0", "main", "batch-xyz", 0))

	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, store)
	_, body := get(t, h, "/")

	if !strings.Contains(body, `class="chip chip-bad chip-batched"`) {
		t.Errorf("expected the batched run's chip to carry chip-batched, body:\n%s", body)
	}
}

// --- auto-refresh: fetch+morph, not meta-refresh -----------------------------

// idiomorphTestPath is the vendored asset's served URL, version-suffixed
// per server.go's idiomorphVersion/idiomorphURL (cache-busting: a re-vendor
// changes the URL) — kept in one place here since dashboard_test is an
// external test package and can't reach the unexported idiomorphURL var
// directly. Bump alongside server.go's idiomorphVersion.
const idiomorphTestPath = "/static/idiomorph-0.7.4.min.js"

// TestStatic_ServesIdiomorph confirms GET /static/idiomorph-<version>.min.js
// serves the vendored asset base.html's auto-refresh script depends on: a
// 200, a JS content type, and a non-empty body (proving the go:embed
// actually picked up the file, not just that the route exists).
func TestStatic_ServesIdiomorph(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	resp, body := get(t, h, idiomorphTestPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/javascript" {
		t.Errorf("Content-Type = %q, want text/javascript", ct)
	}
	if body == "" {
		t.Error("body is empty")
	}
	if !strings.Contains(body, "Idiomorph") {
		t.Errorf("body doesn't look like idiomorph's source:\n%s", body)
	}
}

// TestRefreshPages_CarryFetchMorphPolling confirms a live page (/, /t/main)
// carries the whole auto-refresh apparatus this phase replaced meta-refresh
// with: the no-JS <noscript> fallback, the vendored idiomorph <script src>,
// the inline poller (Idiomorph.morph/setInterval) and its in-flight guard
// (the busy flag, so a slow response can't morph stale state over a fresher
// one) — and that the old bare <meta http-equiv="refresh"> (a full reload,
// no noscript guard) is gone.
func TestRefreshPages_CarryFetchMorphPolling(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)

	for _, path := range []string{"/", "/t/main"} {
		_, body := get(t, h, path)
		for _, want := range []string{
			`<noscript><meta http-equiv="refresh" content="5"></noscript>`,
			`<script src="` + idiomorphTestPath + `"></script>`,
			"Idiomorph.morph(document.body, doc.body)",
			"setInterval(function ()",
			"var busy = false;",
			"if (document.hidden || busy) return;",
			".finally(function () { busy = false; });",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q\nbody:\n%s", path, want, body)
			}
		}
		if strings.Contains(body, `<meta http-equiv="refresh" content="5">` /* bare, no noscript */) &&
			!strings.Contains(body, `<noscript><meta http-equiv="refresh" content="5">`) {
			t.Errorf("%s: found a bare (non-noscript) meta-refresh tag:\n%s", path, body)
		}
	}
}

// TestNonRefreshPages_NoPolling confirms a static/historical page (/run/{id},
// which never sets baseData.Refresh) gets none of the polling machinery —
// no noscript meta-refresh, no idiomorph script tag, no poller — matching
// the pre-existing behavior that these pages don't auto-refresh at all.
func TestNonRefreshPages_NoPolling(t *testing.T) {
	h := dashboard.New(func() *queue.Snapshot { return nil }, nil)

	_, body := get(t, h, "/run/whatever")
	for _, absent := range []string{
		"meta http-equiv=\"refresh\"",
		"idiomorph-",
		"setInterval(function ()",
	} {
		if strings.Contains(body, absent) {
			t.Errorf("/run/whatever unexpectedly contains %q:\n%s", absent, body)
		}
	}
	// The tooltip machinery itself (applyTimeTooltips, unconditional on every
	// page) must still be present — only the .Refresh-gated polling is absent.
	if !strings.Contains(body, "function applyTimeTooltips()") {
		t.Error("/run/whatever missing applyTimeTooltips (should be present on every page, refresh or not)")
	}
}
