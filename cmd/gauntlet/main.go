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
	"github.com/sgrankin/gauntlet/internal/obs"
	"github.com/sgrankin/gauntlet/internal/queue"
)

func main() {
	// "land", "status", "retry", and "version" are client-side porcelain
	// (cmd/gauntlet/land.go, cmd/gauntlet/status.go, cmd/gauntlet/version.go):
	// thin HTTP/git clients (or, for "version", pure local info) that don't
	// run the daemon. Everything else is the daemon itself.
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
	// is always safe and cleans up anything orphaned by a prior crash.
	trialsDir := filepath.Join(*statePath, "trials")
	if err := os.RemoveAll(trialsDir); err != nil {
		return fmt.Errorf("sweep trials dir %s: %w", trialsDir, err)
	}
	if err := os.MkdirAll(trialsDir, 0o755); err != nil {
		return fmt.Errorf("create trials dir %s: %w", trialsDir, err)
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

	ex, err := buildExecutor(cfg)
	if err != nil {
		return fmt.Errorf("build executor: %w", err)
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

	sc, err := buildSlackChannel(cfg)
	if err != nil {
		return fmt.Errorf("build slack channel: %w", err)
	}
	if sc != nil {
		chans = append(chans, sc)
		go func() {
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
	if hr := buildHooksRunner(cfg, repo, ex, hooksDir, logsDir, func(ctx context.Context, ev core.Event) {
		for _, ch := range notifyChans {
			_ = ch.Emit(ctx, ev)
		}
	}); hr != nil {
		// Mirrors trialsDir above (F2): hooksDir only ever holds
		// ephemeral per-landing exports for whatever hook is currently
		// running, never anything that needs to survive a restart, so
		// sweeping it at startup is always safe.
		if err := os.RemoveAll(hooksDir); err != nil {
			return fmt.Errorf("sweep hooks dir %s: %w", hooksDir, err)
		}
		if err := os.MkdirAll(hooksDir, 0o755); err != nil {
			return fmt.Errorf("create hooks dir %s: %w", hooksDir, err)
		}
		chans = append(chans, hr)
		go func() {
			if err := hr.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "gauntlet: hooks: %v\n", err)
			}
		}()
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
	d, err := queue.New(repo, ex, chans, queue.Config{
		Targets:      cfg.Targets,
		CheckSpec:    cfg.CheckSpec,
		Committer:    cfg.Committer,
		MergeMessage: cfg.MergeMsg,
		MergeBody:    mergeBody,
		WorkDir:      trialsDir,
		LogDir:       logsDir,
	}, nil)
	if err != nil {
		return fmt.Errorf("init queue: %w", err)
	}

	// wg tracks the dashboard's goroutines (Shutdown watcher + ListenAndServe),
	// the depth sampler, and the log pruner, all of which may still be
	// querying or writing store (or, for the pruner, logsDir) after d.Run
	// returns on ctx cancellation. Waiting on wg before store.Close() runs
	// (deferred above) keeps that Close from racing them (cmd wiring review,
	// docs/plans/phase23.md): sampler exits, dashboard's graceful Shutdown
	// completes, then — only then — the store closes.
	var wg sync.WaitGroup
	startDashboard(ctx, cfg, d.Snapshot, store, dashCh, logsDir, &wg)
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
