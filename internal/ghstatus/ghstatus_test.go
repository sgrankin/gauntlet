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

func TestChannel_NonMappedEventKindsDoNotPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := New(Params{Owner: "acme", Repo: "widgets", Token: "tok", APIURL: srv.URL, Log: io.Discard})

	for _, kind := range []core.EventKind{core.EventQueued, core.EventCheckStarted, core.EventCheckFinished} {
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

func TestChannel_DefaultAPIURL(t *testing.T) {
	c := New(Params{Owner: "a", Repo: "b", Token: "t"})
	if c.apiURL != "https://api.github.com" {
		t.Fatalf("apiURL = %q, want default", c.apiURL)
	}
}
