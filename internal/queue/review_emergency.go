package queue

import (
	"context"
	"sort"
	"strconv"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
)

func (d *Daemon) reviewEmergency(ctx context.Context, t config.Target, refs map[string]string, cands map[string]core.Candidate) {
	groups := map[string][]core.Candidate{}
	for _, c := range cands {
		if c.SkipChecks && c.EmergencyID != "" {
			groups[c.EmergencyID] = append(groups[c.EmergencyID], c)
		}
	}
	ids := []string{}
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.ParseInt(ids[i], 10, 64)
		b, _ := strconv.ParseInt(ids[j], 10, 64)
		return a < b
	})
	for _, id := range ids {
		number, err := strconv.ParseInt(id, 10, 64)
		if err != nil || number <= d.controls.ReviewFloor || d.controls.ProcessedReviews[id] {
			continue
		}
		members := groups[id]
		if len(members) != members[0].RequestedCount {
			continue
		}
		ordered := []core.Candidate{}
		selected := map[string]bool{}
		for len(ordered) < len(members) {
			progress := false
			for _, c := range members {
				if selected[c.Ref] {
					continue
				}
				if c.DependsOn != "" && !selected[c.DependsOn] {
					continue
				}
				selected[c.Ref] = true
				ordered = append(ordered, c)
				progress = true
			}
			if !progress {
				break
			}
		}
		if len(ordered) != len(members) {
			continue
		}
		var principal *core.Principal
		if source, ok := d.cfg.Reviews.(interface {
			PolicyInput(context.Context, core.Candidate) (policy.Input, error)
		}); ok {
			input, err := source.PolicyInput(ctx, members[0])
			if err != nil {
				d.controls.LastError = err.Error()
				continue
			}
			principal = &input.Principal
		}
		cmd := core.Command{Principal: principal, Kind: core.CommandMergeAnyway, Target: t.Name, Actor: "github:" + members[0].Requester, Reason: members[0].RequestReason, OverridePause: members[0].OverridePause, RequestID: id}
		for _, c := range ordered {
			cmd.Revisions = append(cmd.Revisions, core.Revision{Ref: c.Ref, SHA: c.SHA, Version: c.Version})
		}
		d.applyControl(ctx, cmd, refs)
		return
	}
}
