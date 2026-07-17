// Isolated-workspace acceptance suite (issue #9): the concurrency and
// lifecycle guarantees the feature exists to make safe, proven at the queue
// API against the fake git harness (daemon_test.go) rather than inferred
// from the implementation. One discriminating regression per boundary:
// distinct per-node directories, private materialization of image-build
// nodes with identity handoff intact, the execution cap bounding in-flight
// materialization, cancellation mid-materialize releasing the slot and
// cleaning partial state, and batch wiring exporting the trial TREE (not the
// chain-tip commit).
package queue

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// jobDirByName returns each recorded RunCheck job's Dir keyed by check name.
func (r *recordingGatedExecutor) jobDirByName() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.jobs))
	for _, j := range r.jobs {
		out[j.Name] = j.Dir
	}
	return out
}

// jobImageByName returns each recorded RunCheck job's Image keyed by name.
func (r *recordingGatedExecutor) jobImageByName() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.jobs))
	for _, j := range r.jobs {
		out[j.Name] = j.Image
	}
	return out
}

// TestIsolated_ConcurrentNodesGetDistinctDirs is the queue half of the
// "two simultaneously live nodes never collide" guarantee (issue #9,
// acceptance criterion 1). Two isolated checks run at once (max-parallel 2,
// two execution slots); each must be handed its OWN private workspace
// directory under WorkDir. The executor then bind-mounts each node's job.Dir
// at the profile's fixed container workdir (container.go's `-v
// job.Dir:Workdir`, asserted in executor/container_test.go), so distinct host
// dirs at the same in-container path follows directly — conflicting
// mutations cannot cross because the host directories are distinct.
func TestIsolated_ConcurrentNodesGetDistinctDirs(t *testing.T) {
	rec := newRecordingGatedExecutor()
	h := newHarnessWithExecutor(t, rec, nil)
	workDir := t.TempDir()
	h.d.cfg.WorkDir = workDir
	h.d.cfg.Slots = core.NewSlots(2) // both nodes may hold a slot at once

	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", isolatedSpecFile(2, "a", "b"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "a")
	h.awaitStarted(runID, "b")

	dirs := rec.jobDirByName()
	da, db := dirs["a"], dirs["b"]
	if da == "" || db == "" {
		t.Fatalf("isolated nodes got empty job.Dir: a=%q b=%q", da, db)
	}
	if da == db {
		t.Fatalf("both isolated nodes share job.Dir %q — they must get distinct private workspaces", da)
	}
	for name, dir := range map[string]string{"a": da, "b": db} {
		if filepath.Dir(dir) != workDir {
			t.Errorf("node %q workspace %q is not directly under WorkDir %q", name, dir, workDir)
		}
		if !strings.HasPrefix(filepath.Base(dir), nodeWorkspacePrefix) {
			t.Errorf("node %q workspace base %q lacks the %q prefix", name, filepath.Base(dir), nodeWorkspacePrefix)
		}
	}

	h.release(runID, "a", core.CheckResult{Name: "a", Status: core.CheckPassed})
	h.release(runID, "b", core.CheckResult{Name: "b", Status: core.CheckPassed})
}

// TestIsolated_ImageBuildNodeIsPrivateAndHandsOffIdentity is acceptance
// criterion 2: an image-build node materializes its OWN private workspace
// (its incidental writes therefore live only in that dir, never a shared
// tree), and the immutable identity it captures still reaches consumers
// unchanged under isolation. Build + two consumers → three private
// materializations, and each consumer's job carries the captured image ID.
func TestIsolated_ImageBuildNodeIsPrivateAndHandsOffIdentity(t *testing.T) {
	rec := newRecordingGatedExecutor()
	h := newHarnessWithExecutor(t, rec, nil)
	h.d.cfg.WorkDir = t.TempDir()
	h.d.cfg.ImageCapableProfile = func(string) bool { return true }

	spec := "workspace \"isolated\"\nmax-parallel 4\n" +
		"image \"go-ci\" {\n    command \"./ci/build-image\"\n}\n" +
		"check \"unit\" {\n    command \"true\"\n    image \"go-ci\"\n}\n" +
		"check \"lint\" {\n    command \"true\"\n    image \"go-ci\"\n}\n"
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", map[string]string{testCheckSpecPath: spec})

	h.reconcile()
	runID := h.currentRunID()
	// The build node itself is isolated: it materializes before it runs.
	h.awaitStarted(runID, "image:go-ci")
	if got := h.git.exportCalls; got != 1 {
		t.Fatalf("exportCalls after build start = %d, want 1 (the build node's own private workspace)", got)
	}
	h.release(runID, "image:go-ci", core.CheckResult{Name: "image:go-ci", Status: core.CheckPassed, Image: localID})

	h.awaitStarted(runID, "unit")
	h.awaitStarted(runID, "lint")
	// Each consumer materialized its own private workspace too: 1 build + 2
	// consumers = 3 exports, none shared.
	if got := h.git.exportCalls; got != 3 {
		t.Fatalf("exportCalls = %d, want 3 (build + two consumers, each private)", got)
	}
	// Identity handoff survives isolation: consumers run with the build's
	// captured immutable ID, not files the build wrote beside its context.
	images := rec.jobImageByName()
	if images["unit"] != localID || images["lint"] != localID {
		t.Fatalf("consumer job.Image = unit:%q lint:%q, want both %q", images["unit"], images["lint"], localID)
	}

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed, Image: localID})
	h.release(runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckPassed, Image: localID})
	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
}

// TestIsolated_CapBoundsInFlightMaterialization is acceptance criterion 3
// (the cap half): materialization begins only after a node wins a
// daemon-wide execution slot, and never exceeds the cap. With one slot and
// two ready isolated nodes, exactly one materialization is ever in flight —
// the second cannot even begin archiving until the first frees its slot.
func TestIsolated_CapBoundsInFlightMaterialization(t *testing.T) {
	rec := newRecordingGatedExecutor()
	h := newHarnessWithExecutor(t, rec, nil)
	h.d.cfg.WorkDir = t.TempDir()
	h.d.cfg.Slots = core.NewSlots(1) // one execution slot for the whole daemon

	var inFlight, maxInFlight int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	h.git.exportHook = func(ctx context.Context, _, _ string) error {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if n <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		atomic.AddInt32(&inFlight, -1)
		return nil
	}

	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", isolatedSpecFile(2, "a", "b"))

	h.reconcile() // one node wins the slot and enters materialization; the other cannot
	<-entered     // first materialization has begun

	// Drive several reconciles while the slot is held: the second node is
	// ready but slotless, so it must not begin materializing.
	for i := 0; i < 5; i++ {
		h.reconcile()
		runtime.Gosched()
	}
	if got := atomic.LoadInt32(&inFlight); got != 1 {
		t.Fatalf("in-flight materializations = %d while one slot is held, want 1", got)
	}
	select {
	case <-entered:
		t.Fatal("a second materialization began before the first freed its execution slot")
	default:
	}

	// Let the first finish; the freed slot lets the second materialize and
	// run. release() keeps reconciling until each check's own
	// EventCheckFinished, so it drives the tick that finally starts "b" once
	// "a" has actually released its slot (a plain awaitStarted would block
	// with nothing reconciling, since slot release is asynchronous to the
	// tick that observes "a" finished).
	firstRunID := h.currentRunID()
	close(release) // unblock every export (this and future)
	h.release(firstRunID, "a", core.CheckResult{Name: "a", Status: core.CheckPassed})
	h.release(firstRunID, "b", core.CheckResult{Name: "b", Status: core.CheckPassed})

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("peak concurrent materializations = %d, want 1 (the cap must bound archives, not just commands)", got)
	}
	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed after both checks pass", last.Outcome)
	}
}

// TestIsolated_CancelDuringMaterializeReleasesSlotAndCleans is acceptance
// criterion 3 (the cancellation half): a run cancelled while its node is
// still materializing must release the execution slot and remove the
// partial private workspace — no leaked slot (which would deadlock the
// daemon) and no orphaned directory.
func TestIsolated_CancelDuringMaterializeReleasesSlotAndCleans(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.WorkDir = t.TempDir()
	h.d.cfg.Slots = core.NewSlots(1)

	var mu sync.Mutex
	var wsDir string
	entered := make(chan struct{}, 1)
	// Block in the export until the run's context is cancelled, then fail —
	// exactly what a real git archive does when its CommandContext is killed.
	h.git.exportHook = func(ctx context.Context, _, dir string) error {
		mu.Lock()
		wsDir = dir
		mu.Unlock()
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}

	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", isolatedSpecFile(1, "a"))

	h.reconcile() // node wins the slot, enters materialization, blocks
	<-entered
	mu.Lock()
	dir := wsDir
	mu.Unlock()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workspace %q should exist while materializing: %v", dir, err)
	}

	// Operator-cancel the in-flight run; cancelRun cancels its context, which
	// unblocks the export with an error.
	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: ref})
	h.reconcile()

	// The goroutine cleans up asynchronously after the context cancel: spin
	// on the observed conditions (slot freed, dir gone), never a sleep.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, statErr := os.Stat(dir)
		gone := os.IsNotExist(statErr)
		freed := h.d.cfg.Slots.TryAcquire()
		if freed {
			h.d.cfg.Slots.Release()
		}
		if gone && freed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after cancel: dirGone=%v slotFreed=%v (want both) — leaked slot or orphaned workspace %q", gone, freed, dir)
		}
		runtime.Gosched()
	}

	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeRejected || last.Detail != cancelDetail {
		t.Fatalf("Outcome/Detail = %v/%q, want Rejected/%q", last.Outcome, last.Detail, cancelDetail)
	}
}

// TestIsolated_ForceDrainCleansInFlightWorkspace is acceptance criterion 4
// (the drain half): when a daemon is force-drained — cmd cancels the root
// context (the SIGTERM force path, drain.go) — an isolated run still in
// flight has its private workspace removed, not leaked. The run is started
// under a cancellable context so its rootCtx derives from it; cancelling
// that context returns the gated RunCheck (gate.go honors ctx.Done), and the
// check goroutine's deferred cleanup removes the workspace.
func TestIsolated_ForceDrainCleansInFlightWorkspace(t *testing.T) {
	rec := newRecordingGatedExecutor()
	h := newHarnessWithExecutor(t, rec, nil)
	h.d.cfg.WorkDir = t.TempDir()

	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", isolatedSpecFile(1, "a"))

	// Start the run under a cancellable context: rootCtx derives from it, so
	// cancel() is exactly cmd's force-drain of an in-flight run.
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.d.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	runID := h.currentRunID()
	h.awaitStarted(runID, "a") // materialized; RunCheck now blocked on the gate

	wsDir := rec.jobDirByName()["a"]
	if wsDir == "" {
		t.Fatal("no workspace dir recorded for the in-flight isolated check")
	}
	if _, err := os.Stat(wsDir); err != nil {
		t.Fatalf("workspace %q should exist mid-run: %v", wsDir, err)
	}

	h.d.Drain(time.Time{}) // enter draining
	cancel()               // force: cancel the root context

	// The check's goroutine returns on the cancel and removes its private
	// workspace; drive the finalize reconcile and spin on the observed
	// condition (dir gone), never a sleep.
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.reconcile()
		if _, err := os.Stat(wsDir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("force-drain leaked the in-flight isolated workspace %q", wsDir)
		}
		runtime.Gosched()
	}
}

// TestIsolated_BatchExportsTrialTreeNotCommit is acceptance criterion 5: the
// batch construction path (finishBatchStart, separate from serial/speculate's
// startRun) must materialize the final trial TREE OID, never the chain-tip
// merge COMMIT — the same exact-tree/export-subst invariant the serial
// export-subst regression proves end-to-end, asserted here at the wiring
// level for batch. A fake-git assertion suffices: the exported object must be
// the run's chainTree (a real tree object), not its chainTip (a commit).
func TestIsolated_BatchExportsTrialTreeNotCommit(t *testing.T) {
	h := newHarness(t, batchTarget(8))
	// Seed the spec on the base so the batch boundary doesn't see every
	// member "introduce" it (see TestBatchLand_OnePushNDeletes).
	h.git.seed("main", isolatedSpecFile(1, "test"))
	refA := candidateRef("main", "alice", "a")
	refB := candidateRef("main", "bob", "b")
	h.git.pushCandidate(refA, "", map[string]string{"a.txt": "a\n"})
	h.git.pushCandidate(refB, "", map[string]string{"b.txt": "b\n"})

	h.reconcile() // both chain into one batch run; "test" starts once
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 2 {
		t.Fatalf("headRun members = %+v, want a 2-member batch", r)
	}
	if !r.isolated {
		t.Fatal("batch run is not isolated despite workspace \"isolated\"")
	}
	runID := h.currentRunID()
	h.awaitStarted(runID, "test") // RunCheck runs only after materialization

	trees := append([]string(nil), h.git.exportTrees...)
	if len(trees) != 1 {
		t.Fatalf("exportTrees = %v, want exactly one isolated materialization for the single check node", trees)
	}
	got := trees[0]
	if got != r.chainTree {
		t.Fatalf("isolated batch exported %q, want the trial tree chainTree %q", got, r.chainTree)
	}
	if got == r.chainTip {
		t.Fatalf("isolated batch exported the chain-tip COMMIT %q — must export the tree, not the commit", got)
	}
	// chainTree is a real tree object; chainTip is a commit. Prove the
	// exported id is the former, categorically.
	h.git.mu.Lock()
	_, isTree := h.git.trees[got]
	_, isCommit := h.git.commits[got]
	h.git.mu.Unlock()
	if !isTree || isCommit {
		t.Fatalf("exported id %q: isTree=%v isCommit=%v, want a tree object", got, isTree, isCommit)
	}

	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	recs := h.ch.Records()
	if last := recs[len(recs)-1]; last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
}
