package services

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
)

// readyPollInterval only bounds real wall-clock wait — see its doc in
// pool.go. Shrunk for the whole test binary so the ensure-failure test
// (which must actually wait out a ReadyTimeout) runs fast.
func init() {
	readyPollInterval = time.Millisecond
}

// fakeDriver is a Driver test double (docs/plans/services-impl.md §3.7): an
// in-memory registry standing in for a real container runtime, with
// scriptable affordances (notReady, notAlive) tests use to drive every
// branch of Pool's ensure/adopt/reap logic without docker.
type fakeDriver struct {
	mu sync.Mutex

	createCount map[string]int // key -> number of Create calls (single-flight assertion)
	destroyed   []string       // names Destroy was called on, in order
	tailed      []string       // names TailLogs was called on

	byName map[string]Instance // what a real runtime would report, by Name

	notReady map[string]bool // key -> ProbeReady always fails
	notAlive map[string]bool // key -> ProbeAlive always reports false

	// blockReadyUntilDone makes ProbeReady block on ctx.Done() and then
	// return ctx.Err() — review BUG 1's repro: an ensure whose ctx gets
	// canceled mid-ready-poll, never a real not-ready response.
	blockReadyUntilDone bool

	// destroyCtxDone records, for every Destroy call in order, whether the
	// ctx it received was already done (canceled/expired) at call time
	// (review BUG 1: before the fix, cleanup ran on the ensure's own ctx,
	// so a canceled ensure meant Destroy's context was already done and
	// exec.CommandContext would never actually start the subprocess).
	destroyCtxDone []bool
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{
		createCount: map[string]int{},
		byName:      map[string]Instance{},
		notReady:    map[string]bool{},
		notAlive:    map[string]bool{},
	}
}

var _ Driver = (*fakeDriver)(nil)

func (f *fakeDriver) Create(ctx context.Context, is InstanceSpec) (Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount[is.Key]++
	inst := Instance{
		Name: is.Name, Key: is.Key, ContainerID: "c-" + is.Name,
		Mode: is.Mode, Host: is.Alias, Port: portString(is.Spec.Port), Spec: is.Spec,
	}
	if is.Mode == ModePublish {
		inst.Host = "127.0.0.1"
	}
	f.byName[is.Name] = inst
	return inst, nil
}

func (f *fakeDriver) ProbeAlive(ctx context.Context, in Instance) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notAlive[in.Key] {
		return false, nil
	}
	_, ok := f.byName[in.Name]
	return ok, nil
}

func (f *fakeDriver) ProbeReady(ctx context.Context, in Instance) error {
	f.mu.Lock()
	block := f.blockReadyUntilDone
	notReady := f.notReady[in.Key]
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if notReady {
		return errNotReady(in.Name)
	}
	return nil
}

func (f *fakeDriver) Destroy(ctx context.Context, in Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCtxDone = append(f.destroyCtxDone, ctx.Err() != nil)
	delete(f.byName, in.Name)
	f.destroyed = append(f.destroyed, in.Name)
	return nil
}

func (f *fakeDriver) Endpoint(in Instance) (string, string) { return in.Host, in.Port }

func (f *fakeDriver) List(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.byName))
	for name := range f.byName {
		names = append(names, name)
	}
	return names, nil
}

func (f *fakeDriver) InspectKey(ctx context.Context, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.byName[name]
	if !ok || inst.Key == "" {
		return "", false, nil
	}
	return inst.Key, true, nil
}

func (f *fakeDriver) TailLogs(ctx context.Context, in Instance) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tailed = append(f.tailed, in.Name)
	return "fake logs for " + in.Name
}

// seed inserts inst directly, as if a prior gauntlet process had already
// created it — the starting point for every Adopt test, which never goes
// through Create.
func (f *fakeDriver) seed(inst Instance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byName[inst.Name] = inst
}

func (f *fakeDriver) setNotReady(key string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notReady[key] = v
}

func (f *fakeDriver) setNotAlive(key string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notAlive[key] = v
}

func (f *fakeDriver) setBlockReadyUntilDone(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockReadyUntilDone = v
}

func (f *fakeDriver) destroyCtxDoneCalls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.destroyCtxDone...)
}

func (f *fakeDriver) createsFor(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount[key]
}

func (f *fakeDriver) wasDestroyed(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.destroyed {
		if n == name {
			return true
		}
	}
	return false
}

func (f *fakeDriver) wasTailed(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.tailed {
		if n == name {
			return true
		}
	}
	return false
}

func portString(p int) string {
	const digits = "0123456789"
	if p == 0 {
		return "0"
	}
	var b []byte
	for p > 0 {
		b = append([]byte{digits[p%10]}, b...)
		p /= 10
	}
	return string(b)
}

type errNotReady string

func (e errNotReady) Error() string { return "fake: " + string(e) + " not ready" }

// fakeClock is the injectable Config.Now every test in this file drives
// deterministically instead of sleeping on wall time.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testConfig(now func() time.Time, stateDir string) Config {
	return Config{
		Remote:       "https://example.test/repo.git",
		Token:        "tok",
		Mode:         ModeNetwork,
		Runtime:      "docker",
		StateDir:     stateDir,
		MaxInstances: 8,
		Now:          now,
	}
}

// --- single-flight ---

func TestEnsureAllSingleFlight(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd)

	svc := config.Service{Name: "pg", Image: "postgres", Port: 5432, ReadyTimeout: time.Second, IdleTTL: time.Hour}

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, err := p.EnsureAll(context.Background(), []config.Service{svc}, []string{"pg"})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureAll[%d]: %v", i, err)
		}
	}

	key := config.ServiceKey(cfg.Remote, svc)
	if got := fd.createsFor(key); got != 1 {
		t.Fatalf("Create called %d times for one key across %d concurrent EnsureAll calls, want 1", got, n)
	}
}

// --- refcount + reap, release-touch (M3), arm gating (q3) ---

func TestReapSkipsRefcountedAndRespectsTTL(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd)
	p.ArmReaper()

	svcIdle := config.Service{Name: "idle", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour}
	svcHeld := config.Service{Name: "held", Image: "img", Port: 2, ReadyTimeout: time.Second, IdleTTL: time.Hour}

	eIdle, err := p.EnsureAll(context.Background(), []config.Service{svcIdle}, []string{"idle"})
	if err != nil {
		t.Fatalf("EnsureAll idle: %v", err)
	}
	if _, err := p.EnsureAll(context.Background(), []config.Service{svcHeld}, []string{"held"}); err != nil {
		t.Fatalf("EnsureAll held: %v", err)
	}

	p.Release(eIdle) // refcount -> 0, lastUsed = t=0
	// eHeld is deliberately never released: its refcount stays 1.

	clock.advance(2 * time.Hour) // both instances are now 2h stale against a 1h TTL
	p.Reap(context.Background())

	keyIdle := config.ServiceKey(cfg.Remote, svcIdle)
	keyHeld := config.ServiceKey(cfg.Remote, svcHeld)

	p.mu.Lock()
	_, idleStillThere := p.instances[keyIdle]
	_, heldStillThere := p.instances[keyHeld]
	p.mu.Unlock()

	if idleStillThere {
		t.Error("idle instance (refcount 0, past TTL) was not reaped")
	}
	if !heldStillThere {
		t.Error("held instance (refcount 1, past TTL) was reaped despite being in use")
	}
}

func TestReleaseTouchesLastUsed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: 2 * time.Hour}
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd)
	p.ArmReaper()

	e, err := p.EnsureAll(context.Background(), []config.Service{svc}, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}

	clock.advance(4 * time.Hour) // a long run holding the reference
	p.Release(e)                 // M3: lastUsed becomes "now" (t=4h), not t=0

	// Reap immediately after release: idle time is ~0, well under the 2h TTL.
	p.Reap(context.Background())

	key := config.ServiceKey(cfg.Remote, svc)
	p.mu.Lock()
	_, stillThere := p.instances[key]
	p.mu.Unlock()
	if !stillThere {
		t.Error("instance reaped immediately after a 4h run released it — M3 (last-used on release) regressed")
	}
}

func TestReapNoopBeforeArmed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Minute}
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd) // ArmReaper deliberately not called

	e, err := p.EnsureAll(context.Background(), []config.Service{svc}, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	p.Release(e)

	clock.advance(time.Hour) // far past the 1-minute TTL
	p.Reap(context.Background())

	key := config.ServiceKey(cfg.Remote, svc)
	p.mu.Lock()
	_, stillThere := p.instances[key]
	p.mu.Unlock()
	if !stillThere {
		t.Error("Reap destroyed an instance before ArmReaper was called — q3 gating regressed")
	}
}

// --- max-instances ---

func TestMaxInstances(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	cfg.MaxInstances = 1
	p := newPool(cfg, fd)

	svcA := config.Service{Name: "a", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour}
	svcB := config.Service{Name: "b", Image: "img", Port: 2, ReadyTimeout: time.Second, IdleTTL: time.Hour}

	eA, err := p.EnsureAll(context.Background(), []config.Service{svcA}, []string{"a"})
	if err != nil {
		t.Fatalf("EnsureAll a (first ensure, at cap): %v", err)
	}

	// Reuse of the SAME key must succeed even at the 1-instance cap
	// (services-impl.md §3.6: "reuse never counts against the cap").
	if _, err := p.EnsureAll(context.Background(), []config.Service{svcA}, []string{"a"}); err != nil {
		t.Fatalf("EnsureAll a (reuse at cap): %v", err)
	}

	// A miss on a different key while at cap must error.
	if _, err := p.EnsureAll(context.Background(), []config.Service{svcB}, []string{"b"}); err == nil {
		t.Fatal("EnsureAll b (miss at cap): want error, got nil")
	}

	p.Release(eA)
}

// TestMaxInstancesHardCapUnderConcurrentDistinctKeyMisses is review BUG 2's
// repro: with MaxInstances=1 and the pool empty (one remaining slot), two
// concurrent EnsureAll calls on two DIFFERENT keys race for that slot.
// Before the fix, doEnsure's cap check (len(p.instances) >= cap) and the
// eventual p.instances[key]=inst insert were separated by the blocking
// create() call, so both goroutines could read "under cap" before either
// had inserted, overshooting it. reserveSlot/releaseSlot close that window
// by reserving the slot atomically under p.mu before create runs, so this
// must hold deterministically regardless of scheduling — not just usually.
func TestMaxInstancesHardCapUnderConcurrentDistinctKeyMisses(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	cfg.MaxInstances = 1
	p := newPool(cfg, fd)

	svcA := config.Service{Name: "a", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour}
	svcB := config.Service{Name: "b", Image: "img", Port: 2, ReadyTimeout: time.Second, IdleTTL: time.Hour}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = p.EnsureAll(context.Background(), []config.Service{svcA}, []string{"a"})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = p.EnsureAll(context.Background(), []config.Service{svcB}, []string{"b"})
	}()
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent distinct-key misses at cap: %d succeeded (errs=%v), want exactly 1 (review BUG 2: max-instances must be a hard cap)", successes, errs)
	}

	p.mu.Lock()
	liveCount := len(p.instances)
	p.mu.Unlock()
	if liveCount != 1 {
		t.Errorf("live instances = %d, want exactly 1 (MaxInstances=1)", liveCount)
	}
}

// --- ensure failure (review m5: TailLogs then Destroy) ---

func TestEnsureFailureTailsLogsAndDestroys(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd)

	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: 10 * time.Millisecond, IdleTTL: time.Hour}
	key := config.ServiceKey(cfg.Remote, svc)
	fd.setNotReady(key, true)

	if _, err := p.EnsureAll(context.Background(), []config.Service{svc}, []string{"db"}); err == nil {
		t.Fatal("EnsureAll: want error for a service that never becomes ready, got nil")
	}
	if got := fd.createsFor(key); got != 1 {
		t.Fatalf("Create called %d times, want exactly 1", got)
	}

	name := "gauntlet-svc-" + cfg.Token + "-" + key[:12]
	if !fd.wasTailed(name) {
		t.Error("TailLogs was not called before destroying the not-ready instance (review m5)")
	}
	if !fd.wasDestroyed(name) {
		t.Error("not-ready instance was not destroyed")
	}
}

// TestCreateCleanupUsesDetachedContext is review BUG 1's repro: an ensure
// whose ctx is canceled while blocked in the ready-poll (the "supersede
// lands during ready-poll" scenario) must still actually run its
// TailLogs/Destroy cleanup. Before the fix, cleanup ran on the same
// (now-canceled) ctx, and exec.CommandContext on an already-canceled
// context never starts the subprocess — Destroy would silently no-op,
// leaking the container under its deterministic name and leaving it
// untracked (poisoning every future ensure of that key with "name already
// in use" until the next restart's Adopt).
func TestCreateCleanupUsesDetachedContext(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	fd.setBlockReadyUntilDone(true) // ProbeReady blocks on ctx.Done(), then returns ctx.Err()
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd)

	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: 30 * time.Second, IdleTTL: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	if _, err := p.EnsureAll(ctx, []config.Service{svc}, []string{"db"}); err == nil {
		t.Fatal("EnsureAll: want an error when ctx is canceled mid-ready-poll, got nil")
	}

	calls := fd.destroyCtxDoneCalls()
	if len(calls) != 1 {
		t.Fatalf("Destroy called %d times, want exactly 1", len(calls))
	}
	if calls[0] {
		t.Error("Destroy was called with an already-done context — cleanup must run on a detached context (review BUG 1), or exec.CommandContext never actually starts `rm`")
	}
}

// --- A4: defensive needs resolution ---

func TestEnsureAllUnknownNeed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd)

	// A4: a needs name absent from svcs must error, never panic — even
	// though chunk 1's spec validation makes this unreachable from a real
	// check spec, the pool must not trust a future caller's contract.
	if _, err := p.EnsureAll(context.Background(), nil, []string{"ghost"}); err == nil {
		t.Fatal("EnsureAll with an unresolvable need: want error, got nil")
	}
}

// --- env/networks ---

func TestEnsureAllEnvAndNetworks(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()

	t.Run("ModeNetwork", func(t *testing.T) {
		cfg := testConfig(clock.now, t.TempDir())
		cfg.Mode = ModeNetwork
		p := newPool(cfg, fd)
		svc := config.Service{Name: "my-db", Image: "img", Port: 5432, ReadyTimeout: time.Second, IdleTTL: time.Hour}

		e, err := p.EnsureAll(context.Background(), []config.Service{svc}, []string{"my-db"})
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Env) != 2 {
			t.Fatalf("Env = %v, want 2 entries", e.Env)
		}
		if !strings.HasPrefix(e.Env[0], "GAUNTLET_SVC_MY_DB_HOST=") {
			t.Errorf("Env[0] = %q, want GAUNTLET_SVC_MY_DB_HOST= prefix", e.Env[0])
		}
		if !strings.HasPrefix(e.Env[1], "GAUNTLET_SVC_MY_DB_PORT=") {
			t.Errorf("Env[1] = %q, want GAUNTLET_SVC_MY_DB_PORT= prefix", e.Env[1])
		}
		if len(e.Networks) != 1 {
			t.Fatalf("Networks = %v, want exactly one shared network", e.Networks)
		}
	})

	t.Run("ModePublish", func(t *testing.T) {
		cfg := testConfig(clock.now, t.TempDir())
		cfg.Mode = ModePublish
		p := newPool(cfg, fd)
		svc := config.Service{Name: "cache", Image: "img", Port: 6379, ReadyTimeout: time.Second, IdleTTL: time.Hour}

		e, err := p.EnsureAll(context.Background(), []config.Service{svc}, []string{"cache"})
		if err != nil {
			t.Fatal(err)
		}
		if e.Networks != nil {
			t.Errorf("Networks = %v, want nil in ModePublish", e.Networks)
		}
	})
}

// --- AnyDead (M1) ---

func TestAnyDead(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	p := newPool(cfg, fd)
	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour}

	e, err := p.EnsureAll(context.Background(), []config.Service{svc}, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}

	if p.AnyDead(context.Background(), e) {
		t.Fatal("AnyDead = true for a live instance")
	}

	key := config.ServiceKey(cfg.Remote, svc)
	fd.setNotAlive(key, true)
	if !p.AnyDead(context.Background(), e) {
		t.Fatal("AnyDead = false for a dead instance")
	}

	p.mu.Lock()
	_, stillTracked := p.instances[key]
	p.mu.Unlock()
	if stillTracked {
		t.Error("dead instance was not evicted by AnyDead")
	}

	// review NIT 1: the wrapper's `defer Release(ens)` still runs after
	// AnyDead's eviction (reconcile.go's M1 path calls both on the same
	// Ensured). Release must not resurrect a lastUsed entry for a key
	// AnyDead already dropped from p.instances — Reap only ever ranges
	// p.instances, so such an entry would be permanently unreachable, a
	// slow leak keyed by every service that ever died mid-run.
	p.Release(e)
	p.mu.Lock()
	_, lastUsedLeaked := p.lastUsed[key]
	p.mu.Unlock()
	if lastUsedLeaked {
		t.Error("Release re-added a lastUsed entry for a key AnyDead already evicted")
	}
}

// --- Adopt ---

// seedAdoptable seeds fd with a live instance for svc under cfg (as if left
// running by a prior process) and writes a matching on-disk record — the
// "everything lines up" baseline each Adopt test starts from and then
// perturbs to hit exactly one destroy path.
func seedAdoptable(t *testing.T, fd *fakeDriver, stateDir string, cfg Config, svc config.Service, mode Mode, lastUsed time.Time) (key, name string) {
	t.Helper()
	key = config.ServiceKey(cfg.Remote, svc)
	name = "gauntlet-svc-" + cfg.Token + "-" + key[:12]
	fd.seed(Instance{Name: name, Key: key, ContainerID: "c-" + key[:6], Mode: mode, Host: "h", Port: "1", Spec: svc})
	rec := Record{
		Key: key, Name: name, Repo: cfg.Remote, Mode: mode.String(),
		Spec: svc, ContainerID: "c-" + key[:6], Endpoint: Endpoint{Host: "h", Port: "1"},
		CreatedAt: lastUsed, LastUsed: lastUsed,
	}
	if err := writeRecord(stateDir, rec); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}
	return key, name
}

func TestAdoptMatchAdopts(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	stateDir := t.TempDir()
	cfg := testConfig(clock.now, stateDir)
	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour}
	key, name := seedAdoptable(t, fd, stateDir, cfg, svc, ModeNetwork, clock.now())

	p := newPool(cfg, fd)
	if err := p.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	p.mu.Lock()
	_, ok := p.instances[key]
	p.mu.Unlock()
	if !ok {
		t.Fatal("matching instance was not adopted")
	}
	if fd.wasDestroyed(name) {
		t.Error("adopted instance was destroyed")
	}
}

func TestAdoptUnmatchedDestroyed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	cfg := testConfig(clock.now, t.TempDir())
	name := "gauntlet-svc-" + cfg.Token + "-orphan00000"
	fd.seed(Instance{Name: name, Key: "deadbeefcafebabe", ContainerID: "c1"}) // labeled, but no record on disk

	p := newPool(cfg, fd)
	if err := p.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !fd.wasDestroyed(name) {
		t.Error("unmatchable instance (no record for its key) was not destroyed")
	}
}

func TestAdoptModeMismatchDestroyed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	stateDir := t.TempDir()
	cfg := testConfig(clock.now, stateDir) // cfg.Mode == ModeNetwork
	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour}
	// Record was written under ModePublish; cfg now runs ModeNetwork — M2.
	key, name := seedAdoptable(t, fd, stateDir, cfg, svc, ModePublish, clock.now())

	p := newPool(cfg, fd)
	if err := p.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	p.mu.Lock()
	_, ok := p.instances[key]
	p.mu.Unlock()
	if ok {
		t.Error("mode-mismatched record was adopted (M2 regressed)")
	}
	if !fd.wasDestroyed(name) {
		t.Error("mode-mismatched instance was not destroyed")
	}
}

func TestAdoptUnreadyDestroyed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	stateDir := t.TempDir()
	cfg := testConfig(clock.now, stateDir)
	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour}
	key, name := seedAdoptable(t, fd, stateDir, cfg, svc, ModeNetwork, clock.now())
	fd.setNotReady(key, true)

	p := newPool(cfg, fd)
	if err := p.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	p.mu.Lock()
	_, ok := p.instances[key]
	p.mu.Unlock()
	if ok {
		t.Error("not-ready instance was adopted")
	}
	if !fd.wasDestroyed(name) {
		t.Error("not-ready instance was not destroyed")
	}
}

func TestAdoptPastTTLDestroyed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	fd := newFakeDriver()
	stateDir := t.TempDir()
	cfg := testConfig(clock.now, stateDir)
	svc := config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Minute}
	// LastUsed an hour ago, TTL is 1 minute: well past due at adopt time.
	key, name := seedAdoptable(t, fd, stateDir, cfg, svc, ModeNetwork, clock.now().Add(-time.Hour))

	p := newPool(cfg, fd)
	if err := p.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	p.mu.Lock()
	_, ok := p.instances[key]
	p.mu.Unlock()
	if ok {
		t.Error("past-TTL instance was adopted")
	}
	if !fd.wasDestroyed(name) {
		t.Error("past-TTL instance was not destroyed")
	}
}
