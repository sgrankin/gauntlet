package config

import (
	"fmt"
	"slices"
	"time"
)

type Policy struct {
	Replace []string      `kdl:"replace"`
	Extend  []string      `kdl:"extend"`
	Rego    string        `kdl:"rego"`
	File    string        `kdl:"rego-file"`
	Timeout time.Duration `kdl:"timeout,format:units"`
	Teams   []string      `kdl:"teams"`
}

func (p *Policy) defaults() {
	if len(p.Replace) == 0 && len(p.Extend) == 0 {
		p.Extend = []string{"submission"}
	}
	if p.Timeout == 0 {
		p.Timeout = 100 * time.Millisecond
	}
}
func (p *Policy) validate() error {
	if (p.Rego == "") == (p.File == "") {
		return fmt.Errorf("policy: use exactly one of rego or rego-file")
	}
	if p.Timeout <= 0 || p.Timeout > time.Second {
		return fmt.Errorf("policy: timeout must be >0 and <=1s")
	}
	seen := map[string]bool{}
	for _, name := range append(slices.Clone(p.Replace), p.Extend...) {
		if !slices.Contains([]string{"command", "submission", "execution", "deployment", "retry"}, name) || seen[name] {
			return fmt.Errorf("policy: unknown or duplicate decision %q", name)
		}
		seen[name] = true
	}
	if len(p.Teams) > 32 {
		return fmt.Errorf("policy: at most 32 teams")
	}
	return nil
}
