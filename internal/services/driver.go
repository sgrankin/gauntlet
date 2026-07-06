package services

import (
	"context"

	"github.com/sgrankin/gauntlet/internal/config"
)

// Mode is the pool-global reachability shape for every instance a single
// daemon creates (docs/plans/services.md §5, review M2). Mode is derived
// once from the executor's kind (cmd wiring, docs/plans/services-impl.md
// §4.5) and never varies within one daemon's lifetime — it is recorded per
// instance, not folded into config.ServiceKey, so the key stays pure spec
// identity and reachability is enforced only where it can actually vary: at
// boot adoption (Pool.Adopt rejects a mode-mismatched record).
type Mode int

const (
	// ModeNetwork attaches service and check containers to a shared
	// per-daemon network; the endpoint host is the service's network alias
	// (its keyhash12), the port is the container's own declared port. Used
	// by the container executor (docker/podman) — no host publish, so
	// instances are unreachable from off-box by construction.
	ModeNetwork Mode = iota
	// ModePublish host-publishes the service on 127.0.0.1 at a
	// kernel-assigned ephemeral port (`-p 127.0.0.1:0:<port>`), read back
	// via the driver after create (services.md §5 "Ports: who allocates,
	// who cares"). Used by the local executor, which runs checks as host
	// processes with no shared container network to join.
	ModePublish
)

// String renders m the way records/adoption compare it (record.go's
// Record.Mode field): stable and lowercase, independent of iota order so a
// future Mode addition can't silently reinterpret an old on-disk record.
func (m Mode) String() string {
	switch m {
	case ModeNetwork:
		return "network"
	case ModePublish:
		return "publish"
	default:
		return "unknown"
	}
}

// InstanceSpec is what the Pool asks a Driver to create: the defaulted
// service spec plus the names/labels/network the pool has already decided
// (the keyhash12 truncation, the per-daemon token namespace) — the driver
// never invents naming, it only executes it (services.md §2 "key material
// vs name material").
type InstanceSpec struct {
	Key   string         // full key (services.md §2); becomes the container label
	Spec  config.Service // defaulted
	Name  string         // gauntlet-svc-<token>-<keyhash12>
	Alias string         // keyhash12 (network alias, ModeNetwork)
	Mode  Mode
	Net   string // gauntlet-svc-<token> (ModeNetwork only)
}

// Instance is a live (or adopted) service instance as the Pool tracks it.
//
// Spec is populated by Create (from the InstanceSpec it was given) and by
// Adopt (from the matched record's spec snapshot) — plan Amendment A1:
// ProbeReady needs the ready-command, which lives in the spec and nowhere
// else on Instance, and this also gives Reap per-instance IdleTTL without a
// registry lookup back into config.
type Instance struct {
	Name        string
	Key         string
	ContainerID string
	Mode        Mode
	Host, Port  string // resolved endpoint; Port is read back via `docker port` in ModePublish
	Spec        config.Service
}

// Driver is the tiny CLI shim behind one Mode/runtime (services.md §6). One
// implementation in phase A (containerDriver, driver_container.go); the
// interface exists so the Pool's unit tests run against a fake with no
// docker, and so a v2 artifact driver (services.md §6.2) can slot in later
// without Pool changes.
type Driver interface {
	// Create starts a new instance for is. ModeNetwork: idempotently
	// ensures is.Net exists, then runs with --network is.Net
	// --network-alias is.Alias. ModePublish: runs with
	// -p 127.0.0.1:0:<is.Spec.Port>, then reads back the kernel-assigned
	// host port. The full key (is.Key) is written as a container label so
	// adoption can match on it later (services.md §2 review m2/m6) — never
	// on the name, which only carries the truncated keyhash12.
	Create(ctx context.Context, is InstanceSpec) (Instance, error)

	// ProbeAlive reports existence + running state only (services.md §6) —
	// NOT readiness. Used at adoption and at the M1 mid-run-death
	// re-probe, both of which only care whether the instance still exists
	// at all.
	ProbeAlive(ctx context.Context, in Instance) (bool, error)

	// ProbeReady runs in.Spec.ReadyCommand inside the instance (`exec`),
	// or — when ReadyCommand is empty — the default probe (services.md §6
	// review q2).
	ProbeReady(ctx context.Context, in Instance) error

	// Destroy always removes the instance AND its anonymous volumes
	// (`rm -f -v` — review m4); specs cannot declare named volumes, so
	// there is nothing else to leak.
	Destroy(ctx context.Context, in Instance) error

	// Endpoint returns the host/port a check should dial for in, resolved
	// per Mode. Checks only ever see this — a networking-shape change
	// never touches the spec, the key, or the harness (services.md §5).
	Endpoint(in Instance) (host, port string)

	// List returns the names of every live gauntlet-svc-<token>-* instance
	// (running or stopped), for Adopt's boot-time reconciliation.
	List(ctx context.Context) ([]string, error)

	// InspectKey returns the full key recorded in name's container label.
	// Adoption must match by this, never by the name itself (services.md
	// §2 review m2/m6: names carry only a 12-hex truncation, and adoption
	// trusts on-box process naming not at all further than it has to). Not
	// part of §3.4's literal interface listing in
	// docs/plans/services-impl.md — added because "match by full key in
	// the label" cannot be implemented through the other six methods alone
	// (see the chunk-2 report's judgment-call note). ok is false, not an
	// error, when name doesn't exist or carries no gauntlet service-key
	// label — both mean "can't adopt this by key", never "adoption failed".
	InspectKey(ctx context.Context, name string) (key string, ok bool, err error)

	// TailLogs returns the instance's last ~50 log lines, failure-path
	// diagnostics only (review m5) — called just before Destroy on a
	// ready-probe failure, never on routine operation.
	TailLogs(ctx context.Context, in Instance) string
}
