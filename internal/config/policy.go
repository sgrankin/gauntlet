package config

import (
	"fmt"
	"time"
)

type Policy struct {
	Rego    string        `kdl:"rego"`
	File    string        `kdl:"rego-file"`
	Timeout time.Duration `kdl:"timeout,format:units"`
	Teams   []string      `kdl:"teams"`
}

func (p *Policy) defaults() {
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
	if len(p.Teams) > 32 {
		return fmt.Errorf("policy: at most 32 teams")
	}
	return nil
}
