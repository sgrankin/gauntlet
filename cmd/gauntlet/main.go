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
	"syscall"
	"time"

	"github.com/sgrankin/gauntlet/internal/channel"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/queue"
)

func main() {
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
	flag.Parse()

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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Key the bare repo's directory off the remote URL so a future
	// multi-remote daemon (or a config that just changes remotes) never
	// collides with a stale clone left under the same state dir.
	repoDir := filepath.Join(*statePath, "repos", remoteKey(cfg.Remote))
	repo, err := gitx.New(ctx, repoDir, cfg.Remote)
	if err != nil {
		return fmt.Errorf("open repo at %s: %w", repoDir, err)
	}

	chans := []core.Channel{channel.NewLogChannel(os.Stderr)}

	d, err := queue.New(repo, executor.LocalExecutor{}, chans, queue.Config{
		Targets:      cfg.Targets,
		CheckSpec:    cfg.CheckSpec,
		Committer:    cfg.Committer,
		MergeMessage: cfg.MergeMsg,
	}, nil)
	if err != nil {
		return fmt.Errorf("init queue: %w", err)
	}

	ticker := time.NewTicker(cfg.Poll)
	defer ticker.Stop()

	if err := d.Run(ctx, ticker.C); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}

// remoteKey derives a stable, filesystem-safe directory name for remote's
// bare-repo clone, so distinct remotes' state never collides on disk.
func remoteKey(remote string) string {
	sum := sha256.Sum256([]byte(remote))
	return hex.EncodeToString(sum[:8])
}
