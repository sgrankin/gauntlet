// Executor-profile suite (issue #3): a repo spec may NAME a profile, the
// queue copies the name onto every CheckJob verbatim (routing itself lives
// in executor.Mux — Invariant 8, the core stays executor-agnostic), and an
// unknown name rejects the spec before any command starts.
package queue

import (
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// profileSpecFile renders a .gauntlet.kdl of "name[:profile]" checks.
func profileSpecFile(entries ...string) map[string]string {
	var b strings.Builder
	for _, e := range entries {
		name, prof, _ := strings.Cut(e, ":")
		b.WriteString("check \"" + name + "\" {\n    command \"true\"\n")
		if prof != "" {
			b.WriteString("    executor \"" + prof + "\"\n")
		}
		b.WriteString("}\n")
	}
	return map[string]string{testCheckSpecPath: b.String()}
}

func TestExecutorProfile_FlowsOntoCheckJob(t *testing.T) {
	rec := newRecordingGatedExecutor()
	h := newHarnessWithExecutor(t, rec, nil)
	h.d.cfg.KnownExecutorProfile = func(name string) bool { return name == "ci" }
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", profileSpecFile("test:ci", "receipt"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "test")
	if got := rec.lastJob().Executor; got != "ci" {
		t.Fatalf("job.Executor = %q, want the spec's profile selection", got)
	}
	h.release(runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	h.awaitStarted(runID, "receipt")
	if got := rec.lastJob().Executor; got != "" {
		t.Fatalf("job.Executor = %q, want \"\" for a check naming no profile", got)
	}
	h.release(runID, "receipt", core.CheckResult{Name: "receipt", Status: core.CheckPassed})

	recs := h.ch.Records()
	if recs[len(recs)-1].Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed (mixed default+profile checks in one candidate)", recs[len(recs)-1].Outcome)
	}
}

func TestExecutorProfile_UnknownRejectsBeforeAnyCommand(t *testing.T) {
	h := newHarness(t)
	// No KnownExecutorProfile at all: the daemon defines no profiles, so
	// ANY selection is unknown.
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	sha := h.git.pushCandidate(ref, "", profileSpecFile("build", "test:ghost"))

	h.reconcile()

	recs := h.ch.Records()
	if len(recs) == 0 {
		t.Fatal("expected a terminal record")
	}
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected (a configuration error, never a red verdict)", last.Outcome)
	}
	if !strings.Contains(last.Detail, `check "test" selects unknown executor profile "ghost"`) {
		t.Fatalf("Detail = %q, want the offending check and profile named", last.Detail)
	}
	// Rejected BEFORE any command started — even "build", which named no
	// profile at all.
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventCheckStarted {
			t.Fatalf("check %q started despite the spec being rejected", e.CheckName)
		}
	}
	// Parked at the pushed SHA, like any other spec validation error.
	if entry, ok := h.d.done["main"][ref]; !ok || entry.SHA != sha {
		t.Fatalf("park entry = %+v ok=%v, want parked at %s", entry, ok, sha)
	}
}
