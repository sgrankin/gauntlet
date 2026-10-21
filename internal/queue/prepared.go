package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/verification"
)

type preparedEntry struct {
	RunID, Commit, Tree string
	At                  time.Time
}

func (d *Daemon) preparedKey(target, base string, c core.Candidate, group []string) string {
	if d.cfg.ControlPath == "" || d.cfg.VerificationContext == "" {
		return ""
	}
	return verification.Key("prepared-v1", d.cfg.VerificationContext, d.cfg.Committer, d.cfg.MergeMessage, d.cfg.Targets, target, base, c, group)
}

func (d *Daemon) recallPrepared(ctx context.Context, key, tree string) preparedEntry {
	entry := d.controls.Prepared[key]
	if key == "" || entry.RunID == "" || entry.Tree != tree || d.now().Sub(entry.At) > 24*time.Hour {
		return preparedEntry{}
	}
	if exists, err := d.git.IsAncestor(ctx, entry.Commit, entry.Commit); err != nil || !exists {
		return preparedEntry{}
	}
	return entry
}

func (d *Daemon) rememberPrepared(ctx context.Context, key string, entry preparedEntry) bool {
	if key == "" {
		return true
	}
	// Archive refs survive the startup pin sweep. Missing objects invalidate a
	// checkpoint safely; live source leases additionally protect running checks.
	release, err := core.RetainSources(ctx, d.git, entry.Commit)
	if err != nil {
		d.controls.LastError = "retain prepared commit: " + err.Error()
		return false
	}
	release()
	next := d.controls.clone()
	if next.Prepared == nil {
		next.Prepared = map[string]preparedEntry{}
	}
	for identity, old := range next.Prepared {
		if old.RunID != entry.RunID && !d.preparedActive(old.RunID) && d.now().Sub(old.At) > 24*time.Hour {
			delete(next.Prepared, identity)
		}
	}
	delete(next.Prepared, key)
	for len(next.Prepared) >= 2048 {
		oldest := ""
		var at time.Time
		for identity, old := range next.Prepared {
			if old.RunID == entry.RunID || d.preparedActive(old.RunID) {
				continue
			}
			if oldest == "" || old.At.Before(at) {
				oldest, at = identity, old.At
			}
		}
		if oldest == "" {
			d.controls.LastError = fmt.Sprintf("prepared trial capacity reached (%d)", len(next.Prepared))
			return false
		}
		delete(next.Prepared, oldest)
	}
	next.Prepared[key] = entry
	return d.saveControls(next)
}

func (d *Daemon) forgetPrepared(runID string) {
	if runID == "" {
		return
	}
	for key, entry := range d.controls.Prepared {
		if entry.RunID == runID {
			delete(d.controls.Prepared, key)
			d.schedulerDirty = true
		}
	}
}

func (d *Daemon) preparedActive(runID string) bool {
	for _, lane := range d.lanes {
		for _, run := range lane.runs {
			if run.runID == runID {
				return true
			}
		}
	}
	return false
}

func (d *Daemon) prunePrepared() {
	for key, entry := range d.controls.Prepared {
		if !d.preparedActive(entry.RunID) && d.now().Sub(entry.At) > 24*time.Hour {
			delete(d.controls.Prepared, key)
			d.schedulerDirty = true
		}
	}
}
