// Command gauntlet runs the merge-queue daemon: it reads a daemon config,
// opens (or creates) a local bare-repo clone of the configured remote under
// the state directory, and reconciles candidates onto their targets on a
// fixed poll interval until asked to stop.
//
// This file is wiring only — flags, config load, dependency construction,
// and the run loop. All behavior lives in the internal packages it wires
// together.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/hooks"
	"github.com/sgrankin/gauntlet/internal/obs"
	"github.com/sgrankin/gauntlet/internal/queue"
	"github.com/sgrankin/gauntlet/internal/services"
)

func main() {
	// "land", "status", "retry", "cancel", "hooks-cancel", "drain",
	// "validate", "fmt", and "version" are client-side porcelain
	// (cmd/gauntlet/land.go, cmd/gauntlet/status.go,
	// cmd/gauntlet/validate.go, cmd/gauntlet/fmt.go,
	// cmd/gauntlet/version.go): thin HTTP/git clients, pure config
	// validation ("validate"), a pure line-based whitespace normalizer over
	// internal/kdlfmt ("fmt" — see that package's doc; issue #12), or (for
	// "version") pure local info — none of them run the daemon. "doctor"
	// (cmd/gauntlet/doctor.go) is NOT pure porcelain like the rest of this
	// list: it actively probes the host and network the daemon is about to
	// run on/against (git, -state, auth, remote, executor runtimes, the
	// dashboard port) — read-only except for minting a real GitHub App
	// token in app-auth mode — but like them, it never runs the daemon loop
	// itself. Everything else is the daemon itself.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "land":
			if err := runLand(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "gauntlet land:", err)
				os.Exit(1)
			}
			return
		case "status":
			if err := runStatus(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "gauntlet status:", err)
				os.Exit(1)
			}
			return
		case "retry":
			if err := runRetry(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "gauntlet retry:", err)
				os.Exit(1)
			}
			return
		case "cancel":
			if err := runCancel(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "gauntlet cancel:", err)
				os.Exit(1)
			}
			return
		case "hooks-cancel":
			if err := runHooksCancel(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "gauntlet hooks-cancel:", err)
				os.Exit(1)
			}
			return
		case "drain":
			if err := runDrain(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "gauntlet drain:", err)
				os.Exit(1)
			}
			return
		case "validate":
			if err := runValidate(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "gauntlet validate:", err)
				os.Exit(1)
			}
			return
		case "fmt":
			if err := runFmt(os.Args[2:]); err != nil {
				// errFmtFailed means the per-file detail (a refuse-to-
				// format error, an I/O error, or -l's listing) was
				// already written by runFmtTo; nothing more to say beyond
				// the exit code. Any other error (bad flags, no files
				// given) gets the usual "gauntlet fmt: <err>" treatment.
				if !errors.Is(err, errFmtFailed) {
					fmt.Fprintln(os.Stderr, "gauntlet fmt:", err)
				}
				os.Exit(1)
			}
			return
		case "doctor":
			if err := runDoctor(os.Args[2:]); err != nil {
				// errDoctorFailed means one or more probes already printed
				// their own FAIL line; nothing more to say beyond the exit
				// code. Any other error (bad flags, missing -config) gets
				// the usual "gauntlet doctor: <err>" treatment.
				if !errors.Is(err, errDoctorFailed) {
					fmt.Fprintln(os.Stderr, "gauntlet doctor:", err)
				}
				os.Exit(1)
			}
			return
		case "version":
			fmt.Println(versionString())
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gauntlet:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultState := ""
	if dir, err := os.UserCacheDir(); err == nil {
		defaultState = filepath.Join(dir, "gauntlet")
	}

	configPath := flag.String("config", "", "path to the daemon config (gauntlet.kdl) [required]")
	statePath := flag.String("state", defaultState, "directory for the daemon's bare repo clone(s)")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return nil
	}

	if *configPath == "" {
		return errors.New("-config is required (path to gauntlet.kdl)")
	}
	if *statePath == "" {
		return errors.New("-state is required: no default cache directory is available on this platform")
	}

	cfg, err := config.LoadDaemon(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Fail loudly, before touching any git plumbing, if the git on $PATH is
	// missing or too old for `git merge-tree --write-tree`, which the
	// trial-merge mechanism rests on. context.Background(): the root ctx
	// (below) doesn't exist yet at this point in startup, and a hanging
	// `git --version` should fail this check on its own rather than being
	// tied to a cancellation scope that isn't wired up yet.
	if err := checkGitVersion(context.Background()); err != nil {
		return fmt.Errorf("git version check: %w", err)
	}

	// Acquire an exclusive advisory lock on -state before touching anything
	// else in it: every sweep below (trials, hooks, and scratch) depends on
	// being the only daemon touching this state directory. AcquireLock
	// enforces that precondition: a second gauntlet process started
	// against this same -state fails fast right here, before it can sweep
	// the first, still-running process's in-flight trial/hook/scratch
	// exports out from under it. Held for the rest of the process's life
	// (deferred Close).
	lock, err := AcquireLock(*statePath)
	if err != nil {
		return err
	}
	defer lock.Close()

	// The root ctx is cancelled only on a FORCE shutdown (issue #8):
	// normal exit (defer), a second signal, a drain deadline, or — in
	// "kill" shutdown mode — the very first signal. A graceful first
	// SIGTERM does NOT cancel it; it requests a drain and the daemon exits
	// cleanly once the in-flight set empties. Signal wiring is installed
	// below, after the Daemon exists.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// obs.InstallProvider must run before queue.New: queue.New's Daemon
	// grabs its Tracer() once at construction, so a provider installed
	// afterward would leave every span from that Daemon un-exported.
	shutdownOTLP, err := obs.InstallProvider(ctx, cfg.OTLP.Endpoint, cfg.OTLP.Insecure)
	if err != nil {
		return fmt.Errorf("otlp: install provider: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownOTLP(sctx)
	}()

	// obs.InstallMeterProvider (issue #14) is the metrics analogue of
	// InstallProvider just above: same otlp config block, same ordering
	// caution — installed before queue.New so nothing that captures an
	// instrument early is left bound to a stale no-op default.
	shutdownOTLPMetrics, err := obs.InstallMeterProvider(ctx, cfg.OTLP.Endpoint, cfg.OTLP.Insecure)
	if err != nil {
		return fmt.Errorf("otlp: install meter provider: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownOTLPMetrics(sctx)
	}()

	// Key the bare repo's directory off the remote URL so a future
	// multi-remote daemon (or a config that just changes remotes) never
	// collides with a stale clone left under the same state dir.
	//
	// Absolute from the start: this path is also handed to every check as
	// GAUNTLET_GIT_DIR (buildExecutor below), where checks run with cwd set
	// to the trial tree — and the container executor needs an absolute -v
	// source (a relative one is a named volume, not a bind mount).
	repoDir, err := filepath.Abs(filepath.Join(*statePath, "repos", remoteKey(cfg.Remote)))
	if err != nil {
		return fmt.Errorf("resolve repo dir under %s: %w", *statePath, err)
	}
	// App-mode GitHub credentials (issue #6): one refreshable provider,
	// built before the repo so git fetch/push and the status channel
	// share it. nil in static-token/disabled mode.
	appTokens, err := buildAppTokens(cfg)
	if err != nil {
		return err
	}
	gitOpts, err := gitAuthOptions(cfg, appTokens)
	if err != nil {
		return err
	}
	repo, err := gitx.New(ctx, repoDir, cfg.Remote, gitOpts...)
	if err != nil {
		return fmt.Errorf("open repo at %s: %w", repoDir, err)
	}

	// GC pins (refs/gauntlet/pin/*) anchor in-flight trial merges against
	// git maintenance for exactly one run's lifetime. A crash strands them;
	// every interrupted run is re-derived from refs and re-run from scratch
	// (Invariant 4), so a stale pin never protects anything this process
	// still needs — swept unconditionally, same rationale (and same lock
	// guarantee) as the trials dir below.
	if _, err := repo.SweepPins(ctx); err != nil {
		return fmt.Errorf("sweep gc pins: %w", err)
	}

	// Trial refs (issue #7) live on the REMOTE and their retention
	// schedule is in-memory, so a crash orphans them; sweep the namespace
	// at startup, same rationale as the pins. Only when the feature is on.
	if cfg.GitHub.TrialRefPrefix != "" {
		// Best-effort, NOT fatal: an orphaned trial ref anchors only a
		// synthetic merge, never correctness, so a delete that fails
		// (transient network, a server-side ref rule) must not stop the
		// queue from starting — the reaper and the next boot try again.
		if _, err := repo.SweepTrialRefs(ctx, cfg.GitHub.TrialRefPrefix); err != nil {
			fmt.Fprintf(os.Stderr, "gauntlet: sweep trial refs: %v\n", err)
		}
	}

	// The trials dir only ever holds ephemeral trial-tree exports for the
	// run currently in flight, never anything that needs to survive a
	// restart, so sweeping it at startup cleans up anything orphaned by a
	// prior crash. This is safe unconditionally because AcquireLock above
	// guarantees no other gauntlet daemon can be using -state concurrently.
	trialsDir := filepath.Join(*statePath, "trials")
	if err := sweepAndRecreate(trialsDir); err != nil {
		return fmt.Errorf("sweep trials dir: %w", err)
	}

	// Executor scratch dirs (LocalExecutor's gauntlet-check-* and
	// ContainerExecutor's gauntlet-container-* result dirs) are rooted
	// under -state/scratch (threaded into buildExecutor below) rather than
	// the OS temp dir, specifically so they fall under this same startup
	// sweep — exactly like trialsDir above, and safe for the same reason
	// (the lock above).
	scratchDir := filepath.Join(*statePath, "scratch")
	if err := sweepAndRecreate(scratchDir); err != nil {
		return fmt.Errorf("sweep scratch dir: %w", err)
	}

	// Unlike trials/, logsDir holds durable state (log files meant to
	// survive restarts, up to cfg.LogRetention — see DESIGN.md's decision
	// ledger, "Full per-check log files") — never swept unconditionally.
	// Wired into queue.Config.LogDir below regardless of which optional
	// sections (history, dashboard, ...) are configured, so full logs are
	// always captured; a startup prune sweep here catches anything past
	// retention from before this process started, and startLogPruner
	// repeats it periodically for the rest of this process's lifetime.
	logsDir := filepath.Join(*statePath, "logs")
	if err := pruneLogFiles(logsDir, time.Now().Add(-cfg.LogRetention)); err != nil {
		return fmt.Errorf("sweep logs dir %s: %w", logsDir, err)
	}

	// A short, stable hash of this daemon's own absolute -state path,
	// namespacing its container names (executor.Params.Token) distinctly
	// from any sibling gauntlet daemon on the same box pointed at a
	// different -state dir. See stateToken's doc for why AcquireLock's
	// flock alone doesn't cover this.
	token, err := stateToken(*statePath)
	if err != nil {
		return fmt.Errorf("derive state token: %w", err)
	}

	// Shared services: pool construction, adoption, and (below, once wg
	// exists) the reaper are all gated on the daemon capability block —
	// see docs/design/services.md ("The model: a cache entry, not a
	// supervised unit") for why the allow list is what permits any service
	// to run at all. len(cfg.Services.Allow)==0 leaves pool nil, and
	// queue.Config.Services stays nil too (byte-identical behavior for
	// every daemon that never opts in).
	var pool *services.Pool
	if len(cfg.Services.Allow) > 0 {
		// Mode/runtime derivation: the container executor's own runtime
		// wins (see docs/design/services.md, "Reachability mode", for the
		// runtime-selection rule); the local executor has no runtime of
		// its own, so cfg.Services.Runtime (validated docker/podman by
		// config.Daemon.validate) supplies it instead.
		mode := services.ModePublish
		runtime := cfg.Services.Runtime
		if cfg.Executor.Kind == "container" {
			mode = services.ModeNetwork
			runtime = cfg.Executor.Runtime
		}
		if mode == services.ModeNetwork && runtime == "container" {
			// NOTE: Apple's container CLI is deliberately unsupported here
			// — it has no docker-style shared user network for a service
			// and a sibling check container to reach each other over. See
			// docs/design/services.md ("Reachability mode").
			return fmt.Errorf("services require docker or podman; Apple's container CLI lacks the shared container network services need")
		}

		svcStateDir := filepath.Join(*statePath, "services")
		if err := os.MkdirAll(svcStateDir, 0o755); err != nil {
			return fmt.Errorf("create services state dir: %w", err)
		}
		pool, err = services.New(services.Config{
			Remote:       cfg.Remote,
			Token:        token,
			Mode:         mode,
			Runtime:      runtime,
			StateDir:     svcStateDir,
			MaxInstances: cfg.Services.MaxInstances,
			Now:          time.Now,
		})
		if err != nil {
			return fmt.Errorf("build services pool: %w", err)
		}
		// Best-effort, like sweepContainerOrphans below: a failed adopt
		// costs a cold pool (slower first runs), never a wrong one.
		if err := pool.Adopt(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "gauntlet: services: adopt: %v\n", err)
		}
	}

	// Config-named operator secret env-var names (issue #13 Gap 1): every
	// local executor profile strips these from a candidate-code job's
	// environment (checks, image builds, receipt producers) so a check
	// command can never observe the daemon's own GitHub/Slack/summarize
	// credentials — see config.Daemon.SecretEnvNames's doc for exactly
	// which names qualify and why, and docs/checks.md for the operator-
	// facing contract.
	ex, err := buildExecutor(cfg, scratchDir, token, repoDir, cfg.SecretEnvNames())
	if err != nil {
		return fmt.Errorf("build executor: %w", err)
	}

	// The daemon-wide execution cap (top-level `max-executions`): one
	// core.Slots instance shared by the queue's checks and the hooks
	// runner, so every bounded execution on the host draws from a single
	// budget. nil (unset) means unlimited — the pre-cap behavior.
	var slots *core.Slots
	if cfg.MaxExecutions > 0 {
		slots = core.NewSlots(cfg.MaxExecutions)
	}

	// Sweep containers orphaned by a prior gauntlet process that crashed
	// mid-check, before its own --rm cleanup ran. Only attempted when a
	// container executor is actually configured. AcquireLock above
	// guarantees no other gauntlet daemon can be using THIS -state dir
	// concurrently, but that alone is not sufficient here: container names
	// live in a host-global namespace shared by every gauntlet daemon on
	// the box, regardless of -state dir, so a live sibling daemon pointed
	// at a *different* -state dir could otherwise have its in-flight
	// containers mistaken for orphans. token (above) closes that gap —
	// sweepContainerOrphans only ever matches this daemon's own
	// "gauntlet-<token>-" prefix. Tolerant of the runtime binary being
	// entirely absent (logs and continues — see sweepContainerOrphans'
	// doc).
	// With named profiles, several distinct runtimes may exist (a docker
	// profile beside a podman one); sweep each exactly once.
	swept := make(map[string]bool, 1)
	for _, e := range append([]config.Executor{cfg.Executor}, cfg.Profiles...) {
		if e.Kind != "container" {
			continue
		}
		runtime := e.Runtime
		if runtime == "" {
			runtime = "container"
		}
		if swept[runtime] {
			continue
		}
		swept[runtime] = true
		sweepContainerOrphans(ctx, runtime, token, os.Stderr)
	}

	// Channels: log always first (it's the one output every deployment
	// gets, config or no config), then the optional channels in
	// config-field order.
	chans := []core.Channel{channel.NewLogChannel(os.Stderr)}

	// LOAD-BEARING ORDER: store must be registered in chans before the
	// hooks Runner hr is (further down, where hr is appended after
	// buildHooksRunner). hook_runs.run_id REFERENCES runs(run_id),
	// FK-enforced (store.go's DSN), so a writeHookStarted/writeHookSkipped
	// insert only succeeds once the landing's runs row already exists. That
	// row is written by history.Store.Emit — a synchronous sqlite write —
	// on EventLanded; queue.Daemon.emit (internal/queue/daemon.go) fans
	// EventLanded out to d.chans strictly in registration order, so
	// history's write completing before hr even dequeues the landing (its
	// own Emit just enqueues, asynchronously) depends entirely on history
	// sitting earlier in this slice than hr. Reorder them and every
	// owed/skipped hook_runs write FK-violates (logged, not silently
	// dropped, but still a correctness regression for crash-discoverability
	// of hook runs). Don't move hr's append above this.
	store, err := buildHistoryStore(cfg)
	if err != nil {
		return fmt.Errorf("build history store: %w", err)
	}
	if store != nil {
		defer store.Close()
		chans = append(chans, store)
	}

	// The dashboard's retry channel is constructed here, ahead of queue.New,
	// even though the dashboard's http.Handler (built in startDashboard,
	// below) needs d.Snapshot, which doesn't exist until queue.New returns.
	// The channel itself doesn't need d — only the handler does — so it can
	// be registered in chans now and wired into the handler once d exists.
	var dashCh *dashboard.Channel
	if cfg.Dashboard.Bind != "" {
		dashCh = dashboard.NewChannel()
		chans = append(chans, dashCh)
	}

	ghStatus, err := buildGHStatusChannel(cfg, appTokens)
	if err != nil {
		return fmt.Errorf("build github status channel: %w", err)
	}
	if ghStatus != nil {
		chans = append(chans, ghStatus)
	}

	// wg tracks every background goroutine started here (Slack, hooks, the
	// dashboard, the depth sampler, the log pruner) that may still be
	// querying or writing store (or, for the pruner, logsDir) after d.Run
	// returns on ctx cancellation. Declared here — ahead of the Slack/hooks
	// goroutines below, not just startDashboard/startDepthSampler/
	// startLogPruner further down — so sc.Run and hr.Run are joined by the
	// same wg.Wait() before store.Close() runs (deferred above): both can
	// still be mid-write (a hook history row, a Slack API call) when ctx is
	// cancelled, and neither was previously counted, so shutdown could race
	// ahead of them and store.Close() out from under an in-flight write.
	var wg sync.WaitGroup

	// Services reaper: a dedicated 30s ticker, no config knob today,
	// destroying instances idle past their IdleTTL. No-op until
	// pool.ArmReaper is called (queue.Daemon calls it once, after the first
	// full ReconcileOnce pass), so this can start immediately without
	// racing boot adoption. Joined by wg like every other background
	// goroutine here.
	if pool != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					pool.Reap(ctx)
				}
			}
		}()
	}

	sc, err := buildSlackChannel(cfg)
	if err != nil {
		return fmt.Errorf("build slack channel: %w", err)
	}
	if sc != nil {
		chans = append(chans, sc)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sc.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "gauntlet: slack: %v\n", err)
			}
		}()
	}

	// Post-land hooks (internal/hooks, DESIGN.md's decision ledger
	// "Deployments as post-land hooks"): notifyChans is a snapshot of
	// every channel built so far, so the hooks runner's own outbound
	// events (EventHookFinished) fan out to the same channels a check's
	// events do, minus the runner itself — no self-feedback. It is only
	// appended to chans (so queue.Daemon's own fan-out reaches it) after
	// the snapshot is taken.
	notifyChans := append([]core.Channel(nil), chans...)
	hooksDir := filepath.Join(*statePath, "hooks")
	hr := buildHooksRunner(cfg, repo, ex, slots, hooksDir, logsDir, func(ctx context.Context, ev core.Event) {
		for _, ch := range notifyChans {
			_ = ch.Emit(ctx, ev)
		}
	})
	// hookCancel is hr.CancelCurrent, threaded into the dashboard/MCP hook-
	// cancel surface (Feature 1) nil-safely when hooks aren't configured at
	// all — mirrors how Retry/mergeBody above are only non-nil when their
	// own optional feature is wired up.
	var hookCancel func(target string) bool
	if hr != nil {
		hookCancel = hr.CancelCurrent
		// Mirrors trialsDir above (F2): hooksDir only ever holds
		// ephemeral per-landing exports for whatever hook is currently
		// running, never anything that needs to survive a restart, so
		// sweeping it at startup cleans up anything orphaned by a prior
		// crash — safe unconditionally now that AcquireLock (S2) above
		// guarantees no other gauntlet daemon can be using -state
		// concurrently.
		if err := sweepAndRecreate(hooksDir); err != nil {
			return fmt.Errorf("sweep hooks dir: %w", err)
		}
		// hr must be appended after store above, never before — see the
		// LOAD-BEARING ORDER comment at store's own append site (S1).
		chans = append(chans, hr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hr.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "gauntlet: hooks: %v\n", err)
			}
		}()
	}

	// hookSnapshot threads hr's live-hook-state accessor into the
	// dashboard/MCP surfaces, nil-safely, mirroring hookCancel above
	// exactly — nil when hooks aren't configured, in which case both
	// surfaces simply omit live hook state.
	var hookSnapshot func(target string) (hooks.LiveState, bool)
	if hr != nil {
		hookSnapshot = hr.Snapshot
	}

	// servicesSnapshot threads the shared-services pool's tuning surface
	// (design §10) into the dashboard/MCP/CLI surfaces, nil-safely mirroring
	// hookSnapshot exactly: nil when no services are configured for this
	// daemon (pool == nil, above), in which case every surface simply omits
	// the pool entirely rather than rendering an empty one.
	var servicesSnapshot func() services.PoolStatus
	if pool != nil {
		servicesSnapshot = pool.Snapshot
	}

	// The summarizer's own Params.Timeout already bounds each Messages API
	// call, but Config.MergeBody's contract (internal/queue/daemon.go)
	// puts the timeout decision at the caller, not in queue: this closure
	// is that caller, wrapping ctx with cfg.Summarize.Timeout before every
	// call so a hung summarizer can never wedge the reconcile loop.
	sum, err := buildSummarizer(cfg, repo)
	if err != nil {
		return fmt.Errorf("build summarizer: %w", err)
	}
	var mergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string
	if sum != nil {
		timeout := cfg.Summarize.Timeout
		mergeBody = func(ctx context.Context, cand core.Candidate, baseOID string) string {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return sum.MergeBody(cctx, cand, baseOID)
		}
	}
	// SeedParks (Feature 2, "park persistence across restarts") is only
	// wired up when history is enabled: with no store there is nothing to
	// seed from, and a nil SeedParks is queue.Daemon's own documented
	// "disabled" state (byte-identical startup behavior).
	var seedParks func(target string) []queue.ParkSeed
	if store != nil {
		seedParks = buildSeedParks(store)
	}

	// KnownExecutorProfile/ImageCapableProfile: the queue rejects a spec
	// naming an undefined profile, or an `image` on a non-container
	// profile, before any of its commands start. Derived by
	// executorPredicates (executor.go), shared with `gauntlet validate`'s
	// cross-check mode so the two gates can't drift apart.
	known, imageCapable := executorPredicates(cfg)

	qcfg := queue.Config{
		Targets:              cfg.Targets,
		CheckSpec:            cfg.CheckSpec,
		Committer:            cfg.Committer,
		MergeMessage:         cfg.MergeMsg,
		MergeBody:            mergeBody,
		WorkDir:              trialsDir,
		LogDir:               logsDir,
		SeedParks:            seedParks,
		Slots:                slots,
		HistoryMtimes:        cfg.Export.Mtimes == "history",
		KnownExecutorProfile: known,
		ImageCapableProfile:  imageCapable,
		// AutoRetryErrors is a *bool defaulted true in config.applyDefaults
		// (absent-vs-explicit-false needs the pointer); the queue takes the
		// resolved value.
		AutoRetryErrors: *cfg.AutoRetryErrors,

		// Trial-ref publication (issue #7): the resolved prefix being
		// non-empty is the enable signal, mirroring the github block.
		TrialRefs:         cfg.GitHub.TrialRefPrefix != "",
		TrialRefPrefix:    cfg.GitHub.TrialRefPrefix,
		TrialRefRetention: cfg.GitHub.TrialRefRetention,

		// Receipt-notes policy (issue #13): config.ReceiptNotes IS
		// queue.Config.ReceiptNotes's type (the queue reuses config's
		// struct rather than a parallel core type — see that field's doc),
		// so this is a direct assignment; nil carries straight through as
		// "disabled".
		ReceiptNotes: cfg.GitHub.ReceiptNotes,
	}
	// pool is a *services.Pool; assigning it into the queue.ServicePool
	// interface field unconditionally (even when nil) would leave a
	// non-nil interface holding a nil pointer — reconcile.go's own `d.cfg.
	// Services == nil` gate (byte-identical current behavior for a daemon
	// with no services block) depends on the field being a genuine nil
	// interface in that case, so this only assigns when pool is real.
	if pool != nil {
		qcfg.Services = pool
	}
	d, err := queue.New(repo, ex, chans, qcfg, nil)
	if err != nil {
		return fmt.Errorf("init queue: %w", err)
	}

	// Daemon gauges (issue #14): queue depth per target, execution-slot
	// occupancy, and runs in flight, registered once against the meter
	// obs.InstallMeterProvider above installed — no polling goroutine of
	// our own, the SDK (or the no-op meter, when otlp is disabled) samples
	// these closures at its own collection cadence. depth and runsInFlight
	// both read d.Snapshot(), the same published, point-in-time view the
	// history depth sampler (startDepthSampler, below) already reads —
	// this adds no new sampling path, just a second reader of the existing
	// one. slots is the same *core.Slots the queue's checks and the hooks
	// runner already share; nil (unlimited) reports ok=false, so the gauge
	// simply observes nothing rather than a misleading zero.
	if _, err := obs.RegisterGauges(
		func() []obs.QueueDepth {
			snap := d.Snapshot()
			if snap == nil {
				return nil
			}
			depths := make([]obs.QueueDepth, len(snap.Targets))
			for i, ts := range snap.Targets {
				depths[i] = obs.QueueDepth{Target: ts.Name, Waiting: len(ts.Waiting), InFlight: len(ts.Pipeline), Parked: len(ts.Parked)}
			}
			return depths
		},
		func() (n int, ok bool) {
			if slots == nil {
				return 0, false
			}
			return slots.InUse(), true
		},
		func() int {
			snap := d.Snapshot()
			if snap == nil {
				return 0
			}
			return snap.ActiveRuns
		},
	); err != nil {
		return fmt.Errorf("otlp: register gauges: %w", err)
	}

	// wg additionally covers the dashboard's goroutines (Shutdown watcher +
	// ListenAndServe), the depth sampler, and the log pruner, all of which
	// may still be querying or writing store (or, for the pruner, logsDir)
	// after d.Run returns on ctx cancellation. Waiting on wg before
	// store.Close() runs (deferred above) keeps that Close from racing any
	// of them: Slack/hooks/sampler/pruner exit, the dashboard's graceful
	// Shutdown completes, then — only then — the store closes.
	// Graceful-drain coordinator (issue #8): beginDrain stops the queue
	// admitting new work (d.Drain) and, when given a deadline, arms a timer
	// that forces the immediate-kill path (cancel) at that instant. Shared
	// by the shutdown signal and POST /api/v1/drain, idempotent, and only
	// ever shortens the effective deadline — never resumes admission.
	var drainMu sync.Mutex
	var drainTimer *time.Timer
	var armedDeadline time.Time
	beginDrain := func(deadline time.Time) {
		drainMu.Lock()
		defer drainMu.Unlock()
		d.Drain(deadline)
		if deadline.IsZero() {
			return
		}
		// Arm — or RE-arm to an earlier instant — the force timer, so a
		// second drain request that shortens the deadline actually forces
		// sooner (the "only ever shortens" contract; the queue's advisory
		// deadline alone shortening would otherwise make status lie about
		// when force happens). Never lengthens: a later deadline is ignored.
		if drainTimer == nil || deadline.Before(armedDeadline) {
			if drainTimer != nil {
				drainTimer.Stop()
			}
			armedDeadline = deadline
			drainTimer = time.AfterFunc(time.Until(deadline), cancel)
		}
	}
	defer func() {
		drainMu.Lock()
		if drainTimer != nil {
			drainTimer.Stop()
		}
		drainMu.Unlock()
	}()

	// Signal wiring. "kill" shutdown mode restores the legacy behavior
	// (first signal cancels everything); "drain" (the default) makes the
	// first signal begin a graceful drain and the second force the kill.
	// Signals are a COMPLETE drain interface on their own, so this works
	// with no HTTP admin surface configured.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
		case <-ctx.Done():
			return
		}
		if cfg.Shutdown == "kill" {
			cancel()
			return
		}
		beginDrain(time.Time{}) // no queue deadline from a signal; systemd TimeoutStopSec is the outer bound
		select {
		case <-sigCh:
			cancel() // second signal forces
		case <-ctx.Done():
		}
	}()

	startDashboard(ctx, cfg, d.Snapshot, store, dashCh, logsDir, hookCancel, hookSnapshot, servicesSnapshot, beginDrain, &wg)
	if store != nil {
		startDepthSampler(ctx, cfg, d.Snapshot, store, &wg)
	}
	startLogPruner(ctx, logsDir, cfg.LogRetention, &wg)

	ticker := time.NewTicker(cfg.Poll)
	defer ticker.Stop()

	runErr := d.Run(ctx, ticker.C)

	// A graceful drain (Run returned cleanly, not forced) finishes the
	// hook backlog before teardown: the queue has stopped landing, so the
	// entire already-queued backlog is bounded, and hooks have no
	// post-restart replay — dropping them would be silent permanent loss
	// (issue #8). A force (ctx already cancelled) skips this: hr.Run
	// already returned on ctx.Done, today's crash-equivalent behavior.
	if runErr == nil && ctx.Err() == nil && hr != nil {
		hr.Drain(ctx)
	}
	cancel() // stop the remaining background goroutines (Slack, dashboard, sampler, pruner)
	wg.Wait()

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return fmt.Errorf("run: %w", runErr)
	}
	return nil
}

// remoteKey derives a stable, filesystem-safe directory name for remote's
// bare-repo clone, so distinct remotes' state never collides on disk.
func remoteKey(remote string) string {
	sum := sha256.Sum256([]byte(remote))
	return hex.EncodeToString(sum[:8])
}

// stateToken derives a short, stable token (the first 8 hex chars of a
// sha256 of the absolute -state directory path) that namespaces this
// daemon's container names. The "gauntlet-" container-name prefix is
// host-global — identical for every gauntlet process on the box — while
// AcquireLock's flock only guards this process's own -state dir. Without a
// further namespace, a daemon restarting against a different -state dir
// could `rm -f` a live sibling daemon's in-flight containers during its
// startup orphan sweep; two daemons on different -state dirs get distinct
// tokens, so each one's sweep only ever matches its own containers.
// Threaded into both executor.Params.Token (which names containers) and
// sweepContainerOrphans (which filters on the same prefix).
func stateToken(stateDir string) (string, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve absolute state dir: %w", err)
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:4]), nil
}
