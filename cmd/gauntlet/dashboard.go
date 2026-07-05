package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// dashboardShutdownTimeout bounds the dashboard's graceful shutdown so
// daemon exit is never hung waiting on a slow client.
const dashboardShutdownTimeout = 5 * time.Second

// startDashboard starts the read-only web dashboard on cfg.Dashboard.Bind,
// if configured, and returns immediately (the server runs in its own
// goroutine). store may be nil (history disabled; dashboard.New already
// degrades every history-backed view for that case). The server shuts down
// gracefully when ctx is done; a ListenAndServe failure other than
// http.ErrServerClosed is treated as fatal, matching main's "loud error,
// exit 1" style, since a dashboard that silently failed to bind would
// otherwise look "up" from the log alone.
//
// wg gains one count per goroutine started here (zero if the dashboard is
// disabled), released once each goroutine actually exits. main waits on wg
// before closing the history store, so a query still in flight against store
// (via the dashboard's history-backed views) can never race a Close (cmd
// wiring review, docs/plans/phase23.md).
func startDashboard(ctx context.Context, cfg *config.Daemon, snapshot func() *queue.Snapshot, store *history.Store, wg *sync.WaitGroup) {
	if cfg.Dashboard.Bind == "" {
		return
	}

	srv := &http.Server{
		Addr:    cfg.Dashboard.Bind,
		Handler: dashboard.New(snapshot, store),
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), dashboardShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "gauntlet: dashboard: %v\n", err)
			os.Exit(1)
		}
	}()
}

// startDepthSampler starts the goroutine that periodically samples queue
// depth into store, per docs/plans/phase23.md §4.8: every cfg.History.
// SampleEvery tick, read d.Snapshot() and record one queue_depth row per
// target. Nil snapshots (no reconcile pass has completed yet) are skipped
// rather than recorded as zero, so an idle-startup gap doesn't read as "an
// empty queue" in the depth series. A snapshot whose At is unchanged from
// the last one actually sampled is skipped too: RecordDepth's INSERT OR
// REPLACE is keyed on (at, target), so re-sampling the same unchanged
// snapshot on a poll interval shorter than SampleEvery's tick would silently
// replace a real point with itself — harmless, but any timing where a
// slow/short poll cadence lets the ticker fire twice against one unchanged
// snapshot would otherwise drop a would-be-distinct sample in favor of a
// duplicate. Only called when store != nil.
//
// wg gains one count, released once this goroutine exits on ctx.Done() — see
// startDashboard's doc for why main waits on it before closing store.
func startDepthSampler(ctx context.Context, cfg *config.Daemon, snapshot func() *queue.Snapshot, store *history.Store, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(cfg.History.SampleEvery)
		defer ticker.Stop()
		var lastAt time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snap := snapshot()
				if snap == nil {
					continue
				}
				if snap.At.Equal(lastAt) {
					continue
				}
				lastAt = snap.At
				for _, ts := range snap.Targets {
					inFlight := 0
					if ts.InFlight != nil {
						inFlight = 1
					}
					if err := store.RecordDepth(snap.At, ts.Name, len(ts.Waiting), inFlight, len(ts.Parked)); err != nil {
						fmt.Fprintf(os.Stderr, "gauntlet: history: record depth: %v\n", err)
					}
				}
			}
		}
	}()
}
