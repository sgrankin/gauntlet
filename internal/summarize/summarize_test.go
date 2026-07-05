package summarize_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/summarize"
)

// fakeGit implements summarize.Git without touching real git.
type fakeGit struct {
	commits  []gitx.CommitInfo
	diffstat string
	logErr   error
	diffErr  error

	logCalled, diffCalled bool
}

func (g *fakeGit) Log(ctx context.Context, base, tip string) ([]gitx.CommitInfo, error) {
	g.logCalled = true
	if g.logErr != nil {
		return nil, g.logErr
	}
	return g.commits, nil
}

func (g *fakeGit) DiffStat(ctx context.Context, base, tip string) (string, error) {
	g.diffCalled = true
	if g.diffErr != nil {
		return "", g.diffErr
	}
	return g.diffstat, nil
}

func candidate() core.Candidate {
	return core.Candidate{
		Ref:    "refs/heads/for/main/alice/widget",
		Target: "main",
		User:   "alice",
		Topic:  "widget",
		SHA:    "cand0123",
	}
}

// decodeRequest parses the JSON body the Summarizer actually sent, for
// assertions on request shape.
func decodeRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

func TestMergeBody_RequestShapeAndParsing(t *testing.T) {
	var gotBody map[string]any
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody = decodeRequest(t, r)
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": "  Adds widget support.  "}},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	git := &fakeGit{
		commits: []gitx.CommitInfo{
			{Subject: "Add widget renderer", Body: "Handles the new widget type end to end."},
			{Subject: "Fix widget lint"},
		},
		diffstat: " widget.go | 42 +++\n 1 file changed, 42 insertions(+)",
	}
	s := summarize.New(summarize.Params{
		Git:     git,
		APIKey:  "test-key",
		BaseURL: srv.URL,
	})

	got := s.MergeBody(context.Background(), candidate(), "base0123")
	if got != "Adds widget support." {
		t.Fatalf("MergeBody = %q, want trimmed text", got)
	}
	if !git.logCalled || !git.diffCalled {
		t.Fatalf("expected both Log and DiffStat to be called")
	}

	if gotHeaders.Get("x-api-key") != "test-key" {
		t.Errorf("x-api-key header = %q", gotHeaders.Get("x-api-key"))
	}
	if gotHeaders.Get("anthropic-version") == "" {
		t.Errorf("anthropic-version header missing")
	}
	if gotBody["model"] != summarize.DefaultModel {
		t.Errorf("model = %v, want default %q", gotBody["model"], summarize.DefaultModel)
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Errorf("max_tokens missing from request")
	}
	sys, _ := gotBody["system"].(string)
	if sys == "" {
		t.Errorf("system prompt missing from request")
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v, want a single user message", gotBody["messages"])
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("messages[0].role = %v, want user", msg["role"])
	}
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "Add widget renderer") || !strings.Contains(content, "Fix widget lint") {
		t.Errorf("user content missing commit subjects: %q", content)
	}
	if !strings.Contains(content, "Handles the new widget type end to end.") {
		t.Errorf("user content missing commit body: %q", content)
	}
	if !strings.Contains(content, "widget.go") {
		t.Errorf("user content missing diffstat: %q", content)
	}
}

func TestMergeBody_CustomModel(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		})
	}))
	defer srv.Close()

	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		Model:   "claude-opus-4-8",
		BaseURL: srv.URL,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "ok" {
		t.Fatalf("MergeBody = %q", got)
	}
	if gotBody["model"] != "claude-opus-4-8" {
		t.Errorf("model = %v, want claude-opus-4-8", gotBody["model"])
	}
}

// TestMergeBody_EffortIncludedWhenConfigured covers the default-model,
// effort-configured shape: output_config.effort must appear, nested (not
// top-level), with exactly the configured value.
func TestMergeBody_EffortIncludedWhenConfigured(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		})
	}))
	defer srv.Close()

	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		Effort:  "medium",
		BaseURL: srv.URL,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "ok" {
		t.Fatalf("MergeBody = %q", got)
	}
	if gotBody["model"] != summarize.DefaultModel {
		t.Errorf("model = %v, want default %q", gotBody["model"], summarize.DefaultModel)
	}
	oc, ok := gotBody["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing or wrong shape: %v", gotBody["output_config"])
	}
	if oc["effort"] != "medium" {
		t.Errorf("output_config.effort = %v, want %q", oc["effort"], "medium")
	}
	if _, top := gotBody["effort"]; top {
		t.Errorf("effort must be nested under output_config, not top-level: %v", gotBody["effort"])
	}
}

// TestMergeBody_EffortAbsentForHaikuWithEmptyEffort covers the
// backward-compatibility contract: a haiku model configured with an empty
// Effort (the zero value) must keep working exactly as before Effort
// existed — no output_config field sent at all, since claude-haiku-4-5
// rejects it.
func TestMergeBody_EffortAbsentForHaikuWithEmptyEffort(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		})
	}))
	defer srv.Close()

	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		Model:   "claude-haiku-4-5",
		Effort:  "",
		BaseURL: srv.URL,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "ok" {
		t.Fatalf("MergeBody = %q", got)
	}
	if gotBody["model"] != "claude-haiku-4-5" {
		t.Errorf("model = %v, want claude-haiku-4-5", gotBody["model"])
	}
	if _, ok := gotBody["output_config"]; ok {
		t.Errorf("output_config = %v, want absent when Effort is empty", gotBody["output_config"])
	}
}

func TestMergeBody_NoCommitsSkipsAPICall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: nil},
		APIKey:  "k",
		BaseURL: srv.URL,
		Log:     &logBuf,
	})
	got := s.MergeBody(context.Background(), candidate(), "base")
	if got != "" {
		t.Fatalf("MergeBody = %q, want empty for no commits", got)
	}
	if called {
		t.Fatalf("expected no API call when there are no commits")
	}
	if logBuf.Len() == 0 {
		t.Errorf("expected a log line for the no-commits case")
	}
}

func TestMergeBody_LogError(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	s := summarize.New(summarize.Params{
		Git:     &fakeGit{logErr: errors.New("boom")},
		APIKey:  "k",
		BaseURL: srv.URL,
		Log:     &logBuf,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "" {
		t.Fatalf("MergeBody = %q, want empty on git log error", got)
	}
	if called {
		t.Fatalf("expected no API call when Log fails")
	}
	if logBuf.Len() == 0 {
		t.Errorf("expected a log line for the git log error")
	}
}

func TestMergeBody_DiffStatError(t *testing.T) {
	var logBuf bytes.Buffer
	s := summarize.New(summarize.Params{
		Git: &fakeGit{
			commits: []gitx.CommitInfo{{Subject: "x"}},
			diffErr: errors.New("boom"),
		},
		APIKey: "k",
		Log:    &logBuf,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "" {
		t.Fatalf("MergeBody = %q, want empty on diffstat error", got)
	}
	if logBuf.Len() == 0 {
		t.Errorf("expected a log line for the diffstat error")
	}
}

func TestMergeBody_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "too slow"}},
		})
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		BaseURL: srv.URL,
		Timeout: 20 * time.Millisecond,
		Log:     &logBuf,
	})
	got := s.MergeBody(context.Background(), candidate(), "base")
	if got != "" {
		t.Fatalf("MergeBody = %q, want empty on timeout", got)
	}
	if logBuf.Len() == 0 {
		t.Errorf("expected a log line for the timeout")
	}
}

func TestMergeBody_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		BaseURL: srv.URL,
		Log:     &logBuf,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "" {
		t.Fatalf("MergeBody = %q, want empty on API 5xx", got)
	}
	if logBuf.Len() == 0 {
		t.Errorf("expected a log line for the API error")
	}
}

func TestMergeBody_API4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`))
	}))
	defer srv.Close()

	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		BaseURL: srv.URL,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "" {
		t.Fatalf("MergeBody = %q, want empty on API 4xx", got)
	}
}

func TestMergeBody_EmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	s := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		BaseURL: srv.URL,
		Log:     &logBuf,
	})
	if got := s.MergeBody(context.Background(), candidate(), "base"); got != "" {
		t.Fatalf("MergeBody = %q, want empty for empty content", got)
	}
	if logBuf.Len() == 0 {
		t.Errorf("expected a log line for the empty-content case")
	}
}

func TestMergeBody_Refusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{},
			"stop_reason": "refusal",
		})
	}))
	defer srv.Close()

	if got := summarize.New(summarize.Params{
		Git:     &fakeGit{commits: []gitx.CommitInfo{{Subject: "x"}}},
		APIKey:  "k",
		BaseURL: srv.URL,
	}).MergeBody(context.Background(), candidate(), "base"); got != "" {
		t.Fatalf("MergeBody = %q, want empty on refusal", got)
	}
}
