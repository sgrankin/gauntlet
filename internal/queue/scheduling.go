package queue

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

type priorityEntry struct {
	Revision core.Revision
	Class    string
}

func (d *Daemon) clockNow() time.Time {
	if d.now == nil {
		return time.Now()
	}
	return d.now()
}

func PriorityRank(class string) (int, bool) {
	switch class {
	case "low":
		return 0, true
	case "", "normal":
		return 1, true
	case "high":
		return 2, true
	case "urgent":
		return 3, true
	}
	return 0, false
}

// ScheduleImpact is computed from a snapshot and revalidated by reconciliation.
// The token binds the preview to positions and run identities, not check timing.
type ScheduleImpact struct {
	Token           string   `json:"token"`
	Order           []string `json:"order"`
	AffectedRuns    []string `json:"affectedRuns"`
	CompletedChecks int      `json:"completedChecks"`
	RunningChecks   int      `json:"runningChecks"`
	SpentSeconds    float64  `json:"spentSeconds"`
	PreemptRequired bool     `json:"preemptRequired"`
}

func PlanSchedule(target TargetSnapshot, cmd core.Command) (ScheduleImpact, error) {
	var out ScheduleImpact
	var candidates []core.Candidate
	var runs []RunSnapshot
	if len(target.Pipeline) > 0 {
		runs = target.Pipeline
	} else if target.InFlight != nil {
		runs = []RunSnapshot{*target.InFlight}
	}
	for _, run := range runs {
		if len(run.Members) > 0 {
			candidates = append(candidates, run.Members...)
		} else {
			candidates = append(candidates, run.Candidate)
		}
	}
	for _, entry := range target.Waiting {
		candidates = append(candidates, entry.Candidate)
	}
	// Parked changes may receive a priority, but moving one does not retry it.
	for _, entry := range target.Parked {
		candidates = append(candidates, entry.Candidate)
	}
	byRef := map[string]core.Candidate{}
	var original []string
	for _, candidate := range candidates {
		if _, ok := byRef[candidate.Ref]; !ok {
			original = append(original, candidate.Ref)
			byRef[candidate.Ref] = candidate
		}
	}
	data, _ := json.Marshal([]any{original, runsIDs(runs), candidates})
	out.Token = fmt.Sprintf("%x", sha256.Sum256(data))
	selected := map[string]bool{}
	for _, revision := range cmd.Revisions {
		candidate, ok := byRef[revision.Ref]
		if !ok || candidate.SHA != revision.SHA || candidate.Version != revision.Version || selected[revision.Ref] {
			return out, fmt.Errorf("selected revision changed or disappeared")
		}
		selected[revision.Ref] = true
	}
	if len(selected) == 0 || len(selected) > 64 {
		return out, fmt.Errorf("select 1..64 exact revisions")
	}
	order := append([]string(nil), original...)
	if cmd.Kind == core.CommandMove || cmd.Preempt {
		if cmd.Before != "" && cmd.After != "" {
			return out, fmt.Errorf("choose before or after, not both")
		}
		anchor := cmd.Before
		if anchor == "" {
			anchor = cmd.After
		}
		if cmd.Kind == core.CommandMove && anchor == "" {
			return out, fmt.Errorf("move requires before or after")
		}
		if anchor != "" {
			if _, ok := byRef[anchor]; !ok || selected[anchor] {
				return out, fmt.Errorf("anchor is unavailable or selected")
			}
		}
		if cmd.Kind == core.CommandMove {
			anchorRank, _ := PriorityRank(byRef[anchor].Priority)
			for ref := range selected {
				rank, _ := PriorityRank(byRef[ref].Priority)
				if rank != anchorRank {
					return out, fmt.Errorf("move within one priority class; set priority first")
				}
			}
		}
		// Required parents travel with an explicit urgent preemption. A move must
		// supply a dependency-valid position rather than silently moving parents.
		if cmd.Kind != core.CommandMove {
			var include func(string)
			include = func(ref string) {
				parent := byRef[ref].DependsOn
				if parent != "" && !selected[parent] {
					if _, ok := byRef[parent]; ok {
						selected[parent] = true
						include(parent)
					}
				}
			}
			for ref := range selected {
				include(ref)
			}
		}
		var block, remaining []string
		for _, ref := range original {
			if selected[ref] {
				block = append(block, ref)
			} else {
				remaining = append(remaining, ref)
			}
		}
		position := 0
		if anchor != "" {
			for i, ref := range remaining {
				if ref == anchor {
					position = i
					if cmd.After != "" {
						position++
					}
					break
				}
			}
		}
		order = append(append(append([]string(nil), remaining[:position]...), block...), remaining[position:]...)
		positions := map[string]int{}
		for i, ref := range order {
			positions[ref] = i
		}
		for ref, candidate := range byRef {
			if parent, ok := positions[candidate.DependsOn]; ok && parent > positions[ref] {
				return out, fmt.Errorf("move would place %s before its prerequisite", ref)
			}
		}
		changed := len(original)
		for i := range order {
			if order[i] != original[i] {
				changed = i
				break
			}
		}
		offset := 0
		for _, run := range runs {
			count := len(run.Members)
			if count == 0 {
				count = 1
			}
			if offset+count > changed {
				out.AffectedRuns = append(out.AffectedRuns, run.RunID)
				out.CompletedChecks += len(run.Done)
				out.RunningChecks += len(run.Running)
				if len(run.Running) == 0 && run.Current != nil {
					out.RunningChecks++
				}
				for _, check := range run.Done {
					out.SpentSeconds += check.Duration.Seconds()
				}
			}
			offset += count
		}
	}
	if cmd.Kind != core.CommandMove && !cmd.Preempt {
		class := cmd.Priority
		if cmd.Kind == core.CommandUrgent {
			class = "urgent"
		}
		if _, ok := PriorityRank(class); !ok {
			return out, fmt.Errorf("unknown priority")
		}
		for ref := range selected {
			c := byRef[ref]
			c.Priority = class
			c.Urgent = false
			byRef[ref] = c
		}
		fixed := 0
		for _, run := range runs {
			fixed += max(1, len(run.Members))
		}
		ranks := make(map[string]int, len(order))
		var rank func(string, map[string]bool) int
		rank = func(ref string, seen map[string]bool) int {
			if seen[ref] {
				return 0
			}
			seen[ref] = true
			r, _ := PriorityRank(byRef[ref].Priority)
			for child, c := range byRef {
				if c.DependsOn == ref {
					r = max(r, rank(child, seen))
				}
			}
			return r
		}
		for _, ref := range order {
			ranks[ref] = rank(ref, map[string]bool{})
		}
		pending := append([]string(nil), order[fixed:]...)
		sort.SliceStable(pending, func(i, j int) bool { return ranks[pending[i]] > ranks[pending[j]] })
		placed := map[string]bool{}
		for _, ref := range order[:fixed] {
			placed[ref] = true
		}
		order = order[:fixed]
		var place func(string)
		place = func(ref string) {
			if placed[ref] {
				return
			}
			placed[ref] = true
			parent := byRef[ref].DependsOn
			if _, ok := byRef[parent]; ok {
				place(parent)
			}
			order = append(order, ref)
		}
		for _, ref := range pending {
			place(ref)
		}
	}
	out.Order = order
	out.PreemptRequired = len(out.AffectedRuns) > 0
	return out, nil
}

func runsIDs(runs []RunSnapshot) []string {
	ids := make([]string, len(runs))
	for i, run := range runs {
		ids[i] = run.RunID
	}
	return ids
}
func (d *Daemon) ownPriority(c core.Candidate, now time.Time) int {
	value, _ := PriorityRank(c.Priority)
	if c.Urgent {
		value = 3
	}
	if entry, ok := d.controls.Priorities[c.Ref]; ok && entry.Revision.SHA == c.SHA && entry.Revision.Version == c.Version {
		value, _ = PriorityRank(entry.Class)
	}
	if at := d.controls.Enqueued[c.Ref]; !at.IsZero() && value < 2 {
		age := int(now.Sub(at) / (30 * time.Minute))
		if age > 0 {
			value = min(2, value+age)
		}
	}
	return value
}

func (d *Daemon) priorityAt(target, ref string, cands map[string]core.Candidate, now time.Time) int {
	seen := map[string]bool{}
	var rank func(string) int
	rank = func(ref string) int {
		if seen[ref] {
			return 0
		}
		seen[ref] = true
		value := d.ownPriority(cands[ref], now)
		for child, next := range cands {
			if next.DependsOn == ref {
				if childRank := rank(child); childRank > value {
					value = childRank
				}
			}
		}
		return value
	}
	return rank(ref)
}

func (d *Daemon) applyScheduling(ctx context.Context, cmd core.Command, target config.Target, refs map[string]string) {
	if d.draining {
		d.controls.LastError = "queue is draining"
		return
	}
	if cmd.Kind == core.CommandUrgent {
		cmd.Priority = "urgent"
	}
	if _, ok := PriorityRank(cmd.Priority); !ok {
		d.controls.LastError = "priority must be low, normal, high, or urgent"
		return
	}
	if cmd.Kind != core.CommandMove && cmd.Preempt && cmd.Priority != "urgent" {
		d.controls.LastError = "preemption requires urgent priority"
		return
	}
	cands := discoverCandidates(cmd.Target, refs)
	for ref, c := range d.external {
		if c.Target == cmd.Target {
			cands[ref] = c
		}
	}
	d.syncBookkeeping(ctx, target, cands)
	snapshot := d.buildSnapshot(refs)
	var state TargetSnapshot
	for _, t := range snapshot.Targets {
		if t.Name == cmd.Target {
			state = t
			break
		}
	}
	impact, err := PlanSchedule(state, cmd)
	if err != nil {
		d.controls.LastError = err.Error()
		return
	}
	if cmd.PlanToken != "" && cmd.PlanToken != impact.Token {
		d.controls.LastError = "queue changed since preview; refresh and preview again"
		return
	}
	if impact.PreemptRequired && !cmd.Preempt {
		d.controls.LastError = "move affects active verification; explicit preemption required"
		return
	}
	next := d.controls.clone()
	next.Order = cloneNested(d.order)
	next.LastError = ""
	if next.Scheduled == nil {
		next.Scheduled = map[string]string{}
	}
	if cmd.RequestID != "" {
		for _, rev := range cmd.Revisions {
			next.Scheduled[rev.Ref] = cmd.RequestID
		}
	}
	if next.Priorities == nil {
		next.Priorities = map[string]priorityEntry{}
	}
	if cmd.Kind != core.CommandMove {
		for _, rev := range cmd.Revisions {
			next.Priorities[rev.Ref] = priorityEntry{Revision: rev, Class: cmd.Priority}
			if next.Enqueued == nil {
				next.Enqueued = map[string]time.Time{}
			}
			next.Enqueued[rev.Ref] = d.now()
		}
	}
	if cmd.Kind == core.CommandMove || cmd.Preempt {
		ordered := make(map[string]int64, len(impact.Order))
		for _, ref := range impact.Order {
			ordered[ref] = d.seq
			d.seq++
		}
		next.Order[cmd.Target] = ordered
	}
	next.Audit = append(next.Audit, controlAudit{d.now(), cmd})
	if !d.saveControls(next) {
		return
	}
	if cmd.Kind == core.CommandMove || cmd.Preempt {
		d.order[cmd.Target] = maps.Clone(next.Order[cmd.Target])
	}
	if len(impact.AffectedRuns) > 0 {
		if lane := d.lanes[cmd.Target]; lane != nil {
			first := 0
			for i, run := range lane.runs {
				if run.runID == impact.AffectedRuns[0] {
					first = i
					break
				}
			}
			d.invalidateSuffix(ctx, target, lane, first, "operator preemption: "+cmd.Reason)
			lane.runs = lane.runs[:first]
		}
	}
}

func (d *Daemon) runUrgent(r *run) bool {
	candidates := maps.Clone(d.external)
	if candidates == nil {
		candidates = map[string]core.Candidate{}
	}
	for _, member := range r.members {
		candidates[member.cand.Ref] = member.cand
	}
	for _, member := range r.members {
		if d.urgent(member.cand.Target, member.cand.Ref, candidates) {
			return true
		}
	}
	return false
}
