package queue

import (
	"context"
	"fmt"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

func (d *Daemon) linearTarget(name string) bool {
	for _, t := range d.cfg.Targets {
		if t.Name == name {
			return t.Landing == "squash"
		}
	}
	return false
}

func (d *Daemon) candidateLanded(ctx context.Context, t config.Target, c core.Candidate, tip string) (bool, error) {
	if t.Landing != "squash" {
		return d.git.IsAncestor(ctx, c.SHA, tip)
	}
	g, ok := d.git.(core.LinearGitRepo)
	if !ok {
		return false, fmt.Errorf("git backend does not support linear landings")
	}
	version := c.Version
	if c.Source != "" {
		// A landing is final even if the PR description or queue command
		// changes between the target push and host acknowledgement.
		version = "*"
	}
	sha, err := g.FindLanding(ctx, tip, c.Ref, c.SHA, version)
	if err == nil && sha == "" && c.Source == "" {
		return d.git.IsAncestor(ctx, c.SHA, tip)
	}
	return sha != "", err
}
