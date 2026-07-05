// Package config parses gauntlet's two KDL config files into plain structs:
// the admin-written daemon config and the repo-side check spec. This is the
// only package that touches KDL, so the config language stays swappable
// (docs/plans/phase1.md §9.8) and callers depend on the structs and
// LoadDaemon/ParseChecks signatures, never on kdl-go directly.
//
// kdl-go's unmarshaler has thin validation (no required-field or
// non-negative-value enforcement), so every exported load function here runs
// a Go-side validation pass afterward; its errors name the offending
// node/field.
package config

import (
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	kdl "github.com/sblinch/kdl-go"

	"github.com/sgrankin/gauntlet/internal/core"
)

// defaultPoll and defaultCheckSpec are applied when the corresponding node
// is absent from the daemon config.
const (
	defaultPoll      = 10 * time.Second
	defaultCheckSpec = ".gauntlet.kdl"
)

// Daemon is the admin-written daemon config (docs/plans/phase1.md §4): one
// remote, the reconcile cadence, the committer identity used for merge
// commits, the merge-message template, and the target branches to
// reconcile.
type Daemon struct {
	Remote    string        `kdl:"remote"`
	Poll      time.Duration `kdl:"poll-interval,format:units"`
	CheckSpec string        `kdl:"check-spec"`
	Committer core.Identity `kdl:"committer"`
	MergeMsg  string        `kdl:"merge-message"`
	Targets   []Target      `kdl:"target,multiple"`
}

// Target is one target branch the daemon reconciles candidates onto. Name
// is the queue-grammar name parsed out of candidate refs
// (refs/heads/for/<name>/...) and must not contain '/'; Branch is the
// actual git branch and may (docs/plans/phase1.md §9.3).
type Target struct {
	Name   string `kdl:",arg"`
	Branch string `kdl:"branch"`
}

// LoadDaemon reads, parses, and validates the daemon config at path.
func LoadDaemon(path string) (*Daemon, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var d Daemon
	if err := kdl.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	d.applyDefaults()
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &d, nil
}

func (d *Daemon) applyDefaults() {
	// Poll's zero value is indistinguishable from "node absent" (kdl-go
	// unmarshals a missing node into the field's zero value); an explicit
	// negative poll-interval is not, so validate() still rejects it after
	// this default is applied.
	if d.Poll == 0 {
		d.Poll = defaultPoll
	}
	if d.CheckSpec == "" {
		d.CheckSpec = defaultCheckSpec
	}
}

func (d *Daemon) validate() error {
	if d.Remote == "" {
		return fmt.Errorf("remote: must not be empty")
	}
	if d.Poll <= 0 {
		return fmt.Errorf("poll-interval: must be positive, got %s", d.Poll)
	}
	if d.Committer.Name == "" {
		return fmt.Errorf("committer: name must not be empty")
	}
	if d.Committer.Email == "" {
		return fmt.Errorf("committer: email must not be empty")
	}
	if _, err := template.New("merge-message").Parse(d.MergeMsg); err != nil {
		return fmt.Errorf("merge-message: %w", err)
	}
	if len(d.Targets) == 0 {
		return fmt.Errorf("no target defined")
	}
	seen := make(map[string]bool, len(d.Targets))
	for _, t := range d.Targets {
		if t.Name == "" {
			return fmt.Errorf("target: name must not be empty")
		}
		if strings.Contains(t.Name, "/") {
			return fmt.Errorf("target %q: name must not contain '/'", t.Name)
		}
		if t.Branch == "" {
			return fmt.Errorf("target %q: branch missing", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("target %q: duplicate", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}
