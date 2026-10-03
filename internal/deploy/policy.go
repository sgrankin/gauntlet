package deploy

import (
	"context"
	"fmt"
	"github.com/sgrankin/gauntlet/internal/policy"
)

func (t *Tracker) decideDeployment(ctx context.Context, env Environment, phase, desired, observed string, nodes []Node) error {
	input := policy.Input{SchemaVersion: 1, Phase: phase, Deployment: map[string]any{"environment": env.Name, "mode": env.Mode, "source_branch": env.SourceBranch, "source_environment": env.SourceEnv, "desired": desired, "observed": observed}}
	name := "deployment"
	if phase == "execution" {
		name = "execution"
		facts := []map[string]any{}
		for _, n := range nodes {
			facts = append(facts, map[string]any{"name": n.Name, "kind": "deploy", "executor": n.Executor, "command": n.Command, "after": n.After})
		}
		input.Execution = map[string]any{"kind": "deploy", "nodes": facts, "services": []map[string]any{}}
	}
	if t.policy.HasCustom(name) {
		refs, err := t.git.ListRefs(ctx)
		if err != nil {
			return fmt.Errorf("deployment policy refs: %w", err)
		}
		local, err := t.git.ListLocalRefs(ctx, "refs/gauntlet/deployed/")
		if err != nil {
			return fmt.Errorf("deployment policy observed refs: %w", err)
		}
		input.Deployment["current_desired"] = refs[DesiredRef(env.Name)]
		input.Deployment["current_observed"] = local[ObservedRef(env.Name)]
		input.Deployment["source_tip"] = refs["refs/heads/"+env.SourceBranch]
		if env.SourceEnv != "" {
			input.Deployment["source_tip"] = local[ObservedRef(env.SourceEnv)]
		}
	}
	decision, err := t.policy.Decide(ctx, name, input)
	if err != nil {
		return fmt.Errorf("%s policy: %w", name, err)
	}
	if !decision.Allow {
		return fmt.Errorf("%s policy: %s", name, decision.Reason())
	}
	return nil
}
