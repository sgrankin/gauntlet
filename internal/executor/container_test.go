package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// --- pure argv-builder tests: no runtime required, run everywhere. ---

func containerJob(command []string) core.CheckJob {
	return core.CheckJob{
		RunID:    "20260704T120000Z-1-abc123def456",
		Target:   "main",
		Name:     "unit test",
		Command:  command,
		Dir:      "/host/trial-tree",
		BaseSHA:  "base-sha",
		MergeSHA: "merge-sha",
		Candidate: core.Candidate{
			Ref:    "refs/heads/for/main/alice/topic",
			Target: "main",
			User:   "alice",
			Topic:  "topic",
			SHA:    "cand-sha",
		},
	}
}

func TestParams_RunArgs_Shape(t *testing.T) {
	p := Params{
		Workdir: "/workspace",
		Image:   "ghcr.io/acme/ci:latest",
		Caches: []Cache{
			{Name: "gocache", Path: "/root/.cache/go-build"},
			{Name: "gomodcache", Path: "/go/pkg/mod"},
		},
	}
	job := containerJob([]string{"go", "test", "./..."})
	name := "gauntlet-run1-check1"

	got := p.runArgs(job, name, "/host/result-dir")

	want := []string{
		"run", "--rm", "--name", "gauntlet-run1-check1",
		"-w", "/workspace",
		"-v", "/host/trial-tree:/workspace",
		"-v", "/host/result-dir:/gauntlet",
		"-e", "GAUNTLET_BASE_SHA=base-sha",
		"-e", "GAUNTLET_MERGE_SHA=merge-sha",
		"-e", "GAUNTLET_CANDIDATE_SHA=cand-sha",
		"-e", "GAUNTLET_REF=refs/heads/for/main/alice/topic",
		"-e", "GAUNTLET_RESULT_FILE=/gauntlet/result",
		"-v", "gocache:/root/.cache/go-build",
		"-v", "gomodcache:/go/pkg/mod",
		"ghcr.io/acme/ci:latest",
		"go", "test", "./...",
	}
	if len(got) != len(want) {
		t.Fatalf("runArgs length = %d, want %d\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runArgs[%d] = %q, want %q\n got=%v\nwant=%v", i, got[i], want[i], got, want)
		}
	}
}

func TestParams_RunArgs_NoCaches(t *testing.T) {
	p := Params{Workdir: "/workspace", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "gauntlet-run1-check1", "/rd")

	// No cache volumes ⇒ no extra -v pairs beyond the two fixed mounts;
	// image immediately follows the GAUNTLET_RESULT_FILE env pair.
	idx := indexOf(got, "img")
	if idx == -1 {
		t.Fatalf("image not found in argv: %v", got)
	}
	if got[idx-1] != "GAUNTLET_RESULT_FILE=/gauntlet/result" {
		t.Fatalf("expected image immediately after result-file env when no caches, got argv: %v", got)
	}
	if got[idx+1] != "true" {
		t.Fatalf("command must follow image: %v", got)
	}
}

func TestParams_RunArgs_EnvVarsAllFive(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	wantEnv := []string{
		core.EnvBaseSHA + "=" + job.BaseSHA,
		core.EnvMergeSHA + "=" + job.MergeSHA,
		core.EnvCandidateSHA + "=" + job.Candidate.SHA,
		core.EnvRef + "=" + job.Candidate.Ref,
		core.EnvResultFile + "=/gauntlet/result",
	}
	for _, e := range wantEnv {
		if !containsPair(got, "-e", e) {
			t.Errorf("argv missing -e %q; argv=%v", e, got)
		}
	}
}

func TestParams_RunArgs_TrialTreeMountReadWrite(t *testing.T) {
	// §4.5: the trial tree is bind-mounted RW at workdir (matches
	// LocalExecutor — no :ro suffix, since export is ephemeral).
	p := Params{Workdir: "/workspace", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	if !containsPair(got, "-v", "/host/trial-tree:/workspace") {
		t.Fatalf("expected RW bind mount job.Dir:workdir with no :ro suffix; argv=%v", got)
	}
}

func TestParams_RunArgs_ResultDirMount(t *testing.T) {
	p := Params{Workdir: "/workspace", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/host/result-dir")

	if !containsPair(got, "-v", "/host/result-dir:/gauntlet") {
		t.Fatalf("expected result dir mounted at /gauntlet; argv=%v", got)
	}
}

func TestParams_RunArgs_MultipleCachesPreserveOrder(t *testing.T) {
	p := Params{
		Workdir: "/w",
		Image:   "img",
		Caches: []Cache{
			{Name: "c1", Path: "/p1"},
			{Name: "c2", Path: "/p2"},
			{Name: "c3", Path: "/p3"},
		},
	}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	prevIdx := -1
	for _, c := range p.Caches {
		idx := indexOfPair(got, "-v", c.Name+":"+c.Path)
		if idx == -1 {
			t.Fatalf("missing cache mount %s:%s; argv=%v", c.Name, c.Path, got)
		}
		if idx < prevIdx {
			t.Fatalf("cache mounts out of order; argv=%v", got)
		}
		prevIdx = idx
	}
}

func TestParams_RunArgs_CommandArgvPassedThrough(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"go", "vet", "./..."})

	got := p.runArgs(job, "n", "/rd")

	tail := got[len(got)-3:]
	want := []string{"go", "vet", "./..."}
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("trailing command argv = %v, want %v", tail, want)
		}
	}
}

func TestParams_RunArgs_NameAndRmRW(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "gauntlet-myname", "/rd")

	if got[0] != "run" {
		t.Fatalf("argv[0] = %q, want %q", got[0], "run")
	}
	if !contains(got, "--rm") {
		t.Fatalf("expected --rm; argv=%v", got)
	}
	if !containsPair(got, "--name", "gauntlet-myname") {
		t.Fatalf("expected --name gauntlet-myname; argv=%v", got)
	}
	if !containsPair(got, "-w", "/w") {
		t.Fatalf("expected -w /w; argv=%v", got)
	}
}

// --- containerName / sanitizeName ---

func TestContainerName(t *testing.T) {
	cases := []struct {
		runID, check, want string
	}{
		{"run1", "build", "gauntlet-run1-build"},
		{"20260704T120000Z-1-abc123def456", "unit test", "gauntlet-20260704T120000Z-1-abc123def456-unit-test"},
		{"weird/run:id", "check/name", "gauntlet-weird-run-id-check-name"},
		{"", "", "gauntlet--"},
	}
	for _, c := range cases {
		got := containerName(c.runID, c.check)
		if got != c.want {
			t.Errorf("containerName(%q, %q) = %q, want %q", c.runID, c.check, got, c.want)
		}
	}
}

func TestSanitizeName_OnlyAllowedCharsSurviveUnescaped(t *testing.T) {
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-"
	in := allowed + " /:@!#$%^&*()+={}[]|\\;\"'<>,?~`"
	got := sanitizeName(in)
	if !strings.HasPrefix(got, allowed) {
		t.Fatalf("sanitizeName must preserve the allowed prefix verbatim, got %q", got)
	}
	for _, r := range got {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			t.Fatalf("sanitizeName produced disallowed rune %q in %q", r, got)
		}
	}
}

// --- New() validation ---

func TestNew_RequiresImage(t *testing.T) {
	_, err := New(Params{Runtime: "docker"})
	if err == nil {
		t.Fatal("expected error when Image is empty")
	}
}

func TestNew_RejectsUnknownRuntime(t *testing.T) {
	_, err := New(Params{Runtime: "bogus", Image: "img"})
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
}

func TestNew_DefaultsRuntimeAndWorkdir(t *testing.T) {
	c, err := New(Params{Image: "img"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.spec.Bin != "container" {
		t.Errorf("default runtime bin = %q, want %q", c.spec.Bin, "container")
	}
	if c.params.Workdir != "/workspace" {
		t.Errorf("default workdir = %q, want %q", c.params.Workdir, "/workspace")
	}
}

func TestNew_AcceptsDockerAndPodman(t *testing.T) {
	for _, rt := range []string{"docker", "podman", "container"} {
		if _, err := New(Params{Runtime: rt, Image: "img"}); err != nil {
			t.Errorf("New(Runtime:%q) unexpected error: %v", rt, err)
		}
	}
}

// --- RunCheck: hermetic missing-binary path (Err, not a verdict). ---
// Portable regardless of what's actually installed on the host: PATH is
// pointed at an empty directory so exec.LookPath fails deterministically.

func TestContainerExecutor_MissingRuntimeBinary_Err(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	c, err := New(Params{Runtime: "container", Image: "img"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()

	res := c.RunCheck(context.Background(), job)

	if res.Err == nil {
		t.Fatalf("expected Err when the runtime binary is missing, got Status=%v", res.Status)
	}
	if res.Status != core.CheckFailed {
		// Status is meaningful only when Err == nil (core.CheckResult
		// contract); a missing binary must not also masquerade as a real
		// verdict via a non-zero Status. Failed is the zero value, so this
		// just confirms nothing else was set.
		t.Errorf("Status = %v, want the zero value (unused since Err is set)", res.Status)
	}
}

// --- RunCheck: ctx already cancelled ⇒ Err, no probe/run attempted. ---

func TestContainerExecutor_CtxAlreadyCancelled_Err(t *testing.T) {
	c, err := New(Params{Runtime: "container", Image: "img"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	job := containerJob([]string{"true"})
	res := c.RunCheck(ctx, job)

	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", res.Err)
	}
}

// --- integration tests against the real runtime, if usable. ---
//
// Per docs/plans/phase23.md §1 Spike C, this machine has only Apple
// `container` installed, and its backing service may be down
// ("apiserver is not running and not registered with launchd"). These tests
// probe first and skip cleanly rather than failing when that's the case.

const testRuntime = "container"

func probeTestRuntime(t *testing.T) error {
	t.Helper()
	spec, ok := runtimeSpecs[testRuntime]
	if !ok {
		t.Fatalf("no runtimeSpec for %q", testRuntime)
	}
	return probeRuntime(context.Background(), spec)
}

// TestContainerExecutor_UnreachableService_Err exercises the real
// environment condition Spike C documented: the container binary is
// present but its service is down. It only runs when that's actually true
// here — if the binary is entirely absent (nothing to demonstrate beyond
// the hermetic PATH test above) or the service is actually reachable (the
// full functional suite below covers that case), it skips.
func TestContainerExecutor_UnreachableService_Err(t *testing.T) {
	if _, err := exec.LookPath(testRuntime); err != nil {
		t.Skipf("container binary not present: %v", err)
	}
	if err := probeTestRuntime(t); err == nil {
		t.Skip("container runtime is usable here; nothing to demonstrate (see the functional suite)")
	}

	c, err := New(Params{Runtime: testRuntime, Image: "does-not-matter"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()

	res := c.RunCheck(context.Background(), job)

	if res.Err == nil {
		t.Fatalf("expected Err when the runtime service is unreachable, got Status=%v Output=%q", res.Status, res.Output)
	}
	t.Logf("confirmed unreachable-service ⇒ Err: %v", res.Err)
}

// localImage returns the first of a few well-known tiny image tags that
// `bin` already has cached locally, or "" if none are present. Never pulls.
func localImage(bin string) string {
	for _, img := range []string{"busybox:latest", "busybox", "alpine:latest", "alpine", "hello-world:latest"} {
		if err := exec.Command(bin, "image", "inspect", img).Run(); err == nil {
			return img
		}
	}
	return ""
}

// containerScript writes a shell script into dir (which the caller must
// bind-mount at workdir) and returns the in-container argv to run it.
func containerScript(t *testing.T, hostDir, workdir, name, body string) []string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(hostDir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return []string{"/bin/sh", filepath.Join(workdir, name)}
}

func TestContainerExecutor_Functional(t *testing.T) {
	if err := probeTestRuntime(t); err != nil {
		t.Skipf("container runtime not usable: %v", err)
	}
	image := localImage(testRuntime)
	if image == "" {
		t.Skip("no locally-cached tiny image available; refusing to pull in tests")
	}

	newExec := func(t *testing.T) *ContainerExecutor {
		c, err := New(Params{Runtime: testRuntime, Image: image, Workdir: "/workspace"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return c
	}
	newJob := func(t *testing.T, command []string) core.CheckJob {
		job := containerJob(command)
		job.Dir = t.TempDir()
		job.RunID = "it-" + t.Name()
		return job
	}

	t.Run("Passed", func(t *testing.T) {
		dir := t.TempDir()
		cmd := containerScript(t, dir, "/workspace", "check.sh", "#!/bin/sh\nexit 0\n")
		job := newJob(t, cmd)
		job.Dir = dir

		res := newExec(t).RunCheck(context.Background(), job)

		if res.Err != nil {
			t.Fatalf("unexpected Err: %v (output=%q)", res.Err, res.Output)
		}
		if res.Status != core.CheckPassed {
			t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
		}
	})

	t.Run("Failed", func(t *testing.T) {
		dir := t.TempDir()
		cmd := containerScript(t, dir, "/workspace", "check.sh", "#!/bin/sh\necho boom\nexit 1\n")
		job := newJob(t, cmd)
		job.Dir = dir

		res := newExec(t).RunCheck(context.Background(), job)

		if res.Err != nil {
			t.Fatalf("unexpected Err: %v", res.Err)
		}
		if res.Status != core.CheckFailed {
			t.Fatalf("Status = %v, want CheckFailed", res.Status)
		}
		if !strings.Contains(res.Output, "boom") {
			t.Errorf("Output = %q, want to contain 'boom'", res.Output)
		}
	})

	t.Run("Skipped", func(t *testing.T) {
		dir := t.TempDir()
		cmd := containerScript(t, dir, "/workspace", "check.sh",
			"#!/bin/sh\nprintf 'skipped' > \"$GAUNTLET_RESULT_FILE\"\nexit 0\n")
		job := newJob(t, cmd)
		job.Dir = dir

		res := newExec(t).RunCheck(context.Background(), job)

		if res.Err != nil {
			t.Fatalf("unexpected Err: %v", res.Err)
		}
		if res.Status != core.CheckSkipped {
			t.Fatalf("Status = %v, want CheckSkipped", res.Status)
		}
	})

	t.Run("Cancel", func(t *testing.T) {
		dir := t.TempDir()
		cmd := containerScript(t, dir, "/workspace", "check.sh", "#!/bin/sh\nsleep 300\n")
		job := newJob(t, cmd)
		job.Dir = dir

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan core.CheckResult, 1)
		go func() {
			done <- newExec(t).RunCheck(ctx, job)
		}()

		time.Sleep(1 * time.Second)
		cancel()

		select {
		case res := <-done:
			if res.Err == nil {
				t.Fatalf("Err = nil, want ctx cancellation error; status=%v", res.Status)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("RunCheck did not return after ctx cancel")
		}
	})
}

// --- small local slice helpers for the argv assertions above. ---

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func indexOfPair(s []string, flag, value string) int {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == flag && s[i+1] == value {
			return i
		}
	}
	return -1
}

func contains(s []string, v string) bool { return indexOf(s, v) != -1 }

func containsPair(s []string, flag, value string) bool { return indexOfPair(s, flag, value) != -1 }
