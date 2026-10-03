package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
)

type PolicyAudit struct {
	At                         time.Time
	Phase, Ref, SHA, InputHash string
	Decision                   policy.Decision
	Version, Error             string
}

func (d *Daemon) evaluatePolicy(ctx context.Context, t config.Target, c core.Candidate, base string, members []core.Candidate, checks []core.CheckResult, phase string, skipChecks, overridePause bool) error {
	if d.cfg.Policy == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	input := map[string]any{"schema_version": 1, "phase": phase, "target": t.Name, "branch": t.Branch, "base_sha": base, "candidate": policyCandidate(c), "stack": policyMembers(members), "checks": policyChecks(checks), "emergency": map[string]bool{"skip_checks": skipChecks, "override_pause": overridePause}, "forge": nil, "paths": nil, "paths_available": false}
	var factsErr error
	if source, ok := d.cfg.Reviews.(interface {
		PolicyFacts(context.Context, core.Candidate) (map[string]any, error)
	}); ok && c.Source != "" {
		facts, ferr := source.PolicyFacts(ctx, c)
		factsErr = ferr
		input["forge"] = facts
		if facts != nil {
			if paths, ok := facts["paths"]; ok {
				input["paths"] = paths
				input["paths_available"] = true
			}
		}
	} else if git, ok := d.git.(interface {
		ChangedPaths(context.Context, string, string) ([]string, error)
	}); ok {
		paths, err := git.ChangedPaths(ctx, base, c.SHA)
		factsErr = err
		if err == nil {
			input["paths"] = paths
			input["paths_available"] = true
		}
	}
	decision, err := d.cfg.Policy.Evaluate(ctx, input)
	if factsErr != nil {
		err = fmt.Errorf("policy facts unavailable: %w", factsErr)
		decision.Allow = false
	}
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	audit := PolicyAudit{At: d.now(), Phase: phase, Ref: c.Ref, SHA: c.SHA, InputHash: hex.EncodeToString(sum[:]), Decision: decision, Version: d.cfg.Policy.Version}
	if err != nil {
		audit.Error = err.Error()
	}
	// Audit only changed inputs/decisions, to avoid writing on every poll.
	changed := true
	for i := len(d.controls.PolicyAudit) - 1; i >= 0; i-- {
		previous := d.controls.PolicyAudit[i]
		if previous.Ref == c.Ref && previous.Phase == phase {
			changed = previous.InputHash != audit.InputHash || previous.Version != audit.Version || previous.Error != audit.Error
			break
		}
	}
	if changed {
		data, _ := json.Marshal(d.controls)
		var next controlState
		json.Unmarshal(data, &next)
		next.PolicyAudit = append(next.PolicyAudit, audit)
		if len(next.PolicyAudit) > 500 {
			next.PolicyAudit = next.PolicyAudit[len(next.PolicyAudit)-500:]
		}
		if !d.saveControls(next) {
			return fmt.Errorf("cannot persist policy audit")
		}
	}
	if err != nil {
		return err
	}
	if !decision.Allow {
		return fmt.Errorf("policy denied: %s", decision.Reason())
	}
	return nil
}

func (d *Daemon) policyAdmission(ctx context.Context, t config.Target, cands map[string]core.Candidate, base string) {
	if d.cfg.Policy == nil {
		return
	}
	for ref, c := range cands {
		cmd, emergency := d.controls.Emergency[t.Name]
		selected := false
		for _, rev := range cmd.Revisions {
			if rev.Ref == ref && rev.SHA == c.SHA && rev.Version == c.Version {
				selected = true
			}
		}
		members := []core.Candidate{c}
		seen := map[string]bool{c.Ref: true}
		parent := c.DependsOn
		for parent != "" {
			p, ok := cands[parent]
			if !ok || seen[parent] {
				break
			}
			seen[parent] = true
			members = append([]core.Candidate{p}, members...)
			parent = p.DependsOn
		}
		err := d.evaluatePolicy(ctx, t, c, base, members, nil, "admission", emergency && selected && cmd.Kind == core.CommandMergeAnyway, cmd.OverridePause)
		if err != nil {
			delete(cands, ref)
			if feedback, ok := d.cfg.Reviews.(interface {
				Feedback(context.Context, core.Event) error
			}); ok && c.Source != "" {
				_ = feedback.Feedback(ctx, core.Event{Candidate: c, Record: &core.RunRecord{Outcome: core.OutcomeRejected, Detail: err.Error()}})
			}
			d.controls.LastError = strings.TrimSpace(err.Error())
		}
	}
}

func policyCandidate(c core.Candidate) map[string]any {
	return map[string]any{"ref": c.Ref, "sha": c.SHA, "version": c.Version, "source": c.Source, "source_base": c.SourceBase, "depends_on": c.DependsOn, "requester": c.Requester, "urgent": c.Urgent}
}
func policyMembers(members []core.Candidate) []map[string]any {
	out := []map[string]any{}
	for _, c := range members {
		out = append(out, policyCandidate(c))
	}
	return out
}
func policyChecks(checks []core.CheckResult) []map[string]any {
	out := []map[string]any{}
	for _, check := range checks {
		status := "failed"
		switch check.Status {
		case core.CheckPassed:
			status = "passed"
		case core.CheckSkipped:
			status = "skipped"
		case core.CheckBlocked:
			status = "blocked"
		case core.CheckWaived:
			status = "waived"
		}
		if check.Err != nil {
			status = "error"
		}
		out = append(out, map[string]any{"name": check.Name, "status": status})
	}
	return out
}
