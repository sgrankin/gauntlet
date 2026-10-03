package policy

import "github.com/sgrankin/gauntlet/internal/config"

// Specification describes requested execution, before any candidate command runs.
func Specification(spec *config.CheckSpec) map[string]any {
	nodes := []map[string]any{}
	for _, c := range spec.Checks {
		nodes = append(nodes, map[string]any{"name": c.Name, "kind": "check", "executor": c.Executor, "command": c.Command, "after": c.After, "image": c.Image, "needs": c.Needs})
	}
	for _, image := range spec.Images {
		nodes = append(nodes, map[string]any{"name": image.Name, "kind": "image", "executor": image.Executor, "command": image.Command})
	}
	if receipt := spec.Receipt(); receipt != nil {
		nodes = append(nodes, map[string]any{"name": "receipt", "kind": "receipt", "executor": receipt.Executor, "command": receipt.Command})
	}
	services := []map[string]any{}
	for _, service := range spec.Services {
		services = append(services, map[string]any{"name": service.Name, "image": service.Image, "port": service.Port, "ready_command": service.ReadyCommand})
	}
	return map[string]any{"kind": "checks", "workspace": spec.Workspace, "max_parallel": spec.MaxParallel, "nodes": nodes, "services": services}
}
