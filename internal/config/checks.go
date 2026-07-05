package config

import (
	"fmt"

	kdl "github.com/sblinch/kdl-go"
)

// CheckSpec is the repo-side check spec (docs/plans/phase1.md §4,
// `.gauntlet.kdl`): every adopter writes this, and it's read out of the
// candidate's own trial tree (never from a file path) so a candidate that
// changes its checks is tested by its own definition.
type CheckSpec struct {
	Checks []Check `kdl:"check,multiple"`
}

// Check is one named check: a command to run against the exported trial
// tree. See core.CheckJob and docs/plans/phase1.md §5A for the
// environment/verdict contract the executor applies when it runs Command.
type Check struct {
	Name    string   `kdl:",arg"`
	Command []string `kdl:"command,child"`
}

// ParseChecks parses and validates a check spec's raw content, as read from
// a git tree via GitRepo.ReadFileFromTree — this does not take a file path.
func ParseChecks(data []byte) (*CheckSpec, error) {
	var cs CheckSpec
	if err := kdl.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("config: check spec: %w", err)
	}
	if err := cs.validate(); err != nil {
		return nil, fmt.Errorf("config: check spec: %w", err)
	}
	return &cs, nil
}

func (cs *CheckSpec) validate() error {
	if len(cs.Checks) == 0 {
		return fmt.Errorf("no checks defined")
	}
	seen := make(map[string]bool, len(cs.Checks))
	for _, c := range cs.Checks {
		if c.Name == "" {
			return fmt.Errorf("check: name must not be empty")
		}
		if len(c.Command) == 0 {
			return fmt.Errorf("check %q: command must not be empty", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("check %q: duplicate", c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}
