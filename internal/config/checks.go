package config

import (
	"fmt"
	"time"

	kdl "github.com/sblinch/kdl-go"
)

// CheckSpec is the repo-side check spec (docs/plans/phase1.md §4,
// `.gauntlet.kdl`): every adopter writes this, and it's read out of the
// candidate's own trial tree (never from a file path) so a candidate that
// changes its checks is tested by its own definition.
type CheckSpec struct {
	Checks   []Check   `kdl:"check,multiple"`
	Services []Service `kdl:"service,multiple"`
}

// Check is one named check: a command to run against the exported trial
// tree. See core.CheckJob and docs/plans/phase1.md §5A for the
// environment/verdict contract the executor applies when it runs Command.
type Check struct {
	Name    string   `kdl:",arg"`
	Command []string `kdl:"command,child"`

	// Needs names the Services (by Service.Name, below) this check requires
	// ensured and reachable before it runs (docs/plans/services.md §1).
	// Every entry must match a Service declared in the same CheckSpec —
	// validate() enforces this so an undeclared need fails loudly at parse
	// time (ParseChecks error → OutcomeRejected), never silently at run
	// time. nil means the check has no service dependencies and is wholly
	// unaffected by the services machinery: a lint check shouldn't block on
	// (or keep warm) a database it never touches (services.md §1).
	Needs []string `kdl:"needs"`
}

// EnvVar is one `env "NAME" "VALUE"` pair set inside a service's container
// (docs/plans/services.md §1).
type EnvVar struct {
	Name  string `kdl:",arg"`
	Value string `kdl:",arg"`
}

// Service is one shared, cached service instance a check may declare a
// dependency on via Check.Needs (docs/plans/services.md §1, §2). Read from
// the trial-merged tree same as Check, so a branch that changes a service's
// image/env/probe is tested against its own declaration.
//
// EVERY field participates in the cache key (servicekey.go's ServiceKey):
// two service declarations differing in any field — including Name itself,
// so `service "mssql-a"`/`"mssql-b"` with identical bodies stay distinct on
// purpose — are distinct cache entries by design (services.md §2).
type Service struct {
	Name  string   `kdl:",arg"`
	Image string   `kdl:"image"`
	Port  int      `kdl:"port"`
	Env   []EnvVar `kdl:"env,multiple"`

	// ReadyCommand, when set, is run inside the instance (docker exec or
	// equivalent — services.md §6 review q2) in place of the default probe.
	// Absent (nil) means the default probe applies: a TCP dial of the
	// resolved endpoint by the daemon.
	ReadyCommand []string `kdl:"ready-command,child"`

	// ReadyTimeout bounds how long ensure polls the ready probe before
	// giving up (docs/plans/services.md §3). Zero after parsing means "left
	// unset" — ParseChecks calls applyServiceDefaults, which fills in
	// defaultReadyTimeout, BEFORE validate() and before any caller can hash
	// this struct via ServiceKey. That ordering matters: the default must
	// participate in the key at its effective value, not as an explicit
	// zero, so that a future change to the default recycles instances whose
	// specs relied on it (services.md §2, servicekey.go's ServiceKey doc).
	ReadyTimeout time.Duration `kdl:"ready-timeout,format:units"`

	// IdleTTL is how long an instance survives with no in-flight reference
	// before the reaper destroys it (docs/plans/services.md §3
	// "Eviction"). Same before-hashing default treatment as ReadyTimeout,
	// via applyServiceDefaults/defaultIdleTTL.
	IdleTTL time.Duration `kdl:"idle-ttl,format:units"`
}

// RequiresServices reports whether this spec declares any dependency on the
// shared-services machinery: at least one Service, or at least one Check
// with a non-empty Needs. The queue calls this right after ParseChecks
// (docs/plans/services-impl.md §4.4) to reject, loudly, a spec that wants
// services on a daemon with no `services` block configured — a spec
// validation error, like a malformed check (docs/plans/services.md §7),
// never a silent no-op.
func (cs *CheckSpec) RequiresServices() bool {
	if len(cs.Services) > 0 {
		return true
	}
	for _, c := range cs.Checks {
		if len(c.Needs) > 0 {
			return true
		}
	}
	return false
}

// applyServiceDefaults fills a Service's defaultable fields when left zero
// by the author. ParseChecks calls this on every declared Service BEFORE
// validate() and before any caller hashes it via ServiceKey
// (docs/plans/services-impl.md §2.4) — see Service.ReadyTimeout's doc for
// why the ordering matters.
func applyServiceDefaults(svc *Service) {
	if svc.ReadyTimeout == 0 {
		svc.ReadyTimeout = defaultReadyTimeout
	}
	if svc.IdleTTL == 0 {
		svc.IdleTTL = defaultIdleTTL
	}
}

// ParseChecks parses and validates a check spec's raw content, as read from
// a git tree via GitRepo.ReadFileFromTree — this does not take a file path.
func ParseChecks(data []byte) (*CheckSpec, error) {
	var cs CheckSpec
	if err := kdl.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("config: check spec: %w", err)
	}
	// Defaults are applied before validate() (which checks ReadyTimeout/
	// IdleTTL are positive) and before ServiceKey ever sees these structs —
	// see applyServiceDefaults's doc.
	for i := range cs.Services {
		applyServiceDefaults(&cs.Services[i])
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

	svcNames := make(map[string]bool, len(cs.Services))
	for _, s := range cs.Services {
		if s.Name == "" {
			return fmt.Errorf("service: name must not be empty")
		}
		if svcNames[s.Name] {
			return fmt.Errorf("service %q: duplicate", s.Name)
		}
		svcNames[s.Name] = true
		if s.Image == "" {
			return fmt.Errorf("service %q: image must not be empty", s.Name)
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("service %q: port must be between 1 and 65535, got %d", s.Name, s.Port)
		}
		// Checked positive AFTER applyServiceDefaults has run (ParseChecks's
		// ordering): a zero here can only mean an explicit "…-timeout 0s" /
		// "idle-ttl 0s", never "left unset" — same zero-vs-absent ambiguity
		// as Daemon.Poll (see daemon.go's applyDefaults comment), resolved
		// the same way (a negative value is unambiguous either way).
		if s.ReadyTimeout <= 0 {
			return fmt.Errorf("service %q: ready-timeout must be positive, got %s", s.Name, s.ReadyTimeout)
		}
		if s.IdleTTL <= 0 {
			return fmt.Errorf("service %q: idle-ttl must be positive, got %s", s.Name, s.IdleTTL)
		}
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

		seenNeed := make(map[string]bool, len(c.Needs))
		for _, n := range c.Needs {
			if !svcNames[n] {
				return fmt.Errorf("check %q: needs %q: no such service declared", c.Name, n)
			}
			if seenNeed[n] {
				return fmt.Errorf("check %q: needs %q: duplicate", c.Name, n)
			}
			seenNeed[n] = true
		}
	}
	return nil
}
