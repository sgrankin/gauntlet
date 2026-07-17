package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// containerResultDir and containerResultFile are the fixed in-container
// paths the writable result-dir mount is bound to. Distinct from the trial
// tree mount (job.Dir -> Workdir), which must stay separate: checks must not
// be able to see or clobber the result-file mechanism from their own
// working directory.
//
// internal/config's validate() duplicates containerResultDir as
// reservedResultDir (that package deliberately doesn't import this one — see
// Params.Caches's doc below) to reject an operator Mount at or under this
// path. Renaming this constant means updating that copy too.
const (
	containerResultDir  = "/gauntlet"
	containerResultFile = containerResultDir + "/result"
)

// containerGitDir is the fixed in-container path the daemon's bare repo is
// bind-mounted at (read-only) when Params.GitDir is set, and the value
// GAUNTLET_GIT_DIR carries in that case. Fixed — never derived from the
// host path — deliberately: path-keyed build caches (Go's build cache keys
// on full file paths) stay stable across builders and daemons this way, the
// same reason the trial tree always lands at one Workdir. A sibling of
// containerResultDir rather than a child (/gauntlet/git) so no runtime ever
// has to nest one bind mount inside another.
//
// internal/config's validate() duplicates this as reservedGitDir (that
// package deliberately doesn't import this one — see containerResultDir's
// note above) to reject an operator Mount at or under this path. Renaming
// this constant means updating that copy too.
const containerGitDir = "/gauntlet-git"

// Cache is a persistent named volume mounted into every check container, so
// build caches (e.g. Go's module/build cache) survive across runs instead of
// re-downloading/rebuilding from scratch in a fresh container each time.
type Cache struct {
	Name string // volume name (docker-compatible runtimes create it on first use)
	Path string // mount path inside the container
}

// Mount is a host bind mount into every check container — a generic
// primitive alongside Cache's named volumes (DESIGN.md decision ledger,
// "Generic container mounts"), config.Mount's package-local mirror (this
// package doesn't import internal/config, same reason Params.Caches exists
// as its own type rather than aliasing config.Cache). The motivating case is
// the host docker socket for testcontainers-based checks, but nothing here
// is docker-specific: Host and Path are just bound with a docker-compatible
// `-v` flag, `:ro` appended when ReadOnly.
type Mount struct {
	Host     string // absolute host path (file or dir)
	Path     string // absolute mount path inside the container
	ReadOnly bool   // when true, mounted read-only (:ro)
}

// Params configures a ContainerExecutor. Package-local: this package does
// not import internal/config; cmd/gauntlet maps config.Executor fields
// into this struct.
type Params struct {
	// Runtime selects the CLI: "docker", "podman", or "container" (Apple's
	// container CLI). Empty defaults to "container" per the config default
	// — this package doesn't require the caller to have resolved that
	// default itself.
	Runtime string

	// Image is the OCI image every check runs in. Required.
	Image string

	// Workdir is where the trial tree is bind-mounted and the check's
	// working directory. Empty defaults to "/workspace".
	Workdir string

	// Caches are persistent named volumes mounted alongside the trial tree
	// and result dir.
	Caches []Cache

	// Mounts are operator-configured host bind mounts, applied after Caches
	// (see runArgs) — e.g. the host docker socket, for repos whose checks
	// run testcontainers against the host daemon.
	Mounts []Mount

	// Env is fixed operator-owned environment ("NAME=VALUE" strings, e.g.
	// TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal), emitted as -e
	// pairs BEFORE the GAUNTLET_* contract and service env — so on any
	// collision the gauntlet-provided values win (last -e wins to every
	// docker-compatible runtime). Config validation already rejects
	// GAUNTLET_-prefixed names outright.
	Env []string

	// AddHosts are --add-host <host>:<gateway> entries (the
	// testcontainers host.docker.internal pattern), pre-joined by the
	// caller into "host:gateway" form.
	AddHosts []string

	// Memory/CPUs, when non-empty, are passed verbatim as --memory/--cpus
	// resource ceilings. Empty emits no flag (the runtime's default).
	Memory string
	CPUs   string

	// GitDir, when non-empty, is the daemon's bare repo path on the host
	// (absolute — a relative -v source is a named volume to every
	// docker-compatible runtime, not a bind). runArgs mounts it read-only
	// at the fixed containerGitDir and exports GAUNTLET_GIT_DIR pointing
	// there, so affected-only checks can `git diff`/`git log` the SHAs the
	// env contract hands them without their own object store
	// (core.EnvGitDir). Empty adds no mount and no variable — the
	// pre-GitDir container run shape, byte-identical.
	GitDir string

	// ScratchDir is the directory each check's ephemeral host-side result
	// dir (gauntlet-container-*, bind-mounted into the container at
	// containerResultDir) is created under via os.MkdirTemp(ScratchDir,
	// ...) — the same fix as LocalExecutor's BaseDir: rooting this under
	// -state/scratch, swept at daemon startup, closes the gap where it
	// used to escape every sweep by defaulting to the OS temp dir. Empty
	// preserves the exact prior behavior
	// (os.MkdirTemp's own "" -> os.TempDir() fallback). This only changes
	// the mount's host-side source path — the in-container path
	// (containerResultDir) and every other mount/flag in runArgs are
	// unaffected, so the container run shape itself is unchanged, just
	// rooted differently on the host.
	ScratchDir string

	// Token namespaces this daemon's container names
	// (gauntlet-<Token>-<runID>-<check>, see containerName) so
	// cmd/gauntlet's startup orphan sweep only ever reaps containers from
	// its own -state dir. Without it, the "gauntlet-" name prefix is
	// host-global — identical for every gauntlet process on the box — so
	// a daemon restarting against a different -state dir could `rm -f` a
	// live sibling daemon's in-flight containers; AcquireLock's flock only
	// guards the -state dir itself, not this shared naming namespace.
	// Minted once in cmd/gauntlet from a short hash of the absolute
	// -state path and threaded through to both this executor and the
	// sweep. Empty preserves the unnamespaced naming exactly (test
	// compatibility).
	Token string
}

// runtimeSpec captures the one CLI-shape difference between supported
// runtimes: the binary name and how to probe whether its backing service is
// reachable. Flags themselves are docker-compatible across all three, so
// RunCheck builds one argv shape for all of them.
type runtimeSpec struct {
	Bin       string
	ProbeArgs []string

	// ExtraRunArgs are runtime-specific flags inserted right after "run".
	// Apple's container CLI emits "[N/6] ..." progress lines on stderr even
	// without a TTY, which pollutes the captured check output (observed
	// live: a GitHub status description quoting "[0/6] [0s]"); --progress
	// none suppresses them at the source. docker/podman emit no progress
	// on run.
	ExtraRunArgs []string
}

var runtimeSpecs = map[string]runtimeSpec{
	"docker":    {Bin: "docker", ProbeArgs: []string{"ps"}},
	"podman":    {Bin: "podman", ProbeArgs: []string{"ps"}},
	"container": {Bin: "container", ProbeArgs: []string{"system", "status"}, ExtraRunArgs: []string{"--progress", "none"}},
}

// ContainerExecutor runs checks inside a container via a docker-compatible
// CLI (docker, podman, or Apple's container). It implements core.Executor
// with the same contract as LocalExecutor: same verdict mapping, same
// 64KiB tail-capped output, same process-group cancel discipline. The one
// executor-specific addition is that a missing runtime binary or an
// unreachable backing service reports CheckResult.Err (a daemon
// condition), never a verdict.
type ContainerExecutor struct {
	spec   runtimeSpec
	params Params
}

// New validates params and returns a ready ContainerExecutor. It does not
// touch the runtime itself — reachability is checked per-RunCheck, since
// the service can come and go across the life of a long-running daemon.
func New(params Params) (*ContainerExecutor, error) {
	if params.Runtime == "" {
		params.Runtime = "container"
	}
	spec, ok := runtimeSpecs[params.Runtime]
	if !ok {
		return nil, fmt.Errorf("executor: unknown container runtime %q (want docker, podman, or container)", params.Runtime)
	}
	if params.Image == "" {
		return nil, errors.New("executor: container image is required")
	}
	if params.Workdir == "" {
		params.Workdir = "/workspace"
	}
	return &ContainerExecutor{spec: spec, params: params}, nil
}

// RunCheck implements core.Executor.
func (c *ContainerExecutor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	start := time.Now()

	if err := ctx.Err(); err != nil {
		return core.CheckResult{Name: job.Name, Err: err, Duration: time.Since(start)}
	}

	// Missing runtime binary or unreachable service is a daemon condition,
	// not a verdict — checked before we ever invoke `run`, so a nonzero
	// exit from `run` itself can be trusted to mean "the containerized
	// check command failed", same as LocalExecutor.
	if err := probeRuntime(ctx, c.spec); err != nil {
		return core.CheckResult{
			Name:     job.Name,
			Err:      fmt.Errorf("executor: %w", err),
			Duration: time.Since(start),
		}
	}

	resultDir, err := os.MkdirTemp(c.params.ScratchDir, "gauntlet-container-")
	if err != nil {
		return core.CheckResult{
			Name:     job.Name,
			Err:      fmt.Errorf("executor: create result dir: %w", err),
			Duration: time.Since(start),
		}
	}
	defer os.RemoveAll(resultDir)

	resultFile := filepath.Join(resultDir, "result")
	if err := os.WriteFile(resultFile, nil, 0o600); err != nil {
		return core.CheckResult{
			Name:     job.Name,
			Err:      fmt.Errorf("executor: create result file: %w", err),
			Duration: time.Since(start),
		}
	}

	name := containerName(c.params.Token, job.RunID, job.Name)
	args := c.params.runArgs(job, name, resultDir)

	cmd := exec.CommandContext(ctx, c.spec.Bin, args...)

	out := &tailBuffer{cap: outputCap}

	// logFile, when non-nil, is teed alongside the tail buffer: the full,
	// uncapped combined output (DESIGN.md "Full per-check log files"),
	// captured identically to LocalExecutor since both just wire up
	// cmd.Stdout/Stderr on the CLI subprocess. Its open error is
	// deliberately swallowed here — see openCheckLog's doc — so a
	// bad/unwritable job.LogPath degrades to the tail-only capture this
	// executor already had, never to a failed check.
	logFile, _ := openCheckLog(job.LogPath)
	var combined io.Writer = out
	if logFile != nil {
		defer logFile.Close()
		combined = io.MultiWriter(out, logFile)
	}
	cmd.Stdout = combined
	cmd.Stderr = combined

	// Own process group so a cancel can kill the whole CLI-and-children
	// tree (same discipline as LocalExecutor), plus a best-effort named
	// kill: the CLI process may exit on SIGKILL before the runtime has torn
	// down the container it started, so ask the runtime to kill it by name
	// too.
	bin := c.spec.Bin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(killCtx, bin, "kill", name).Run()
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	duration := time.Since(start)

	// The command has now been attempted regardless of outcome, so every
	// return from here on reports whether the full log file actually got
	// written: LogPath is set iff logFile was successfully opened above,
	// empty otherwise (no file requested, or the open/mkdir fallback).
	logPath := ""
	if logFile != nil {
		logPath = job.LogPath
	}

	// ctx cancellation takes precedence over any run error, same as
	// LocalExecutor: the CLI may exit signalled/non-zero as a side effect
	// of the cancel, but that is not a verdict.
	if ctx.Err() != nil {
		return core.CheckResult{
			Name:     job.Name,
			Err:      ctx.Err(),
			Output:   out.String(),
			LogPath:  logPath,
			Duration: duration,
		}
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// Nonzero exit is a verdict regardless of the result file,
			// same as LocalExecutor: the file only splits the exit-0 case.
			res := core.CheckResult{
				Name:     job.Name,
				Status:   core.CheckFailed,
				Output:   out.String(),
				LogPath:  logPath,
				Duration: duration,
			}
			// README "docker-on-macOS footguns": when the runtime VM
			// doesn't share the host path the trial tree lives under
			// (e.g. colima's default share list), `-v job.Dir:workdir`
			// silently bind-mounts an empty directory instead of
			// erroring, and the check fails against nothing — a daemon
			// misconfiguration masquerading as a genuine red. Only a
			// failed check pays for the extra probe; a passing check
			// never touches this.
			if c.checkEmptyMount(ctx, job) == diagMountEmpty {
				// Same conversion shape as the queue's mid-run-death
				// rewrite (internal/queue/reconcile.go: res.Err = ...,
				// Output/Duration left exactly as RunCheck set them) —
				// a verdict manufactured by daemon-side plumbing gets
				// reclassified to Err rather than standing as a red.
				res.Err = fmt.Errorf("executor: trial tree mounted empty in the container — the runtime VM likely does not share %s; see README 'docker-on-macOS footguns'", job.Dir)
			}
			return res
		}
		// The CLI itself failed to start (e.g. it vanished between the
		// preflight probe and this exec). Runtime reachability was already
		// checked above, so this is treated the same as LocalExecutor's
		// exec-start-failure branch: a verdict, not Err.
		output := out.String()
		if output != "" {
			output += "\n"
		}
		output += "executor: failed to start container CLI: " + runErr.Error()
		return core.CheckResult{
			Name:     job.Name,
			Status:   core.CheckFailed,
			Output:   output,
			LogPath:  logPath,
			Duration: duration,
		}
	}

	status := core.CheckPassed
	if data, err := os.ReadFile(resultFile); err == nil && strings.TrimSpace(string(data)) == "skipped" {
		status = core.CheckSkipped
	}
	return core.CheckResult{
		Name:     job.Name,
		Status:   status,
		Output:   out.String(),
		LogPath:  logPath,
		Duration: duration,
	}
}

// runArgs builds the full docker-compatible `run` argv (everything after
// the binary name) for job:
//
//	<bin> run --rm --name gauntlet-<runID>-<check> \
//	  -w <workdir> \
//	  -v <job.Dir>:<workdir>            # trial tree, read-write \
//	  -v <resultDir>:/gauntlet          # writable result dir \
//	  -v <p.GitDir>:/gauntlet-git:ro    # bare repo, only when GitDir is set \
//	  -e GAUNTLET_* (all six, plus GAUNTLET_GIT_DIR when GitDir is set) \
//	  -v <cacheName>:<cachePath> ...    # persistent named cache volumes \
//	  -v <mountHost>:<mountPath>[:ro] ... # operator-configured host bind mounts \
//	  --network <n> ...                # one per job.Networks (ModeNetwork) \
//	  -e <kv> ...                      # one per job.ServiceEnv (resolved `needs`) \
//	  <image> <job.Command...>
//
// Pure and exec-free: exhaustively unit-testable without any runtime.
func (p Params) runArgs(job core.CheckJob, name, resultDir string) []string {
	runtime := p.Runtime
	if runtime == "" {
		runtime = "container"
	}
	args := append([]string{"run"}, runtimeSpecs[runtime].ExtraRunArgs...)
	args = append(args,
		"--rm", "--name", name,
		"-w", p.Workdir,
		"-v", job.Dir+":"+p.Workdir,
		"-v", resultDir+":"+containerResultDir,
	)
	if p.GitDir != "" {
		args = append(args, "-v", p.GitDir+":"+containerGitDir+":ro")
	}
	for _, ah := range p.AddHosts {
		args = append(args, "--add-host", ah)
	}
	if p.Memory != "" {
		args = append(args, "--memory", p.Memory)
	}
	if p.CPUs != "" {
		args = append(args, "--cpus", p.CPUs)
	}
	// Fixed profile env FIRST: the GAUNTLET_* contract and per-run service
	// env below must win any collision (last -e wins).
	for _, kv := range p.Env {
		args = append(args, "-e", kv)
	}
	args = append(args,
		"-e", core.EnvBaseSHA+"="+job.BaseSHA,
		"-e", core.EnvMergeSHA+"="+job.MergeSHA,
		"-e", core.EnvCandidateSHA+"="+job.Candidate.SHA,
		"-e", core.EnvRef+"="+job.Candidate.Ref,
		"-e", core.EnvResultFile+"="+containerResultFile,
		"-e", core.EnvRunID+"="+job.RunID,
	)
	if p.GitDir != "" {
		args = append(args, "-e", core.EnvGitDir+"="+containerGitDir)
	}
	for _, c := range p.Caches {
		args = append(args, "-v", c.Name+":"+c.Path)
	}
	for _, m := range p.Mounts {
		v := m.Host + ":" + m.Path
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}
	// Shared-services wiring: nil for checks with no `needs`, so this is a
	// no-op append in the common case.
	for _, n := range job.Networks {
		args = append(args, "--network", n)
	}
	for _, kv := range job.ServiceEnv {
		args = append(args, "-e", kv)
	}
	args = append(args, p.Image)
	args = append(args, job.Command...)
	return args
}

// mountDiagnosis is checkEmptyMount's verdict on why a check just failed:
// whether the trial-tree mount arrived empty in the container (the
// docker-on-macOS footgun this preflight exists for), genuinely had content
// (an ordinary check-side red), or the diagnostic itself couldn't run (e.g.
// the check image has no /bin/sh) and so proves nothing either way.
type mountDiagnosis int

const (
	diagInconclusive    mountDiagnosis = iota // diagnostic errored; leave the red alone
	diagMountEmpty                            // listing succeeded and found nothing
	diagMountHasContent                       // listing succeeded and found something
)

// diagnoseMount classifies a diagnostic listing's outcome. Pure: no exec, no
// I/O, exhaustively unit-testable.
func diagnoseMount(diagOutput string, diagErr error) mountDiagnosis {
	if diagErr != nil {
		return diagInconclusive
	}
	if strings.TrimSpace(diagOutput) == "" {
		return diagMountEmpty
	}
	return diagMountHasContent
}

// emptyMountDiagArgs builds the argv for the post-failure diagnostic listing:
// a throwaway container of the SAME check image (no new image dependency,
// unlike reaching for e.g. alpine) that re-mounts job.Dir read-only at
// Workdir and lists it. --entrypoint /bin/sh bypasses whatever ENTRYPOINT
// the image ships so the check's own command never runs a second time; this
// does assume the image has a shell, which is not guaranteed for an
// arbitrary check image — see diagnoseMount's diagInconclusive case for what
// happens when it doesn't (the red stands untouched rather than risking a
// false reclassification).
//
// Pure and exec-free, same shape as runArgs.
func (p Params) emptyMountDiagArgs(job core.CheckJob) []string {
	runtime := p.Runtime
	if runtime == "" {
		runtime = "container"
	}
	args := append([]string{"run"}, runtimeSpecs[runtime].ExtraRunArgs...)
	args = append(args,
		"--rm",
		"-v", job.Dir+":"+p.Workdir+":ro",
		"--entrypoint", "/bin/sh",
		p.Image,
		"-c", "ls -A "+p.Workdir+" | head -1",
	)
	return args
}

// checkEmptyMount runs the diagnostic listing and classifies its outcome.
// Only called from RunCheck's CheckFailed path (a passing check never pays
// for this). ctx is expected non-cancelled at the call site (RunCheck
// already returns early on ctx.Err() before reaching here); a short timeout
// of its own bounds the extra container run regardless.
func (c *ContainerExecutor) checkEmptyMount(ctx context.Context, job core.CheckJob) mountDiagnosis {
	diagCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := c.params.emptyMountDiagArgs(job)
	out, err := exec.CommandContext(diagCtx, c.spec.Bin, args...).CombinedOutput()
	return diagnoseMount(string(out), err)
}

// maxContainerNameLen conservatively caps the assembled container name well
// below every known runtime's practical limit. Only the runID+check tail
// is ever truncated to fit — the "gauntlet-
// <token>-" prefix is never touched, since cmd/gauntlet/sweep.go's
// sweepContainerOrphans matches on that exact prefix to scope its kills to
// this daemon; truncating it would break that scoping.
const maxContainerNameLen = 200

// containerName derives a docker-compatible container name from a run ID
// and check name (run IDs are unique per-process, giving container names
// real collision-avoidance teeth), namespaced by token:
// "gauntlet-<token>-<runID>-<check>" when token is non-empty, or the
// unnamespaced "gauntlet-<runID>-<check>" when it's empty (test
// compatibility — see Params.Token's doc for why the namespace exists at
// all). Both runID and check are sanitized since neither is guaranteed
// name-safe (check names are free-form config; run IDs embed a trial-tree
// OID prefix); token is minted in cmd/gauntlet as a hex hash and needs no
// sanitizing.
func containerName(token, runID, check string) string {
	prefix := "gauntlet-"
	if token != "" {
		prefix += token + "-"
	}
	tail := sanitizeName(runID) + "-" + sanitizeName(check)
	if budget := maxContainerNameLen - len(prefix); budget > 0 && len(tail) > budget {
		tail = tail[:budget]
	}
	return prefix + tail
}

// sanitizeName replaces every rune outside [A-Za-z0-9_.-] with '-', the
// portable-name-safe subset shared by docker/podman/Apple container
// container-ID syntax. A thin wrapper over core.SanitizeName so
// internal/queue's per-check log path sanitization (DESIGN.md "Full
// per-check log files") shares exactly this logic instead of a second,
// possibly-drifting copy.
func sanitizeName(s string) string {
	return core.SanitizeName(s)
}

// probeRuntime reports whether spec's CLI is installed and its backing
// service is reachable. Used both as RunCheck's preflight (missing binary
// or unreachable service becomes CheckResult.Err, never a verdict) and by
// integration tests deciding whether to skip (the runtime's service may
// simply be down).
func probeRuntime(ctx context.Context, spec runtimeSpec) error {
	if _, err := exec.LookPath(spec.Bin); err != nil {
		return fmt.Errorf("container runtime binary %q not found: %w", spec.Bin, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, spec.Bin, spec.ProbeArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("container runtime %q service unreachable: %w (%s)", spec.Bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}
