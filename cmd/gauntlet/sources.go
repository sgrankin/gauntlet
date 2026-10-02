package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/gitx"
)

func startSourcePruner(ctx context.Context, repo *gitx.Repo, retention time.Duration, wg *sync.WaitGroup) {
	wg.Go(func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		runSourcePruner(ctx, repo, retention, ticker.C)
	})
}

// Sweep at startup as well as hourly; shutdown cancels and joins the worker.
func runSourcePruner(ctx context.Context, repo *gitx.Repo, retention time.Duration, ticks <-chan time.Time) {
	for {
		workCtx, cancel := context.WithTimeout(ctx, time.Minute)
		_, err := repo.PruneSources(workCtx, time.Now().Add(-retention), true)
		cancel()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "gauntlet: sources: prune: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
		}
	}
}
