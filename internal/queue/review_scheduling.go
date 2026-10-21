package queue

import (
	"context"
	"sort"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
)

// Forge priority is declarative; preemption is applied once per request, even
// when polling rediscovers the same request after a scheduler restart.
func (d *Daemon) reviewScheduling(ctx context.Context, t config.Target, refs map[string]string, cands map[string]core.Candidate) {
	groups := map[string][]core.Candidate{}
	for _, c := range cands {
		if c.Preempt && !c.SkipChecks && c.RequestID != "" && d.controls.Scheduled[c.Ref] != c.RequestID {
			groups[c.RequestID] = append(groups[c.RequestID], c)
		}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		members := groups[id]
		if len(members) != members[0].RequestedCount {
			continue
		}
		source, ok := d.cfg.Reviews.(interface {
			PolicyInput(context.Context, core.Candidate) (policy.Input, error)
		})
		if !ok {
			continue
		}
		facts, err := source.PolicyInput(ctx, members[0])
		if err != nil {
			d.controls.LastError = err.Error()
			continue
		}
		cmd := core.Command{Kind: core.CommandPriority, Target: t.Name, Priority: "urgent", Preempt: true, Actor: "github:" + members[0].Requester, Reason: members[0].RequestReason, RequestID: id, Principal: &facts.Principal}
		for _, c := range members {
			cmd.Revisions = append(cmd.Revisions, core.Revision{Ref: c.Ref, SHA: c.SHA, Version: c.Version})
		}
		d.applyControl(ctx, cmd, refs)
	}
}
