package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// The deploy node-graph scheduler: `after` edges plus max-parallel, the
// same whole grammar checks have, run for one environment's graph.
//
// It is a deliberate SECOND implementation of internal/queue's
// advanceChecks, not an extraction of it — the D2 spike's verdict
// (docs/design/deployment.md, Phase D2) after finding that only ~85 of the
// queue scheduler's lines are graph logic, that the rest is queue tenancy
// (image/receipt validation mutating results mid-drain, batch attribution,
// trial-ref gates), and that the two tenants want different admission
// modes: the reconcile loop must never block, so it polls with TryAcquire
// and carries readyAt bookkeeping across ticks, while a deploy lane is a
// goroutine (the internal/hooks precedent) that simply blocks. The four
// steps below mirror advanceChecks's four exactly, and the one predicate
// that IS shared — core.NodeGreen — is shared rather than restated.
//
// The divergence guard is the test suite: graph_test.go ports every
// behavior of internal/queue/parallel_test.go under the same names. When
// one scheduler's behavior changes, the other's port fails or visibly
// diverges — which is the point of writing them twice.

// Node is one declared deploy node: a name, its `after` edges, and what to
// run. The slice order IS spec-declaration order — the durable per-node
// identity (Seq, log filenames, row order) — so callers build it from the
// revision's own spec and never sort it.
//
// The scheduler itself reads only Name and After: Command and Executor are
// carried here for Exec's benefit (the lane runner turns them into a
// core.CheckJob), so that "what the graph is" and "what each node runs"
// arrive as one value read from one spec rather than two lookups that could
// disagree.
type Node struct {
	Name  string
	After []string

	// Command is the argv to execute; Executor names the operator-defined
	// execution profile it runs on ("" = the daemon's default), gated
	// against the daemon's known profiles at spec load.
	Command  []string
	Executor string
}

// Exec runs one node's command and returns its verdict. It is called on
// that node's own goroutine with the daemon-wide execution slot ALREADY
// held, so it may block freely; idx is the node's spec position, for
// per-node log paths and Seq-shaped bookkeeping.
//
// ctx cancellation must kill the command: the scheduler's fail-fast
// cancels it and then WAITS for this function to return (see Run), so an
// Exec that ignores cancellation stalls the whole lane.
type Exec func(ctx context.Context, idx int, n Node) core.CheckResult

// Scheduler runs one graph, once. It is not reusable and not safe for
// concurrent use: the lane runner constructs one per graph run, exactly as
// a run is constructed per candidate.
type Scheduler struct {
	// Nodes is the graph in spec order; MaxParallel bounds how many of its
	// nodes may be in flight at once (<= 0 is read as 1 — config already
	// guarantees >= 1, this is only defense against a hand-built value).
	Nodes       []Node
	MaxParallel int

	// Slots is the daemon-wide execution cap every bounded invocation
	// shares — checks, hooks, image builds, and now deploy nodes, so a
	// deploy burst and a check burst negotiate over the same honest host
	// capacity. nil means unlimited.
	Slots *core.Slots

	// Exec runs a node; required.
	Exec Exec

	// Now is the injected clock behind CheckResult.Waited. nil means
	// time.Now.
	Now func() time.Time

	// OnStart and OnFinish are the scheduler's whole observability surface
	// (the lane runner turns them into EventDeployStarted-scoped node
	// events and log lines). Both are called ON RUN'S OWN GOROUTINE — never
	// from a node goroutine — so an implementation needs no locking of its
	// own. Either may be nil.
	//
	// OnStart fires immediately AFTER a node's goroutine is launched, the
	// same point queue's startCheck emits EventCheckStarted: that goroutine
	// may already be blocked on the daemon-wide cap, or even finished, by
	// the time this returns. Ordering is still sound — OnFinish can only
	// run once Run leaves its start loop.
	//
	// OnFinish fires only for a node whose result is KEPT: a straggler
	// returning after fail-fast is discarded and reported as a blocked row
	// instead, and firing "finished" for a row that says "never ran" is
	// exactly the lie the drain-then-cull ordering exists to prevent.
	OnStart  func(idx int, n Node)
	OnFinish func(idx int, n Node, res core.CheckResult)
}

// Run executes the graph and returns one row per DECLARED node in spec
// order, the culprit (the node whose non-green result failed the graph, ""
// if none), and an error for a run that was concluded from OUTSIDE — ctx
// cancellation (a `cancel`-policy desired move, daemon shutdown) or a graph
// that cannot make progress.
//
// The four steps mirror internal/queue's advanceChecks:
//
//  1. START every READY node — all `after` edges core.NodeGreen — in spec
//     order, up to MaxParallel in flight. Each node's goroutine takes its
//     daemon-wide slot FIRST and holds it until its command has fully
//     returned, so a freed slot never represents a live process.
//  2. DRAIN: block for one result, then consume every other result already
//     buffered before deciding anything. A node that ran to completion in
//     the same window as a failing sibling keeps its real result — culling
//     first would record a node that RAN as "blocked, never ran".
//  3. CULL: with the drain complete, the first non-green result in SPEC
//     order (deterministic whatever the completion order was) becomes the
//     culprit and cancels everything still running.
//  4. ROWS: every declared node appears, finished ones with their real
//     result, unfinished ones as CheckBlocked naming what blocked them.
//
// Run always returns rows — a graph that failed still describes which half
// deployed, which is precisely what a parked lane needs to say.
func (s *Scheduler) Run(ctx context.Context) (rows []core.CheckResult, culprit string, err error) {
	now := s.Now
	if now == nil {
		now = time.Now
	}
	maxParallel := s.MaxParallel
	if maxParallel < 1 {
		maxParallel = 1
	}

	// runCtx is what nodes run under: cancelling it is the fail-fast, and
	// it dies with ctx so an external cancellation reaches the commands
	// too. The deferred cancel covers every early return.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered by the node count so a node goroutine can always deliver and
	// exit — including a straggler nobody will read until the wait below,
	// and one whose result is discarded outright.
	done := make(chan nodeDone, len(s.Nodes))

	results := make(map[string]core.CheckResult, len(s.Nodes))
	started := make([]bool, len(s.Nodes))
	inflight := 0

	// keep decides whether an arriving result is recorded or discarded. A
	// run that has already concluded — fail-fast culprit, or an external
	// cancellation — keeps nothing further: those commands' outcomes can no
	// longer matter, and recording a cancelled node's Err result as its
	// verdict would blame the node for the lane's own decision. This
	// mirrors queue's cancelRun, which drops in-flight results wholesale.
	keep := func() bool { return culprit == "" && ctx.Err() == nil }

	store := func(f nodeDone) {
		if !keep() {
			return
		}
		results[s.Nodes[f.idx].Name] = f.res
		if s.OnFinish != nil {
			s.OnFinish(f.idx, s.Nodes[f.idx], f.res)
		}
	}

	for {
		// (1) start every ready node, spec order, under the caps. A
		// concluded run starts nothing more.
		if keep() {
			for i := range s.Nodes {
				if inflight >= maxParallel {
					break
				}
				n := s.Nodes[i]
				if started[i] {
					continue
				}
				ready := true
				for _, dep := range n.After {
					if res, ok := results[dep]; !ok || !core.NodeGreen(res) {
						ready = false
						break
					}
				}
				if !ready {
					continue
				}
				started[i] = true
				inflight++
				s.start(runCtx, done, i, n, now)
				if s.OnStart != nil {
					s.OnStart(i, n)
				}
			}
		}

		if inflight == 0 {
			break
		}

		// (2) drain: one blocking receive, then everything else already
		// buffered, BEFORE any culling decision. Every started goroutine
		// sends exactly once — including one that never got a slot — so
		// this receive cannot hang on a cancelled run.
		//
		// Blocking here is also how a concluded run WAITS OUT its
		// stragglers: after the cull below cancels them, the loop keeps
		// receiving (and discarding) until inflight reaches zero. That is a
		// deliberate divergence from queue's cancelRun, which abandons
		// in-flight checks and lets their goroutines finish unobserved —
		// the reconcile loop cannot afford to wait, a lane goroutine can,
		// and waiting is what makes "never two graph runs in flight for one
		// environment" true of the COMMANDS rather than only of the
		// bookkeeping. A deploy graph re-run whose predecessor's migration
		// is still running is exactly the overlap the lane is supposed to
		// prevent.
		f := <-done
		inflight--
		store(f)
		for draining := true; draining; {
			select {
			case f := <-done:
				inflight--
				store(f)
			default:
				draining = false
			}
		}

		// (3) cull: first non-green in SPEC order, then fail fast. Skipped
		// counts green (core.NodeGreen) — a node's own "this revision needs
		// nothing from me" verdict satisfies its dependents' edges.
		if keep() {
			for i := range s.Nodes {
				if res, ok := results[s.Nodes[i].Name]; ok && !core.NodeGreen(res) {
					culprit = s.Nodes[i].Name
					cancel()
					break
				}
			}
		}
	}

	switch {
	case ctx.Err() != nil:
		// Concluded from outside: no failure to attribute to any node, and
		// the caller's own terminal record explains the rest. A culprit
		// found in the same window as the cancellation is dropped for the
		// same reason.
		culprit, err = "", ctx.Err()
	case culprit == "" && len(results) != len(s.Nodes):
		// Defense in depth: nothing is in flight, nothing is ready, and
		// nodes remain — an `after` edge naming a node outside this run's
		// graph (config validates edges at spec load, and an environment's
		// `nodes` selection is closed over them, so this is unreachable
		// through the supported path). Fail loudly rather than report a
		// green graph that never ran.
		err = fmt.Errorf("deploy: %d of %d nodes never became ready: unsatisfiable after edges", len(s.Nodes)-len(results), len(s.Nodes))
	}

	return s.materialize(results, culprit), culprit, err
}

// nodeDone is one node goroutine's single message back to Run: its spec
// index and its result.
type nodeDone struct {
	idx int
	res core.CheckResult
}

// start launches one node's goroutine. The slot is taken FIRST and
// released only after Exec has returned — the hooks runner's blocking
// Acquire, not the queue's non-blocking TryAcquire: a lane goroutine may
// block, and a deploy node that waits for capacity is waiting, not failing.
func (s *Scheduler) start(ctx context.Context, done chan<- nodeDone, idx int, n Node, now func() time.Time) {
	go func() {
		// Waited is stamped ONLY when the daemon-wide cap actually denied
		// this node, exactly as the queue stamps readyAt only on a denial:
		// a node queued behind its own graph's max-parallel is not starving
		// and must not report capacity pressure. TryAcquire first keeps the
		// common immediate start at a clean zero.
		var waited time.Duration
		if !s.Slots.TryAcquire() {
			start := now()
			if err := s.Slots.Acquire(ctx); err != nil {
				// Cancelled while waiting for capacity: no command ever
				// ran, so this is a daemon-caused Err, never a verdict
				// (core.CheckResult's Status-vs-Err contract). It is SENT
				// rather than dropped because Run counts every started
				// node back in before it returns — a silent exit here
				// would hang the lane. Run will discard the value itself:
				// Acquire only fails once this ctx is dead, and a run
				// whose ctx is dead keeps nothing further.
				done <- nodeDone{idx, core.CheckResult{Name: n.Name, Waited: now().Sub(start), Err: fmt.Errorf("waiting for an execution slot: %w", err)}}
				return
			}
			waited = now().Sub(start)
		}
		defer s.Slots.Release()

		res := s.Exec(ctx, idx, n)
		res.Name = n.Name // the scheduler owns node identity, not Exec
		res.Waited = waited
		done <- nodeDone{idx, res}
	}()
}

// materialize builds one row per DECLARED node in spec order with Seq the
// 1-based spec position — the durable per-node identity history's seq
// column and the log filename prefix share, stable regardless of the order
// results arrived.
//
// A failed graph gives every unfinished node a CheckBlocked row whose
// BlockedBy names its own non-green `after` edges when it has any, and
// otherwise the run's culprit (a node that was in flight, or independent,
// when the graph went red) — materializeChecks's exact fallback. A run
// concluded EXTERNALLY materializes only what finished: there is no failure
// to attribute, and fabricating blocked rows would claim the graph reached
// a verdict it never reached.
func (s *Scheduler) materialize(results map[string]core.CheckResult, culprit string) []core.CheckResult {
	var out []core.CheckResult
	for i := range s.Nodes {
		n := s.Nodes[i]
		if res, ok := results[n.Name]; ok {
			res.Seq = i + 1
			out = append(out, res)
			continue
		}
		if culprit == "" {
			continue // externally concluded (or stuck); unstarted nodes get no row
		}
		var blockedBy []string
		for _, dep := range n.After {
			if res, ok := results[dep]; !ok || !core.NodeGreen(res) {
				blockedBy = append(blockedBy, dep)
			}
		}
		if len(blockedBy) == 0 {
			blockedBy = []string{culprit}
		}
		out = append(out, core.CheckResult{Name: n.Name, Seq: i + 1, Status: core.CheckBlocked, BlockedBy: blockedBy})
	}
	return out
}
