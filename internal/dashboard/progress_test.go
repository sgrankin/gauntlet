package dashboard_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/queue"
)

func TestCommitProgressTracksWaitingBatchAndRetry(t *testing.T) {
	store := openTestStore(t)
	sha := strings.Repeat("a", 40)
	c := core.Candidate{SHA: sha, Ref: "review", Target: "main", User: "author", Topic: "change"}
	now := time.Now()
	snap := &queue.Snapshot{At: now, Targets: []queue.TargetSnapshot{{Name: "main"}}}
	h := dashboard.New(func() *queue.Snapshot { return snap }, store)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	path := "/commit/" + sha + "?target=main&ref=review"
	if w := get(path); w.Code != 200 || !strings.Contains(w.Body.String(), "No run yet") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("pre-admission: %d %s", w.Code, w.Body.String())
	}
	snap.Targets[0].Waiting = []queue.WaitingEntry{{Candidate: c}}
	if w := get(path); w.Code != 200 || !strings.Contains(w.Body.String(), "Queued;") {
		t.Fatalf("waiting: %d %s", w.Code, w.Body.String())
	}
	other := core.Candidate{SHA: strings.Repeat("b", 40), Target: "main", Ref: "other", User: "second", Topic: "sibling"}
	tip := strings.Repeat("c", 40)
	snap.Targets[0].Waiting = nil
	snap.Targets[0].Pipeline = []queue.RunSnapshot{{Candidate: other, Members: []core.Candidate{other, c}, RunID: "batch", BatchID: "batch", MergeSHA: tip,
		StartedAt: now.Add(-time.Minute), Running: []queue.CurrentCheck{{Name: "test", StartedAt: now.Add(-time.Second)}}}}
	for _, lookup := range []string{path, "/commit/" + other.SHA, "/commit/" + tip} {
		w := get(lookup)
		if w.Code != http.StatusFound || w.Header().Get("Location") != "/run/batch" {
			t.Fatalf("live commit %s: %d %s", lookup, w.Code, w.Header().Get("Location"))
		}
	}
	if w := get("/run/batch"); w.Code != 200 || !strings.Contains(w.Body.String(), "Verification is in progress") || !strings.Contains(w.Body.String(), "author/change") || !strings.Contains(w.Body.String(), "test") {
		t.Fatalf("live run: %d %s", w.Code, w.Body.String())
	}
	snap.Targets[0].Pipeline = nil
	first := &core.RunRecord{RunID: "old", Candidate: c, Target: "main", MergeSHA: tip, Outcome: core.OutcomeRejected, StartedAt: now, EndedAt: now}
	emitRun(t, store, first)
	if w := get(path); w.Header().Get("Location") != "/run/old" {
		t.Fatalf("completed run: %d %s", w.Code, w.Header().Get("Location"))
	}
	second := *first
	second.RunID, second.Outcome = "retry", core.OutcomeLanded
	second.StartedAt, second.EndedAt = now.Add(time.Minute), now.Add(2*time.Minute)
	second.MergeSHA = strings.Repeat("d", 40)
	emitRun(t, store, &second)
	for _, lookup := range []string{path, "/commit/" + second.MergeSHA} {
		if w := get(lookup); w.Header().Get("Location") != "/run/retry" {
			t.Fatalf("latest attempt: %s %d %s", lookup, w.Code, w.Header().Get("Location"))
		}
	}
	if w := get("/commit/" + sha + "?target=other"); w.Code != 200 || !strings.Contains(w.Body.String(), "No run yet") {
		t.Fatal("target filter matched another target")
	}
	if w := get("/commit/" + sha + "?ref=other"); w.Code != 200 || !strings.Contains(w.Body.String(), "No run yet") {
		t.Fatal("ref filter matched another queue slot")
	}
	if w := get("/commit/not-a-sha"); w.Code != http.StatusBadRequest {
		t.Fatal("invalid SHA accepted")
	}
}

func TestCommitAndLiveRunWorkWithoutHistory(t *testing.T) {
	snap := testSnapshot()
	h := dashboard.New(func() *queue.Snapshot { return snap }, nil)
	for _, path := range []string{"/run/run-inflight-1", "/change?target=main&ref=unknown", "/commit/" + strings.Repeat("f", 40)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("no history %s: %d", path, w.Code)
		}
		if path == "/run/run-inflight-1" && !strings.Contains(w.Body.String(), "Verification is in progress") {
			t.Fatal("live run depends on history")
		}
	}
}

func TestChangeProgressFollowsNewHeadsAndAttempts(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	ref := "refs/heads/for/main/github/pr-0000000042"
	c := core.Candidate{SHA: strings.Repeat("a", 40), Ref: ref, Target: "main", User: "author", Topic: "PR42"}
	snap := &queue.Snapshot{At: now, Targets: []queue.TargetSnapshot{{Name: "main"}}}
	h := dashboard.New(func() *queue.Snapshot { return snap }, store)
	path := "/change?target=main&ref=" + url.QueryEscape(ref)
	get := func() *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Location") != "" {
			t.Fatalf("stable change URL: %d %v", w.Code, w.Header())
		}
		return w
	}
	if w := get(); !strings.Contains(w.Body.String(), "No run yet") {
		t.Fatal("pre-admission change link needs a run")
	}
	emitRun(t, store, &core.RunRecord{RunID: "failed-head", Candidate: c, Target: "main", Outcome: core.OutcomeRejected, StartedAt: now, EndedAt: now})
	if w := get(); !strings.Contains(w.Body.String(), "failed-head") {
		t.Fatal("completed change did not show its latest attempt")
	}
	c.SHA = strings.Repeat("b", 40)
	snap.Targets[0].Waiting = []queue.WaitingEntry{{Candidate: c}}
	if w := get(); !strings.Contains(w.Body.String(), "Queued;") {
		t.Fatal("new queued head was hidden by the previous failure")
	}
	snap.Targets[0].Waiting = nil
	snap.Targets[0].Pipeline = []queue.RunSnapshot{{Candidate: c, RunID: "new-head", StartedAt: now}}
	if w := get(); !strings.Contains(w.Body.String(), "new-head") || !strings.Contains(w.Body.String(), "Verification is in progress") {
		t.Fatal("change link did not follow new source head")
	}
	snap.Targets[0].Pipeline[0].RunID = "rebase-retry"
	if w := get(); !strings.Contains(w.Body.String(), "rebase-retry") {
		t.Fatal("change link did not follow rebase retry")
	}
	snap.Targets[0].Pipeline = nil
	emitRun(t, store, &core.RunRecord{RunID: "landed-head", Candidate: c, Target: "main", Outcome: core.OutcomeLanded, StartedAt: now.Add(time.Minute), EndedAt: now.Add(time.Minute)})
	if w := get(); !strings.Contains(w.Body.String(), "landed-head") || !strings.Contains(w.Body.String(), "Following the latest attempt") {
		t.Fatal("change link did not show the new head's results")
	}
	for _, invalid := range []string{"/change", "/change?target=main", "/change?ref=review"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", invalid, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("missing change identity accepted: %s", invalid)
		}
	}
}
