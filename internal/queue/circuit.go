package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

type InfrastructureFailure struct {
	Revision string
	At       time.Time
}
type Circuit struct {
	ProbeRunID string
	Failures   []InfrastructureFailure
	Until      time.Time
	Backoff    time.Duration
	Reason     string
}

func (d *Daemon) circuitBlocked(target string) bool {
	return d.now().Before(d.controls.Circuits[target].Until)
}
func (d *Daemon) observeInfrastructure(ev core.Event) {
	cfg := d.cfg.CircuitBreaker
	if cfg == nil || ev.Target == "" || ev.Candidate.Ref == "" {
		return
	}
	c := d.controls.Circuits[ev.Target]
	failed := ev.Record != nil && ev.Record.Outcome == core.OutcomeError
	if ev.Check != nil {
		failed = ev.Check.Err != nil
	}
	if ev.Record != nil && c.Backoff > 0 && ev.RunID != "" && ev.RunID == c.ProbeRunID && (ev.Record.Outcome == core.OutcomeLanded || ev.Record.Outcome == core.OutcomeRejected) {
		healthy := true
		for _, check := range ev.Record.Checks {
			if check.Err != nil {
				healthy = false
			}
		}
		if healthy {
			nextData, _ := json.Marshal(d.controls)
			var next controlState
			json.Unmarshal(nextData, &next)
			delete(next.Circuits, ev.Target)
			d.saveControls(next)
			return
		}
	}
	if !failed {
		return
	}
	revision := ev.Candidate.Ref + "@" + ev.Candidate.SHA
	cutoff := d.now().Add(-cfg.Window)
	fresh := []InfrastructureFailure{}
	duplicate := false
	for _, f := range c.Failures {
		if f.At.After(cutoff) {
			fresh = append(fresh, f)
			if f.Revision == revision {
				duplicate = true
			}
		}
	}
	c.Failures = fresh
	if !duplicate {
		c.Failures = append(c.Failures, InfrastructureFailure{revision, d.now()})
	}
	if len(c.Failures) >= cfg.Threshold || c.Backoff > 0 {
		// Multiple events from the same failed probe do not compound backoff.
		if !d.now().Before(c.Until) {
			c.Backoff = min(cfg.MaxBackoff, max(cfg.Backoff, 2*c.Backoff))
			c.Until = d.now().Add(c.Backoff)
		}
		c.Reason = fmt.Sprintf("infrastructure failures across %d revisions; next probe after %s", len(c.Failures), c.Until.Format(time.RFC3339))
	}
	data, _ := json.Marshal(d.controls)
	var next controlState
	json.Unmarshal(data, &next)
	next.Circuits[ev.Target] = c
	d.saveControls(next)
}

func (d *Daemon) prepareCircuitProbe(ctx context.Context, target string, cands map[string]core.Candidate) {
	if d.controls.Circuits[target].Backoff == 0 || d.circuitBlocked(target) || d.draining {
		return
	}
	if lane := d.lanes[target]; lane != nil && len(lane.runs) > 0 {
		return
	}
	// Restore one infrastructure park for a bounded probe; ordinary red
	// verdicts and explicit operator cancellations remain parked.
	var chosen string
	for ref, entry := range d.done[target] {
		if entry.Outcome != core.OutcomeError {
			continue
		}
		if _, ok := cands[ref]; !ok {
			continue
		}
		if chosen == "" || d.order[target][ref] < d.order[target][chosen] {
			chosen = ref
		}
	}
	if chosen != "" {
		d.clearParkAndRetry(ctx, target, chosen, "circuit recovery probe", "automatic infrastructure probe")
	}
}

func (d *Daemon) markCircuitProbe(r *run) {
	c := d.controls.Circuits[r.target]
	if c.Backoff == 0 || d.circuitBlocked(r.target) || c.ProbeRunID == r.runID {
		return
	}
	if r.members[0].rec.StartedAt.Before(c.Until) {
		return
	}
	nextData, _ := json.Marshal(d.controls)
	var next controlState
	json.Unmarshal(nextData, &next)
	c.ProbeRunID = r.runID
	next.Circuits[r.target] = c
	d.saveControls(next)
}
