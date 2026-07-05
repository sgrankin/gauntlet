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
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/dashboard"
	"github.com/sgrankin/gauntlet/internal/history"
	gauntletmcp "github.com/sgrankin/gauntlet/internal/mcp"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// dashboardShutdownTimeout bounds the dashboard's graceful shutdown so
// daemon exit is never hung waiting on a slow client.
const dashboardShutdownTimeout = 5 * time.Second

// startDashboard starts the read-only web dashboard (plus its JSON API, work
// chunk E4, and the MCP server, work chunk E5) on cfg.Dashboard.Bind, if
// configured, and returns immediately (the server runs in its own
// goroutine). store may be nil (history disabled; dashboard.New and
// gauntletmcp.New both already degrade every history-backed view/tool for
// that case). dashCh, if non-nil, is wired so both POST /api/v1/retry and
// the MCP "retry" tool feed it — it must already be registered in the
// channel list passed to queue.New (main.go does this before queue.New
// runs, since dashCh doesn't depend on anything queue.New produces, unlike
// the handlers built here). The server shuts down gracefully when ctx is
// done; a ListenAndServe failure other than http.ErrServerClosed is treated
// as fatal, matching main's "loud error, exit 1" style, since a dashboard
// that silently failed to bind would otherwise look "up" from the log
// alone.
//
// wg gains one count per goroutine started here (zero if the dashboard is
// disabled), released once each goroutine actually exits. main waits on wg
// before closing the history store, so a query still in flight against store
// (via the dashboard's or MCP server's history-backed views) can never race
// a Close (cmd wiring review, docs/plans/phase23.md).
//
// logDir is the same directory passed as queue.Config.LogDir (main.go's
// logsDir) — wired here as both dashboard.WithLogRoot and
// gauntletmcp.Params.LogRoot so GET /run/{id}/log/{check} and the MCP run
// tool's logUrl serve full per-check logs (DESIGN.md "Full per-check log
// files") under the exact containment boundary the executor ever writes
// into. This is unconditional: full logging is wired up regardless of
// whether the dashboard/history are configured (main.go), so log serving
// is available whenever the dashboard itself is.
func startDashboard(ctx context.Context, cfg *config.Daemon, snapshot func() *queue.Snapshot, store *history.Store, dashCh *dashboard.Channel, logDir string, wg *sync.WaitGroup) {
	if cfg.Dashboard.Bind == "" {
		return
	}

	var opts []dashboard.Option
	var retry func(core.Command) bool
	if dashCh != nil {
		opts = append(opts, dashboard.WithChannel(dashCh))
		retry = dashCh.TrySend
	}
	// version is main.go's package var (version.go), stamped at build time
	// via -ldflags; "devel" for a plain `go build`. Surfaced in the
	// dashboard footer purely as an operator convenience (docs/deploy.md).
	opts = append(opts, dashboard.WithVersion(version))
	opts = append(opts, dashboard.WithLogRoot(logDir))

	// The MCP server (chunk E5) is mounted at /mcp on the same listener as
	// the dashboard, since it's meant for agents that already know the
	// daemon's HTTP address — not a separate bind/port to configure. "/"
	// keeps the dashboard's own mux (HTML + JSON API), which registers
	// "GET /{$}" for its index rather than a catch-all, so it doesn't
	// shadow /mcp.
	mux := http.NewServeMux()
	mux.Handle("/mcp", gauntletmcp.New(gauntletmcp.Params{Snapshot: snapshot, Store: store, Retry: retry, LogRoot: logDir}))
	mux.Handle("/", dashboard.New(snapshot, store, opts...))

	srv := &http.Server{
		Addr:    cfg.Dashboard.Bind,
		Handler: mux,
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

// depthHeartbeat bounds how long a target's queue_depth series can go
// without a sample even when nothing changes: shouldRecord skips unchanged
// samples to keep the series small, but a chart with no points for hours
// during a genuinely idle/steady period would misread as "sampling stopped"
// rather than "nothing to report". A point at least this often keeps the
// series alive through steady stretches.
const depthHeartbeat = 10 * time.Minute

// depthTuple is the part of a queue_depth sample that matters for
// change-only sampling: (waiting, in-flight, parked) for one target. Two
// samples with an equal tuple carry no new information regardless of At.
//
// InFlight is len(TargetSnapshot.Pipeline) — the lane's pipeline depth
// (docs/plans/phase5.md §10 amendment 5), not a 0/1 "is something running"
// flag: today, before speculation/batching land, Pipeline has at most one
// element (mirroring InFlight != nil), so this is 0 when idle and 1 when
// serial-busy, byte-identical to the tuple this sampler recorded before —
// no series discontinuity. Once a target runs in speculate mode, this
// becomes the queue-depth series' actual pipeline-occupancy signal, which is
// the whole point of sampling it: the dashboard's tuning instrument for
// sizing `window`.
type depthTuple struct {
	Waiting, InFlight, Parked int
}

// buildDepthTuple extracts one target's depthTuple from a fresh snapshot —
// pulled out as a pure function so the sampler's per-tick decision is
// testable without spinning up the sampler goroutine.
func buildDepthTuple(ts queue.TargetSnapshot) depthTuple {
	return depthTuple{Waiting: len(ts.Waiting), InFlight: len(ts.Pipeline), Parked: len(ts.Parked)}
}

// depthSample is the last tuple recorded for a target, and when.
type depthSample struct {
	tuple depthTuple
	at    time.Time // zero => never recorded
}

// shouldRecord is the pure decision behind the depth sampler's change-only
// recording: record when the tuple differs from the last one recorded for
// this target (including the very first sample, where lastAt is the zero
// time), or when the last recording is old enough that depthHeartbeat has
// elapsed since — so a steady-state series still gets periodic points
// rather than going silent indefinitely. now is the current sample's
// timestamp (snap.At), not wall-clock time, so the decision is driven by the
// same clock the samples themselves are keyed on.
func shouldRecord(last, current depthTuple, lastAt, now time.Time) bool {
	if current != last {
		return true
	}
	if lastAt.IsZero() {
		return true
	}
	return now.Sub(lastAt) >= depthHeartbeat
}

// startDepthSampler starts the goroutine that periodically samples queue
// depth into store, per docs/plans/phase23.md §4.8: every cfg.History.
// SampleEvery tick, read snapshot() and consider recording one queue_depth
// row per target. Nil snapshots (no reconcile pass has completed yet) are
// skipped rather than recorded as zero, so an idle-startup gap doesn't read
// as "an empty queue" in the depth series.
//
// Per target, a sample is only actually written when shouldRecord says so:
// the (waiting, in-flight, parked) tuple changed since the last sample this
// goroutine wrote for that target, or the heartbeat interval has elapsed —
// bounding the depth series to actual state transitions plus a keepalive,
// rather than one row per SampleEvery tick forever (chunk E1).
//
// Once per heartbeat interval this also prunes queue_depth rows older than
// now-cfg.History.DepthRetention (Store.PruneDepth) — opportunistically,
// piggybacking on the sampler's own tick rather than a separate timer. Runs
// and checks are never pruned; see PruneDepth's doc for why only the depth
// series gets a retention bound.
//
// Only called when store != nil. wg gains one count, released once this
// goroutine exits on ctx.Done() — see startDashboard's doc for why main
// waits on it before closing store.
func startDepthSampler(ctx context.Context, cfg *config.Daemon, snapshot func() *queue.Snapshot, store *history.Store, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(cfg.History.SampleEvery)
		defer ticker.Stop()

		last := make(map[string]depthSample)
		var lastPrune time.Time

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snap := snapshot()
				if snap == nil {
					continue
				}
				for _, ts := range snap.Targets {
					current := buildDepthTuple(ts)
					prev := last[ts.Name]
					if !shouldRecord(prev.tuple, current, prev.at, snap.At) {
						continue
					}
					if err := store.RecordDepth(snap.At, ts.Name, current.Waiting, current.InFlight, current.Parked); err != nil {
						fmt.Fprintf(os.Stderr, "gauntlet: history: record depth: %v\n", err)
					}
					last[ts.Name] = depthSample{tuple: current, at: snap.At}
				}

				if lastPrune.IsZero() || snap.At.Sub(lastPrune) >= depthHeartbeat {
					if err := store.PruneDepth(snap.At.Add(-cfg.History.DepthRetention)); err != nil {
						fmt.Fprintf(os.Stderr, "gauntlet: history: prune depth: %v\n", err)
					}
					lastPrune = snap.At
				}
			}
		}
	}()
}
