// Package services implements the shared-services cache: a per-daemon pool
// of container instances keyed by hash(remote, canonical spec)
// (docs/plans/services.md, docs/plans/services-impl.md §3). The pool holds
// no correctness state — destroy every instance and record and the next
// runs are merely slower (services.md §0).
package services

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
)

// readyPollInterval is how often doEnsure/create re-polls ProbeReady while
// waiting out a Service's ReadyTimeout. A var, not a const, so pool_test.go
// can shrink it to make the ensure-failure test (which must actually wait
// out a timeout) fast — the same injectable-knob idea as Config.Now, just
// for a value that isn't itself a clock read.
var readyPollInterval = 500 * time.Millisecond

// Config configures a Pool (docs/plans/services-impl.md §3.1).
type Config struct {
	Remote       string // the daemon's remote URL — key material (services.md §2, M5)
	Token        string // state-dir token; namespaces names/network (== cmd's stateToken)
	Mode         Mode   // derived from executor kind (cmd wiring, §4.5)
	Runtime      string // "docker" | "podman"; "container" (Apple) REJECTED in phase A
	StateDir     string // <state>/services lives here (records)
	MaxInstances int
	Now          func() time.Time // injectable clock (tests); nil means time.Now

	// Log receives one line per lifecycle event (create/ready-probe
	// failure/evict/reap/adopt/ensure-failure) — the pool's own
	// operator-visible trail, distinct from the on-disk Record (an
	// efficiency hint) and Snapshot (pull-based). Defaults to os.Stderr
	// when nil, matching hooks.Params.Log / slack.Params.Log's convention.
	// newPool (every test in this package) leaves it nil deliberately —
	// logf no-ops on a nil Log, so tests stay quiet; only New's production
	// path defaults it.
	Log io.Writer
}

// Ensured is EnsureAll's output: env+networks to reach the resolved
// services, plus an opaque handle for Release/AnyDead. keys is unexported
// deliberately — callers (the queue) hold it opaquely and hand it back
// unmodified, never inspecting or reconstructing it (docs/plans/
// services-impl.md §3.2's "keys arrive opaque" framing, Amendment A6).
type Ensured struct {
	Env      []string // ["GAUNTLET_SVC_MSSQL_HOST=…","GAUNTLET_SVC_MSSQL_PORT=…"]
	Networks []string // ["gauntlet-svc-<token>"] in ModeNetwork; nil in ModePublish
	keys     []string
}

// Pool is the per-daemon cache of shared service instances (services.md §0,
// §3). It holds no correctness state: destroy every instance and record and
// the next runs are merely slower.
//
// The zero value is not usable; construct with New (wires the real
// containerDriver) or newPool (an injected Driver — what every unit test in
// this package uses instead of New, to run with no docker).
type Pool struct {
	cfg    Config
	driver Driver

	mu          sync.Mutex
	instances   map[string]Instance      // key -> live/adopted instance
	refcount    map[string]int           // key -> in-flight reference count
	lastUsed    map[string]time.Time     // key -> M3 idle clock
	createdAt   map[string]time.Time     // key -> when the live instance was created/adopted (Snapshot's tuning surface)
	hits        map[string]int           // key -> warm resolutions (one per alive+ready reuse, NOT per EnsureAll call: single-flight piggybackers coalesce to the leader's one; Snapshot's "is reuse happening" signal)
	inflight    map[string]*inflightCall // key -> in-progress ensure, single-flight
	reaperArmed bool
	pending     int // in-flight creates that reserved a slot but haven't landed in instances yet (review BUG 2)

	logMu sync.Mutex // guards writes to cfg.Log (create/evict/reap/adopt all run on different goroutines)
}

// inflightCall is one in-progress ensure, shared by every concurrent caller
// resolving the same key (docs/plans/services-impl.md §3.6 — a homegrown
// per-key single-flight, deliberately not golang.org/x/sync/singleflight,
// to keep the direct-dependency list unchanged for ~25 lines of benefit).
type inflightCall struct {
	done chan struct{}
	inst Instance
	err  error
}

// New validates cfg and returns a ready Pool backed by the real container
// driver. Rejects Runtime=="container" (Apple) — deferred in phase A
// (services.md §9; services-impl.md §4.5's hard fail at cmd-wiring time).
func New(cfg Config) (*Pool, error) {
	if cfg.Runtime == "container" {
		return nil, fmt.Errorf("services: runtime %q not supported in phase A; Apple container networking is deferred (docs/plans/services.md §9)", cfg.Runtime)
	}
	if cfg.Runtime != "docker" && cfg.Runtime != "podman" {
		return nil, fmt.Errorf("services: unknown runtime %q (want docker or podman)", cfg.Runtime)
	}
	if cfg.MaxInstances < 1 {
		return nil, fmt.Errorf("services: MaxInstances must be >= 1, got %d", cfg.MaxInstances)
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("services: StateDir is required")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("services: create state dir: %w", err)
	}
	if cfg.Log == nil {
		cfg.Log = os.Stderr
	}
	return newPool(cfg, newContainerDriver(cfg.Runtime, cfg.Token)), nil
}

// newPool builds a Pool over an arbitrary Driver — New's shared plumbing,
// and the seam every test in this package uses to run against a fake
// Driver instead of a real container runtime (docs/plans/services-impl.md
// §3, no docker required).
func newPool(cfg Config, driver Driver) *Pool {
	return &Pool{
		cfg:       cfg,
		driver:    driver,
		instances: make(map[string]Instance),
		refcount:  make(map[string]int),
		lastUsed:  make(map[string]time.Time),
		createdAt: make(map[string]time.Time),
		hits:      make(map[string]int),
		inflight:  make(map[string]*inflightCall),
	}
}

func (p *Pool) now() time.Time {
	if p.cfg.Now != nil {
		return p.cfg.Now()
	}
	return time.Now()
}

// logf writes one lifecycle log line, "services: "-prefixed to match
// hooks.Runner/Slack's own package-prefix convention (their lines read
// "hooks: ..."/"slack: ...", with "gauntlet: " added only by cmd's own
// direct os.Stderr writes, never by a package's internal logf). Nil Log
// (every unit test's newPool call, which never sets Config.Log) makes this
// a silent no-op — losing a diagnostic line must never do anything worse
// than lose it, the same discipline Slack.logf's doc states.
func (p *Pool) logf(format string, args ...any) {
	if p.cfg.Log == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintf(p.cfg.Log, "services: "+format+"\n", args...)
}

// networkName is the one shared network every ModeNetwork instance this
// pool creates joins (services.md §5) — a single per-daemon network, not
// one per service, so N services coexist as N aliases on it.
func (p *Pool) networkName() string {
	return "gauntlet-svc-" + p.cfg.Token
}

// namePrefix is the namespace Adopt/List filter against — mirrors
// cmd/gauntlet/sweep.go's containerNamePrefix shape but with the "svc-"
// infix that keeps the check-container sweep and the service pool
// structurally disjoint (services.md §3 "Adoption at boot, not reaping").
func (p *Pool) namePrefix() string {
	return "gauntlet-svc-" + p.cfg.Token + "-"
}

// instanceName derives the one naming scheme every service instance uses:
// gauntlet-svc-<token>-<keyhash12> (services.md §2 "key material vs name
// material"). A single helper (review NIT 3) so create's naming and
// namePrefix's independent reconstruction of the same prefix can't drift
// apart on a future rename.
func (p *Pool) instanceName(key string) string {
	return p.namePrefix() + key[:12]
}

// EnsureAll resolves every name in needs against svcs to a ready instance
// and returns the env+networks a check needs to reach them. BLOCKING
// (create + up-to-ReadyTimeout ready-poll) and SINGLE-FLIGHT per key
// (docs/plans/services-impl.md §3.2) — callers MUST invoke this only from a
// check-execution goroutine, NEVER the reconcile goroutine (review F1): a
// single cold service would otherwise stall every target's reconciliation
// on one blocking call.
//
// Each resolved need increments that instance's refcount (decremented by
// Release). On any failure — a needs name with no matching Service (plan
// Amendment A4: defensive against a contract violation chunk 1's spec
// validation should already prevent, but the pool must not panic on it),
// create failure, not-ready-in-time, or max-instances exceeded on a miss —
// EnsureAll releases whatever it already ensured for this call before
// returning the error, so a caller that only calls Release on success never
// leaks a partial ensure.
func (p *Pool) EnsureAll(ctx context.Context, svcs []config.Service, needs []string) (Ensured, error) {
	byName := make(map[string]config.Service, len(svcs))
	for _, s := range svcs {
		byName[s.Name] = s
	}

	var ensured Ensured
	for _, name := range needs {
		svc, ok := byName[name]
		if !ok {
			p.releaseKeys(ensured.keys)
			return Ensured{}, fmt.Errorf("services: needs %q: no matching service declared", name)
		}
		key := config.ServiceKey(p.cfg.Remote, svc)
		inst, err := p.ensureOne(ctx, key, svc)
		if err != nil {
			p.releaseKeys(ensured.keys)
			// The pool-side cause, logged next to the check-level park this
			// error becomes once it flows up into CheckResult.Err — greppable
			// by key hash without cross-referencing the check's own log.
			p.logf("ensure failed need=%q key=%s: %v", name, key[:12], err)
			return Ensured{}, fmt.Errorf("services: ensure %q: %w", name, err)
		}
		ensured.keys = append(ensured.keys, key)
		host, port := p.driver.Endpoint(inst)
		envName := envSafeName(name)
		ensured.Env = append(ensured.Env,
			"GAUNTLET_SVC_"+envName+"_HOST="+host,
			"GAUNTLET_SVC_"+envName+"_PORT="+port,
		)
	}
	if p.cfg.Mode == ModeNetwork && len(ensured.keys) > 0 {
		ensured.Networks = []string{p.networkName()}
	}
	return ensured, nil
}

// ensureOne resolves key/svc to a ready Instance, coalescing concurrent
// callers for the same key into one underlying doEnsure call
// (services-impl.md §3.6). Every caller — whether it ran doEnsure itself or
// piggybacked on someone else's inflight call — gets its own refcount
// increment on success; refcount is deliberately NOT touched inside
// doEnsure, so a coalesced hit still counts as N references for N callers.
func (p *Pool) ensureOne(ctx context.Context, key string, svc config.Service) (Instance, error) {
	p.mu.Lock()
	if call, ok := p.inflight[key]; ok {
		p.mu.Unlock()
		<-call.done
		if call.err == nil {
			p.mu.Lock()
			p.refcount[key]++
			p.mu.Unlock()
		}
		return call.inst, call.err
	}
	call := &inflightCall{done: make(chan struct{})}
	p.inflight[key] = call
	p.mu.Unlock()

	inst, err := p.doEnsure(ctx, key, svc)

	p.mu.Lock()
	delete(p.inflight, key)
	if err == nil {
		p.refcount[key]++
	}
	p.mu.Unlock()

	call.inst, call.err = inst, err
	close(call.done)
	return inst, err
}

// doEnsure runs the registry-hit-or-create algorithm for key (services.md
// §3 "The ensure algorithm"), always inside the single per-key inflight
// call ensureOne coalesces onto. Does NOT touch refcount — see ensureOne's
// doc — and does NOT touch lastUsed on a hit: M3 pins the idle clock to
// Release, not ensure, so a reused instance's lastUsed is left exactly as
// Release last set it (or as this function set it at creation, if never
// released yet).
func (p *Pool) doEnsure(ctx context.Context, key string, svc config.Service) (Instance, error) {
	p.mu.Lock()
	inst, ok := p.instances[key]
	p.mu.Unlock()

	if ok {
		if alive, err := p.driver.ProbeAlive(ctx, inst); err == nil && alive {
			if err := p.driver.ProbeReady(ctx, inst); err == nil {
				// Snapshot's hit counter (S5-surface, design §10 tuning
				// instrument): this is the one true "reuse, not create" path —
				// a concurrent caller that instead coalesces onto this
				// key's inflight call (ensureOne) never reaches doEnsure at
				// all, so it's not double-counted here.
				p.mu.Lock()
				p.hits[key]++
				p.mu.Unlock()
				return inst, nil
			}
		}
		// services.md §3 step 3: a supposedly-live instance that fails its
		// probe is evicted and falls through to creation, once; a second
		// failure below (in create) is reported as an ordinary ensure
		// error (§7 "Ensure-time failure").
		p.logf("evict %s key=%s instance=%s reason=probe-failed", svc.Name, key[:12], inst.Name)
		p.evict(ctx, key, inst)
	}

	// Reuse (the `ok` branch above) never counts against the cap — only a
	// miss that would grow the live set does (services-impl.md §3.6).
	// reserveSlot/releaseSlot count in-flight creates too, not just
	// p.instances, so two concurrent misses on DIFFERENT keys can't both
	// pass the gate before either lands (review BUG 2: a plain
	// check-then-create on p.instances alone is a TOCTOU race that lets
	// the "hard" cap overshoot under concurrent distinct-key misses).
	if !p.reserveSlot() {
		return Instance{}, fmt.Errorf("max-instances (%d) reached", p.cfg.MaxInstances)
	}
	defer p.releaseSlot()

	inst, err := p.create(ctx, key, svc)
	if err != nil {
		return Instance{}, err
	}

	now := p.now()
	p.mu.Lock()
	p.instances[key] = inst
	p.lastUsed[key] = now
	p.createdAt[key] = now
	p.mu.Unlock()

	rec := Record{
		Key: key, Name: inst.Name, Repo: p.cfg.Remote, Mode: p.cfg.Mode.String(),
		Spec: svc, ContainerID: inst.ContainerID,
		Endpoint:  Endpoint{Host: inst.Host, Port: inst.Port},
		CreatedAt: now, LastUsed: now,
	}
	if p.cfg.Mode == ModeNetwork {
		rec.Network = p.networkName()
	}
	// Best-effort (record.go's doc): a record is an efficiency hint, never
	// load-bearing for correctness — a failed write here just means a
	// slower Adopt after the next restart, not a wrong one.
	_ = writeRecord(p.cfg.StateDir, rec)

	return inst, nil
}

// reserveSlot atomically reserves one instance slot against MaxInstances,
// counting both already-live instances and other creates currently in
// flight (review BUG 2). Returns false, reserving nothing, if the cap is
// already reached. Paired with releaseSlot, which every reserveSlot caller
// must defer regardless of outcome — pending is bookkeeping only, never a
// substitute for the authoritative p.instances count once a create lands.
func (p *Pool) reserveSlot() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.instances)+p.pending >= p.cfg.MaxInstances {
		return false
	}
	p.pending++
	return true
}

func (p *Pool) releaseSlot() {
	p.mu.Lock()
	p.pending--
	p.mu.Unlock()
}

// create runs one driver Create + ready-poll for key/svc, tearing the
// instance back down (log tail then destroy — review m5) if it never
// becomes ready within svc.ReadyTimeout.
func (p *Pool) create(ctx context.Context, key string, svc config.Service) (Instance, error) {
	is := InstanceSpec{
		Key:   key,
		Spec:  svc,
		Name:  p.instanceName(key),
		Alias: key[:12],
		Mode:  p.cfg.Mode,
	}
	if p.cfg.Mode == ModeNetwork {
		is.Net = p.networkName()
	}

	p.logf("create start %s key=%s instance=%s image=%s", svc.Name, key[:12], is.Name, svc.Image)
	readyStart := time.Now()

	inst, err := p.driver.Create(ctx, is)
	if err != nil {
		return Instance{}, fmt.Errorf("create %s: %w", svc.Name, err)
	}
	inst.Spec = svc // A1: reasserted defensively even though Create should already set it

	if err := p.pollReady(ctx, inst, svc.ReadyTimeout); err != nil {
		// review BUG 1: cleanup MUST NOT run on ctx — if ctx is what just
		// got canceled (e.g. a superseded run canceling mid-poll),
		// exec.CommandContext on an already-canceled context never starts
		// the subprocess at all, so Destroy would silently no-op, leaking
		// the half-created container under its deterministic name AND
		// leaving it untracked (this create never reached doEnsure's
		// instances-map insert or record write) — every future ensure of
		// this key then fails "name already in use" until the next
		// restart's Adopt cleans it up. cleanupContext detaches so the
		// teardown itself still actually runs.
		cctx, cancel := cleanupContext(ctx)
		logs := p.driver.TailLogs(cctx, inst) // review m5: capture before destroy
		p.driver.Destroy(cctx, inst)
		cancel()
		// The tail lands on the pool's own logger too, not just the error
		// this function returns — an operator reading stderr sees the same
		// diagnostic a caller would otherwise only get by threading the
		// returned error all the way up.
		p.logf("ready-probe failed %s key=%s instance=%s: %v\nlast output:\n%s", svc.Name, key[:12], is.Name, err, logs)
		return Instance{}, fmt.Errorf("%s not ready within %s: %w\nlast output:\n%s", svc.Name, svc.ReadyTimeout, err, logs)
	}
	p.logf("create succeeded %s key=%s instance=%s endpoint=%s:%s ready_after=%s", svc.Name, key[:12], is.Name, inst.Host, inst.Port, time.Since(readyStart))
	return inst, nil
}

// cleanupContext returns a context detached from ctx's own cancellation
// (context.WithoutCancel) but still time-bounded, for driver calls that
// must run to completion even when ctx is exactly what just got canceled
// (review BUG 1) — teardown after a canceled ensure, not the ensure itself.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// pollReady polls ProbeReady until it succeeds, svc's ReadyTimeout elapses,
// or ctx is done. Deliberately measured against real wall-clock time
// (time.Now, not p.now()): ReadyTimeout bounds how long the pool actually
// waits for a container to come up — a real-world duration independent of
// Config.Now, which only drives idle-time bookkeeping (lastUsed/Reap) and
// is frozen in tests. Using the injectable clock here would make this loop
// un-terminating against a test's frozen fake clock (readyPollInterval,
// not the deadline, is this function's one test-tunable knob).
func (p *Pool) pollReady(ctx context.Context, inst Instance, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = p.driver.ProbeReady(ctx, inst)
		if lastErr == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

// Release drops one reference per key ensured by e and touches last-used =
// now on each (review M3 — the idle clock starts when the LAST reference
// drops, not at ensure). Never destroys; Reap does. Safe to call with an
// empty/zero Ensured (e.g. after EnsureAll's own partial-failure cleanup
// already released these keys) — decrementing an already-zero refcount is a
// no-op, not an underflow.
func (p *Pool) Release(e Ensured) {
	p.releaseKeys(e.keys)
}

func (p *Pool) releaseKeys(keys []string) {
	if len(keys) == 0 {
		return
	}
	now := p.now()
	p.mu.Lock()
	for _, key := range keys {
		if p.refcount[key] > 0 {
			p.refcount[key]--
		}
		// Guard on the instance still being tracked (review NIT 1): a key
		// AnyDead already evicted (mid-run death, M1) has no instances
		// entry, and this unconditional write used to re-add a lastUsed
		// entry for it anyway — Reap only ever ranges p.instances, so that
		// entry was permanently unreachable, a slow leak keyed by every
		// service that ever died mid-run.
		if _, ok := p.instances[key]; ok {
			p.lastUsed[key] = now
		}
	}
	p.mu.Unlock()

	for _, key := range keys {
		// Best-effort — touchRecordLastUsed's doc explains why a failure
		// here is never surfaced: records are hints, not truth.
		_ = touchRecordLastUsed(p.cfg.StateDir, key, now)
	}
}

// AnyDead probe-alives every instance referenced by e and reports whether
// any is dead (review M1). BLOCKING; callers MUST call this only from the
// check goroutine, and only on a FAILED check (services-impl.md §4.3) — a
// passing check never re-probes. Every dead instance found is evicted here
// so the next EnsureAll re-creates it; a key this Pool no longer tracks at
// all (e.g. already evicted by a racing caller) counts as dead too.
func (p *Pool) AnyDead(ctx context.Context, e Ensured) bool {
	dead := false
	for _, key := range e.keys {
		p.mu.Lock()
		inst, ok := p.instances[key]
		p.mu.Unlock()
		if !ok {
			dead = true
			continue
		}
		if alive, err := p.driver.ProbeAlive(ctx, inst); err != nil || !alive {
			dead = true
			p.logf("evict %s key=%s instance=%s reason=mid-run-death", inst.Spec.Name, key[:12], inst.Name)
			p.evict(ctx, key, inst)
		}
	}
	return dead
}

// evict removes key from the in-memory registry and destroys inst (via the
// driver) and its on-disk record. Called on a failed liveness/ready probe
// (doEnsure step 3, AnyDead) and during Adopt for anything unmatchable.
//
// p.hits is deliberately NOT cleared here: key is spec identity (services.md
// §2), so a hit count earned before an evict+recreate cycle is still a true
// count of how often this exact spec has been reused over the Pool's
// lifetime — Snapshot's "is reuse actually happening" signal is more useful
// cumulative than reset to zero by an incidental eviction. Reap, by
// contrast, DOES drop the counter (see its doc): a TTL'd-out key is
// abandoned, not mid-cycle, and hits was otherwise the one per-key map with
// no cleanup path at all (phase-B review NIT-1).
//
// Destroy runs on a detached cleanup context, not ctx (review BUG 1, same
// reasoning as create's doc): AnyDead and the doEnsure reuse-probe-failure
// path both call evict with the live ensure/check ctx, which can be exactly
// what's canceling right now — an already-canceled ctx would make
// exec.CommandContext silently skip starting `rm` at all.
func (p *Pool) evict(ctx context.Context, key string, inst Instance) {
	p.mu.Lock()
	delete(p.instances, key)
	delete(p.refcount, key)
	delete(p.lastUsed, key)
	delete(p.createdAt, key)
	p.mu.Unlock()
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	// Best-effort: a failed rm just leaks a container for a human to clean
	// up (the pool's own accounting is already consistent regardless).
	_ = p.driver.Destroy(cctx, inst)
	removeRecord(p.cfg.StateDir, key)
}

// Adopt lists every live gauntlet-svc-<token>-* instance and matches each
// against pool records BY FULL KEY IN THE INSTANCE LABEL — never by name,
// which carries only a 12-hex truncation (services.md §2 review m2, m6). A
// match is re-checked against the current recorded Mode (review M2) and
// probed ready; anything unmatchable, unready, mode-mismatched, or beyond
// its IdleTTL is destroyed. Called once at boot, before any check goroutine
// or the reaper.
func (p *Pool) Adopt(ctx context.Context) error {
	names, err := p.driver.List(ctx)
	if err != nil {
		return fmt.Errorf("services: adopt: list: %w", err)
	}
	records, err := listRecords(p.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("services: adopt: records: %w", err)
	}
	byKey := make(map[string]Record, len(records))
	for _, r := range records {
		byKey[r.Key] = r
	}

	prefix := p.namePrefix()
	now := p.now()
	adopted, destroyed := 0, 0
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue // not this daemon's namespace (token-scoped, mirrors sweep.go)
		}

		key, ok, err := p.driver.InspectKey(ctx, name)
		if err != nil || !ok {
			p.logf("adopt reject instance=%s reason=no-key-label", name)
			_ = p.driver.Destroy(ctx, Instance{Name: name})
			destroyed++
			continue
		}
		rec, ok := byKey[key]
		if !ok {
			p.logf("adopt reject instance=%s key=%s reason=no-record", name, key[:12])
			_ = p.driver.Destroy(ctx, Instance{Name: name, Key: key})
			destroyed++
			continue
		}
		if rec.Mode != p.cfg.Mode.String() {
			// M2: an operator who switched executor kind (and so Mode)
			// since the last run gets a cold, correct pool here, never a
			// warm instance whose endpoint nothing can reach.
			p.logf("adopt reject %s key=%s instance=%s reason=mode-mismatch", rec.Spec.Name, key[:12], name)
			_ = p.driver.Destroy(ctx, Instance{Name: name, Key: key})
			removeRecord(p.cfg.StateDir, key)
			destroyed++
			continue
		}

		inst := Instance{
			Name: name, Key: key, ContainerID: rec.ContainerID,
			Mode: p.cfg.Mode, Host: rec.Endpoint.Host, Port: rec.Endpoint.Port,
			Spec: rec.Spec, // A1: from the record's spec snapshot, not re-derived
		}
		if alive, err := p.driver.ProbeAlive(ctx, inst); err != nil || !alive {
			p.logf("adopt reject %s key=%s instance=%s reason=not-alive", rec.Spec.Name, key[:12], name)
			_ = p.driver.Destroy(ctx, inst)
			removeRecord(p.cfg.StateDir, key)
			destroyed++
			continue
		}
		if err := p.driver.ProbeReady(ctx, inst); err != nil {
			p.logf("adopt reject %s key=%s instance=%s reason=not-ready", rec.Spec.Name, key[:12], name)
			_ = p.driver.Destroy(ctx, inst)
			removeRecord(p.cfg.StateDir, key)
			destroyed++
			continue
		}
		if now.Sub(rec.LastUsed) > rec.Spec.IdleTTL {
			p.logf("adopt reject %s key=%s instance=%s reason=idle-ttl-exceeded", rec.Spec.Name, key[:12], name)
			_ = p.driver.Destroy(ctx, inst)
			removeRecord(p.cfg.StateDir, key)
			destroyed++
			continue
		}

		p.mu.Lock()
		p.instances[key] = inst
		p.lastUsed[key] = rec.LastUsed
		p.createdAt[key] = rec.CreatedAt
		p.mu.Unlock()
		p.logf("adopt %s key=%s instance=%s endpoint=%s:%s", rec.Spec.Name, key[:12], name, inst.Host, inst.Port)
		adopted++
	}
	p.logf("adopt summary adopted=%d destroyed=%d", adopted, destroyed)
	return nil
}

// ArmReaper marks the reaper live (idempotent). The in-memory refcount is
// lost on restart, so the reaper must not run until the queue's first full
// reconcile pass has re-ensured (and so refcounted) everything still in
// flight (review q3) — cmd calls this once, after that pass completes.
func (p *Pool) ArmReaper() {
	p.mu.Lock()
	p.reaperArmed = true
	p.mu.Unlock()
}

// Reap destroys every instance whose (now - last-used) exceeds its
// Spec.IdleTTL and whose refcount is 0. No-op until ArmReaper (review q3).
// BLOCKING destroys — callers MUST run this on its own goroutine (cmd's
// reaper ticker), never the reconcile loop.
func (p *Pool) Reap(ctx context.Context) {
	p.mu.Lock()
	if !p.reaperArmed {
		p.mu.Unlock()
		return
	}
	now := p.now()
	var due []string
	for key, inst := range p.instances {
		if p.refcount[key] > 0 {
			continue
		}
		if now.Sub(p.lastUsed[key]) > inst.Spec.IdleTTL {
			due = append(due, key)
		}
	}
	p.mu.Unlock()

	for _, key := range due {
		p.mu.Lock()
		inst, ok := p.instances[key]
		stillDue := ok && p.refcount[key] == 0
		lastUsed := p.lastUsed[key]
		p.mu.Unlock()
		if !stillDue {
			continue // raced: refcounted or already evicted since the scan above
		}
		p.logf("reap %s key=%s instance=%s idle=%s ttl=%s", inst.Spec.Name, key[:12], inst.Name, now.Sub(lastUsed), inst.Spec.IdleTTL)
		p.evict(ctx, key, inst)
		// A reaped key is abandoned (idle past its own TTL), so its hit
		// counter goes with it — unlike evict's other callers (AnyDead, the
		// reuse-probe failure), where the same key is usually recreated on
		// the next run and the cumulative count stays meaningful (evict's
		// doc). Without this, hits would be the one per-key map that never
		// shrinks (phase-B review NIT-1) — every retired spec, including
		// every pre-upgrade key after a canonical-encoding change, would
		// hold an entry for the daemon's lifetime.
		p.mu.Lock()
		delete(p.hits, key)
		p.mu.Unlock()
	}
}

// InstanceStatus is one live instance's observability view (design §10's
// tuning instrument: an operator sizing idle-ttl/max-instances needs to SEE
// the pool, not just poke it blind). KeyHash12 is the same truncation
// instanceName uses for container names/network aliases — safe for compact
// display; Key is the full key, carried for a caller (the JSON API, MCP)
// that wants to correlate exactly, since only the full key is guaranteed
// collision-free (services.md §2 review m2/m6).
type InstanceStatus struct {
	Service    string // config.Service.Name
	Image      string
	Key        string
	KeyHash12  string
	Mode       Mode
	Host, Port string
	CreatedAt  time.Time
	LastUsed   time.Time
	Refcount   int

	// Hits is how many EnsureAll calls resolved this key by reuse rather
	// than create (doEnsure's alive+ready reuse path), cumulative for the
	// key's whole lifetime in this Pool — including across an evict+recreate
	// cycle, since key is spec identity (evict's doc). The "is reuse
	// actually happening" signal: a key with Refcount==0 and Hits==0 has
	// never once saved a cold create.
	Hits int
}

// PoolStatus is Pool.Snapshot's output: the pool's own tuning knobs
// (MaxInstances, the configured cap; Pending, in-flight creates that
// reserved a slot but haven't landed in Instances yet — reserveSlot's
// bookkeeping) alongside one InstanceStatus per live instance.
type PoolStatus struct {
	MaxInstances int
	Pending      int
	Instances    []InstanceStatus
}

// Snapshot returns a point-in-time view of the whole pool for the
// dashboard/API/MCP tuning surface (design §10; S5's hard parity ruling —
// every operator-visible fact appears on all three surfaces). Instances is
// sorted by (service name, key) — p.instances is a map, so iteration order
// is otherwise undefined, and a stable order is what makes the dashboard
// table (and a diff between two snapshots) readable.
func (p *Pool) Snapshot() PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := PoolStatus{MaxInstances: p.cfg.MaxInstances, Pending: p.pending}
	out.Instances = make([]InstanceStatus, 0, len(p.instances))
	for key, inst := range p.instances {
		out.Instances = append(out.Instances, InstanceStatus{
			Service:   inst.Spec.Name,
			Image:     inst.Spec.Image,
			Key:       key,
			KeyHash12: key[:12],
			Mode:      inst.Mode,
			Host:      inst.Host,
			Port:      inst.Port,
			CreatedAt: p.createdAt[key],
			LastUsed:  p.lastUsed[key],
			Refcount:  p.refcount[key],
			Hits:      p.hits[key],
		})
	}
	sort.Slice(out.Instances, func(i, j int) bool {
		a, b := out.Instances[i], out.Instances[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Key < b.Key
	})
	return out
}

// envSafeName upcases name and replaces every non-alphanumeric rune with
// '_' (services.md §4's GAUNTLET_SVC_<NAME>_HOST/PORT contract).
// internal/config/checks.go duplicates this exact transform (also named
// envSafeName) so CheckSpec.validate() can reject two service names that
// collide once mangled (adversarial review BUG 3) — config can't import
// this package, same reservedResultDir-style split as daemon.go/
// internal/executor. Keep both copies byte-for-byte identical.
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
