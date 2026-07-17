// Candidate-built image suite (issue #2): image builds are synthetic
// "image:<name>" nodes in the run's ordinary dependency graph — built once
// per run, an implicit prerequisite of every consumer, releasing them only
// with a validated IMMUTABLE identity. Everything downstream (fail-fast,
// blocked rows, capacity, history) is the same machinery the parallel
// suite already proves; these tests pin the image-specific seams.
package queue

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

const localID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// imageSpecFile renders a .gauntlet.kdl with one image "go-ci" plus checks:
// "name" (plain) or "name*" (consumes go-ci).
func imageSpecFile(entries ...string) map[string]string {
	var b strings.Builder
	b.WriteString("max-parallel 4\nimage \"go-ci\" {\n    command \"./ci/build-image\"\n}\n")
	for _, e := range entries {
		name, consumes := strings.CutSuffix(e, "*")
		b.WriteString("check \"" + name + "\" {\n    command \"true\"\n")
		if consumes {
			b.WriteString("    image \"go-ci\"\n")
		}
		b.WriteString("}\n")
	}
	return map[string]string{testCheckSpecPath: b.String()}
}

// imageCapableHarness marks every profile (incl. the default) container-
// capable, since these tests' interest is the graph, not the gate.
func imageCapableHarness(t *testing.T) (*testHarness, *recordingGatedExecutor) {
	rec := newRecordingGatedExecutor()
	h := newHarnessWithExecutor(t, rec, nil)
	h.d.cfg.ImageCapableProfile = func(string) bool { return true }
	return h, rec
}

func TestImage_BuildsOnceAndReleasesBothConsumers(t *testing.T) {
	h, rec := imageCapableHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", imageSpecFile("unit*", "lint*"))

	h.reconcile()
	runID := h.currentRunID()
	// The build is the only ready node; both consumers wait on their
	// implicit edge.
	h.awaitStarted(runID, "image:go-ci")
	if h.started(runID, "unit") || h.started(runID, "lint") {
		t.Fatal("a consumer started before its image was built")
	}
	if !rec.lastJob().ImageBuild {
		t.Fatal("the build node's job is not marked ImageBuild")
	}

	h.release(runID, "image:go-ci", core.CheckResult{Name: "image:go-ci", Status: core.CheckPassed, Image: localID})
	h.awaitStarted(runID, "unit")
	h.awaitStarted(runID, "lint") // ONE build released BOTH consumers

	// Every consumer runs by the captured immutable identity.
	for _, j := range rec.jobs {
		if j.Name == "unit" || j.Name == "lint" {
			if j.Image != localID {
				t.Errorf("consumer %q job.Image = %q, want the captured ID", j.Name, j.Image)
			}
			if j.ImageBuild {
				t.Errorf("consumer %q wrongly marked ImageBuild", j.Name)
			}
		}
	}

	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, "lint", core.CheckResult{Name: "lint", Status: core.CheckPassed})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", last.Outcome)
	}
	// One row per node, spec order: the build first, its identity recorded;
	// consumers carry the identity they ran in (provenance).
	if len(last.Checks) != 3 || last.Checks[0].Name != "image:go-ci" {
		t.Fatalf("Checks = %+v, want [image:go-ci unit lint]", last.Checks)
	}
	if last.Checks[0].Image != localID {
		t.Errorf("build row Image = %q, want the captured ID", last.Checks[0].Image)
	}
	for _, c := range last.Checks[1:] {
		if c.Image != localID {
			t.Errorf("consumer %q row Image = %q, want the consumed ID", c.Name, c.Image)
		}
	}
}

func TestImage_InvalidResultIsTheBuildsFailureNotConsumers(t *testing.T) {
	cases := []struct {
		name, ref, wantDetail string
	}{
		{"mutable tag", "go-ci:latest", "not immutable"},
		{"empty result", "", "wrote no image reference"},
		{"multiple lines", localID + "\n" + localID, "exactly one reference"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := imageCapableHarness(t)
			h.git.seed("main", nil)
			ref := candidateRef("main", "alice", "w-"+tc.name)
			h.git.pushCandidate(ref, "", imageSpecFile("unit*"))

			h.reconcile()
			runID := h.currentRunID()
			// The executor exits 0 but the captured result is unusable: the
			// BUILD goes red (one root cause), the consumer blocks on it.
			h.release(runID, "image:go-ci", core.CheckResult{Name: "image:go-ci", Status: core.CheckPassed, Image: tc.ref})

			recs := h.ch.Records()
			last := recs[len(recs)-1]
			if last.Outcome != core.OutcomeRejected {
				t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
			}
			if !strings.Contains(last.Detail, `check "image:go-ci" failed`) {
				t.Fatalf("Detail = %q, want the build named as the one root cause", last.Detail)
			}
			build := last.Checks[0]
			if build.Status != core.CheckFailed || build.Image != "" || !strings.Contains(build.Output, tc.wantDetail) {
				t.Errorf("build row = %+v, want failed with %q in output and no image recorded", build, tc.wantDetail)
			}
			unit := last.Checks[1]
			if unit.Status != core.CheckBlocked || len(unit.BlockedBy) != 1 || unit.BlockedBy[0] != "image:go-ci" {
				t.Errorf("consumer row = %+v, want blocked by the build node", unit)
			}
		})
	}
}

func TestImage_DigestPinnedRegistryRefAccepted(t *testing.T) {
	h, rec := imageCapableHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", imageSpecFile("unit*"))

	h.reconcile()
	runID := h.currentRunID()
	digestRef := "registry.example:5000/acme/go-ci@" + localID
	h.release(runID, "image:go-ci", core.CheckResult{Name: "image:go-ci", Status: core.CheckPassed, Image: digestRef})
	h.awaitStarted(runID, "unit")
	if got := rec.lastJob().Image; got != digestRef {
		t.Fatalf("consumer job.Image = %q, want the digest-pinned ref", got)
	}
	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
}

func TestImage_BuildFailureBlocksConsumersWithOneRootCause(t *testing.T) {
	h, _ := imageCapableHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", imageSpecFile("unit*", "receipt"))

	h.reconcile()
	runID := h.currentRunID()
	// "receipt" is independent of the image and may run concurrently.
	h.awaitStarted(runID, "receipt")
	h.release(runID, "image:go-ci", core.CheckResult{Name: "image:go-ci", Status: core.CheckFailed, Output: "step 3/9 failed"})

	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected || !strings.Contains(last.Detail, `check "image:go-ci" failed`) {
		t.Fatalf("terminal = %v %q, want rejection rooted at the build", last.Outcome, last.Detail)
	}
	unit := last.Checks[1]
	if unit.Name != "unit" || unit.Status != core.CheckBlocked {
		t.Errorf("unit = %+v, want blocked", unit)
	}
}

func TestImage_UnconsumedImageStillBuildsAndGates(t *testing.T) {
	h, _ := imageCapableHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	// A candidate changing only its Dockerfile: the build alone gates the
	// merge even with no consumer.
	h.git.pushCandidate(ref, "", imageSpecFile("unit"))

	h.reconcile()
	runID := h.currentRunID()
	h.awaitStarted(runID, "image:go-ci")
	h.release(runID, "unit", core.CheckResult{Name: "unit", Status: core.CheckPassed})
	h.release(runID, "image:go-ci", core.CheckResult{Name: "image:go-ci", Status: core.CheckPassed, Image: localID})

	recs := h.ch.Records()
	if recs[len(recs)-1].Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed", recs[len(recs)-1].Outcome)
	}
}

// TestIntegration_ImageBuildEndToEnd exercises the whole image seam with a
// REAL LocalExecutor subprocess: spec parse -> image node -> the executor
// exporting GAUNTLET_IMAGE_RESULT_FILE (and not the check result file) ->
// the build script writing an ID -> queue validation -> the consumer job
// stamped with the captured identity -> both history rows carrying it.
// (The txtar scenario harnesses run the gated executor, which never execs
// a command, so this executor seam lives here in the integration tier —
// same placement as the result-file and env-contract rows.)
func TestIntegration_ImageBuildEndToEnd(t *testing.T) {
	h := newIntegrationHarness(t, nil, executor.LocalExecutor{})
	h.d.cfg.ImageCapableProfile = func(string) bool { return true }
	remote := h.remote
	remote.Seed("main", map[string]string{"README.md": "seed\n"})

	files := map[string]string{
		testCheckSpecPath: "image \"go-ci\" {\n    command \"/bin/sh\" \"build.sh\"\n}\n" +
			"check \"unit\" {\n    command \"/bin/sh\" \"unit.sh\"\n    image \"go-ci\"\n}\n",
		"build.sh": fmt.Sprintf(`#!/bin/sh
set -eu
[ -n "$%s" ] || exit 1
[ -z "${%s+x}" ] || { echo "check result file leaked into a build"; exit 1; }
printf '%s' > "$%s"
`, core.EnvImageResultFile, core.EnvResultFile, localID, core.EnvImageResultFile),
		"unit.sh": "#!/bin/sh\nexit 0\n",
	}
	remote.PushCandidate("main", "alice", "widget", files)

	before := len(h.ch.Records())
	h.reconcile()
	rec := h.pumpUntilRecord(before)

	if rec.Outcome != core.OutcomeLanded {
		t.Fatalf("Outcome = %v, want Landed; Detail=%q Checks=%+v", rec.Outcome, rec.Detail, rec.Checks)
	}
	if len(rec.Checks) != 2 || rec.Checks[0].Name != "image:go-ci" || rec.Checks[1].Name != "unit" {
		t.Fatalf("Checks = %+v, want [image:go-ci unit]", rec.Checks)
	}
	if rec.Checks[0].Image != localID {
		t.Errorf("build row Image = %q, want the ID the script wrote", rec.Checks[0].Image)
	}
	if rec.Checks[1].Image != localID {
		t.Errorf("consumer row Image = %q, want the consumed identity (provenance)", rec.Checks[1].Image)
	}
}

func TestImage_ConsumerOnNonContainerProfileRejects(t *testing.T) {
	h := newHarness(t) // ImageCapableProfile nil: nothing is capable
	h.git.seed("main", nil)
	ref := candidateRef("main", "alice", "widget")
	h.git.pushCandidate(ref, "", imageSpecFile("unit*"))

	h.reconcile()
	recs := h.ch.Records()
	last := recs[len(recs)-1]
	if last.Outcome != core.OutcomeRejected {
		t.Fatalf("Outcome = %v, want Rejected", last.Outcome)
	}
	if !strings.Contains(last.Detail, "not a container profile") {
		t.Fatalf("Detail = %q, want the capability gate named", last.Detail)
	}
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventCheckStarted {
			t.Fatalf("%q started despite the gate", e.CheckName)
		}
	}
}
