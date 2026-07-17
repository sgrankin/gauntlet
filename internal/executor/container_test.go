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
		"run", "--progress", "none", "--rm", "--name", "gauntlet-run1-check1",
		"-w", "/workspace",
		"-v", "/host/trial-tree:/workspace",
		"-v", "/host/result-dir:/gauntlet",
		"-e", "GAUNTLET_BASE_SHA=base-sha",
		"-e", "GAUNTLET_MERGE_SHA=merge-sha",
		"-e", "GAUNTLET_CANDIDATE_SHA=cand-sha",
		"-e", "GAUNTLET_REF=refs/heads/for/main/alice/topic",
		"-e", "GAUNTLET_RESULT_FILE=/gauntlet/result",
		"-e", "GAUNTLET_RUN_ID=20260704T120000Z-1-abc123def456",
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

// TestParams_RunArgs_DistinctDirsSameWorkdir is the executor half of issue
// #9's isolation guarantee: two graph nodes materialized into DISTINCT host
// directories are each bind-mounted at the SAME fixed container workdir. The
// queue hands each isolated node its own private job.Dir (proven in
// queue/isolated_acceptance_test.go, TestIsolated_ConcurrentNodesGetDistinctDirs);
// here we prove the container executor maps those distinct host paths onto
// one in-container workdir, so conflicting mutations cannot cross even though
// both checks see the same /workspace path.
func TestParams_RunArgs_DistinctDirsSameWorkdir(t *testing.T) {
	p := Params{Workdir: "/workspace", Image: "img"}

	jobA := containerJob([]string{"true"})
	jobA.Dir = "/host/node-a"
	jobB := containerJob([]string{"true"})
	jobB.Dir = "/host/node-b"

	gotA := p.runArgs(jobA, "gauntlet-run1-a", "/rd")
	gotB := p.runArgs(jobB, "gauntlet-run1-b", "/rd")

	if !containsPair(gotA, "-v", "/host/node-a:/workspace") {
		t.Fatalf("node A: want distinct host dir at the fixed workdir; argv=%v", gotA)
	}
	if !containsPair(gotB, "-v", "/host/node-b:/workspace") {
		t.Fatalf("node B: want distinct host dir at the fixed workdir; argv=%v", gotB)
	}
	// Same in-container workdir, different host sources — no shared bytes.
	if containsPair(gotB, "-v", "/host/node-a:/workspace") {
		t.Fatalf("node B mounted node A's host dir — isolation broken; argv=%v", gotB)
	}
}

func TestParams_RunArgs_NoCaches(t *testing.T) {
	p := Params{Workdir: "/workspace", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "gauntlet-run1-check1", "/rd")

	// No cache volumes ⇒ no extra -v pairs beyond the two fixed mounts;
	// image immediately follows the GAUNTLET_RUN_ID env pair.
	idx := indexOf(got, "img")
	if idx == -1 {
		t.Fatalf("image not found in argv: %v", got)
	}
	if got[idx-1] != "GAUNTLET_RUN_ID="+job.RunID {
		t.Fatalf("expected image immediately after run-id env when no caches, got argv: %v", got)
	}
	if got[idx+1] != "true" {
		t.Fatalf("command must follow image: %v", got)
	}
}

func TestParams_RunArgs_EnvVarsAllSix(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	wantEnv := []string{
		core.EnvBaseSHA + "=" + job.BaseSHA,
		core.EnvMergeSHA + "=" + job.MergeSHA,
		core.EnvCandidateSHA + "=" + job.Candidate.SHA,
		core.EnvRef + "=" + job.Candidate.Ref,
		core.EnvResultFile + "=/gauntlet/result",
		core.EnvRunID + "=" + job.RunID,
	}
	for _, e := range wantEnv {
		if !containsPair(got, "-e", e) {
			t.Errorf("argv missing -e %q; argv=%v", e, got)
		}
	}
}

func TestParams_RunArgs_TrialTreeMountReadWrite(t *testing.T) {
	// The trial tree is bind-mounted RW at workdir (matches LocalExecutor
	// — no :ro suffix, since export is ephemeral).
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

func TestParams_RunArgs_GitDirMountAndEnv(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img", GitDir: "/state/repos/origin.git"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	// The bare repo is mounted read-only at the FIXED containerGitDir —
	// never at a host-derived path — so path-keyed build caches (e.g. Go's,
	// keyed on full file paths) stay stable across builders and daemons.
	if !containsPair(got, "-v", "/state/repos/origin.git:/gauntlet-git:ro") {
		t.Fatalf("expected read-only bare-repo mount at %s; argv=%v", containerGitDir, got)
	}
	if !containsPair(got, "-e", "GAUNTLET_GIT_DIR=/gauntlet-git") {
		t.Fatalf("expected GAUNTLET_GIT_DIR pointing at the fixed in-container path; argv=%v", got)
	}
	mountIdx := indexOfPair(got, "-v", "/state/repos/origin.git:/gauntlet-git:ro")
	envIdx := indexOfPair(got, "-e", "GAUNTLET_GIT_DIR=/gauntlet-git")
	imgIdx := indexOf(got, "img")
	if !(mountIdx < imgIdx && envIdx < imgIdx) {
		t.Fatalf("git-dir mount and env must precede the image; argv=%v", got)
	}
}

func TestParams_RunArgs_EmptyGitDirAddsNothing(t *testing.T) {
	// Empty GitDir keeps the pre-GitDir run shape byte-identical: no extra
	// mount, no GAUNTLET_GIT_DIR env pair.
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	for _, a := range got {
		if strings.Contains(a, "GAUNTLET_GIT_DIR") || strings.Contains(a, containerGitDir) {
			t.Fatalf("empty GitDir must add no git-dir argv entries, found %q; argv=%v", a, got)
		}
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

func TestParams_RunArgs_MountsAfterCachesBeforeImage(t *testing.T) {
	p := Params{
		Workdir: "/w",
		Image:   "img",
		Caches: []Cache{
			{Name: "gocache", Path: "/root/.cache/go-build"},
		},
		Mounts: []Mount{
			{Host: "/var/run/docker.sock", Path: "/var/run/docker.sock"},
		},
	}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	cacheIdx := indexOfPair(got, "-v", "gocache:/root/.cache/go-build")
	mountIdx := indexOfPair(got, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	imageIdx := indexOf(got, "img")
	if cacheIdx == -1 || mountIdx == -1 || imageIdx == -1 {
		t.Fatalf("missing expected entries; argv=%v", got)
	}
	if !(cacheIdx < mountIdx && mountIdx < imageIdx) {
		t.Fatalf("want cache mount, then bind mount, then image, in that order; argv=%v", got)
	}
}

func TestParams_RunArgs_MountReadOnlySuffix(t *testing.T) {
	p := Params{
		Workdir: "/w",
		Image:   "img",
		Mounts: []Mount{
			{Host: "/host/rw", Path: "/rw"},
			{Host: "/host/ro", Path: "/ro", ReadOnly: true},
		},
	}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	if !containsPair(got, "-v", "/host/rw:/rw") {
		t.Fatalf("expected read-write mount with no :ro suffix; argv=%v", got)
	}
	if !containsPair(got, "-v", "/host/ro:/ro:ro") {
		t.Fatalf("expected read-only mount with :ro suffix; argv=%v", got)
	}
}

func TestParams_RunArgs_MultipleMountsPreserveOrder(t *testing.T) {
	p := Params{
		Workdir: "/w",
		Image:   "img",
		Mounts: []Mount{
			{Host: "/a", Path: "/a"},
			{Host: "/b", Path: "/b"},
			{Host: "/c", Path: "/c"},
		},
	}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	prevIdx := -1
	for _, m := range p.Mounts {
		idx := indexOfPair(got, "-v", m.Host+":"+m.Path)
		if idx == -1 {
			t.Fatalf("missing mount %s:%s; argv=%v", m.Host, m.Path, got)
		}
		if idx < prevIdx {
			t.Fatalf("mounts out of order; argv=%v", got)
		}
		prevIdx = idx
	}
}

func TestParams_RunArgs_NoMounts(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	// No mounts configured ⇒ no extra -v pairs beyond the fixed mounts (and
	// any caches); image immediately follows whatever came before it.
	idx := indexOf(got, "img")
	if idx == -1 {
		t.Fatalf("image not found in argv: %v", got)
	}
	if got[idx-1] != "GAUNTLET_RUN_ID="+job.RunID {
		t.Fatalf("expected image immediately after run-id env when no caches/mounts, got argv: %v", got)
	}
}

func TestParams_RunArgs_NetworksAndServiceEnv(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})
	job.Networks = []string{"gauntlet-svc-abcd1234"}
	job.ServiceEnv = []string{"GAUNTLET_SVC_PG_HOST=keyhash12", "GAUNTLET_SVC_PG_PORT=5432"}

	got := p.runArgs(job, "n", "/rd")

	if !containsPair(got, "--network", "gauntlet-svc-abcd1234") {
		t.Fatalf("expected --network gauntlet-svc-abcd1234; argv=%v", got)
	}
	if !containsPair(got, "-e", "GAUNTLET_SVC_PG_HOST=keyhash12") {
		t.Fatalf("expected -e GAUNTLET_SVC_PG_HOST=keyhash12; argv=%v", got)
	}
	if !containsPair(got, "-e", "GAUNTLET_SVC_PG_PORT=5432") {
		t.Fatalf("expected -e GAUNTLET_SVC_PG_PORT=5432; argv=%v", got)
	}
	imgIdx := indexOf(got, "img")
	netIdx := indexOfPair(got, "--network", "gauntlet-svc-abcd1234")
	envIdx := indexOfPair(got, "-e", "GAUNTLET_SVC_PG_HOST=keyhash12")
	if imgIdx == -1 || netIdx == -1 || envIdx == -1 || !(netIdx < imgIdx && envIdx < imgIdx) {
		t.Fatalf("expected --network/-e (services) before the image; argv=%v", got)
	}
}

func TestParams_RunArgs_NoNetworksNoServiceEnv(t *testing.T) {
	p := Params{Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.runArgs(job, "n", "/rd")

	if contains(got, "--network") {
		t.Fatalf("expected no --network flag for a needs-free job; argv=%v", got)
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
		token, runID, check, want string
	}{
		// Empty token preserves the pre-B1 shape exactly (test compatibility).
		{"", "run1", "build", "gauntlet-run1-build"},
		{"", "20260704T120000Z-1-abc123def456", "unit test", "gauntlet-20260704T120000Z-1-abc123def456-unit-test"},
		{"", "weird/run:id", "check/name", "gauntlet-weird-run-id-check-name"},
		{"", "", "", "gauntlet--"},
		// B1: a non-empty token namespaces the name so
		// cmd/gauntlet/sweep.go's orphan sweep can scope to it.
		{"a1b2c3d4", "run1", "build", "gauntlet-a1b2c3d4-run1-build"},
		{"a1b2c3d4", "", "", "gauntlet-a1b2c3d4--"},
	}
	for _, c := range cases {
		got := containerName(c.token, c.runID, c.check)
		if got != c.want {
			t.Errorf("containerName(%q, %q, %q) = %q, want %q", c.token, c.runID, c.check, got, c.want)
		}
	}
}

// TestContainerName_TruncatesTailNotToken proves a pathologically long
// runID/check never eats into the "gauntlet-<token>-" prefix that
// sweepContainerOrphans matches on (B1): the token must always survive
// intact.
func TestContainerName_TruncatesTailNotToken(t *testing.T) {
	longRunID := strings.Repeat("x", 300)
	got := containerName("mytoken", longRunID, "check")
	const wantPrefix = "gauntlet-mytoken-"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("containerName truncation ate into the prefix: got %q, want prefix %q", got, wantPrefix)
	}
	if len(got) > maxContainerNameLen {
		t.Fatalf("containerName length = %d, want <= %d (got %q)", len(got), maxContainerNameLen, got)
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

// --- RunCheck: ScratchDir roots the host-side result-dir mount. ---
// Uses a fake runtime script on PATH rather than a real container runtime:
// this only proves resultDir's location, not anything about real
// container behavior — verify the actual mount still works against a real
// `container` CLI separately.

// TestContainerExecutor_ScratchDirRootsResultDirMount proves that a
// non-empty Params.ScratchDir roots the ephemeral result-dir
// (bind-mounted at containerResultDir) under it instead of the OS default
// temp dir, without changing anything else about the `run` invocation —
// same mount count, same flags, same in-container path.
func TestContainerExecutor_ScratchDirRootsResultDirMount(t *testing.T) {
	scratch := t.TempDir()
	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "captured-args")

	// A fake "container" CLI: logs every invocation's argv (one line each)
	// to capture and exits 0 unconditionally — satisfies both the preflight
	// probe ("system status") and the actual "run" call.
	script := "#!/bin/sh\necho \"$@\" >> " + capture + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "container"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	t.Setenv("PATH", binDir)

	c, err := New(Params{Runtime: "container", Image: "img", ScratchDir: scratch})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()

	res := c.RunCheck(context.Background(), job)
	if res.Err != nil {
		t.Fatalf("unexpected Err: %v", res.Err)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	var runLine string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "run ") {
			runLine = line
		}
	}
	if runLine == "" {
		t.Fatalf("no `run` invocation captured; log=%q", data)
	}
	if !strings.Contains(runLine, "-v "+scratch+string(os.PathSeparator)) {
		t.Fatalf("run args = %q, want a -v mount whose host side is rooted under ScratchDir %q", runLine, scratch)
	}
}

// --- diagnoseMount: pure decision function. ---

func TestDiagnoseMount(t *testing.T) {
	cases := []struct {
		name       string
		diagOutput string
		diagErr    error
		want       mountDiagnosis
	}{
		{"errored diagnostic is inconclusive regardless of output", "some output", errors.New("exit status 127"), diagInconclusive},
		{"empty output with no error means the mount arrived empty", "", nil, diagMountEmpty},
		{"whitespace-only output still counts as empty", "  \n\t \n", nil, diagMountEmpty},
		{"non-empty output means the mount had real content", "main.go\n", nil, diagMountHasContent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diagnoseMount(c.diagOutput, c.diagErr)
			if got != c.want {
				t.Errorf("diagnoseMount(%q, %v) = %v, want %v", c.diagOutput, c.diagErr, got, c.want)
			}
		})
	}
}

// --- emptyMountDiagArgs: pure argv builder, no runtime required. ---

func TestParams_EmptyMountDiagArgs_Shape(t *testing.T) {
	p := Params{Workdir: "/workspace", Image: "ghcr.io/acme/ci:latest"}
	job := containerJob([]string{"go", "test", "./..."})

	got := p.emptyMountDiagArgs(job)

	want := []string{
		// Empty Runtime defaults to "container" (New's default, mirrored
		// here since emptyMountDiagArgs is a Params method called before
		// New's normalization would apply), so --progress none is present
		// same as runArgs.
		"run", "--progress", "none", "--rm",
		"-v", "/host/trial-tree:/workspace:ro",
		"--entrypoint", "/bin/sh",
		"ghcr.io/acme/ci:latest",
		"-c", "ls -A /workspace | head -1",
	}
	if len(got) != len(want) {
		t.Fatalf("emptyMountDiagArgs length = %d, want %d\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("emptyMountDiagArgs[%d] = %q, want %q\n got=%v\nwant=%v", i, got[i], want[i], got, want)
		}
	}
}

func TestParams_EmptyMountDiagArgs_ContainerRuntimeGetsProgressFlag(t *testing.T) {
	// Same --progress none suppression as runArgs (Apple's container CLI
	// pollutes stderr otherwise) — the diagnostic run is still a `run`.
	p := Params{Runtime: "container", Workdir: "/w", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.emptyMountDiagArgs(job)

	if !containsPair(got, "--progress", "none") {
		t.Fatalf("expected --progress none for the container runtime; argv=%v", got)
	}
}

func TestParams_EmptyMountDiagArgs_MountReadOnly(t *testing.T) {
	// :ro, unlike the trial-tree mount in the real run — the diagnostic
	// never needs to write, and read-only avoids any risk of the listing
	// container mutating the trial tree out from under the real run (moot
	// in practice since it only runs after the real container has exited,
	// but cheap to assert).
	p := Params{Workdir: "/workspace", Image: "img"}
	job := containerJob([]string{"true"})

	got := p.emptyMountDiagArgs(job)

	if !containsPair(got, "-v", "/host/trial-tree:/workspace:ro") {
		t.Fatalf("expected read-only trial-tree mount; argv=%v", got)
	}
}

// --- RunCheck: empty-mount reclassification, via the fake-CLI harness. ---
//
// Same technique as TestContainerExecutor_ScratchDirRootsResultDirMount: a
// fake "container" CLI on PATH stands in for the real runtime so this proves
// RunCheck's wiring (which argv triggers which behavior) without needing a
// real container runtime. The fake distinguishes the diagnostic invocation
// from the probe and the real `run` by the presence of "--entrypoint" in its
// argv (emptyMountDiagArgs is the only caller that ever passes it).

// writeFakeRuntime installs a fake "container" CLI on PATH that: always
// succeeds the probe ("system status"), always fails the real check run
// (nonzero exit, so RunCheck's CheckFailed/diagnostic path is reached), and
// for the diagnostic run (argv containing "--entrypoint") prints
// diagOutput's content iff the sentinel file at contentMarker exists,
// mimicking a listing that finds content vs. an empty directory.
func writeFakeRuntime(t *testing.T, contentMarker string) {
	t.Helper()
	binDir := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "--entrypoint" ]; then
    if [ -f "` + contentMarker + `" ]; then
      echo "somefile"
    fi
    exit 0
  fi
done
case "$1 $2" in
  "system status") exit 0 ;;
esac
echo boom
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "container"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	t.Setenv("PATH", binDir)
}

func TestContainerExecutor_EmptyMount_ReclassifiesToErr(t *testing.T) {
	writeFakeRuntime(t, filepath.Join(t.TempDir(), "never-created"))

	c, err := New(Params{Runtime: "container", Image: "img"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()

	res := c.RunCheck(context.Background(), job)

	if res.Err == nil {
		t.Fatalf("expected Err (empty-mount reclassification), got Status=%v Output=%q", res.Status, res.Output)
	}
	if !strings.Contains(res.Err.Error(), job.Dir) {
		t.Errorf("Err = %v, want it to mention job.Dir %q", res.Err, job.Dir)
	}
	if res.Output != "boom\n" {
		t.Errorf("Output = %q, want the original captured output preserved", res.Output)
	}
}

func TestContainerExecutor_NonEmptyMount_KeepsRedUnconverted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "has-content")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	writeFakeRuntime(t, marker)

	c, err := New(Params{Runtime: "container", Image: "img"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()

	res := c.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("expected no reclassification when the mount has content, got Err: %v", res.Err)
	}
	if res.Status != core.CheckFailed {
		t.Errorf("Status = %v, want CheckFailed", res.Status)
	}
}

func TestContainerExecutor_InconclusiveDiagnostic_KeepsRedUnconverted(t *testing.T) {
	// The check image has no /bin/sh (or the diagnostic errors for any
	// other reason): the fake CLI fails whenever --entrypoint is present,
	// simulating that. Reclassification must not happen — an unprovable
	// guess about an empty mount must never override a real check-side red.
	binDir := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "--entrypoint" ]; then
    echo "no such file or directory: /bin/sh" >&2
    exit 127
  fi
done
case "$1 $2" in
  "system status") exit 0 ;;
esac
echo boom
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "container"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	t.Setenv("PATH", binDir)

	c, err := New(Params{Runtime: "container", Image: "img"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job := containerJob([]string{"true"})
	job.Dir = t.TempDir()

	res := c.RunCheck(context.Background(), job)

	if res.Err != nil {
		t.Fatalf("expected no reclassification when the diagnostic itself errors, got Err: %v", res.Err)
	}
	if res.Status != core.CheckFailed {
		t.Errorf("Status = %v, want CheckFailed", res.Status)
	}
}

// --- integration tests against the real runtime, if usable. ---
//
// A machine with only Apple's `container` installed may have its backing
// service down ("apiserver is not running and not registered with
// launchd"). These tests probe first and skip cleanly rather than failing
// when that's the case.

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

	t.Run("GitDirMountedReadOnly", func(t *testing.T) {
		// A REAL bare repo (holding an unpushed synthetic merge — the exact
		// object topology a live run mounts) at the fixed /gauntlet-git:
		// the check must be able to read through $GAUNTLET_GIT_DIR but
		// never write through it. The tiny cached test images carry no git
		// binary, so the read/write probes use shell built-ins; the git
		// *query* contract against this same repo shape is proven
		// end-to-end by TestLocalExecutor_GitDirEndToEndQueries — the
		// container-specific deltas under test here are the fixed mount
		// path, the env value, and the :ro enforcement.
		gitDir, _, _, mergeSHA := bareRepoWithUnpushedMerge(t)

		dir := t.TempDir()
		cmd := containerScript(t, dir, "/workspace", "check.sh", `#!/bin/sh
set -eu
[ "$GAUNTLET_GIT_DIR" = "/gauntlet-git" ] || { echo "GAUNTLET_GIT_DIR=$GAUNTLET_GIT_DIR"; exit 1; }
[ -f "$GAUNTLET_GIT_DIR/HEAD" ] || { echo "no HEAD under the mount"; exit 1; }
if echo poison > "$GAUNTLET_GIT_DIR/objects/poison" 2>/dev/null; then
    echo "object store is writable through the mount"
    exit 1
fi
if echo poison > "$GAUNTLET_GIT_DIR/config" 2>/dev/null; then
    echo "config is writable through the mount"
    exit 1
fi
`)
		c, err := New(Params{Runtime: testRuntime, Image: image, Workdir: "/workspace", GitDir: gitDir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		job := newJob(t, cmd)
		job.Dir = dir
		job.MergeSHA = mergeSHA

		res := c.RunCheck(context.Background(), job)

		if res.Err != nil {
			t.Fatalf("unexpected Err: %v (output=%q)", res.Err, res.Output)
		}
		if res.Status != core.CheckPassed {
			t.Fatalf("Status = %v, want CheckPassed; output=%q", res.Status, res.Output)
		}
		// The write probes must have failed inside the container without
		// leaving a trace on the host either.
		if _, err := os.Stat(filepath.Join(gitDir, "objects", "poison")); !os.IsNotExist(err) {
			t.Fatalf("write probe reached the host object store (stat err=%v)", err)
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
