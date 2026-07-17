package queue

import (
	"context"
	"fmt"
	"os"
	"time"
)

// nodeWorkspacePrefix names the per-node private trial directories
// (isolated mode, issue #9) so cmd's startup orphan sweep of WorkDir can
// recognize and remove crash leftovers alongside the run-level
// "gauntlet-trial-" dirs.
const nodeWorkspacePrefix = "gauntlet-node-"

// materializeNode creates one graph node's private workspace (isolated
// mode): a fresh materialization of the run's exact chain-tip tree, with
// the history-mtime pass applied when enabled, so parallel or
// after-related nodes never share a writable tree. Called from a check's
// own goroutine AFTER it holds an execution slot, so the daemon-wide cap
// bounds simultaneous archives/mtime walks as well as commands.
//
// Returns the directory (the caller removes it after the executor child
// stops) and how long materialization took — wall time, like the
// command's own Duration, since this runs off the reconcile goroutine and
// its injected clock. Any failure is returned for the caller to turn into
// an OutcomeError result (park-as-error), never a silent fallback to a
// shared dir or wall-clock mtimes; partial state is cleaned before
// returning.
func (d *Daemon) materializeNode(ctx context.Context, chainTip string) (dir string, took time.Duration, err error) {
	start := time.Now()
	dir, err = os.MkdirTemp(d.cfg.WorkDir, nodeWorkspacePrefix)
	if err != nil {
		return "", 0, fmt.Errorf("mkdir: %w", err)
	}
	// git archive accepts a commit-ish and materializes its tree, so the
	// chain-tip merge commit yields the exact tested tree — the same
	// content the shared-mode run export produces.
	if err := d.git.ExportTree(ctx, chainTip, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", 0, fmt.Errorf("export tree: %w", err)
	}
	if d.cfg.HistoryMtimes {
		if _, err := d.git.RestoreMtimes(ctx, chainTip, dir); err != nil {
			_ = os.RemoveAll(dir)
			return "", 0, fmt.Errorf("restore mtimes: %w", err)
		}
	}
	return dir, time.Since(start), nil
}
