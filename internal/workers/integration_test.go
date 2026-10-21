package workers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"github.com/sgrankin/gauntlet/internal/flaky"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

func sourceFixture(t *testing.T) (*gitx.Repo, core.CheckJob) {
	t.Helper()
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"base": "base"})
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "change", map[string]string{"source": "exact bytes\n", ".gitattributes": "source export-subst\n"})
	head := remote.Ref(ref)
	repo, err := gitx.New(t.Context(), remote.BareClone(), remote.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	return repo, core.CheckJob{RecordLogs: true, Name: "test", Executor: "sql", RunID: "20261021T010101Z-original", MergeSHA: head, BaseSHA: base, Candidate: core.Candidate{Ref: ref, SHA: head}, Command: []string{"sh", "-c", `test "$(cat source)" = 'exact bytes' && git --git-dir="$GAUNTLET_GIT_DIR" cat-file -e "$GAUNTLET_CANDIDATE_SHA^{commit}" && echo verified && printf '%s' '{"tests":[{"id":"Sql.Test","status":"passed"}]}' > "$GAUNTLET_TEST_RESULTS_FILE"`}, TestResults: "json"}
}

func newAgentFixture(t *testing.T, url string, exec core.Executor) *Agent {
	t.Helper()
	state := t.TempDir()
	gitDir := filepath.Join(state, "objects.git")
	objects, err := gitx.OpenObjects(t.Context(), gitDir)
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{ShutdownTimeout: time.Second, Client: Client{URL: url, Token: "test-token"}, StateDir: state, GitDir: gitDir, Objects: objects, Executor: exec, Worker: workerFixture(), Poll: 5 * time.Millisecond}
}

func TestPullWorkerExecutesExactSourceAndUploadsLogsAndTests(t *testing.T) {
	objects, job := sourceFixture(t)
	broker, err := Open(filepath.Join(t.TempDir(), "broker.db"), time.Minute, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	server := httptest.NewServer((&Server{Broker: broker, Objects: objects, Token: "test-token", LogDir: t.TempDir()}).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	agent := newAgentFixture(t, server.URL, nil)
	agent.Executor = executor.Mux{Named: map[string]core.Executor{"sql": executor.LocalExecutor{GitDir: agent.GitDir}}}
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(ctx) }()
	remote := &Executor{Broker: broker, Root: ctx, Context: "config", Profiles: map[string]Requirements{"sql": requirementsFixture()}}
	result := remote.RunCheck(ctx, job)
	cancel()
	if err := <-agentDone; err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Status != core.CheckPassed || result.ExecutedSHA != job.MergeSHA || result.Worker != agent.Worker.ID || len(result.Tests) != 1 || result.ExecutionID == "" {
		t.Fatalf("wrong execution: %+v", result)
	}
	if _, err := os.Stat(result.LogPath); err != nil {
		t.Fatalf("missing transported log: %v", err)
	}
}

func TestSchedulerRestartRecoversRunningWorkerAndBufferedCompletion(t *testing.T) {
	objects, job := sourceFixture(t)
	job.TestResults = ""
	path := filepath.Join(t.TempDir(), "broker.db")
	logs := t.TempDir()
	broker, err := Open(path, time.Minute, 2*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[Server]
	current.Store(&Server{Broker: broker, Objects: objects, Token: "test-token", LogDir: logs})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := current.Load()
		if s == nil {
			http.Error(w, "scheduler restarting", 503)
			return
		}
		s.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	gate := executor.NewGatedExecutor()
	agent := newAgentFixture(t, server.URL, gate)
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	firstCtx, stopFirst := context.WithCancel(ctx)
	first := &Executor{Broker: broker, Root: firstCtx, Context: "unchanged", Profiles: map[string]Requirements{"sql": requirementsFixture()}}
	firstDone := make(chan core.CheckResult, 1)
	go func() { firstDone <- first.RunCheck(firstCtx, job) }()
	select {
	case <-gate.Started(job.RunID, job.Name):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	current.Store(nil)
	stopFirst()
	<-firstDone
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	gate.Release(job.RunID, job.Name, core.CheckResult{Status: core.CheckPassed, Output: "finished during scheduler outage"})
	// The command is allowed to finish while the endpoint is unavailable. The
	// same worker keeps and retries its completion across the scheduler restart.
	broker, err = Open(path, time.Minute, 2*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	current.Store(&Server{Broker: broker, Objects: objects, Token: "test-token", LogDir: logs})
	restored := &Executor{Broker: broker, Root: ctx, Context: "unchanged", Profiles: map[string]Requirements{"sql": requirementsFixture()}}
	result := restored.RunCheck(ctx, job)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Status != core.CheckPassed || result.Output != "finished during scheduler outage" || result.ExecutedSHA != job.MergeSHA {
		t.Fatalf("did not adopt exact completion: %+v", result)
	}
}

func TestWorkerEndpointsRequireBearerAndRejectUnownedAttempt(t *testing.T) {
	broker, err := Open(filepath.Join(t.TempDir(), "broker.db"), time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	handler := (&Server{Broker: broker, Token: "secret", LogDir: t.TempDir()}).Handler()
	request := httptest.NewRequest("POST", "/v1/claim", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("unauthenticated worker reached broker")
	}
	id, err := broker.Submit(t.Context(), "owned", jobFixture(), requirementsFixture(), 1)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := broker.Claim(t.Context(), workerFixture())
	if err != nil || attempt == nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("POST", "/v1/attempt/"+id+"/renew", strings.NewReader(`{"token":"wrong"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 409 {
		t.Fatalf("unowned lease renewed: %d", response.Code)
	}
}

func TestInputIdentityCommandsAlwaysExecuteFresh(t *testing.T) {
	objects, job := sourceFixture(t)
	job.TestResults = ""
	job.InputsCapture = true
	job.Command = []string{"sh", "-c", `printf stable-inputs > "$GAUNTLET_INPUTS_RESULT_FILE"`}
	broker, err := Open(filepath.Join(t.TempDir(), "broker.db"), time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	server := httptest.NewServer((&Server{Broker: broker, Objects: objects, Token: "test-token", LogDir: t.TempDir()}).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	agent := newAgentFixture(t, server.URL, nil)
	agent.Executor = executor.Mux{Named: map[string]core.Executor{"sql": executor.LocalExecutor{GitDir: agent.GitDir}}}
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	remote := &Executor{Broker: broker, Root: ctx, Context: "unchanged", Profiles: map[string]Requirements{"sql": requirementsFixture()}}
	first := remote.RunCheck(ctx, job)
	second := remote.RunCheck(ctx, job)
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if first.Err != nil || second.Err != nil || first.InputsKey != "stable-inputs" || second.InputsKey != first.InputsKey || first.ExecutionID == second.ExecutionID {
		t.Fatalf("input command was cached: %+v %+v", first, second)
	}
}

type retryClassifier struct{ calls atomic.Int32 }

func (c *retryClassifier) Classify(context.Context, core.CheckJob, core.CheckResult) (flaky.Decision, error) {
	c.calls.Add(1)
	return flaky.Decision{Action: "retry", Kind: "flake", Confidence: 1, Reason: "transient fixture failure"}, nil
}

func TestRemoteSubsetRetryRetainsLogsCoverageAndArtifactDigests(t *testing.T) {
	objects, job := sourceFixture(t)
	job.Command = []string{"sh", "-c", `echo ORIGINAL; printf '%s' '{"tests":[{"id":"green","status":"passed"},{"id":"red","status":"failed","message":"temporary"}]}' > "$GAUNTLET_TEST_RESULTS_FILE"; exit 1`}
	job.RetryCommand = []string{"sh", "-c", `test "$(cat "$GAUNTLET_RETRY_TESTS_FILE")" = '["red"]' && echo RETRY && printf '%s' '{"tests":[{"id":"red","status":"passed"}]}' > "$GAUNTLET_TEST_RESULTS_FILE"`}
	broker, err := Open(filepath.Join(t.TempDir(), "broker.db"), time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	server := httptest.NewServer((&Server{Broker: broker, Objects: objects, Token: "test-token", LogDir: t.TempDir()}).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	agent := newAgentFixture(t, server.URL, nil)
	agent.Executor = executor.Mux{Named: map[string]core.Executor{"sql": executor.LocalExecutor{GitDir: agent.GitDir}}}
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	remote := &Executor{Broker: broker, Root: ctx, Context: "retry", Profiles: map[string]Requirements{"sql": requirementsFixture()}}
	history, err := flaky.OpenHistory(filepath.Join(t.TempDir(), "failures.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	classifier := &retryClassifier{}
	retrier := &flaky.Retrier{History: history, Classifier: classifier, Checks: []string{"test"}, MaxRetries: 1, MaxRunRetries: 3, MinConfidence: .8, Timeout: time.Second}
	result := retrier.Run(ctx, job, remote.RunCheck)
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Status != core.CheckPassed || len(result.Tests) != 2 || len(result.TestAttempts) != 2 || classifier.calls.Load() != 1 {
		t.Fatalf("remote retry: %+v", result)
	}
	file, err := os.Open(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := zstd.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "ORIGINAL\n") != 1 || strings.Count(string(data), "RETRY\n") != 1 {
		t.Fatalf("attempt logs lost or duplicated: %s", data)
	}
	for _, attempt := range result.TestAttempts {
		data, err := os.ReadFile(attempt.LogPath)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != attempt.LogDigest {
			t.Fatal("uploaded attempt artifact was modified")
		}
	}
}
