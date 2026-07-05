// Command gauntlet runs the merge-queue daemon: it reads a daemon config,
// opens (or creates) a local bare-repo clone of the configured remote under
// the state directory, and reconciles candidates onto their targets on a
// fixed poll interval until asked to stop.
//
// This file is wiring only — flags, config load, dependency construction,
// and the run loop — per docs/plans/phase1.md §1/§6 (work chunk C8). All
// behavior lives in the internal packages it wires together.
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
)

func main() {
	// "land", "status", "retry", "cancel", "hooks-cancel", and "version" are
	// client-side porcelain (cmd/gauntlet/land.go, cmd/gauntlet/status.go,
	// cmd/gauntlet/version.go): thin HTTP/git clients (or, for "version",
	// pure local info) that don't run the daemon. Everything else is the
	// daemon itself.
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

	// F3 (docs/plans/phase23.md §10): fail loudly, before touching any git
	// plumbing, if the git on $PATH is missing or too old for `git
	// merge-tree --write-tree`, which the trial-merge mechanism rests on.
	if err := checkGitVersion(); err != nil {
		return fmt.Errorf("git version check: %w", err)
	}

	// S2 (phase-6 audit synthesis, lifecycle #1): acquire an exclusive
	// advisory lock on -state before touching anything else in it — every
	// sweep below (trials, hooks, and, as of S16, scratch) used to simply
	// assert "always safe" in a comment with nothing enforcing the
	// single-daemon-per-state-dir precondition that assertion depended on.
	// AcquireLock turns that assumption into an actual, enforced
	// precondition: a second gauntlet process started against this same
	// -state now fails fast right here, before it can sweep the first,
	// still-running process's in-flight trial/hook/scratch exports out from
	// under it. Held for the rest of the process's life (deferred Close).
	lock, err := AcquireLock(*statePath)
	if err != nil {
		return err
	}
	defer lock.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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

	// Key the bare repo's directory off the remote URL so a future
	// multi-remote daemon (or a config that just changes remotes) never
	// collides with a stale clone left under the same state dir.
	repoDir := filepath.Join(*statePath, "repos", remoteKey(cfg.Remote))
	repo, err := gitx.New(ctx, repoDir, cfg.Remote)
	if err != nil {
		return fmt.Errorf("open repo at %s: %w", repoDir, err)
	}

	// F2 (docs/plans/phase23.md §10): the trials dir only ever holds
	// ephemeral trial-tree exports for the run currently in flight, never
	// anything that needs to survive a restart, so sweeping it at startup
	// cleans up anything orphaned by a prior crash. This is safe
	// unconditionally now that AcquireLock (S2) above guarantees no other
	// gauntlet daemon can be using -state concurrently — a claim this
	// comment used to just assert.
	trialsDir := filepath.Join(*statePath, "trials")
	if err := sweepAndRecreate(trialsDir); err != nil {
		return fmt.Errorf("sweep trials dir: %w", err)
	}

	// S16 (phase-6 audit synthesis, lifecycle #5): executor scratch dirs
	// (LocalExecutor's gauntlet-check-* and ContainerExecutor's
	// gauntlet-container-* result dirs) used to default to the OS temp
	// dir, escaping this sweep and every other one entirely. Rooting them
	// under -state/scratch (threaded into buildExecutor below) and
	// sweeping it here at startup — exactly like trialsDir above, and safe
	// for the same reason (the lock above) — closes that gap.
	scratchDir := filepath.Join(*statePath, "scratch")
	if err := sweepAndRecreate(scratchDir); err != nil {
		return fmt.Errorf("sweep scratch dir: %w", err)
	}

	// F-b (DESIGN.md "Full per-check log files"): unlike trials/, logsDir
	// holds durable state (log files meant to survive restarts, up to
	// cfg.LogRetention) — never swept unconditionally. Wired into
	// queue.Config.LogDir below regardless of which optional sections
	// (history, dashboard, ...) are configured, so full logs are always
	// captured; a startup prune sweep here catches anything past retention
	// from before this process started, and startLogPruner repeats it
	// periodically for the rest of this process's lifetime.
	logsDir := filepath.Join(*statePath, "logs")
	if err := pruneLogFiles(logsDir, time.Now().Add(-cfg.LogRetention)); err != nil {
		return fmt.Errorf("sweep logs dir %s: %w", logsDir, err)
	}

	ex, err := buildExecutor(cfg, scratchDir)
	if err != nil {
		return fmt.Errorf("build executor: %w", err)
	}

	// S16 (phase-6 audit synthesis): sweep containers orphaned by a prior
	// gauntlet process that crashed mid-check, before its own --rm cleanup
	// ran. Only attempted when a container executor is actually configured,
	// and only safe now that AcquireLock (S2) above guarantees no live
	// sibling daemon's own in-flight containers could be mistaken for
	// orphans. Tolerant of the runtime binary being entirely absent (logs
	// and continues — see sweepContainerOrphans' doc).
	//
	// CAUTION: this phase's own verification only ran sweepContainerOrphans
	// against a fake runtime script (sweep_test.go), never a real
	// container runtime — the demo box runs Apple `container` on macOS,
	// where behavior against the actual CLI (in particular whether
	// `container ps --filter name=... --format {{.Names}}` matches
	// docker/podman's output shape) has not been live-verified. Verify on
	// the demo box before relying on this in that environment.
	if cfg.Executor.Kind == "container" {
		runtime := cfg.Executor.Runtime
		if runtime == "" {
			runtime = "container"
		}
		sweepContainerOrphans(ctx, runtime, os.Stderr)
	}

	// Channels: log always first (it's the one output every deployment
	// gets, config or no config), then the optional phase-2/3 channels in
	// config-field order.
	chans := []core.Channel{channel.NewLogChannel(os.Stderr)}

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

	ghStatus, err := buildGHStatusChannel(cfg)
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
	hr := buildHooksRunner(cfg, repo, ex, hooksDir, logsDir, func(ctx context.Context, ev core.Event) {
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
		chans = append(chans, hr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hr.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "gauntlet: hooks: %v\n", err)
			}
		}()
	}

	// hookSnapshot is the wiring half of S5 (phase-6 audit synthesis):
	// threads hr's live-hook-state accessor into the dashboard/MCP
	// surfaces, nil-safely, mirroring hookCancel above exactly — nil when
	// hooks aren't configured, in which case both surfaces simply omit
	// live hook state.
	var hookSnapshot func(target string) (hooks.LiveState, bool)
	if hr != nil {
		hookSnapshot = hr.Snapshot
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

	d, err := queue.New(repo, ex, chans, queue.Config{
		Targets:      cfg.Targets,
		CheckSpec:    cfg.CheckSpec,
		Committer:    cfg.Committer,
		MergeMessage: cfg.MergeMsg,
		MergeBody:    mergeBody,
		WorkDir:      trialsDir,
		LogDir:       logsDir,
		SeedParks:    seedParks,
	}, nil)
	if err != nil {
		return fmt.Errorf("init queue: %w", err)
	}

	// wg additionally covers the dashboard's goroutines (Shutdown watcher +
	// ListenAndServe), the depth sampler, and the log pruner, all of which
	// may still be querying or writing store (or, for the pruner, logsDir)
	// after d.Run returns on ctx cancellation. Waiting on wg before
	// store.Close() runs (deferred above) keeps that Close from racing any
	// of them (cmd wiring review, docs/plans/phase23.md): Slack/hooks/
	// sampler/pruner exit, the dashboard's graceful Shutdown completes,
	// then — only then — the store closes.
	//
	startDashboard(ctx, cfg, d.Snapshot, store, dashCh, logsDir, hookCancel, hookSnapshot, &wg)
	if store != nil {
		startDepthSampler(ctx, cfg, d.Snapshot, store, &wg)
	}
	startLogPruner(ctx, logsDir, cfg.LogRetention, &wg)

	ticker := time.NewTicker(cfg.Poll)
	defer ticker.Stop()

	runErr := d.Run(ctx, ticker.C)
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
