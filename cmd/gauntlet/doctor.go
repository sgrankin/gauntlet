// `gauntlet doctor` is a host preflight, NOT client-side porcelain like
// validate/status/land: it actively probes the host and network the daemon
// is about to run on/against, using the exact code paths the daemon itself
// uses at startup (checkGitVersion, ghauth's key/JWT/mint path, gitx's
// credential-scoped git, internal/executor's runtime-reachability probe) —
// never a reimplementation of any of them. Its ONE externally visible
// action is minting a real GitHub App installation token in app-auth mode
// (the auth-mint probe, below); every other probe is strictly read-only:
// no push, no ref mutation, no write under -state, no image pull, no
// container start, no lock a running daemon could contend on. See
// docs/deploy.md's "Preflight" section.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/ghauth"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/history"
)

// doctorProbeTimeout bounds every network/subprocess probe (ls-remote, a
// runtime's `... ps`/`... info` reachability check) so a wedged host or
// network can never hang doctor. Shared via doctorEnv.timeout, which tests
// override to a short duration so a deliberately-stalled fake (a listener
// that accepts and never answers) proves the bound without a real ~10s
// wait.
//
// The auth-mint probe is the one exception: ghauth.App.Token deliberately
// mints on context.WithoutCancel (issue #6 — a shared in-flight mint must
// outlive any single caller's ctx), so this timeout does not bound it. The
// mint is instead bounded by ghauth's own mintTimeout (30s).
const doctorProbeTimeout = 10 * time.Second

// status is one probe's verdict.
type status int

const (
	statusPass status = iota
	statusWarn
	statusFail
)

func (s status) String() string {
	switch s {
	case statusPass:
		return "PASS"
	case statusWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// probeResult is one probe's outcome: a status, a one-line human detail,
// and — required whenever status is FAIL, per the doctor contract — a
// one-sentence remedy. detail/remedy must never carry credential bytes;
// printResult redacts defensively regardless.
type probeResult struct {
	status status
	detail string
	remedy string
}

func pass(detail string) probeResult { return probeResult{status: statusPass, detail: detail} }
func warn(detail string) probeResult { return probeResult{status: statusWarn, detail: detail} }
func fail(detail, remedy string) probeResult {
	return probeResult{status: statusFail, detail: detail, remedy: remedy}
}

// probe is one named host check. The table is built fresh per run from the
// loaded config (buildProbes) — only what the config actually uses is
// probed — and is exercised directly (probe.fn) by doctor_test.go, without
// going through the CLI/output layer.
type probe struct {
	name string
	fn   func(ctx context.Context) probeResult
}

// doctorEnv carries every probe's shared, test-overridable inputs.
type doctorEnv struct {
	cfg       *config.Daemon
	statePath string
	timeout   time.Duration

	// appTokens/appErr are buildAppTokens(cfg)'s result, computed once and
	// shared by the auth-mint probe and the remote probe (which
	// authenticates exactly like the daemon's own git fetch/push —
	// gitAuthOptions) — mirroring how main.go's run() builds it once and
	// threads it into both gitx.New and buildGHStatusChannel.
	appTokens *ghauth.App
	appErr    error
}

// runDoctor implements `gauntlet doctor`, writing to os.Stdout.
func runDoctor(args []string) error {
	return runDoctorTo(os.Stdout, args)
}

// errDoctorFailed is returned by runDoctorTo when at least one probe FAILed
// (WARN alone does not trigger it) — the detail was already printed by the
// probe loop, so main's error handler must not print it again, only exit 1.
var errDoctorFailed = errors.New("doctor: one or more probes failed")

// runDoctorTo does the actual work: load the config (a failure here prints
// one FAIL line and short-circuits — every other probe derives from the
// config, so there is nothing left to probe), then run every applicable
// probe independently (one FAIL never stops the rest) and print one
// deterministically-ordered line per probe. Exit-code contract: 0 when
// nothing FAILed (a WARN-only run still exits 0), 1 otherwise.
func runDoctorTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the daemon config (gauntlet.kdl) [required]")
	defaultState := ""
	if dir, err := os.UserCacheDir(); err == nil {
		defaultState = filepath.Join(dir, "gauntlet")
	}
	statePath := fs.String("state", defaultState, "directory for the daemon's bare repo clone(s) (probed, never written to)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required (path to gauntlet.kdl)")
	}

	cfg, err := config.LoadDaemon(*configPath)
	if err != nil {
		printResult(w, "config", fail(err.Error(), "fix the reported config error; every other probe derives from a valid config"))
		return errDoctorFailed
	}
	printResult(w, "config", pass(*configPath+": ok"))

	env := &doctorEnv{cfg: cfg, statePath: *statePath, timeout: doctorProbeTimeout}
	env.appTokens, env.appErr = buildAppTokens(cfg)

	hasFail := false
	for _, p := range buildProbes(env) {
		ctx, cancel := context.WithTimeout(context.Background(), env.timeout)
		res := p.fn(ctx)
		cancel()
		printResult(w, p.name, res)
		if res.status == statusFail {
			hasFail = true
		}
	}
	if hasFail {
		return errDoctorFailed
	}
	return nil
}

// credsRE matches the userinfo segment of a URL embedded in probe output
// (e.g. a static-mode HTTPS+PAT remote, or a git error message that echoes
// one back) — "://user:pass@" or "://user@". Doctor's own probe text never
// constructs one of these deliberately, but a subprocess's stderr (git's
// own error messages sometimes echo the resolved URL) might; printResult
// redacts unconditionally rather than trusting every probe to remember to.
var credsRE = regexp.MustCompile(`://[^/\s@]+@`)

func redactCreds(s string) string {
	return credsRE.ReplaceAllString(s, "://[redacted]@")
}

// printResult renders one probe's result as exactly one line: status, name,
// detail, and — FAIL only — an appended remedy clause.
func printResult(w io.Writer, name string, r probeResult) {
	line := fmt.Sprintf("%-4s %-24s %s", r.status, name, redactCreds(r.detail))
	if r.status == statusFail {
		line += " — remedy: " + redactCreds(r.remedy)
	}
	fmt.Fprintln(w, line)
}

// buildProbes assembles the probe table from env.cfg alone: a probe is only
// included when the config section it inspects is actually in use (an
// unconfigured github/history/dashboard/executor block means nothing to
// probe there), and the resulting slice order is what printResult's callers
// iterate in — fixed and deterministic (config order, never map iteration).
func buildProbes(env *doctorEnv) []probe {
	var probes []probe

	probes = append(probes, probe{"git", func(ctx context.Context) probeResult { return probeGit(ctx) }})
	probes = append(probes, probe{"state", func(ctx context.Context) probeResult { return probeState(env.statePath) }})

	if env.cfg.History.Path != "" {
		path := env.cfg.History.Path
		probes = append(probes, probe{"history", func(ctx context.Context) probeResult { return probeHistory(path) }})
	}

	if env.cfg.GitHub.Repo != "" {
		if env.cfg.GitHub.Auth != nil {
			cfg := env.cfg
			probes = append(probes,
				probe{"auth-key-perms", func(ctx context.Context) probeResult { return probeAuthKeyPerms(cfg) }},
				probe{"auth-tmpdir-exec", func(ctx context.Context) probeResult { return probeTMPDirExec(ctx) }},
				probe{"auth-mint", func(ctx context.Context) probeResult { return probeAuthMint(ctx, env.appTokens, env.appErr) }},
			)
		} else {
			cfg := env.cfg
			probes = append(probes, probe{"auth-token-env", func(ctx context.Context) probeResult { return probeAuthTokenEnv(cfg) }})
		}
	}

	probes = append(probes, probe{"remote", func(ctx context.Context) probeResult {
		return probeRemote(ctx, env.cfg, env.appTokens, env.appErr)
	}})

	// Slack/summarize: gated exactly like buildSlackChannel/buildSummarizer
	// (channels.go) gate the daemon's own startup — a configured block with
	// its env var unset fails the daemon loudly at boot today, with no
	// probe to catch it ahead of time before this.
	if env.cfg.Slack.Channel != "" {
		cfg := env.cfg
		probes = append(probes, probe{"slack-token-env", func(ctx context.Context) probeResult { return probeSlackTokenEnv(cfg) }})
	}
	if env.cfg.Summarize != nil {
		cfg := env.cfg
		probes = append(probes, probe{"summarize-token-env", func(ctx context.Context) probeResult { return probeSummarizeTokenEnv(cfg) }})
	}

	profiles := containerProfiles(env.cfg)
	// Runtime usages are derived from container-kind executor profiles PLUS
	// (when configured) the services pool, which shells out to a runtime of
	// its own via internal/services but is not itself a container-kind
	// executor profile — a services-only config (local executor, no
	// container profiles) would otherwise get zero runtime probes despite
	// the daemon depending on that runtime being reachable. Fed through the
	// same runtimeUsages grouping as profiles, not a bolted-on duplicate
	// probe, so a services runtime that coincides with a profile's (e.g.
	// both on "docker") still probes it exactly once, naming both usages.
	runtimeProfiles := profiles
	if len(env.cfg.Services.Allow) > 0 {
		runtimeProfiles = append(runtimeProfiles, containerProfile{label: "services", runtime: servicesRuntime(env.cfg)})
	}
	for _, u := range runtimeUsages(runtimeProfiles) {
		u := u
		probes = append(probes, probe{"executor-runtime:" + u.runtime, func(ctx context.Context) probeResult {
			return probeExecutorRuntime(ctx, u)
		}})
	}
	for _, p := range profiles {
		p := p
		probes = append(probes, probe{"executor-image:" + p.label, func(ctx context.Context) probeResult {
			return probeImagePresent(ctx, p.runtime, p.image)
		}})
	}

	if env.cfg.Dashboard.Bind != "" {
		cfg := env.cfg
		probes = append(probes, probe{"endpoint", func(ctx context.Context) probeResult { return probeEndpoint(cfg) }})
	}

	return probes
}

// --- git ------------------------------------------------------------------

// probeGit reuses checkGitVersion (gitcheck.go) — the exact check the
// daemon runs at startup — rather than re-deriving the version comparison
// here.
func probeGit(ctx context.Context) probeResult {
	if err := checkGitVersion(ctx); err != nil {
		return fail(err.Error(), fmt.Sprintf("install or upgrade git to %d.%d or newer and ensure it is on $PATH", minGitMajor, minGitMinor))
	}
	out, _ := exec.CommandContext(ctx, "git", "--version").Output()
	return pass(strings.TrimSpace(string(out)))
}

// --- state ------------------------------------------------------------------

// probeState checks -state exists-or-is-creatable and writable, WITHOUT
// ever writing anything: unix.Access (a mode check, not an open/write) is
// the only way to answer "writable" under doctor's read-only contract. Note
// for anyone debugging a green probeState locally as root: access(2) always
// grants W_OK to a privileged (uid 0) caller regardless of the actual mode
// bits, so this probe cannot distinguish "writable" from "read-only but I'm
// root" in that case — same caveat the daemon's own root-run deployments
// already live with.
func probeState(statePath string) probeResult {
	if statePath == "" {
		return fail("no -state given and no default cache directory is available on this platform", "pass -state explicitly")
	}
	info, err := os.Stat(statePath)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fail(fmt.Sprintf("%s exists but is not a directory", statePath), fmt.Sprintf("remove %s (or point -state elsewhere) so gauntlet can use it as a directory", statePath))
		}
		if aerr := unix.Access(statePath, unix.W_OK); aerr != nil {
			return fail(fmt.Sprintf("%s is not writable: %v", statePath, aerr), fmt.Sprintf("grant the daemon's user write access to %s", statePath))
		}
		return pass(fmt.Sprintf("%s exists and is writable", statePath))
	case os.IsNotExist(err):
		parent := nearestExistingAncestor(statePath)
		if aerr := unix.Access(parent, unix.W_OK); aerr != nil {
			return fail(fmt.Sprintf("%s does not exist and %s is not writable: %v", statePath, parent, aerr), fmt.Sprintf("create %s yourself, or grant the daemon's user write access to %s", statePath, parent))
		}
		return pass(fmt.Sprintf("%s does not exist yet but is creatable under %s", statePath, parent))
	default:
		return fail(fmt.Sprintf("stat %s: %v", statePath, err), "resolve the filesystem error reported above")
	}
}

// nearestExistingAncestor walks up from path until it finds a directory
// that already exists, for reporting "creatable under X" — MkdirAll's own
// contract (every missing ancestor gets created) means X's writability is
// what actually determines "creatable".
func nearestExistingAncestor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	for {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return abs // reached the filesystem root
		}
		abs = parent
	}
}

// --- history ----------------------------------------------------------------

// probeHistory checks a history database's on-disk schema against this
// binary's, via history.ReadSchemaVersion — a genuinely read-only open
// (sqlite "mode=ro"), never Open's migrate-in-place. Absent is fine (the
// daemon creates it on first start); older is fine (the daemon migrates it
// in place on next start); NEWER means this binary predates the schema on
// disk and cannot safely run against it.
func probeHistory(path string) probeResult {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return pass(fmt.Sprintf("%s does not exist yet; the daemon creates it on first start", path))
		}
		return fail(fmt.Sprintf("stat %s: %v", path, err), "resolve the filesystem error reported above")
	}
	dbVersion, err := history.ReadSchemaVersion(path)
	if err != nil {
		return fail(
			fmt.Sprintf("%s: %v", path, err),
			"confirm the file is a gauntlet history database, that no filesystem error (permissions, missing directory) is blocking the read, and that it isn't corrupted — a live daemon writing to it concurrently is not itself a problem (ReadSchemaVersion is immutable-mode)",
		)
	}
	switch {
	case dbVersion > history.SchemaVersion:
		return fail(
			fmt.Sprintf("%s is schema version %d, newer than this binary's %d", path, dbVersion, history.SchemaVersion),
			"this binary is older than the daemon that wrote it; upgrade gauntlet before pointing it at this state",
		)
	case dbVersion < history.SchemaVersion:
		return pass(fmt.Sprintf("%s is schema version %d; will migrate to %d on next daemon start", path, dbVersion, history.SchemaVersion))
	default:
		return pass(fmt.Sprintf("%s is schema version %d (current)", path, dbVersion))
	}
}

// --- auth -------------------------------------------------------------------

// probeAuthKeyPerms reuses ghauth.LoadPrivateKey — the exact startup check
// buildAppTokens performs (owner-only file mode, then PEM/PKCS parse) —
// rather than re-deriving the mode check here.
func probeAuthKeyPerms(cfg *config.Daemon) probeResult {
	path := cfg.GitHub.Auth.PrivateKeyFile
	if _, err := ghauth.LoadPrivateKey(path); err != nil {
		return fail(err.Error(), fmt.Sprintf("chmod 0600 %s (owner-only, no group/other access)", path))
	}
	return pass(fmt.Sprintf("%s is owner-only and parses as an RSA private key", path))
}

// probeTMPDirExec proves TMPDIR allows creating AND executing a file — a
// probe file is written, made executable, and run, then removed regardless
// of outcome — mirroring exactly what gitx's ephemeral GIT_ASKPASS helper
// needs from TMPDIR (internal/gitx/auth.go's runAuthed) to authenticate any
// app-mode git operation. A noexec-mounted TMPDIR fails silently at that
// point today; this probe catches it up front, by name.
func probeTMPDirExec(ctx context.Context) probeResult {
	return doProbeTMPDirExec(ctx, "#!/bin/sh\n")
}

// doProbeTMPDirExec is probeTMPDirExec's body, parameterized on the probe
// script's shebang line so doctor_test.go can force a deterministic exec
// failure (an interpreter that doesn't exist, which fails the same way a
// real noexec mount would — exec.Run returning an error) without needing a
// real noexec-mounted TMPDIR, which the test sandbox generally can't
// provision.
func doProbeTMPDirExec(ctx context.Context, shebang string) probeResult {
	dir, err := os.MkdirTemp("", "gauntlet-doctor-exec-")
	if err != nil {
		return fail(fmt.Sprintf("create a temp dir under TMPDIR (%s): %v", os.TempDir(), err), "ensure TMPDIR (or /tmp) exists and is writable by the daemon's user")
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(script, []byte(shebang+"exit 0\n"), 0o700); err != nil {
		return fail(fmt.Sprintf("write an exec probe under %s: %v", dir, err), "ensure TMPDIR (or /tmp) is writable by the daemon's user")
	}
	if err := exec.CommandContext(ctx, script).Run(); err != nil {
		return fail(fmt.Sprintf("execute a probe script under %s (TMPDIR=%s): %v", dir, os.TempDir(), err), "TMPDIR is likely mounted noexec; point TMPDIR at a filesystem that allows execution — gauntlet's own GIT_ASKPASS helper needs exactly this")
	}
	return pass(fmt.Sprintf("%s allows creating and executing files", os.TempDir()))
}

// probeAuthMint performs doctor's one externally visible action: a REAL
// GitHub App JWT signature plus a real installation-token exchange —
// through the SAME provider instance the remote probe's git operation
// authenticates with (doctorEnv.appTokens, built once by buildAppTokens
// exactly as main.go's run() does), so a full doctor run mints once, not
// once per probe: ghauth caches the installation token in the provider and
// the remote probe reuses it. The minted token itself never appears in
// probe output (see ghauth's package doc: a token is handed out as a
// value, never logged by that package, and this probe never prints it
// either).
func probeAuthMint(ctx context.Context, app *ghauth.App, appErr error) probeResult {
	if appErr != nil {
		return fail(fmt.Sprintf("mint: cannot build the GitHub App token provider: %v", appErr), "fix auth \"app\"'s private-key-file first (see the auth-key-perms probe above)")
	}
	if _, err := app.Token(ctx); err != nil {
		return fail(fmt.Sprintf("mint: %v", err), "confirm app-id, installation-id, api-url, and network egress to GitHub are all correct")
	}
	return pass("minted a real installation token (not shown)")
}

// probeAuthTokenEnv is static-token-mode's cheap check: the env var is set
// and non-empty, exactly what buildGHStatusChannel itself requires
// (cmd/gauntlet/channels.go) before it will authenticate a single request —
// no GitHub API call here, deliberately: doctor must not spend a caller's
// rate limit just to confirm a string is non-empty.
func probeAuthTokenEnv(cfg *config.Daemon) probeResult {
	env := cfg.GitHub.TokenEnv
	if os.Getenv(env) == "" {
		return fail(fmt.Sprintf("%s is empty or unset", env), fmt.Sprintf("export %s with a valid GitHub token before starting the daemon", env))
	}
	return pass(fmt.Sprintf("%s is set", env))
}

// --- channels -----------------------------------------------------------------

// probeSlackTokenEnv mirrors buildSlackChannel's own gating exactly
// (cmd/gauntlet/channels.go): a configured slack block (Channel != "")
// requires both cfg.Slack.AppTokenEnv (default SLACK_APP_TOKEN) and
// cfg.Slack.BotTokenEnv (default SLACK_BOT_TOKEN) to be set, checked in the
// same order buildSlackChannel checks them, before the daemon will start
// Slack at all — no Slack API call here, same rationale as
// probeAuthTokenEnv.
func probeSlackTokenEnv(cfg *config.Daemon) probeResult {
	if os.Getenv(cfg.Slack.AppTokenEnv) == "" {
		return fail(
			fmt.Sprintf("%s is empty or unset", cfg.Slack.AppTokenEnv),
			fmt.Sprintf("export %s with a valid Slack app-level token before starting the daemon", cfg.Slack.AppTokenEnv),
		)
	}
	if os.Getenv(cfg.Slack.BotTokenEnv) == "" {
		return fail(
			fmt.Sprintf("%s is empty or unset", cfg.Slack.BotTokenEnv),
			fmt.Sprintf("export %s with a valid Slack bot token before starting the daemon", cfg.Slack.BotTokenEnv),
		)
	}
	return pass(fmt.Sprintf("%s and %s are set", cfg.Slack.AppTokenEnv, cfg.Slack.BotTokenEnv))
}

// probeSummarizeTokenEnv mirrors buildSummarizer's own gating exactly
// (cmd/gauntlet/channels.go): a configured summarize block requires
// cfg.Summarize.APIKeyEnv (default ANTHROPIC_API_KEY) to be set before the
// daemon will start the summarizer at all — no Messages API call here,
// same rationale as probeAuthTokenEnv.
func probeSummarizeTokenEnv(cfg *config.Daemon) probeResult {
	env := cfg.Summarize.APIKeyEnv
	if os.Getenv(env) == "" {
		return fail(fmt.Sprintf("%s is empty or unset", env), fmt.Sprintf("export %s with a valid API key before starting the daemon", env))
	}
	return pass(fmt.Sprintf("%s is set", env))
}

// --- remote -----------------------------------------------------------------

// probeRemote performs a read-only `ls-remote` against the configured
// remote through the exact credential path the daemon uses: gitAuthOptions
// (cmd/gauntlet/auth.go) resolves to the same WithTokenSource option
// main.go's run() passes to gitx.New in app mode, or no option at all in
// static/disabled mode (ambient git auth, same as the daemon). The gitx.Repo
// itself is built fresh under a throwaway temp dir — NEVER under -state —
// and removed before returning, so this never touches (let alone creates)
// the daemon's own bare-repo clone.
func probeRemote(ctx context.Context, cfg *config.Daemon, appTokens *ghauth.App, appErr error) probeResult {
	if cfg.GitHub.Auth != nil && appErr != nil {
		return fail(fmt.Sprintf("cannot test with GitHub App credentials: %v", appErr), "fix the auth \"app\" private-key-file issue (see the auth-key-perms probe) before retesting the remote")
	}
	opts, err := gitAuthOptions(cfg, appTokens)
	if err != nil {
		return fail(err.Error(), "confirm the remote URL and the github block agree on host/owner/repo")
	}
	dir, err := os.MkdirTemp("", "gauntlet-doctor-remote-")
	if err != nil {
		return fail(fmt.Sprintf("create a scratch dir for the probe: %v", err), "ensure TMPDIR is writable")
	}
	defer os.RemoveAll(dir)
	repo, err := gitx.New(ctx, dir, cfg.Remote, opts...)
	if err != nil {
		return fail(fmt.Sprintf("prepare a scratch clone: %v", err), "confirm git is on $PATH and the remote URL is well-formed")
	}
	if _, err := repo.ListRemoteRefs(ctx, "HEAD"); err != nil {
		return fail(fmt.Sprintf("ls-remote: %v", err), "confirm the remote is reachable from this host and, if using HTTPS+PAT/App auth, that the credential is valid")
	}
	return pass(fmt.Sprintf("ls-remote %s reachable", redactCreds(cfg.Remote)))
}

// --- executors ----------------------------------------------------------------

// containerProfile is one container-kind executor profile's doctor-relevant
// shape: the label doctor prints it under (the default profile's is
// "default"; a named profile's is its own name), its runtime, and its
// image.
type containerProfile struct {
	label   string
	runtime string
	image   string
}

// containerProfiles returns every container-kind executor profile in
// config order (the default profile first, if container-kind, then
// cfg.Profiles) — never map iteration, so doctor's output order is
// deterministic run to run.
func containerProfiles(cfg *config.Daemon) []containerProfile {
	var out []containerProfile
	if cfg.Executor.Kind == "container" {
		out = append(out, containerProfile{label: "default", runtime: cfg.Executor.Runtime, image: cfg.Executor.Image})
	}
	for _, p := range cfg.Profiles {
		if p.Kind == "container" {
			out = append(out, containerProfile{label: p.Name, runtime: p.Runtime, image: p.Image})
		}
	}
	return out
}

// servicesRuntime returns the runtime the services pool will actually shell
// out to, mirroring main.go's run() mode/runtime derivation exactly: the
// local executor has no runtime of its own, so cfg.Services.Runtime
// (defaulted docker/podman by config.Daemon.applyDefaults when the executor
// is local) supplies it; under a container executor, the executor's own
// Runtime wins instead (config.Services.Runtime's field doc — validate()
// only requires it to agree when both are set, and applyDefaults leaves it
// "" otherwise).
func servicesRuntime(cfg *config.Daemon) string {
	if cfg.Executor.Kind == "container" {
		return cfg.Executor.Runtime
	}
	return cfg.Services.Runtime
}

// runtimeUsage groups containerProfiles by distinct runtime, so
// probeExecutorRuntime runs (and names) each runtime exactly once even when
// several profiles share it.
type runtimeUsage struct {
	runtime  string
	profiles []string // profile labels using this runtime, in config order
}

// runtimeUsages derives runtimeUsage groups from profiles, first-seen order
// (not map iteration), so the executor-runtime probe list is deterministic.
func runtimeUsages(profiles []containerProfile) []runtimeUsage {
	var order []string
	byRuntime := make(map[string][]string, len(profiles))
	for _, p := range profiles {
		if _, ok := byRuntime[p.runtime]; !ok {
			order = append(order, p.runtime)
		}
		byRuntime[p.runtime] = append(byRuntime[p.runtime], p.label)
	}
	out := make([]runtimeUsage, 0, len(order))
	for _, rt := range order {
		out = append(out, runtimeUsage{runtime: rt, profiles: byRuntime[rt]})
	}
	return out
}

// probeExecutorRuntime reuses executor.ProbeRuntime — the identical
// binary-on-$PATH-plus-reachable check RunCheck's own preflight runs
// (internal/executor/container.go) — so doctor and the daemon can never
// disagree on what "reachable" means. A FAIL names every profile that needs
// this runtime, not just the runtime itself, so an operator knows exactly
// what breaks.
func probeExecutorRuntime(ctx context.Context, u runtimeUsage) probeResult {
	if err := executor.ProbeRuntime(ctx, u.runtime); err != nil {
		return fail(
			fmt.Sprintf("%v (needed by executor profile(s): %s)", err, strings.Join(u.profiles, ", ")),
			fmt.Sprintf("install %s and ensure its daemon/service is running and reachable from this host", u.runtime),
		)
	}
	return pass(fmt.Sprintf("%s on $PATH and reachable (profile(s): %s)", u.runtime, strings.Join(u.profiles, ", ")))
}

// probeImagePresent checks whether image already exists in runtime's LOCAL
// image store — `<runtime> image inspect <image>`, the docker-compatible
// shape all three supported runtimes share (container.go's runtimeSpec doc:
// "flags themselves are docker-compatible across all three") — and WARNs,
// never FAILs, on any failure: an absent image is not a host problem, it
// just means the first real run pays a pull, and this never pulls to find
// out. `image inspect` failing is NOT proof the image is absent, though —
// an unreachable runtime fails the exact same way, so the WARN names both
// possibilities rather than asserting the image-absent one as fact; the
// executor-runtime probe is what actually tells reachability apart from
// absence.
func probeImagePresent(ctx context.Context, runtime, image string) probeResult {
	if err := exec.CommandContext(ctx, runtime, "image", "inspect", image).Run(); err != nil {
		return warn(fmt.Sprintf("image %s not present locally on %s, or %s is unreachable (see the executor-runtime probe above); if absent, the first run will pull it (doctor never pulls)", image, runtime, runtime))
	}
	return pass(fmt.Sprintf("image %s present locally on %s", image, runtime))
}

// --- endpoint -----------------------------------------------------------------

// probeEndpoint tries to bind the configured dashboard address and closes
// it immediately — never actually serving anything. Address-in-use is a
// WARN, not a FAIL: the overwhelmingly likely cause is that the daemon
// doctor is checking is already running against this same config.
func probeEndpoint(cfg *config.Daemon) probeResult {
	addr := cfg.Dashboard.Bind
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return warn(fmt.Sprintf("%s is already in use — likely the daemon is already running", addr))
		}
		return fail(fmt.Sprintf("cannot bind %s: %v", addr, err), "confirm the address is valid and this host can bind it (permissions, interface exists)")
	}
	_ = ln.Close()
	return pass(fmt.Sprintf("%s is free", addr))
}
