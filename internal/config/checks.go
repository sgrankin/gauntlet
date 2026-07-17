package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	kdl "github.com/sblinch/kdl-go"
)

// CheckSpec is the repo-side check spec (`.gauntlet.kdl`): every adopter
// writes this, and it's read out of the
// candidate's own trial tree (never from a file path) so a candidate that
// changes its checks is tested by its own definition.
type CheckSpec struct {
	Checks   []Check   `kdl:"check,multiple"`
	Services []Service `kdl:"service,multiple"`

	// MaxParallel is how many of this candidate's ready checks may run
	// concurrently. Unset (zero) means 1 — the pre-parallelism contract,
	// preserved exactly: checks run one at a time in declaration order, so
	// merely upgrading gauntlet never races commands that relied on that
	// order. Raising it is the candidate's opt-in to overlap; only checks
	// whose `after` edges (Check.After) are satisfied become ready, and
	// undeclared orderings are independent BY DESIGN — an author enabling
	// parallelism must declare every real edge. The operator's daemon-wide
	// execution cap (executor `max-executions`) still bounds the whole
	// host; this knob only widens one candidate's slice of it.
	MaxParallel int `kdl:"max-parallel"`
}

// Check is one named check: a command to run against the exported trial
// tree. See core.CheckJob and docs/checks.md for the environment/verdict
// contract the executor applies when it runs Command.
type Check struct {
	Name    string   `kdl:",arg"`
	Command []string `kdl:"command,child"`

	// Needs names the Services (by Service.Name, below) this check requires
	// ensured and reachable before it runs. Every entry must match a
	// Service declared in the same CheckSpec — validate() enforces this so
	// an undeclared need fails loudly at parse time (ParseChecks error →
	// OutcomeRejected), never silently at run time. nil means the check has
	// no service dependencies and is wholly unaffected by the services
	// machinery: a lint check shouldn't block on (or keep warm) a database
	// it never touches.
	Needs []string `kdl:"needs"`

	// After names the checks (by Check.Name) that must PASS (or report
	// skipped — the same results that keep a candidate green) before this
	// one becomes ready. A failed or errored prerequisite blocks this check
	// instead of running it (core.CheckBlocked in the run record). Edges
	// are validated unconditionally — unknown names, self-dependencies,
	// duplicates, and cycles are spec errors even while max-parallel is 1,
	// so raising parallelism later can never reveal a latently invalid
	// graph. With max-parallel 1 the declared edges are redundant with
	// declaration order but still enforced as documentation-with-teeth.
	// This is deliberately the whole dependency grammar: no conditions, no
	// matrices, no dataflow — a check that needs those implements them in
	// its own command (the "jobs are commands, no DSL" wall).
	After []string `kdl:"after"`
}

// EnvVar is one `env "NAME" "VALUE"` pair set inside a service's container.
type EnvVar struct {
	Name  string `kdl:",arg"`
	Value string `kdl:",arg"`
}

// Service is one shared, cached service instance a check may declare a
// dependency on via Check.Needs. Read from the trial-merged tree same as
// Check, so a branch that changes a service's image/env/probe is tested
// against its own declaration.
//
// EVERY field participates in the cache key (servicekey.go's ServiceKey):
// two service declarations differing in any field — including Name itself,
// so `service "mssql-a"`/`"mssql-b"` with identical bodies stay distinct on
// purpose — are distinct cache entries by design.
type Service struct {
	Name  string   `kdl:",arg"`
	Image string   `kdl:"image"`
	Port  int      `kdl:"port"`
	Env   []EnvVar `kdl:"env,multiple"`

	// ReadyCommand, when set, is run inside the instance (docker exec or
	// equivalent) in place of the default probe.
	// Absent (nil) means the default probe applies: a TCP dial of the
	// resolved endpoint by the daemon.
	ReadyCommand []string `kdl:"ready-command,child"`

	// ReadyTimeout bounds how long ensure polls the ready probe before
	// giving up. Zero after parsing means "left unset" — ParseChecks calls
	// applyServiceDefaults, which fills in defaultReadyTimeout, BEFORE
	// validate() and before any caller can hash this struct via ServiceKey.
	// That ordering matters: the default must participate in the key at its
	// effective value, not as an explicit zero, so that a future change to
	// the default recycles instances whose specs relied on it (see
	// servicekey.go's ServiceKey doc).
	ReadyTimeout time.Duration `kdl:"ready-timeout,format:units"`

	// IdleTTL is how long an instance survives with no in-flight reference
	// before the reaper destroys it. Same before-hashing default treatment
	// as ReadyTimeout, via applyServiceDefaults/defaultIdleTTL.
	IdleTTL time.Duration `kdl:"idle-ttl,format:units"`

	// Memory is docker/podman's --memory value (e.g. "2g"), passed through
	// to the runtime verbatim — gauntlet does not interpret or normalize it.
	// Empty (the zero value) means no --memory flag is emitted at all: the
	// runtime's own default (typically unlimited), never a gauntlet-chosen
	// one.
	Memory string `kdl:"memory"`

	// CPUs is docker/podman's --cpus value (e.g. "2" or "1.5"), same
	// verbatim-passthrough and no-flag-if-empty treatment as Memory above.
	CPUs string `kdl:"cpus"`
}

// memoryPattern and cpusPattern are plausibility checks only, not a full
// grammar for docker/podman's --memory/--cpus syntax (which also accepts
// things like fractional bytes or explicit units this doesn't bother
// distinguishing) — good enough to reject an obvious typo loudly at spec-load
// time rather than pass it through to a runtime error mid-Create.
var (
	memoryPattern = regexp.MustCompile(`(?i)^[0-9]+[bkmg]?$`)
	cpusPattern   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

// RequiresServices reports whether this spec declares any dependency on the
// shared-services machinery: at least one Service, or at least one Check
// with a non-empty Needs. The queue calls this right after ParseChecks to
// reject, loudly, a spec that wants services on a daemon with no `services`
// block configured — a spec validation error, like a malformed check,
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
// validate() and before any caller hashes it via ServiceKey — see
// Service.ReadyTimeout's doc for why the ordering matters.
func applyServiceDefaults(svc *Service) {
	if svc.ReadyTimeout == 0 {
		svc.ReadyTimeout = defaultReadyTimeout
	}
	if svc.IdleTTL == 0 {
		svc.IdleTTL = defaultIdleTTL
	}
}

// envSafeName upcases name and replaces every non-alphanumeric rune with '_'
// (the GAUNTLET_SVC_<NAME>_HOST/PORT contract) — duplicated from
// internal/services' function of the same name (pool.go) because config
// must stay import-free of services; see reservedResultDir in daemon.go
// for the identical duplication pattern against internal/executor. Keep the
// two copies in sync: this one exists solely so validate() below can catch
// two distinct service names that collide once mangled into an env var
// name — an out-of-sync copy would silently stop catching exactly that.
func envSafeName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
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
	envNames := make(map[string]string, len(cs.Services)) // derived env name -> owning service name
	for _, s := range cs.Services {
		if s.Name == "" {
			return fmt.Errorf("service: name must not be empty")
		}
		if svcNames[s.Name] {
			return fmt.Errorf("service %q: duplicate", s.Name)
		}
		svcNames[s.Name] = true
		// Two exact-string-distinct names (e.g. "my-db" and "my_db") can
		// still mangle to the same GAUNTLET_SVC_<NAME>_* pair (envSafeName
		// above) — the executor's env is a last-wins slice, so one service
		// would silently shadow the other's endpoint. Caught here, at
		// spec-load time, rather than left for a check to discover it can
		// only ever reach one of its two needs.
		envName := envSafeName(s.Name)
		if owner, ok := envNames[envName]; ok {
			return fmt.Errorf("service %q: collides with service %q under env var name GAUNTLET_SVC_%s_* — rename one", s.Name, owner, envName)
		}
		envNames[envName] = s.Name
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
		// Both optional (zero value = no flag emitted) — only checked for
		// plausibility when the author actually set one.
		if s.Memory != "" && !memoryPattern.MatchString(s.Memory) {
			return fmt.Errorf("service %q: memory %q: must match %s (e.g. \"2g\")", s.Name, s.Memory, memoryPattern.String())
		}
		if s.CPUs != "" && !cpusPattern.MatchString(s.CPUs) {
			return fmt.Errorf("service %q: cpus %q: must match %s (e.g. \"1.5\")", s.Name, s.CPUs, cpusPattern.String())
		}
	}

	// Two passes over the checks: names first, so `after` may reference a
	// check declared later in the file (edges form a graph, not a
	// sequence), then per-check fields and edges against the full name set.
	seen := make(map[string]bool, len(cs.Checks))
	for _, c := range cs.Checks {
		if c.Name == "" {
			return fmt.Errorf("check: name must not be empty")
		}
		if seen[c.Name] {
			return fmt.Errorf("check %q: duplicate", c.Name)
		}
		seen[c.Name] = true
	}

	for _, c := range cs.Checks {
		if len(c.Command) == 0 {
			return fmt.Errorf("check %q: command must not be empty", c.Name)
		}

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

		seenAfter := make(map[string]bool, len(c.After))
		for _, a := range c.After {
			if a == c.Name {
				return fmt.Errorf("check %q: after %q: a check cannot depend on itself", c.Name, a)
			}
			if !seen[a] {
				return fmt.Errorf("check %q: after %q: no such check declared", c.Name, a)
			}
			if seenAfter[a] {
				return fmt.Errorf("check %q: after %q: duplicate", c.Name, a)
			}
			seenAfter[a] = true
		}
	}

	if err := cs.validateAcyclic(); err != nil {
		return err
	}

	// Zero is "left unset" (the field doc: means 1); like Daemon.Poll's
	// zero-vs-absent ambiguity, an explicit `max-parallel 0` is
	// indistinguishable from absence and gets the same serial default.
	if cs.MaxParallel < 0 {
		return fmt.Errorf("max-parallel must not be negative, got %d", cs.MaxParallel)
	}
	if cs.MaxParallel > maxAllowedMaxParallel {
		return fmt.Errorf("max-parallel %d exceeds the maximum of %d", cs.MaxParallel, maxAllowedMaxParallel)
	}
	return nil
}

// maxAllowedMaxParallel is a sane safety valve on CheckSpec.MaxParallel,
// not a hard architectural requirement — the same stance as daemon.go's
// maxAllowedMaxBatch/maxAllowedWindow. The operator's `max-executions` cap
// is the real host bound; this just rejects an obvious typo (a candidate
// asking for thousands of concurrent checks) at spec-load time.
const maxAllowedMaxParallel = 64

// validateAcyclic rejects any cycle in the checks' `after` graph with a
// deterministic message naming a check on the cycle. Iterative DFS with
// tri-color marking, visiting checks in declaration order so the same spec
// always reports the same cycle member.
func (cs *CheckSpec) validateAcyclic() error {
	edges := make(map[string][]string, len(cs.Checks))
	for _, c := range cs.Checks {
		edges[c.Name] = c.After
	}
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS path
		black = 2 // fully explored, cycle-free
	)
	color := make(map[string]int, len(cs.Checks))
	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		for _, dep := range edges[name] {
			switch color[dep] {
			case gray:
				return fmt.Errorf("check %q: after %q: dependency cycle", name, dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[name] = black
		return nil
	}
	for _, c := range cs.Checks {
		if color[c.Name] == white {
			if err := visit(c.Name); err != nil {
				return err
			}
		}
	}
	return nil
}
