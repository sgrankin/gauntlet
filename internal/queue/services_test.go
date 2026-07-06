package queue

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/services"
)

// fakeServicePool is a ServicePool test double (docs/plans/services-impl.md
// §4.6): an in-memory affordance struct — scriptable EnsureAll error,
// scriptable AnyDead, and recorded Release calls — in the same spirit as
// executor.GatedExecutor, never a mock framework. It never touches a real
// driver/container: EnsureAll fabricates env pairs straight from needs, the
// same shape the real pool produces (services.md §4), so the queue-layer
// wrapper (reconcile.go's startCheck) can be tested without chunk 2's own
// pool machinery.
type fakeServicePool struct {
	mu sync.Mutex

	ensureErr error // returned by every EnsureAll call, if non-nil
	anyDead   bool  // returned by every AnyDead call

	ensureCalls  int
	anyDeadCalls int
	releaseCalls []services.Ensured
	armed        bool
}

func (f *fakeServicePool) EnsureAll(ctx context.Context, svcs []config.Service, needs []string) (services.Ensured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	if f.ensureErr != nil {
		return services.Ensured{}, f.ensureErr
	}
	var env []string
	for _, name := range needs {
		up := strings.ToUpper(name)
		env = append(env, "GAUNTLET_SVC_"+up+"_HOST=127.0.0.1", "GAUNTLET_SVC_"+up+"_PORT=54321")
	}
	return services.Ensured{Env: env}, nil
}

func (f *fakeServicePool) Release(e services.Ensured) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, e)
}

func (f *fakeServicePool) AnyDead(ctx context.Context, e services.Ensured) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.anyDeadCalls++
	return f.anyDead
}

func (f *fakeServicePool) ArmReaper() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = true
}

func (f *fakeServicePool) releaseCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.releaseCalls)
}

// recordingGatedExecutor wraps executor.GatedExecutor to additionally record
// every core.CheckJob RunCheck is called with, so tests can assert what the
// startCheck wrapper actually handed the executor (docs/plans/
// services-impl.md §4.6 "env injection", "passing check + needs") — a thin
// recording layer, not a replacement gate/release mechanism, which it
// delegates to the embedded GatedExecutor unchanged.
type recordingGatedExecutor struct {
	*executor.GatedExecutor
	mu   sync.Mutex
	jobs []core.CheckJob
}

func newRecordingGatedExecutor() *recordingGatedExecutor {
	return &recordingGatedExecutor{GatedExecutor: executor.NewGatedExecutor()}
}

func (r *recordingGatedExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	r.mu.Lock()
	r.jobs = append(r.jobs, job)
	r.mu.Unlock()
	return r.GatedExecutor.RunCheck(ctx, job)
}

func (r *recordingGatedExecutor) lastJob() core.CheckJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[len(r.jobs)-1]
}

// serviceCheckSpec renders a .gauntlet.kdl declaring one service ("mssql",
// matching the design doc's own running example — services-impl.md §4.6's
// "env injection" row names it GAUNTLET_SVC_MSSQL_HOST/PORT) and one check
// per checkNames, each declaring `needs "mssql"`. image/port are arbitrary:
// fakeServicePool never creates anything, so they're never dereferenced —
// they only need to satisfy config.ParseChecks's validation.
func serviceCheckSpec(checkNames ...string) map[string]string {
	var b strings.Builder
	b.WriteString("service \"mssql\" {\n    image \"example/mssql\"\n    port 1433\n}\n")
	for _, n := range checkNames {
		fmt.Fprintf(&b, "check %q {\n    command \"true\"\n    needs \"mssql\"\n}\n", n)
	}
	return map[string]string{testCheckSpecPath: b.String()}
}

// waitForRunTerminal polls ReconcileOnce (never sleeping) until a terminal
// event for runID appears, or fails the test. Unlike testHarness.release,
// this doesn't deliver anything to the executor first — it exists for the
// ensure-time-failure path, where the check goroutine never calls
// d.exec.RunCheck at all (EnsureAll itself errors before RunCheck would ever
// run), so there is nothing to Release and h.release would simply never see
// its own EventCheckFinished.
func waitForRunTerminal(h *testHarness, runID string) core.Event {
	h.t.Helper()
	for i := 0; i < 100000; i++ {
		h.reconcile()
		for _, e := range h.ch.Events() {
			if e.RunID == runID && isTerminalEventKind(e.Kind) {
				return e
			}
		}
		runtime.Gosched()
	}
	h.t.Fatalf("no terminal event observed for run %s", runID)
	return core.Event{}
}

// TestServices_Gating_RejectedLoud covers docs/plans/services-impl.md §4.6's
// "gating" row: a spec declaring needs on a daemon with Config.Services ==
// nil is rejected loudly (services.md §7 "loud like a malformed check"),
// never silently run without its dependency.
func TestServices_Gating_RejectedLoud(t *testing.T) {
	h := newHarness(t) // plain harness: Config.Services stays nil
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", serviceCheckSpec("test"))

	h.reconcile()

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if !strings.Contains(last.Detail, "no services block") {
		t.Errorf("Detail = %q, want it to mention the missing services block", last.Detail)
	}
}

// TestServices_NeedsFreeCheckUnaffected covers the "needs-free unaffected"
// row: a check with no `needs` must never touch the pool at all, even when
// one is configured — poisoning EnsureAll here would fail the test if the
// wrapper ever called it for this check.
func TestServices_NeedsFreeCheckUnaffected(t *testing.T) {
	pool := &fakeServicePool{ensureErr: errors.New("EnsureAll must not be called for a needs-free check")}
	h := newHarnessWithServices(t, pool)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test")) // no `needs`

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if pool.ensureCalls != 0 {
		t.Errorf("EnsureAll called %d times, want 0 for a needs-free check", pool.ensureCalls)
	}
}

// TestServices_EnsureTimeFailure covers "ensure-time failure": EnsureAll
// erroring must park the run as OutcomeError (park-as-error, §7), never
// OutcomeRejected — a candidate must not read as "your code is broken"
// because a service failed to come up.
func TestServices_EnsureTimeFailure(t *testing.T) {
	pool := &fakeServicePool{ensureErr: errors.New("ensure boom")}
	h := newHarnessWithServices(t, pool)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", serviceCheckSpec("test"))

	h.reconcile()
	runID := h.currentRunID()
	waitForRunTerminal(h, runID)

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error (ensure-time failure parks as error, not rejected)", last.Outcome)
	}
	if pool.releaseCallCount() != 0 {
		t.Errorf("Release called %d times, want 0 (EnsureAll itself failed; nothing was ever ensured)", pool.releaseCallCount())
	}
}

// TestServices_MidRunDeath_M1 covers "mid-run death (M1)": a FAILED check
// whose service died mid-run must convert to OutcomeError (never a
// rejected/false-negative park), with the check's captured output retained
// for the skeptical.
func TestServices_MidRunDeath_M1(t *testing.T) {
	pool := &fakeServicePool{anyDead: true}
	h := newHarnessWithServices(t, pool)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", serviceCheckSpec("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckFailed, Output: "boom output"})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeError {
		t.Fatalf("Outcome = %v, want Error (M1 red->Err conversion)", last.Outcome)
	}
	lastCheck := last.Checks[len(last.Checks)-1]
	if lastCheck.Err == nil {
		t.Fatal("Checks[last].Err = nil, want the M1 conversion error")
	}
	if !strings.Contains(lastCheck.Output, "boom output") {
		t.Errorf("Checks[last].Output = %q, want it to retain the check's captured output", lastCheck.Output)
	}
	if pool.ensureCalls != 1 {
		t.Errorf("EnsureAll called %d times, want 1", pool.ensureCalls)
	}
	if pool.anyDeadCalls != 1 {
		t.Errorf("AnyDead called %d times, want 1 (a failed check re-probes exactly once)", pool.anyDeadCalls)
	}
	if pool.releaseCallCount() != 1 {
		t.Errorf("Release called %d times, want 1", pool.releaseCallCount())
	}
}

// TestServices_PassingCheck_EnvInjectionAndNoDeadProbe covers both "passing
// check + needs" (AnyDead never consulted; ServiceEnv reached the executor)
// and "env injection" (GAUNTLET_SVC_MSSQL_HOST/PORT present in the job the
// recording executor saw) in one run, plus "release always"'s pass-case row.
func TestServices_PassingCheck_EnvInjectionAndNoDeadProbe(t *testing.T) {
	pool := &fakeServicePool{}
	exec := newRecordingGatedExecutor()
	h := newHarnessWithExecutor(t, exec, pool)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", serviceCheckSpec("test"))

	h.reconcile()
	runID := h.currentRunID()
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	if pool.anyDeadCalls != 0 {
		t.Errorf("AnyDead called %d times, want 0 (a passing check never re-probes)", pool.anyDeadCalls)
	}
	if pool.releaseCallCount() != 1 {
		t.Errorf("Release called %d times, want 1", pool.releaseCallCount())
	}

	job := exec.lastJob()
	wantEnv := []string{"GAUNTLET_SVC_MSSQL_HOST=127.0.0.1", "GAUNTLET_SVC_MSSQL_PORT=54321"}
	if !slices.Equal(job.ServiceEnv, wantEnv) {
		t.Errorf("job.ServiceEnv = %v, want %v", job.ServiceEnv, wantEnv)
	}
}
