package ghstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// recorder captures one inbound request, guarded by a mutex so reads from
// the test goroutine after Emit returns are properly synchronized with
// writes from the httptest.Server's handler goroutine (kept -race clean).
type recorder struct {
	mu     sync.Mutex
	method string
	path   string
	auth   string
	raw    map[string]any
	body   statusPayload
}

func newRecordingServer(t *testing.T, status int) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Errorf("unmarshal raw body: %v", err)
		}
		var payload statusPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("unmarshal body: %v", err)
		}
		rec.mu.Lock()
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.auth = r.Header.Get("Authorization")
		rec.raw = raw
		rec.body = payload
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (r *recorder) snapshot() recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recorder{method: r.method, path: r.path, auth: r.auth, raw: r.raw, body: r.body}
}

func TestChannel_EventKindsPostExpectedStatus(t *testing.T) {
	cases := []struct {
		name      string
		ev        core.Event
		wantState string
		wantDesc  string
	}{
		{
			name: "trial clean",
			ev: core.Event{
				Kind:      core.EventTrialClean,
				Target:    "main",
				Candidate: core.Candidate{SHA: "deadbeefcafe"},
				RunID:     "run-1",
			},
			wantState: "pending",
			wantDesc:  "running checks",
		},
		{
			name: "landed",
			ev: core.Event{
				Kind:      core.EventLanded,
				Target:    "main",
				Candidate: core.Candidate{SHA: "deadbeefcafe"},
				RunID:     "run-1",
			},
			wantState: "success",
			wantDesc:  "landed",
		},
		{
			name: "rejected",
			ev: core.Event{
				Kind:      core.EventRejected,
				Target:    "main",
				Candidate: core.Candidate{SHA: "deadbeefcafe"},
				RunID:     "run-1",
				Record:    &core.RunRecord{Detail: "lint failed: 3 errors"},
			},
			wantState: "failure",
			wantDesc:  "lint failed: 3 errors",
		},
		{
			name: "trial conflict",
			ev: core.Event{
				Kind:      core.EventTrialConflict,
				Target:    "main",
				Candidate: core.Candidate{SHA: "deadbeefcafe"},
				RunID:     "run-1",
			},
			wantState: "failure",
			wantDesc:  "trial merge conflict",
		},
		{
			name: "error",
			ev: core.Event{
				Kind:      core.EventError,
				Target:    "main",
				Candidate: core.Candidate{SHA: "deadbeefcafe"},
				RunID:     "run-1",
				Record:    &core.RunRecord{Detail: "tempdir failed"},
			},
			wantState: "error",
			wantDesc:  "tempdir failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := newRecordingServer(t, http.StatusCreated)

			c := New(Params{
				Owner:        "acme",
				Repo:         "widgets",
				Token:        "tok123",
				APIURL:       srv.URL,
				DashboardURL: "https://dash.example",
				Log:          io.Discard,
			})

			if err := c.Emit(context.Background(), tc.ev); err != nil {
				t.Fatalf("Emit: %v", err)
			}

			got := rec.snapshot()
			if got.method != http.MethodPost {
				t.Errorf("method = %q, want POST", got.method)
			}
			wantPath := "/repos/acme/widgets/statuses/deadbeefcafe"
			if got.path != wantPath {
				t.Errorf("path = %q, want %q", got.path, wantPath)
			}
			if got.auth != "token tok123" {
				t.Errorf("Authorization = %q, want %q", got.auth, "token tok123")
			}
			if got.body.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.body.State, tc.wantState)
			}
			if got.body.Description != tc.wantDesc {
				t.Errorf("description = %q, want %q", got.body.Description, tc.wantDesc)
			}
			wantContext := "gauntlet/main"
			if got.body.Context != wantContext {
				t.Errorf("context = %q, want %q", got.body.Context, wantContext)
			}
			wantURL := "https://dash.example/run/run-1"
			if got.body.TargetURL != wantURL {
				t.Errorf("target_url = %q, want %q", got.body.TargetURL, wantURL)
			}
		})
	}
}

// The description is deliberately the record's Detail, never check output:
// live-run experience showed output first-lines quoting runtime/progress
// noise rather than the failing assertion. See detailOf.
func TestChannel_RejectedDescriptionIsRecordDetailNotOutput(t *testing.T) {
	srv, rec := newRecordingServer(t, http.StatusCreated)

	c := New(Params{Owner: "acme", Repo: "widgets", Token: "tok", APIURL: srv.URL, Log: io.Discard})
	ev := core.Event{
		Kind:      core.EventRejected,
		Target:    "main",
		Candidate: core.Candidate{SHA: "deadbeefcafe"},
		RunID:     "run-1",
		Record: &core.RunRecord{
			Detail: "check \"test\" failed",
			Checks: []core.CheckResult{
				{Name: "lint", Status: core.CheckPassed},
				{Name: "test", Status: core.CheckFailed,
					Output: "airbag_test.go:18: deploy at 148ms, want <= 25ms\nmore output\n"},
			},
		},
	}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := rec.snapshot()
	want := `check "test" failed`
	if got.body.Description != want {
		t.Errorf("description = %q, want %q", got.body.Description, want)
	}
}

func TestChannel_RejectedDescriptionFallsBackToDetailWithoutFailingCheck(t *testing.T) {
	srv, rec := newRecordingServer(t, http.StatusCreated)

	c := New(Params{Owner: "acme", Repo: "widgets", Token: "tok", APIURL: srv.URL, Log: io.Discard})
	ev := core.Event{
		Kind:      core.EventRejected,
		Target:    "main",
		Candidate: core.Candidate{SHA: "deadbeefcafe"},
		RunID:     "run-1",
		Record: &core.RunRecord{
			Detail: "missing .gauntlet.kdl",
			Checks: []core.CheckResult{{Name: "lint", Status: core.CheckPassed}},
		},
	}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := rec.snapshot()
	if got.body.Description != "missing .gauntlet.kdl" {
		t.Errorf("description = %q, want fallback to Record.Detail", got.body.Description)
	}
}

func TestChannel_SkippedDoesNotPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := New(Params{Owner: "acme", Repo: "widgets", Token: "tok", APIURL: srv.URL, Log: io.Discard})

	ev := core.Event{Kind: core.EventSkipped, Target: "main", Candidate: core.Candidate{SHA: "deadbeef"}}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

// TestChannel_NonMappedEventKindsDoNotPost also covers the universal
// contract: core.EventKind(999) (a future kind statusFor's switch has never
// heard of) must fall into the same default "no post" case as any other
// non-mapped kind, not panic or otherwise misbehave.
func TestChannel_NonMappedEventKindsDoNotPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := New(Params{Owner: "acme", Repo: "widgets", Token: "tok", APIURL: srv.URL, Log: io.Discard})

	for _, kind := range []core.EventKind{core.EventQueued, core.EventCheckStarted, core.EventCheckFinished, core.EventKind(999)} {
		ev := core.Event{Kind: kind, Target: "main", Candidate: core.Candidate{SHA: "deadbeef"}}
		if err := c.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit(%v): %v", kind, err)
		}
	}
}

func TestChannel_DescriptionCapped(t *testing.T) {
	long := strings.Repeat("x", 200)
	srv, rec := newRecordingServer(t, http.StatusCreated)

	c := New(Params{Owner: "a", Repo: "b", Token: "t", APIURL: srv.URL, Log: io.Discard})
	ev := core.Event{
		Kind:      core.EventRejected,
		Target:    "main",
		Candidate: core.Candidate{SHA: "deadbeef"},
		Record:    &core.RunRecord{Detail: long},
	}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := rec.snapshot()
	if n := len([]rune(got.body.Description)); n != descriptionCap {
		t.Errorf("description length = %d runes, want %d", n, descriptionCap)
	}
	if want := long[:descriptionCap]; got.body.Description != want {
		t.Errorf("description = %q, want first %d chars of input", got.body.Description, descriptionCap)
	}
}

func TestChannel_TargetURLOmittedWithoutDashboard(t *testing.T) {
	srv, rec := newRecordingServer(t, http.StatusCreated)

	c := New(Params{Owner: "a", Repo: "b", Token: "t", APIURL: srv.URL, Log: io.Discard})
	ev := core.Event{Kind: core.EventLanded, Target: "main", Candidate: core.Candidate{SHA: "deadbeef"}, RunID: "run-1"}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := rec.snapshot()
	if _, ok := got.raw["target_url"]; ok {
		t.Errorf("target_url present in body without a dashboard configured: %v", got.raw)
	}
}

func TestChannel_EmitDropsPostErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	c := New(Params{Owner: "a", Repo: "b", Token: "t", APIURL: srv.URL, Log: &logBuf})
	ev := core.Event{Kind: core.EventLanded, Target: "main", Candidate: core.Candidate{SHA: "deadbeef"}}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit must swallow POST errors, got: %v", err)
	}
	if logBuf.Len() == 0 {
		t.Error("expected the dropped error to be logged, got nothing")
	}
}

func TestChannel_EmitDropsUnreachableServerErrors(t *testing.T) {
	var logBuf bytes.Buffer
	// Port 0 on loopback with no listener: connection refused, fast.
	c := New(Params{Owner: "a", Repo: "b", Token: "t", APIURL: "http://127.0.0.1:0", Log: &logBuf})
	ev := core.Event{Kind: core.EventLanded, Target: "main", Candidate: core.Candidate{SHA: "deadbeef"}}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit must swallow connection errors, got: %v", err)
	}
	if logBuf.Len() == 0 {
		t.Error("expected the dropped error to be logged, got nothing")
	}
}

// A post-land hook failure must not repaint an already-green landing
// status: EventHookFinished is deliberately ignored, so Emit must never
// issue an HTTP request for it, pass or fail.
func TestChannel_HookFinishedDoesNotPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := New(Params{Owner: "acme", Repo: "widgets", Token: "tok", APIURL: srv.URL, Log: io.Discard})

	for _, check := range []*core.CheckResult{
		{Name: "deploy", Status: core.CheckFailed},
		{Name: "deploy", Status: core.CheckPassed},
	} {
		ev := core.Event{
			Kind:      core.EventHookFinished,
			Target:    "main",
			Candidate: core.Candidate{SHA: "deadbeef"},
			RunID:     "run-1",
			CheckName: "deploy",
			Check:     check,
		}
		if err := c.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit(%+v): %v", check, err)
		}
	}
}

func TestChannel_CommandsNeverYields(t *testing.T) {
	c := New(Params{Owner: "a", Repo: "b", Token: "t", Log: io.Discard})
	select {
	case cmd, ok := <-c.Commands():
		t.Fatalf("expected no command, got %v (ok=%v)", cmd, ok)
	case <-time.After(20 * time.Millisecond):
		// expected: nothing arrived
	}
}

func TestChannel_NilLogDefaultsToStderr(t *testing.T) {
	c := New(Params{Owner: "a", Repo: "b", Token: "t"})
	if c.log != os.Stderr {
		t.Fatalf("expected default log writer os.Stderr, got %v", c.log)
	}
}

// TestChannel_BatchLandingPostsOneStatusPerMemberSHA covers a batch's 3
// per-member EventLanded events (internal/queue/reconcile.go's landRun,
// looping FIFO over r.members): ghstatus is driven purely by ev.RunID/
// ev.Candidate.SHA/ev.Target/ev.Record, all already distinct per member
// before and after the RunID data-loss fix (queue.memberRunID) — so no
// ghstatus code change was needed, but this proves the batch shape
// explicitly: 3 members must produce exactly 3 status POSTs, one per
// member's own candidate SHA, each with its own record's target_url (now
// pointing at that member's own, possibly-suffixed, run page).
func TestChannel_BatchLandingPostsOneStatusPerMemberSHA(t *testing.T) {
	var mu sync.Mutex
	var posts []statusPayload
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var payload statusPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("unmarshal body: %v", err)
		}
		mu.Lock()
		posts = append(posts, payload)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(Params{
		Owner: "acme", Repo: "widgets", Token: "tok123",
		APIURL: srv.URL, DashboardURL: "https://dash.example", Log: io.Discard,
	})

	batchID := "20260705T130000Z-1-abc123def456"
	shas := []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc"}
	wantRunIDs := []string{batchID, batchID + "-m1", batchID + "-m2"}

	for i, sha := range shas {
		ev := core.Event{
			Kind:      core.EventLanded,
			Target:    "main",
			Candidate: core.Candidate{SHA: sha},
			RunID:     wantRunIDs[i],
			Record:    &core.RunRecord{RunID: wantRunIDs[i], BatchID: batchID, Position: i, BatchSize: 3, Outcome: core.OutcomeLanded},
		}
		if err := c.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit(member %d): %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 3 {
		t.Fatalf("got %d status POSTs, want 3 (one per batch member)", len(posts))
	}
	for i, sha := range shas {
		wantPath := "/repos/acme/widgets/statuses/" + sha
		if paths[i] != wantPath {
			t.Errorf("post %d path = %q, want %q", i, paths[i], wantPath)
		}
		if posts[i].State != "success" {
			t.Errorf("post %d state = %q, want success", i, posts[i].State)
		}
		wantURL := "https://dash.example/run/" + wantRunIDs[i]
		if posts[i].TargetURL != wantURL {
			t.Errorf("post %d target_url = %q, want %q (member %d's own run page)", i, posts[i].TargetURL, wantURL, i)
		}
	}
}

// scriptedTokens is a TokenSource handing out a fixed sequence; it
// records invalidations and advances only when the CURRENT token is the
// one invalidated (the ghauth.App guard).
type scriptedTokens struct {
	mu          sync.Mutex
	seq         []string
	i           int
	invalidated []string
}

func (s *scriptedTokens) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq[s.i], nil
}

func (s *scriptedTokens) Invalidate(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, tok)
	if s.seq[s.i] == tok && s.i+1 < len(s.seq) {
		s.i++
	}
}

// TestChannel_401InvalidatesAndRetriesOnce: an expired installation token
// gets exactly one fresh-mint retry; the second attempt must carry the
// NEW token.
func TestChannel_401InvalidatesAndRetriesOnce(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "token ghs_FAKENEW" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	tokens := &scriptedTokens{seq: []string{"ghs_FAKEOLD", "ghs_FAKENEW"}}
	var logBuf bytes.Buffer
	c := New(Params{Owner: "a", Repo: "b", Tokens: tokens, APIURL: srv.URL, Log: &logBuf})

	ev := core.Event{Kind: core.EventLanded, Target: "main", Candidate: core.Candidate{SHA: "deadbeef"}}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "token ghs_FAKEOLD" || auths[1] != "token ghs_FAKENEW" {
		t.Fatalf("auths = %v, want the old token then exactly one retry with the new one", auths)
	}
	if len(tokens.invalidated) != 1 || tokens.invalidated[0] != "ghs_FAKEOLD" {
		t.Fatalf("invalidated = %v, want exactly [ghs_FAKEOLD]", tokens.invalidated)
	}
	if logBuf.Len() != 0 {
		t.Errorf("log = %q after a recovered 401, want nothing (the retry succeeded)", logBuf.String())
	}
}

// TestChannel_Non401FailureIsNotRetried: only a clear credential
// rejection triggers the retry; a 500 (or 403) posts once and logs.
func TestChannel_Non401FailureIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	tokens := &scriptedTokens{seq: []string{"ghs_FAKEA"}}
	var logBuf bytes.Buffer
	c := New(Params{Owner: "a", Repo: "b", Tokens: tokens, APIURL: srv.URL, Log: &logBuf})

	ev := core.Event{Kind: core.EventLanded, Target: "main", Candidate: core.Candidate{SHA: "deadbeef"}}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit must swallow POST errors, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("posts = %d, want 1 (no retry on 403)", posts)
	}
	if len(tokens.invalidated) != 0 {
		t.Fatalf("invalidated = %v, want none", tokens.invalidated)
	}
	if logBuf.Len() == 0 {
		t.Error("expected the dropped 403 to be logged")
	}
	if strings.Contains(logBuf.String(), "ghs_FAKEA") {
		t.Errorf("token leaked into the log: %q", logBuf.String())
	}
}

// TestChannel_VerificationMode covers issue #7's merge-SHA statuses: in
// TrialRefs mode the rollup describes the synthetic MERGE, posted to the
// merge SHA, with EventLanded/EventTrialClean posting nothing.
func TestChannel_VerificationMode(t *testing.T) {
	const mergeSHA = "0f17merge17merge17merge17merge17merge170"
	rec := &core.RunRecord{MergeSHA: mergeSHA}
	cases := []struct {
		name      string
		ev        core.Event
		wantPost  bool
		wantState string
		wantSHA   string
	}{
		{
			name:      "trial merged -> pending @ merge",
			ev:        core.Event{Kind: core.EventTrialMerged, Target: "main", Candidate: core.Candidate{SHA: "cand"}, Record: rec},
			wantPost:  true,
			wantState: "pending",
			wantSHA:   mergeSHA,
		},
		{
			name:      "verified -> success @ merge",
			ev:        core.Event{Kind: core.EventVerified, Target: "main", Candidate: core.Candidate{SHA: "cand"}, Record: rec},
			wantPost:  true,
			wantState: "success",
			wantSHA:   mergeSHA,
		},
		{
			name:      "rejected -> failure @ merge",
			ev:        core.Event{Kind: core.EventRejected, Target: "main", Candidate: core.Candidate{SHA: "cand"}, Record: &core.RunRecord{MergeSHA: mergeSHA, Detail: "test failed"}},
			wantPost:  true,
			wantState: "failure",
			wantSHA:   mergeSHA,
		},
		{
			name:      "error -> error @ merge",
			ev:        core.Event{Kind: core.EventError, Target: "main", Candidate: core.Candidate{SHA: "cand"}, Record: &core.RunRecord{MergeSHA: mergeSHA, Detail: "export failed"}},
			wantPost:  true,
			wantState: "error",
			wantSHA:   mergeSHA,
		},
		{
			name:      "trial conflict -> failure @ candidate (no merge exists)",
			ev:        core.Event{Kind: core.EventTrialConflict, Target: "main", Candidate: core.Candidate{SHA: "candconflict"}},
			wantPost:  true,
			wantState: "failure",
			wantSHA:   "candconflict",
		},
		{
			// The landing must NOT repaint the verification status: success
			// already posted on EventVerified.
			name:     "landed posts nothing",
			ev:       core.Event{Kind: core.EventLanded, Target: "main", Candidate: core.Candidate{SHA: "cand"}, Record: rec},
			wantPost: false,
		},
		{
			// The pre-merge pending came from EventTrialMerged; the candidate
			// pending must not double-post.
			name:     "trial clean posts nothing",
			ev:       core.Event{Kind: core.EventTrialClean, Target: "main", Candidate: core.Candidate{SHA: "cand"}},
			wantPost: false,
		},
		{
			// A superseded/re-tested merge (e.g. batch -> serial fallback):
			// the serial re-runs carry the definitive statuses.
			name:     "skipped posts nothing",
			ev:       core.Event{Kind: core.EventSkipped, Target: "main", Candidate: core.Candidate{SHA: "cand"}, Record: rec},
			wantPost: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			posted := false
			var gotState, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				var p statusPayload
				_ = json.Unmarshal(data, &p)
				mu.Lock()
				posted = true
				gotState = p.State
				gotPath = r.URL.Path
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			c := New(Params{Owner: "acme", Repo: "widgets", Token: "t", APIURL: srv.URL, TrialRefs: true, Log: io.Discard})
			if err := c.Emit(context.Background(), tc.ev); err != nil {
				t.Fatalf("Emit: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if posted != tc.wantPost {
				t.Fatalf("posted = %v, want %v", posted, tc.wantPost)
			}
			if !tc.wantPost {
				return
			}
			if gotState != tc.wantState {
				t.Errorf("state = %q, want %q", gotState, tc.wantState)
			}
			if want := "/repos/acme/widgets/statuses/" + tc.wantSHA; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
		})
	}
}

// TestChannel_VerificationModeNoMergeSHASkips: a terminal event whose
// record carries no MergeSHA (defensive — should not happen for a
// published run) posts nothing rather than to an empty SHA.
func TestChannel_VerificationModeNoMergeSHASkips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := New(Params{Owner: "a", Repo: "b", Token: "t", APIURL: srv.URL, TrialRefs: true, Log: io.Discard})
	ev := core.Event{Kind: core.EventVerified, Target: "main", Record: &core.RunRecord{MergeSHA: ""}}
	if err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

func TestChannel_DefaultAPIURL(t *testing.T) {
	c := New(Params{Owner: "a", Repo: "b", Token: "t"})
	if c.apiURL != "https://api.github.com" {
		t.Fatalf("apiURL = %q, want default", c.apiURL)
	}
}
