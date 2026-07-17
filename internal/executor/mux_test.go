package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// stampExecutor is a trivial core.Executor double that stamps its identity
// onto the result, so Mux routing is directly observable.
type stampExecutor struct{ id string }

func (s stampExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	return core.CheckResult{Name: job.Name, Status: core.CheckPassed, Output: s.id}
}

func TestMux_RoutesByProfileName(t *testing.T) {
	m := Mux{
		Default: stampExecutor{id: "default"},
		Named: map[string]core.Executor{
			"ci":   stampExecutor{id: "ci"},
			"host": stampExecutor{id: "host"},
		},
	}

	for _, tc := range []struct{ profile, want string }{
		{"", "default"},
		{"ci", "ci"},
		{"host", "host"},
	} {
		res := m.RunCheck(context.Background(), core.CheckJob{Name: "t", Executor: tc.profile})
		if res.Err != nil || res.Output != tc.want {
			t.Errorf("RunCheck(executor=%q) = %+v, want routed to %q", tc.profile, res, tc.want)
		}
	}
}

func TestMux_UnknownProfileIsErrNotVerdict(t *testing.T) {
	m := Mux{Default: stampExecutor{id: "default"}}
	res := m.RunCheck(context.Background(), core.CheckJob{Name: "t", Executor: "ghost"})
	if res.Err == nil {
		t.Fatalf("RunCheck(unknown profile) = %+v, want Err (park-as-error, not a verdict)", res)
	}
	if !strings.Contains(res.Err.Error(), `"ghost"`) {
		t.Errorf("Err = %v, want it to name the profile", res.Err)
	}
}

func TestParams_RunArgs_ProfileOptions(t *testing.T) {
	p, err := New(Params{
		Runtime:  "docker",
		Image:    "go-ci:latest",
		Workdir:  "/workspace",
		Env:      []string{"TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal"},
		AddHosts: []string{"host.docker.internal:host-gateway"},
		Memory:   "8g",
		CPUs:     "4",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()
	args := p.params.runArgs(job, "name", t.TempDir())

	if !containsPair(args, "--add-host", "host.docker.internal:host-gateway") {
		t.Errorf("missing --add-host pair in %v", args)
	}
	if !containsPair(args, "--memory", "8g") || !containsPair(args, "--cpus", "4") {
		t.Errorf("missing resource flags in %v", args)
	}
	fixed := indexOfPair(args, "-e", "TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal")
	if fixed == -1 {
		t.Fatalf("missing fixed profile env in %v", args)
	}
	// Precedence: the profile's fixed env must precede every gauntlet-
	// provided -e (last -e wins to the runtime, so gauntlet's values win a
	// collision).
	firstContract := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" && strings.HasPrefix(args[i+1], "GAUNTLET_") {
			firstContract = i
			break
		}
	}
	if firstContract == -1 || fixed > firstContract {
		t.Errorf("fixed env at %d, first GAUNTLET_* -e at %d; profile env must come first", fixed, firstContract)
	}
	// All before the image.
	img := indexOf(args, "go-ci:latest")
	for _, i := range []int{fixed, firstContract} {
		if i > img {
			t.Errorf("flag at %d after image at %d", i, img)
		}
	}
}

func TestLocalExecutor_ImageBuildResultProtocol(t *testing.T) {
	const id = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	cmd := script(t, dir, "build.sh", `#!/bin/sh
set -eu
[ -n "$GAUNTLET_IMAGE_RESULT_FILE" ] || { echo "no image result file"; exit 1; }
[ -z "${GAUNTLET_RESULT_FILE+x}" ] || { echo "check result file leaked into a build"; exit 1; }
printf '%s\n' "`+id+`" > "$GAUNTLET_IMAGE_RESULT_FILE"
`)
	job := baseJob(t, cmd)
	job.ImageBuild = true

	res := LocalExecutor{}.RunCheck(context.Background(), job)
	if res.Err != nil || res.Status != core.CheckPassed {
		t.Fatalf("res = %+v (output %q)", res, res.Output)
	}
	if res.Image != id {
		t.Fatalf("res.Image = %q, want the file content read back verbatim", res.Image)
	}
}

func TestLocalExecutor_ImageBuildNonZeroExitIgnoresFile(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "build.sh", `#!/bin/sh
printf 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' > "$GAUNTLET_IMAGE_RESULT_FILE"
exit 1
`)
	job := baseJob(t, cmd)
	job.ImageBuild = true

	res := LocalExecutor{}.RunCheck(context.Background(), job)
	if res.Status != core.CheckFailed || res.Image != "" {
		t.Fatalf("res = %+v, want a plain failure with no image captured (non-zero exit is a build failure regardless of the file)", res)
	}
}

func TestParams_RunArgs_ImageOverride(t *testing.T) {
	p, err := New(Params{Runtime: "docker", Image: "static:latest", Workdir: "/workspace"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const id = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()
	job.Image = id

	args := p.params.runArgs(job, "name", t.TempDir())
	if !contains(args, id) {
		t.Fatalf("argv %v missing the captured image ID", args)
	}
	if contains(args, "static:latest") {
		t.Fatalf("argv %v still names the profile's static image alongside the override", args)
	}
	// And an image-build job swaps the result-file variable.
	buildJob := containerJob([]string{"true"})
	buildJob.Dir = t.TempDir()
	buildJob.ImageBuild = true
	buildArgs := p.params.runArgs(buildJob, "name", t.TempDir())
	if !containsPair(buildArgs, "-e", core.EnvImageResultFile+"="+containerResultFile) {
		t.Fatalf("build argv %v missing %s", buildArgs, core.EnvImageResultFile)
	}
	for i := 0; i+1 < len(buildArgs); i++ {
		if buildArgs[i] == "-e" && strings.HasPrefix(buildArgs[i+1], core.EnvResultFile+"=") {
			t.Fatalf("build argv exports the CHECK result file too: %v", buildArgs)
		}
	}
}

func TestLocalExecutor_ProfileEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	cmd := script(t, dir, "check.sh", `#!/bin/sh
[ "$DEPLOY_TARGET" = "staging" ] || { echo "DEPLOY_TARGET=$DEPLOY_TARGET"; exit 1; }
[ "$GAUNTLET_REF" = "refs/heads/for/main/alice/topic" ] || { echo "contract lost: $GAUNTLET_REF"; exit 1; }
`)
	job := baseJob(t, cmd)

	res := LocalExecutor{Env: []string{"DEPLOY_TARGET=staging", "GAUNTLET_REF=squatted"}}.RunCheck(context.Background(), job)
	if res.Err != nil || res.Status != core.CheckPassed {
		t.Fatalf("res = %+v (output %q): fixed env must be visible and the GAUNTLET_* contract must win a collision", res, res.Output)
	}
}
