package policy

import "github.com/sgrankin/gauntlet/internal/core"

type Principal = core.Principal
type Command struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	Urgent bool   `json:"urgent"`
}
type Settings struct {
	Approvals             int      `json:"approvals"`
	RequiredChecks        []string `json:"required_checks"`
	ResolvedConversations bool     `json:"resolved_conversations"`
	EmergencyEnabled      bool     `json:"emergency_enabled"`
}
type Emergency struct {
	SkipChecks    bool `json:"skip_checks"`
	OverridePause bool `json:"override_pause"`
}

// Input is the envelope shared by decision entry points. Domain facts remain
// explicit JSON objects so adapters can expose their own forge vocabulary.
type Input struct {
	AdapterValidated bool             `json:"adapter_validated"`
	SchemaVersion    int              `json:"schema_version"`
	Phase            string           `json:"phase"`
	Principal        Principal        `json:"principal"`
	Command          Command          `json:"command"`
	Settings         Settings         `json:"settings"`
	Emergency        Emergency        `json:"emergency"`
	Target           string           `json:"target"`
	Branch           string           `json:"branch"`
	BaseSHA          string           `json:"base_sha"`
	Candidate        map[string]any   `json:"candidate"`
	Forge            map[string]any   `json:"forge"`
	Stack            []map[string]any `json:"stack"`
	Checks           []map[string]any `json:"checks"`
	Paths            []string         `json:"paths"`
	PathsAvailable   bool             `json:"paths_available"`
	Execution        map[string]any   `json:"execution"`
	Deployment       map[string]any   `json:"deployment"`
	Retry            map[string]any   `json:"retry"`
}
