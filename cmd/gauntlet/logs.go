// Full per-check log retention (DESIGN.md "Full per-check log files"):
// cmd/gauntlet wires <state>/logs as queue.Config.LogDir unconditionally
// (main.go), so unlike history/dashboard/etc. this sweep always runs,
// regardless of which optional sections are configured.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logPruneInterval is the cadence of the periodic sweep started by
// startLogPruner. It's independent of cfg.Poll (which can be much shorter
// than makes sense for a retention sweep) and of
// cfg.History.SampleEvery/depthHeartbeat (dashboard.go's depth sampler,
// which doesn't run at all when history is disabled) — log retention
// applies whether or not history is configured, so it gets its own modest,
// fixed cadence rather than piggybacking on either.
const logPruneInterval = 1 * time.Hour

// pruneLogFiles deletes every directory entry directly under logDir whose
// modtime is at or before cutoff. Each such entry is one run's full-log
// directory (queue.Config.LogDir's <runID>/<check>.log layout,
// internal/queue/reconcile.go's job.LogPath assignment): the directory is
// created once, when the first check of that run opens its log file
// (internal/executor/logfile.go's openCheckLog, via os.MkdirAll), and never
// written to again afterward — so its modtime is a faithful "how long ago
// this run's logs were captured" timestamp, exactly what an age-based
// retention policy needs. Non-directory entries directly under logDir
// (there shouldn't be any, by construction) are left alone.
//
// A logDir that doesn't exist yet (a fresh state directory, or one where no
// run has ever assigned a log path) is not an error: it reports success
// with nothing pruned, the same "nothing to do yet" shape as every other
// degrade-gracefully path in this codebase.
func pruneLogFiles(logDir string, cutoff time.Time) error {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read log dir %s: %w", logDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Entry vanished between ReadDir and Info (e.g. a racing prune,
			// possibly from a previous process instance) — nothing left to
			// prune here, move on rather than fail the whole sweep.
			continue
		}
		if !info.ModTime().After(cutoff) {
			path := filepath.Join(logDir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("prune log dir %s: %w", path, err)
			}
		}
	}
	return nil
}

// startLogPruner starts the goroutine that repeats pruneLogFiles every
// logPruneInterval until ctx is done. run() already performs one sweep
// synchronously at startup (mirroring the trials-dir sweep just above it);
// this goroutine is what keeps sweeping for the lifetime of a long-running
// daemon.
//
// wg gains one count, released when this goroutine exits on ctx.Done() —
// the same accounting pattern startDashboard/startDepthSampler use
// (dashboard.go), so main's wg.Wait() covers every background goroutine
// run() starts, even though nothing else needs to wait on this one
// specifically (it touches no shared store).
func startLogPruner(ctx context.Context, logDir string, retention time.Duration, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(logPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := pruneLogFiles(logDir, time.Now().Add(-retention)); err != nil {
					fmt.Fprintf(os.Stderr, "gauntlet: logs: prune: %v\n", err)
				}
			}
		}
	}()
}
