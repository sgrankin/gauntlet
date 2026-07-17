package queue

import (
	"context"
	"fmt"
	"os"
	"time"
)

// nodeWorkspacePrefix names the per-node private trial directories
// (isolated mode, issue #9). They live under WorkDir alongside the
// run-level "gauntlet-trial-" dirs, so cmd's startup sweep — which wipes
// WorkDir wholesale, not by prefix — clears any crash leftovers.
const nodeWorkspacePrefix = "gauntlet-node-"

// materializeNode creates one graph node's private workspace (isolated
// mode): a fresh materialization of the run's exact chain-tip tree, with
// the history-mtime pass applied when enabled, so parallel or
// after-related nodes never share a writable tree. Called from a check's
// own goroutine AFTER it holds an execution slot, so the daemon-wide cap
// bounds simultaneous archives/mtime walks as well as commands.
//
// treeOID is the exact tree object to export; chainTip is the merge commit
// the tree belongs to, used only as the mtime history anchor. These MUST
// be kept distinct: `git archive <commit>` applies `.gitattributes`
// export-subst (substituting $Format:%H$ etc. against the commit), while
// `git archive <tree>` does not — so archiving the commit would put bytes
// in the workspace that are not in the tested tree and that differ from
// shared mode, breaking the exact-tree invariant. Shared-mode setup
// likewise exports trial.TreeOID (the tree), never the merge commit.
//
// Returns the directory (the caller removes it after the executor child
// stops) and how long materialization took — wall time, like the
// command's own Duration, since this runs off the reconcile goroutine and
// its injected clock. Any failure is returned for the caller to turn into
// an OutcomeError result (park-as-error), never a silent fallback to a
// shared dir or wall-clock mtimes; partial state is cleaned before
// returning.
func (d *Daemon) materializeNode(ctx context.Context, treeOID, chainTip string) (dir string, took time.Duration, err error) {
	start := time.Now()
	dir, err = os.MkdirTemp(d.cfg.WorkDir, nodeWorkspacePrefix)
	if err != nil {
		return "", 0, fmt.Errorf("mkdir: %w", err)
	}
	// Export the tree OID, not the merge commit: this is the exact tested
	// tree the shared-mode run export produces, with no commit-scoped
	// export-subst rewriting.
	if err := d.git.ExportTree(ctx, treeOID, dir); err != nil {
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
