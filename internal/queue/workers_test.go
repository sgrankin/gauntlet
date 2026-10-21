package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/workers"
)

func TestRemoteChecksRecoverPreparedSHAAndLandWithoutRerunningWorker(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.NewGatedExecutor(), config.Target{Name: "main", Branch: "main", Landing: "squash"})
	h.remote.Seed("main", map[string]string{"base": "base"})
	ref := h.remote.PushCandidate("main", "alice", "fix", map[string]string{testCheckSpecPath: "workspace \"isolated\"\ncheck \"test\" {\n command \"true\"\n executor \"ci\"\n}\n"})
	state := t.TempDir()
	dbPath := filepath.Join(state, "workers.db")
	logs := filepath.Join(state, "logs")
	broker, err := workers.Open(dbPath, time.Minute, 2*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	var service atomic.Pointer[workers.Server]
	service.Store(&workers.Server{Broker: broker, Objects: h.git, Token: "token", LogDir: logs})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := service.Load()
		if s == nil {
			http.Error(w, "restarting", 503)
			return
		}
		s.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	workerDir := t.TempDir()
	workerGit := filepath.Join(workerDir, "objects.git")
	objects, err := gitx.OpenObjects(ctx, workerGit)
	if err != nil {
		t.Fatal(err)
	}
	gate := executor.NewGatedExecutor()
	agent := &workers.Agent{ShutdownTimeout: time.Second, Client: workers.Client{URL: server.URL, Token: "token"}, StateDir: workerDir, GitDir: workerGit, Objects: objects, Executor: gate, Worker: workers.Worker{ID: "worker-1", Profiles: []string{"ci"}, Capacity: workers.Resources{Slots: 1, CPU: 1000, Memory: 1 << 30}}, Poll: 5 * time.Millisecond}
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(ctx) }()
	firstCtx, stopFirst := context.WithCancel(ctx)
	remote := func(b *workers.Broker, root context.Context) *workers.Executor {
		return &workers.Executor{Broker: b, Root: root, Context: "same", Profiles: map[string]workers.Requirements{"ci": {Resources: workers.Resources{Slots: 1, CPU: 1000, Memory: 128 << 20}}}}
	}
	h.d.exec = remote(broker, firstCtx)
	h.d.cfg.ControlPath = filepath.Join(state, "controls.json")
	h.d.cfg.VerificationContext = "same"
	h.d.cfg.SourceGitDir = h.dir
	h.d.cfg.KnownExecutorProfile = func(name string) bool { return name == "ci" }
	h.d.cfg.RemoteExecutorProfile = h.d.cfg.KnownExecutorProfile
	if err = h.d.ReconcileOnce(firstCtx); err != nil {
		t.Fatal(err)
	}
	run := h.d.headRun("main")
	runID, testedSHA := run.runID, run.chainTip
	select {
	case <-gate.Started(runID, "test"):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stopFirst()
	service.Store(nil)
	// Wait for the scheduler-side waiter to detach before closing its database.
	select {
	case <-run.inflight["test"].result:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = broker.Close(); err != nil {
		t.Fatal(err)
	}
	broker, err = workers.Open(dbPath, time.Minute, 2*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	service.Store(&workers.Server{Broker: broker, Objects: h.git, Token: "token", LogDir: logs})
	h.d.cfg.KeepExecutions = broker.KeepRun
	restored, err := New(h.git, remote(broker, ctx), []core.Channel{h.ch}, h.d.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.d = restored
	if err = h.d.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if h.d.headRun("main").runID != runID || h.d.headRun("main").chainTip != testedSHA {
		t.Fatal("restart changed exact worker execution identity")
	}
	h.releaseGated(gate, runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if h.remote.Ref("refs/heads/main") != testedSHA || h.remote.Ref(ref) != "" {
		t.Fatal("did not publish exact recovered verification")
	}
	cancel()
	if err = <-agentDone; err != nil {
		t.Fatal(err)
	}
	records := h.ch.Records()
	landed := false
	for _, record := range records {
		if record.Outcome == core.OutcomeLanded {
			landed = true
			if len(record.Checks) != 1 || record.Checks[0].Worker != "worker-1" || record.Checks[0].ExecutedSHA != testedSHA {
				t.Fatalf("lost worker proof: %+v", record.Checks)
			}
		}
	}
	if !landed {
		t.Fatal("no landed record")
	}
}
