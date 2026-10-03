package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/policy"
)

type PolicyAudit struct {
	At                                 time.Time
	Target, Phase, Ref, SHA, InputHash string
	Decision                           policy.Decision
	Version, Error                     string
}

func (d *Daemon) evaluatePolicy(ctx context.Context, t config.Target, c core.Candidate, base string, members []core.Candidate, checks []core.CheckResult, phase string, skipChecks, overridePause bool, prepared ...policy.Input) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	input := policy.Input{SchemaVersion: 1, Forge: map[string]any{}}
	var factsErr error
	if len(prepared) > 0 {
		input = prepared[0]
	} else if source, ok := d.cfg.Reviews.(interface {
		PolicyInput(context.Context, core.Candidate) (policy.Input, error)
	}); ok && c.Source != "" {
		input, factsErr = source.PolicyInput(ctx, c)
	} else if d.cfg.Policy.HasCustom("submission") {
		if source, ok := d.cfg.Reviews.(interface {
			PolicyFacts(context.Context, core.Candidate) (map[string]any, error)
		}); ok && c.Source != "" {
			input.Forge, factsErr = source.PolicyFacts(ctx, c)
			if paths, ok := input.Forge["paths"].([]string); ok {
				input.Paths = paths
				input.PathsAvailable = true
			}
		} else if git, ok := d.git.(interface {
			ChangedPaths(context.Context, string, string) ([]string, error)
		}); ok {
			input.Paths, factsErr = git.ChangedPaths(ctx, base, c.SHA)
			input.PathsAvailable = factsErr == nil
		}
	}
	input.Phase, input.Target, input.Branch, input.BaseSHA = phase, t.Name, t.Branch, base
	input.Candidate, input.Stack, input.Checks = policyCandidate(c), policyMembers(members), policyChecks(checks)
	// Adapters without a policy fact contract own their source validation.
	_, sharedFacts := d.cfg.Reviews.(interface {
		PolicyInput(context.Context, core.Candidate) (policy.Input, error)
	})
	if !sharedFacts && c.Source != "" && !d.cfg.Policy.HasCustom("submission") {
		input.AdapterValidated = true
	}
	input.Emergency = policy.Emergency{SkipChecks: skipChecks, OverridePause: overridePause}
	decision, err := d.cfg.Policy.Decide(ctx, "submission", input)
	if factsErr != nil {
		err = fmt.Errorf("policy facts unavailable: %w", factsErr)
		decision.Allow = false
	}
	if !d.recordPolicyDecision(c.Ref, c.SHA, phase, input, decision, err) {
		return fmt.Errorf("cannot persist policy audit")
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
	all := maps.Clone(cands)
	for ref, c := range all {
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
			p, ok := all[parent]
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

func (d *Daemon) authorizeCommand(ctx context.Context, cmd core.Command) bool {
	principal := policy.Principal{}
	if cmd.Principal != nil {
		principal = *cmd.Principal
	}
	input := policy.Input{SchemaVersion: 1, Phase: "command", Principal: principal, Target: cmd.Target, Command: policy.Command{Kind: cmd.Kind, ID: cmd.RequestID, Reason: cmd.Reason}, Settings: policy.Settings{EmergencyEnabled: d.cfg.AllowEmergency}, Emergency: policy.Emergency{SkipChecks: cmd.Kind == core.CommandMergeAnyway, OverridePause: cmd.OverridePause}}
	if principal.Source == "github" && len(cmd.Revisions) > 0 {
		if source, ok := d.cfg.Reviews.(interface {
			PolicyInput(context.Context, core.Candidate) (policy.Input, error)
		}); ok {
			candidate, exists := d.external[cmd.Revisions[0].Ref]
			if !exists {
				d.controls.LastError = "command revision unavailable"
				return false
			}
			prepared, err := source.PolicyInput(ctx, candidate)
			if err != nil {
				d.controls.LastError = err.Error()
				return false
			}
			prepared.Phase = "command"
			input = prepared
		}
	}
	decision, err := d.cfg.Policy.Decide(ctx, "command", input)
	if !d.recordPolicyDecision(cmd.Target+":"+cmd.Ref, "", "command:"+cmd.Kind, input, decision, err) {
		d.controls.LastError = "cannot persist command policy audit"
		return false
	}
	if err != nil {
		d.controls.LastError = "command policy: " + err.Error()
		return false
	}
	if !decision.Allow {
		d.controls.LastError = "command policy: " + decision.Reason()
		return false
	}
	return true
}

func (d *Daemon) recordPolicyDecision(ref, sha, phase string, input any, decision policy.Decision, err error) bool {
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	audit := PolicyAudit{At: d.now(), Phase: phase, Ref: ref, SHA: sha, InputHash: hex.EncodeToString(sum[:]), Decision: decision, Version: d.cfg.Policy.Version}
	if facts, ok := input.(policy.Input); ok {
		audit.Target = facts.Target
	}
	if err != nil {
		audit.Error = err.Error()
	}
	// Audit only changed inputs/decisions, to avoid writing on every poll.
	changed := true
	for i := len(d.controls.PolicyAudit) - 1; i >= 0; i-- {
		previous := d.controls.PolicyAudit[i]
		if previous.Ref == ref && previous.Phase == phase {
			changed = previous.InputHash != audit.InputHash || previous.Version != audit.Version || previous.Error != audit.Error
			break
		}
	}
	if changed {
		next := d.controls.clone()
		next.PolicyAudit = append(next.PolicyAudit, audit)
		if len(next.PolicyAudit) > 500 {
			next.PolicyAudit = next.PolicyAudit[len(next.PolicyAudit)-500:]
		}
		if !d.saveControls(next) {
			return false
		}
	}

	return true
}

func (d *Daemon) executionPolicy(ctx context.Context, t config.Target, base, sha string, members []core.Candidate, spec *config.CheckSpec) error {
	input := policy.Input{SchemaVersion: 1, Phase: "execution", Target: t.Name, Branch: t.Branch, BaseSHA: base, Stack: policyMembers(members), Execution: policy.Specification(spec)}
	decision, err := d.cfg.Policy.Decide(ctx, "execution", input)
	if !d.recordPolicyDecision(t.Name, sha, "execution", input, decision, err) {
		return fmt.Errorf("cannot persist execution policy audit")
	}
	if err != nil {
		return fmt.Errorf("execution policy: %w", err)
	}
	if !decision.Allow {
		return fmt.Errorf("execution policy: %s", decision.Reason())
	}
	return nil
}
