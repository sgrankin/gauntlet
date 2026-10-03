package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/statefile"
)

type Pause struct {
	Actor  string    `json:"actor"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type controlState struct {
	ReviewFloor      int64
	ProcessedReviews map[string]bool
	PolicyAudit      []PolicyAudit
	Circuits         map[string]Circuit
	Version          int
	Pauses           map[string]Pause
	Urgent           map[string]core.Revision
	Emergency        map[string]core.Command
	Audit            []controlAudit
	LastError        string
	Uncertain        bool `json:"-"`
}

// clone keeps updates separate from the published state, including fields
// excluded from disk serialization. Persistence alone decides uncertainty.
func (s controlState) clone() controlState {
	s.ProcessedReviews = maps.Clone(s.ProcessedReviews)
	s.Pauses = maps.Clone(s.Pauses)
	s.Urgent = maps.Clone(s.Urgent)
	s.Emergency = maps.Clone(s.Emergency)
	for target, cmd := range s.Emergency {
		cmd.Revisions = slices.Clone(cmd.Revisions)
		s.Emergency[target] = cmd
	}
	s.Circuits = maps.Clone(s.Circuits)
	for target, circuit := range s.Circuits {
		circuit.Failures = slices.Clone(circuit.Failures)
		s.Circuits[target] = circuit
	}
	s.Audit = slices.Clone(s.Audit)
	for i := range s.Audit {
		s.Audit[i].Command.Revisions = slices.Clone(s.Audit[i].Command.Revisions)
	}
	s.PolicyAudit = slices.Clone(s.PolicyAudit)
	for i := range s.PolicyAudit {
		s.PolicyAudit[i].Decision.Requirements = slices.Clone(s.PolicyAudit[i].Decision.Requirements)
	}
	return s
}

type controlAudit struct {
	At      time.Time
	Command core.Command
}

func loadControls(path string) (controlState, error) {
	s := controlState{ProcessedReviews: map[string]bool{}, Circuits: map[string]Circuit{}, Version: 1, Pauses: map[string]Pause{}, Urgent: map[string]core.Revision{}, Emergency: map[string]core.Command{}}
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
	if s.ProcessedReviews == nil {
		s.ProcessedReviews = map[string]bool{}
	}
	if s.Circuits == nil {
		s.Circuits = map[string]Circuit{}
	}
	return s, nil
}

func (d *Daemon) saveControls(next controlState) bool {
	if len(next.Audit) > 500 {
		next.Audit = next.Audit[len(next.Audit)-500:]
	}
	for {
		data, _ := json.Marshal(next)
		if len(data) <= 3<<20 {
			break
		}
		if len(next.Audit) > 0 {
			next.Audit = next.Audit[1:]
			continue
		}
		if len(next.PolicyAudit) > 0 {
			next.PolicyAudit = next.PolicyAudit[1:]
			continue
		}
		d.controls = next
		d.controls.LastError = "incident control state exceeds storage limit"
		d.controls.Uncertain = true
		return false
	}
	if path := d.cfg.ControlPath; path != "" {
		if err := writeControls(path, next); err != nil {
			d.controls = next
			d.controls.LastError = "cannot persist incident control: " + err.Error()
			d.controls.Uncertain = true
			return false
		}
	}
	next.Uncertain = false
	d.controls = next
	return true
}

func writeControls(path string, state controlState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return statefile.Write(path, data)
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
	if d.draining && ((cmd.Kind == core.CommandMergeAnyway || cmd.Kind == core.CommandMergePaused) || cmd.Kind == core.CommandUrgent) {
		d.controls.LastError = "queue is draining; no new merge requests accepted"
		return
	}
	cmd.Actor, cmd.Reason = strings.TrimSpace(cmd.Actor), strings.TrimSpace(cmd.Reason)
	if cmd.Actor == "" || cmd.Reason == "" || len(cmd.Actor) > 256 || len(cmd.Reason) > 2048 {
		d.controls.LastError = "incident controls require actor and reason"
		return
	}
	if cmd.Kind == core.CommandMergePaused && !cmd.OverridePause {
		d.controls.LastError = "explicit pause override required"
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
	next := d.controls.clone()
	next.LastError = ""
	if cmd.Kind == core.CommandPause {
		next.Pauses[cmd.Target] = Pause{cmd.Actor, cmd.Reason, d.now()}
	}
	if cmd.Kind == core.CommandResume {
		delete(next.Pauses, cmd.Target)
	}
	if cmd.Kind == core.CommandUrgent || (cmd.Kind == core.CommandMergeAnyway || cmd.Kind == core.CommandMergePaused) {
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
				if len(next.Urgent) >= 4096 {
					d.controls.LastError = "priority capacity reached"
					return
				}
				next.Urgent[rev.Ref] = rev
			}
		}
		if cmd.Kind == core.CommandMergeAnyway || cmd.Kind == core.CommandMergePaused {
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
	if cmd.RequestID != "" {
		next.ProcessedReviews[cmd.RequestID] = true
		if len(next.ProcessedReviews) > 500 {
			ids := []int64{}
			for id := range next.ProcessedReviews {
				n, err := strconv.ParseInt(id, 10, 64)
				if err == nil {
					ids = append(ids, n)
				}
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			if len(ids) > 0 {
				next.ReviewFloor = max(next.ReviewFloor, ids[0])
				delete(next.ProcessedReviews, strconv.FormatInt(ids[0], 10))
			}
		}
	}
	next.Audit = append(next.Audit, controlAudit{d.now(), cmd})
	if !d.saveControls(next) {
		return
	}
	if cmd.Kind == core.CommandPause || (cmd.Kind == core.CommandMergeAnyway || cmd.Kind == core.CommandMergePaused) {
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
	if d.draining {
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
	if r := d.headRun(t.Name); r != nil && len(r.members) != len(picked) {
		d.cancelRun(r)
		d.finishRun(ctx, t, r, core.OutcomeSkipped, "selected emergency prefix crosses a check-spec boundary; select a shorter prefix", false)
		d.lanes[t.Name].runs = nil
		d.clearEmergency(t.Name, "select a shorter prefix: check-spec boundary")
		return true
	}
	if d.headRun(t.Name) == nil {
		d.clearEmergency(t.Name, "emergency history could not be constructed; see run failure")
	}
	return true
}

func (d *Daemon) clearEmergency(target, reason string) {
	next := d.controls.clone()
	delete(next.Emergency, target)
	next.LastError = reason
	d.saveControls(next)
}

func (d *Daemon) prepareEmergency(r *run) {
	if r.controlPrepared {
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
	r.controlPrepared = true
	if cmd.Kind == core.CommandMergePaused {
		r.overridePause = true
		r.pauseOverrideReason = cmd.Actor + ": " + cmd.Reason
		return
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
