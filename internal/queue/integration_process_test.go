package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// TestIntegration_CancelledCheckKillsProcessGroup is §5's "Cancelled check
// kills process group" row (§9.5): a check that spawns a background child
// must have that child killed too when the run is cancelled (Invariant 5),
// not just the direct child LocalExecutor started. Exercised against a real
// LocalExecutor subprocess — the executor contract end-to-end, mirroring
// executor/local_test.go's own TestLocalExecutor_ProcessGroupKill but
// driven through the full queue state machine and a real git remote.
func TestIntegration_CancelledCheckKillsProcessGroup(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	const pidFileName = "child.pid"
	script := fmt.Sprintf("#!/bin/sh\nsleep 300 &\necho $! > %s\nsleep 300\n", pidFileName)
	files := shellCheckSpec("spawn", script)
	ref := remote.PushCandidate("main", "alice", "widget", files)

	h.reconcile() // trial starts; LocalExecutor runs the check in the background
	r := h.d.runs["main"]
	if r == nil {
		t.Fatal("no in-flight run after trial start")
	}
	dir := r.dir
	pidPath := filepath.Join(dir, pidFileName)

	// Wait for the background child's pid to be written. This polls real
	// OS/subprocess state that nothing in the queue's own event/record
	// machinery observes, mirroring executor/local_test.go's identical
	// wait — not a substitute for the queue-level determinism the rest of
	// this suite uses (ReconcileOnce, releaseGated, RecordingChannel).
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(pidPath); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				fmt.Sscanf(s, "%d", &childPID)
				if childPID > 0 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("background child never wrote its pid")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Trigger Invariant 5's cancellation: a direct push moves the target
	// out from under the in-flight run mid-check.
	remote.DirectPush("main", map[string]string{"human.txt": "a direct human push"})
	h.reconcile() // detects the move, cancels + Skips

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeSkipped {
		t.Fatalf("Outcome = %v, want Skipped", last.Outcome)
	}
	if remote.Ref(ref) == "" {
		t.Fatal("candidate slot removed on a target-moved Skip")
	}

	// The background child (grandchild of the process-group leader) must
	// be dead too, not just the direct child (§9.5).
	deadline = time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if err == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child pid %d still alive after group kill", childPID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("export dir %s still exists after cancellation; should have been removed", dir)
	}
}
