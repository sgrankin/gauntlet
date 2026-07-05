package dashboard_test

import (
	"context"
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
