package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

type Pause struct {
	Actor  string    `json:"actor"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type controlState struct {
	Version   int
	Pauses    map[string]Pause
	Urgent    map[string]core.Revision
	Emergency map[string]core.Command
	Audit     []controlAudit
	LastError string
	Uncertain bool `json:"-"`
}

type controlAudit struct {
	At      time.Time
	Command core.Command
}

func loadControls(path string) (controlState, error) {
	s := controlState{Version: 1, Pauses: map[string]Pause{}, Urgent: map[string]core.Revision{}, Emergency: map[string]core.Command{}}
	if path == "" {
		return s, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return s, fmt.Errorf("queue controls: cannot read state")
	}
	if err := json.Unmarshal(data, &s); err != nil || s.Version != 1 || s.Pauses == nil || s.Urgent == nil || s.Emergency == nil {
		return s, fmt.Errorf("queue controls: invalid state; refusing to forget incident controls")
	}
	return s, nil
}

func (d *Daemon) saveControls(next controlState) bool {
	next.Audit = slices.Clone(next.Audit)
	if len(next.Audit) > 500 {
		next.Audit = next.Audit[len(next.Audit)-500:]
	}
	if path := d.cfg.ControlPath; path != "" {
		if err := writeControls(path, next); err != nil {
			d.controls.LastError = "cannot persist incident control: " + err.Error()
			d.controls.Uncertain = true
			return false
		}
	}
	d.controls = next
	return true
}

func writeControls(path string, state controlState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".queue-controls-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (d *Daemon) applyControl(ctx context.Context, cmd core.Command, refs map[string]string) {
	if cmd.Target == "*" && (cmd.Kind == core.CommandPause || cmd.Kind == core.CommandResume) {
		for _, t := range d.cfg.Targets {
			copy := cmd
			copy.Target = t.Name
			d.applyControl(ctx, copy, refs)
		}
		return
	}
	cmd.Actor, cmd.Reason = strings.TrimSpace(cmd.Actor), strings.TrimSpace(cmd.Reason)
	if cmd.Actor == "" || cmd.Reason == "" || len(cmd.Actor) > 256 || len(cmd.Reason) > 2048 {
		d.controls.LastError = "incident controls require actor and reason"
		return
	}
	var target config.Target
	for _, t := range d.cfg.Targets {
		if t.Name == cmd.Target {
			target = t
			break
		}
	}
	if target.Name == "" {
		d.controls.LastError = "unknown target"
		return
	}
	data, _ := json.Marshal(d.controls)
	var next controlState
	_ = json.Unmarshal(data, &next)
	next.LastError = ""
	if cmd.Kind == core.CommandPause {
		next.Pauses[cmd.Target] = Pause{cmd.Actor, cmd.Reason, d.now()}
	}
	if cmd.Kind == core.CommandResume {
		delete(next.Pauses, cmd.Target)
	}
	if cmd.Kind == core.CommandUrgent || cmd.Kind == core.CommandMergeAnyway {
		cands := discoverCandidates(cmd.Target, refs)
		for ref, c := range d.external {
			if c.Target == cmd.Target {
				cands[ref] = c
			}
		}
		if len(cmd.Revisions) == 0 || len(cmd.Revisions) > 64 {
			d.controls.LastError = "select 1..64 exact revisions"
			return
		}
		selected := map[string]bool{}
		for _, rev := range cmd.Revisions {
			c, ok := cands[rev.Ref]
			if !ok || c.SHA != rev.SHA || c.Version != rev.Version || selected[rev.Ref] {
				d.controls.LastError = "selected revision changed or disappeared; request again"
				return
			}
			if c.DependsOn != "" && !selected[c.DependsOn] {
				d.controls.LastError = "select prerequisites before their successors"
				return
			}
			selected[rev.Ref] = true
			if cmd.Kind == core.CommandUrgent {
				next.Urgent[rev.Ref] = rev
			}
		}
		if cmd.Kind == core.CommandMergeAnyway {
			if !d.cfg.AllowEmergency {
				d.controls.LastError = "emergency merging is not enabled by the operator"
				return
			}
			if _, paused := next.Pauses[cmd.Target]; paused && !cmd.OverridePause {
				d.controls.LastError = "target paused; an explicit pause override is required"
				return
			}
			next.Emergency[cmd.Target] = cmd
		}
	}
	next.Audit = append(next.Audit, controlAudit{d.now(), cmd})
	if !d.saveControls(next) {
		return
	}
	if cmd.Kind == core.CommandPause || cmd.Kind == core.CommandMergeAnyway {
		if l := d.lanes[cmd.Target]; l != nil {
			d.invalidateSuffix(ctx, target, l, 0, "operator incident control: "+cmd.Reason)
		}
	}
}

func (d *Daemon) urgent(target, ref string, cands map[string]core.Candidate) bool {
	seen := map[string]bool{}
	var wanted func(string) bool
	wanted = func(ref string) bool {
		if seen[ref] {
			return false
		}
		seen[ref] = true
		c := cands[ref]
		if c.Urgent {
			return true
		}
		if r, ok := d.controls.Urgent[ref]; ok && r.SHA == c.SHA && r.Version == c.Version {
			return true
		}
		for child, next := range cands {
			if next.DependsOn == ref && wanted(child) {
				return true
			}
		}
		return false
	}
	return wanted(ref)
}

func (d *Daemon) reconcileEmergency(ctx context.Context, t config.Target, tip string, cands map[string]core.Candidate) bool {
	if d.controls.Uncertain {
		return true
	}
	cmd, ok := d.controls.Emergency[t.Name]
	if !ok {
		return false
	}
	if _, paused := d.controls.Pauses[t.Name]; paused && !cmd.OverridePause {
		return true
	}
	if l := d.lanes[t.Name]; l != nil && len(l.runs) > 0 {
		d.advanceLane(ctx, t, tip, cands, l)
		return true
	}
	var picked []core.Candidate
	for _, rev := range cmd.Revisions {
		c, exists := cands[rev.Ref]
		if !exists || c.SHA != rev.SHA || c.Version != rev.Version {
			d.clearEmergency(t.Name, "emergency revision changed; request again")
			return true
		}
		picked = append(picked, c)
	}
	d.startBatchRun(ctx, t, tip, picked)
	if d.headRun(t.Name) == nil {
		d.clearEmergency(t.Name, "emergency history could not be constructed; see run failure")
	}
	return true
}

func (d *Daemon) clearEmergency(target, reason string) {
	data, _ := json.Marshal(d.controls)
	var next controlState
	_ = json.Unmarshal(data, &next)
	delete(next.Emergency, target)
	next.LastError = reason
	d.saveControls(next)
}

func (d *Daemon) prepareEmergency(r *run) {
	if r.emergencyReason != "" {
		return
	}
	cmd, ok := d.controls.Emergency[r.target]
	if !ok || len(cmd.Revisions) != len(r.members) {
		return
	}
	for i, m := range r.members {
		rev := cmd.Revisions[i]
		if rev.Ref != m.cand.Ref || rev.SHA != m.cand.SHA || rev.Version != m.cand.Version {
			return
		}
	}
	r.emergencyReason = cmd.Actor + ": " + cmd.Reason
	r.overridePause = cmd.OverridePause
	for i, c := range r.checks {
		_, receipt := receiptNodeName(c.Name)
		_, image := imageNodeName(c.Name)
		if receipt || (image && d.cfg.ReceiptNotes != nil) {
			continue
		}
		r.results[c.Name] = core.CheckResult{Name: c.Name, Seq: i + 1, Command: c.Command, Status: core.CheckWaived, Output: "Operator bypass: " + r.emergencyReason}
	}
}

func (r *run) nodeGreen(result core.CheckResult) bool {
	return core.NodeGreen(result) || (r.emergencyReason != "" && result.Err == nil && result.Status == core.CheckWaived)
}
