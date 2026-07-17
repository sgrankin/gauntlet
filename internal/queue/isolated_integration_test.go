package queue

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// twoCheckSpec builds a .gauntlet.kdl (files map for PushCandidate) with a
// workspace policy, max-parallel, and two /bin/sh checks where `consumer`
// runs after `producer`.
func twoCheckSpec(workspace, producerSh, consumerSh string) map[string]string {
	var b strings.Builder
	if workspace != "" {
		b.WriteString("workspace \"" + workspace + "\"\n")
	}
	b.WriteString("max-parallel 2\n")
	b.WriteString("check \"producer\" {\n    command \"/bin/sh\" \"producer.sh\"\n}\n")
	b.WriteString("check \"consumer\" {\n    command \"/bin/sh\" \"consumer.sh\"\n    after \"producer\"\n}\n")
	return map[string]string{
		testCheckSpecPath: b.String(),
		"producer.sh":     producerSh,
		"consumer.sh":     consumerSh,
	}
}

// TestIntegration_IsolatedAfterDependentCannotSeePrerequisiteFile is the
// core isolation guarantee (issue #9, criterion 6): in isolated mode an
// `after` dependent starts from the pristine merge tree and CANNOT see a
// file its prerequisite wrote — `after` is verdict ordering, not shared
// dataflow. producer writes artifact.txt; consumer passes iff it is
// absent, so the whole run lands green only under real isolation.
func TestIntegration_IsolatedAfterDependentCannotSeePrerequisiteFile(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	h.remote.Seed("main", map[string]string{"README.md": "seed\n"})
	h.remote.PushCandidate("main", "alice", "widget", twoCheckSpec(
		"isolated",
		"echo x > artifact.txt\n",   // producer pollutes its own private cwd
		"test ! -e artifact.txt\n")) // consumer must NOT see it

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)
	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v (detail %q), want Landed — the isolated consumer must not see producer's file", rec.Outcome, rec.Detail)
	}
}

// TestIntegration_SharedAfterDependentSeesPrerequisiteFile is the
// contrast: shared mode (the default) retains today's filesystem
// visibility, so the same consumer DOES see producer's file and the run is
// rejected. This proves the two modes genuinely differ.
func TestIntegration_SharedAfterDependentSeesPrerequisiteFile(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	h.remote.Seed("main", map[string]string{"README.md": "seed\n"})
	h.remote.PushCandidate("main", "alice", "widget", twoCheckSpec(
		"", // shared (absent policy = today's default)
		"echo x > artifact.txt\n",
		"test ! -e artifact.txt\n"))

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)
	if rec.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected — shared mode's consumer sees producer's file and fails", rec.Outcome)
	}
}

// TestIntegration_IsolatedHistoryMtimesDeterministic: with history mtimes
// enabled, two isolated node workspaces for the same merge report the
// IDENTICAL history-derived mtime for a file last changed before the
// candidate — isolation preserves deterministic metadata (criterion 4).
func TestIntegration_IsolatedHistoryMtimesDeterministic(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	h.d.cfg.HistoryMtimes = true
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	// seed.txt's one and only touching commit is pinned far from now, so
	// only the history walk can produce this mtime — and it must match
	// across both isolated node workspaces.
	seedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pastDatedDirectPush(t, remote, "main", seedTime, map[string]string{"seed.txt": "s\n"})

	outA := filepath.Join(t.TempDir(), "a")
	outB := filepath.Join(t.TempDir(), "b")
	// Two independent checks (no after edge → may run in parallel), each
	// records seed.txt's mtime from its own private workspace.
	spec := map[string]string{
		testCheckSpecPath: "workspace \"isolated\"\nmax-parallel 2\n" +
			"check \"a\" {\n    command \"/bin/sh\" \"a.sh\"\n}\n" +
			"check \"b\" {\n    command \"/bin/sh\" \"b.sh\"\n}\n",
		"a.sh": "stat -c %Y seed.txt > " + outA + "\n",
		"b.sh": "stat -c %Y seed.txt > " + outB + "\n",
	}
	remote.PushCandidate("main", "alice", "mtimes", spec)

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)
	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v (detail %q), want Landed", rec.Outcome, rec.Detail)
	}

	mtA := readMtime(t, outA)
	mtB := readMtime(t, outB)
	if mtA != seedTime.Unix() || mtB != seedTime.Unix() {
		t.Fatalf("node mtimes = %d / %d, want both %d (history-derived, identical across isolated workspaces)", mtA, mtB, seedTime.Unix())
	}
}

func readMtime(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("check never wrote %s: %v", path, err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		t.Fatalf("parse mtime %q: %v", raw, err)
	}
	return v
}
