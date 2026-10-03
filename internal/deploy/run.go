package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

// The LANE RUNNER: the half of D2 that makes an environment actually deploy.
//
// The tracker (deploy.go) keeps DESIRED refs where they belong; this file
// closes the loop by running the revision's own deploy graph whenever a
// lane's desired and observed refs disagree, and CAS-advancing OBSERVED
// only once that whole graph finished green. Everything here is
// level-triggered: nothing is queued, nothing is replayed, and every
// decision is re-derived from the two refs plus a lane's in-memory park.
//
// Concurrency shape, and the contract that holds it together:
//
//   - ReconcileOnce stays SINGLE-GOROUTINE (D1's watch item). It never runs
//     a command; it only starts, cancels, and observes lane goroutines.
//   - One goroutine per environment lane, at most one at a time per lane
//     (lane serialization: an environment sees a strict sequence of deploy
//     attempts). Different environments run concurrently.
//   - Lane goroutines NEVER call ReconcileOnce. They communicate back
//     through Tracker.lanes under Tracker.mu, which the next reconcile pass
//     reads — so a run's result reaches the published Snapshot exactly one
//     pass later, the same one-tick propagation the ref model already has.
//
// The four terminal mappings, which are the whole of what a deploy run
// means (Scheduler.Run's return contract on the left):
//
//	culprit == "" && err == nil   → CAS observed O→D; OutcomeLanded
//	culprit != "" with res.Err    → OutcomeError   park (+ auto-retry once)
//	culprit != "" otherwise       → OutcomeRejected park (a red node: no retry)
//	err != nil (ctx cancelled)    → OutcomeSkipped, NO park, reconcile again
//
// A run rejected before its graph could start (an unreadable or invalid
// spec, an environment naming a node the revision doesn't declare, an
// unknown executor profile, a failed export) emits ONLY the terminal
// EventDeployFinished — there was no graph to report started, and claiming
// otherwise would be the fabrication the fail-closed stance exists to
// prevent. Channels must therefore not assume a terminal deploy event was
// preceded by a started one; the D3 consumers (Slack's root-per-run post
// especially) need to handle a terminal that opens and closes at once.

// specRejectPrefix marks the Detail of a park caused by the deployed
// revision's own spec rather than by anything a node did. Never a red
// verdict: no command ran, so no node has a verdict to report — the
// fail-closed stance from docs/architecture/deployment.md ("an environment must
// never silently believe it deployed when no command existed to run").
const specRejectPrefix = "spec reject: "

// runIDTimeFormat is the UTC timestamp portion of a deploy run ID:
// yyyymmddThhmmssZ, the same shape (and the same reason) as the queue's.
const runIDTimeFormat = "20060102T150405Z"

// deployRunIDCounter is the monotonic per-process sequence in every deploy
// run ID. Package-level, exactly like the queue's: two Trackers in one
// process (the tests construct them, and a restart-in-place would too) must
// not mint colliding IDs either. Uniqueness matters here for the same
// reasons it does for a run: log directory names, executor container names,
// and the (RunID, Name) key a gated executor — or a container runtime —
// treats as a job's identity.
var deployRunIDCounter atomic.Int64

// newDeployRunID builds a deploy run ID: "deploy-<utc>-<seq>-<env>-<sha12>".
// Filename-safe by construction (the environment name is sanitized; the
// timestamp and OID are already), unique per attempt (the counter), and
// human-correlatable to both the lane and the revision. The "deploy-" prefix
// keeps the ID self-describing wherever the two ID spaces meet — a log
// directory listing, a container name, an event stream.
func newDeployRunID(t time.Time, env, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	seq := deployRunIDCounter.Add(1)
	return fmt.Sprintf("deploy-%s-%d-%s-%s", t.UTC().Format(runIDTimeFormat), seq, core.SanitizeName(env), sha)
}

// lane is one environment's mutable runner state, guarded by Tracker.mu.
// Everything in it is in-memory by design: the refs are the ground truth,
// so a restart that forgets a park, a spent auto-retry budget, or a synced
// marker costs at most one re-run — and a re-run is the contract deploy
// commands already have to tolerate (docs/architecture/deployment.md,
// "Crash mid-graph").
type lane struct {
	// running/run/cancel describe the graph in flight, if any.
	running bool
	run     laneRun
	cancel  context.CancelFunc

	// cancelled records that this lane's in-flight run has already been
	// signalled (a desired move under "cancel", an operator cancel, ...),
	// so a repeated pass doesn't re-log it; cancelReason is what the run's
	// terminal record says about it.
	cancelled    bool
	cancelReason string

	// parked is the lane's park: level-triggered state about ONE desired
	// revision, cleared by a new desired SHA or an explicit Retry.
	parked *LanePark

	// last is the most recent finished run's summary.
	last *LaneResult

	// syncedSHA is the desired SHA whose observed-ref CAS this process has
	// already completed. It exists because the observed ref is read from
	// the local MIRROR (refreshed by the queue's fetch) while it is written
	// on the REMOTE: without it, every pass between a green run's CAS and
	// the next fetch would see desired != observed and start the very same
	// graph again.
	syncedSHA string

	// autoRetried is the desired SHA whose auto-retry-once budget is spent.
	// A new desired SHA differs, and so gets a fresh budget, with no
	// bookkeeping to prune.
	autoRetried string
}

// laneRun is the in-flight graph's identity plus the nodes currently
// executing, maintained by the scheduler's OnStart/OnFinish callbacks (both
// called on the scheduler's own goroutine, one lane at a time).
type laneRun struct {
	runID     string
	sha       string
	prev      string
	startedAt time.Time
	nodes     []string
}

// runnable reports whether this Tracker was built with the lane-runner
// surface (Params.Exec). Without it the Tracker is exactly D1's: it
// advances desired refs and runs nothing, which is what keeps a daemon
// with no executor wiring byte-identical to the one that shipped before.
func (t *Tracker) runnable() bool { return t.exec != nil }

// stepLane is the reconcile pass's whole interaction with the lane runner,
// called once per environment per pass on the ReconcileOnce goroutine with
// ls already carrying this pass's ref picture. It starts a run, cancels one,
// or does nothing — and always republishes the lane's runner state onto ls.
func (t *Tracker) stepLane(ctx context.Context, env Environment, ls *LaneState) {
	t.mu.Lock()
	defer t.mu.Unlock()

	l := t.lanes[env.Name]
	if l == nil {
		l = &lane{}
		t.lanes[env.Name] = l
	}

	// A park is a statement about one desired revision, so ANY new desired
	// un-parks — including a rollback push to an older SHA, and including
	// while a run is in flight. Done before the switch so it holds even on
	// a pass that can start nothing (draining, no runner wired).
	if l.parked != nil && l.parked.SHA != ls.Desired {
		l.parked = nil
	}

	switch {
	case l.running:
		// on-desired-move. "finish" (the default) does nothing at all: the
		// run completes against the revision it started on, and the next
		// pass after it sees the newer desired and starts a second run —
		// one more tick, one more run. "cancel" kills the in-flight graph
		// now; the run records OutcomeSkipped and does NOT park, so the
		// next pass starts the newer revision.
		if env.OnDesiredMove == OnMoveCancel && ls.Desired != "" && ls.Desired != l.run.sha && !l.cancelled {
			l.cancelled = true
			l.cancelReason = fmt.Sprintf("desired ref moved to %s mid-run (on-desired-move %q)", shortSHA(ls.Desired), OnMoveCancel)
			fmt.Fprintf(t.log, "deploy: %s: cancelling run %s: %s\n", env.Name, l.run.runID, l.cancelReason)
			// Safe under t.mu: a CancelFunc never blocks, and the lane
			// goroutine it wakes simply queues behind this pass for the
			// mutex it needs to record its own conclusion.
			l.cancel()
		}
	default:
		// The admission predicate is consulted exactly once, and its answer
		// is BOTH the decision to start and the published LaneState.Pending
		// — one call site, so the flag an operator (and the idle signal)
		// reads can never drift from the rule that actually admits runs.
		// See LaneState.Pending's doc for what that inheritance implies
		// about draining and about the syncedSHA mirror window.
		ls.Pending = t.canStart(l, ls)
		if ls.Pending {
			t.mu.Unlock()
			err := t.decideDeployment(ctx, env, "admission", ls.Desired, ls.Observed, nil)
			t.mu.Lock()
			ls.Pending = err == nil && t.canStart(l, ls)
			if err != nil {
				ls.LastError = err.Error()
			}
			if ls.Pending {
				t.startLane(ctx, env, l, ls)
			}
		}
	}

	ls.Running = l.runningState()
	ls.Parked = l.parked.clone()
	ls.LastResult = l.last.clone()
}

// canStart is the admission test for a NEW graph run, in the order the
// design states it: a runner exists, the daemon is not draining, the lane
// has drift to close, this process hasn't already closed it, and the lane
// isn't parked at exactly this revision.
func (t *Tracker) canStart(l *lane, ls *LaneState) bool {
	switch {
	case !t.runnable(), t.draining:
		return false
	case ls.Desired == "" || ls.Desired == ls.Observed:
		return false
	case l.syncedSHA == ls.Desired:
		// Already deployed by this process; the mirror just hasn't caught
		// up with the observed ref we pushed.
		return false
	case l.parked != nil:
		// Parked AT this desired revision (a different one was cleared
		// above): level-triggered park semantics — no re-run until a new
		// desired SHA or an explicit Retry.
		return false
	}
	return true
}

// startLane mints the run, marks the lane busy, and launches its goroutine.
// Called with t.mu held.
//
// ctx is the context ReconcileOnce was given, and the run outlives the pass
// that started it: cmd/gauntlet hands the DAEMON's context to ReconcileOnce
// (never a per-tick one), so cancelling it is the hard stop for every
// in-flight deploy graph, exactly as it is for the hooks runner.
func (t *Tracker) startLane(ctx context.Context, env Environment, l *lane, ls *LaneState) {
	runCtx, cancel := context.WithCancel(ctx)
	runID := newDeployRunID(t.now(), env.Name, ls.Desired)

	l.running = true
	l.cancel = cancel
	l.cancelled = false
	l.cancelReason = ""
	l.run = laneRun{runID: runID, sha: ls.Desired, prev: ls.Observed, startedAt: t.now()}

	t.wg.Add(1)
	go t.runGraph(runCtx, env, l, runID, ls.Desired, ls.Observed)
}

// runGraph is one lane goroutine's whole life: execute the graph, then
// conclude it. The split matters — execGraph owns everything the run
// ACQUIRES (the GC pin, the export directory) and releases all of it before
// returning, so by the time finishGraph publishes the terminal event and
// frees the lane, this run holds nothing at all. A terminal deploy event is
// therefore a complete statement: nothing is still running, nothing is
// still pinned, nothing is still on disk.
func (t *Tracker) runGraph(ctx context.Context, env Environment, l *lane, runID, desired, observed string) {
	defer t.wg.Done()

	rec := core.DeployRecord{
		Env:         env.Name,
		RunID:       runID,
		DeploySHA:   desired,
		DeployedSHA: observed,
		StartedAt:   t.now(),
	}
	outcome, detail := t.execGraph(ctx, env, l, &rec, runID, desired, observed)
	t.finishGraph(ctx, env, l, rec, outcome, detail)
}

// execGraph pins the revision, reads its own deploy graph out of its own
// tree, exports it, runs the nodes, and maps the scheduler's verdict — plus
// the observed-ref advance a green graph earns — onto one terminal outcome
// and its explanation. It never touches the lane's own state; that is
// finishGraph's job.
func (t *Tracker) execGraph(ctx context.Context, env Environment, l *lane, rec *core.DeployRecord, runID, desired, observed string) (core.Outcome, string) {
	// GC pin FIRST, before anything reads through the revision: the spec
	// read, the export, and every node resolving GAUNTLET_DEPLOY_SHA
	// through GAUNTLET_GIT_DIR all depend on it surviving git maintenance
	// for the run's whole lifetime. The desired BRANCH keeps it reachable
	// on the remote; the pin is what keeps it reachable here.
	if err := t.git.Pin(ctx, desired); err != nil {
		return core.OutcomeError, "pin deploy revision: " + err.Error()
	}
	// Released on EVERY terminal path, cancellation included — which is
	// what WithoutCancel is for: the pin outlives the context that ended
	// the run, and leaking it would strand an object nothing else
	// references until the next startup sweep.
	defer func() {
		if err := t.git.Unpin(context.WithoutCancel(ctx), desired); err != nil {
			fmt.Fprintf(t.log, "deploy: %s: unpin %s: %v\n", env.Name, shortSHA(desired), err)
		}
	}()

	// The spec comes from the tree of the revision BEING DEPLOYED — the
	// deploy-side twin of "a candidate is tested by its own definition".
	// Rolling an environment back to last week's SHA runs last week's
	// graph, not today's.
	nodes, reason := t.deployGraph(ctx, env, desired)
	if reason != "" {
		return core.OutcomeRejected, specRejectPrefix + reason
	}

	if err := t.decideDeployment(ctx, env, "execution", desired, observed, nodes); err != nil {
		return core.OutcomeRejected, err.Error()
	}

	// ONE shared export for the whole graph run. internal/hooks — the
	// runner this package is a sibling of — exports once per landing and
	// has no isolated mode at all, and a deploy graph's nodes deploy the
	// same revision to the same environment rather than building in a
	// workspace each could poison. If a spec's `workspace "isolated"`
	// policy is ever wanted here, it belongs where the queue puts it: per
	// node, after that node holds its execution slot.
	dir, err := os.MkdirTemp(t.workDir, "gauntlet-deploy-")
	if err != nil {
		return core.OutcomeError, "export tree: mkdir temp: " + err.Error()
	}
	defer os.RemoveAll(dir)
	if err := t.git.ExportTree(ctx, desired, dir); err != nil {
		return core.OutcomeError, "export tree: " + err.Error()
	}
	if t.mtimes {
		// `export { mtimes "history" }` is a daemon-wide statement about
		// every materialization, not one executor's, so a deploy export
		// honors it too — and a failure fails the run rather than running
		// against a tree whose metadata isn't what the config promises.
		if _, err := t.git.RestoreMtimes(ctx, desired, dir); err != nil {
			return core.OutcomeError, "restore mtimes: " + err.Error()
		}
	}

	t.emit(ctx, core.Event{
		Kind:        core.EventDeployStarted,
		At:          t.now(),
		RunID:       runID,
		DeployEnv:   env.Name,
		DeploySHA:   desired,
		DeployedSHA: observed,
	})

	sched := &Scheduler{
		Nodes:       nodes,
		MaxParallel: env.MaxParallel,
		Slots:       t.slots,
		Now:         t.now,
		Exec: func(ctx context.Context, idx int, n Node) core.CheckResult {
			return t.exec.RunCheck(ctx, core.CheckJob{
				RunID:   runID,
				Name:    n.Name,
				Command: n.Command,
				// The node's operator-defined execution profile, routed by
				// executor.Mux exactly as a check's is; "" is the default
				// executor. Gated against the daemon's known profiles at
				// spec load above, never interpreted here.
				Executor: n.Executor,
				Dir:      dir,
				LogPath:  t.nodeLogPath(runID, idx, n.Name),
				// The deploy coordinates. MergeSHA is what every executor
				// already exports as GAUNTLET_MERGE_SHA and what the deploy
				// contract re-exports as GAUNTLET_DEPLOY_SHA; DeployedSHA is
				// the baseline the diff-based skip protocol needs, and is
				// legitimately empty on a first-ever deploy.
				BaseSHA:     observed,
				MergeSHA:    desired,
				Candidate:   core.Candidate{Ref: DesiredRef(env.Name), SHA: desired},
				DeployEnv:   env.Name,
				DeployNode:  n.Name,
				DeployedSHA: observed,
				// Deploy commands are CANDIDATE-CODE class: landed, gated,
				// but still repo-authored, so issue #13's secret stripping
				// applies unchanged. Deploy credentials arrive the way check
				// credentials do — fixed env on an operator-owned executor
				// profile, or workload identity on the builder host.
				OperatorOwned: false,
			})
		},
		OnStart: func(idx int, n Node) { t.nodeStarted(l, n.Name) },
		OnFinish: func(idx int, n Node, res core.CheckResult) {
			t.nodeFinished(l, n.Name)
			t.emit(ctx, core.Event{
				Kind:        core.EventDeployNodeFinished,
				At:          t.now(),
				RunID:       runID,
				CheckName:   n.Name,
				Check:       &res,
				DeployEnv:   env.Name,
				DeploySHA:   desired,
				DeployedSHA: observed,
			})
		},
	}

	rows, culprit, runErr := sched.Run(ctx)
	rec.Nodes = rows
	rec.Culprit = culprit

	switch {
	case runErr != nil && ctx.Err() != nil:
		// Concluded from OUTSIDE: a `cancel`-policy desired move, an
		// operator cancel, a daemon shutdown. Nothing failed, so nothing
		// parks — the lane reconciles fresh on the next tick.
		return core.OutcomeSkipped, t.cancelDetail(l, runErr)
	case runErr != nil:
		// A graph that cannot make progress (unsatisfiable `after` edges,
		// unreachable through the validated path): daemon-side, so it parks
		// as an error rather than blaming a node.
		return core.OutcomeError, runErr.Error()
	case culprit != "":
		if res := rowFor(rows, culprit); res != nil && res.Err != nil {
			// Infra-shaped: the command never returned a verdict (executor
			// unreachable, a slot wait cancelled). Auto-retry-once applies.
			return core.OutcomeError, fmt.Sprintf("deploy node %q: %v", culprit, res.Err)
		}
		// A red verdict is an author problem, exactly like a red candidate:
		// park, no retry loop, cleared by a new desired SHA or a human.
		return core.OutcomeRejected, fmt.Sprintf("deploy node %q failed", culprit)
	default:
		// All green (skipped counts green): advance OBSERVED, and only now.
		if err := t.decideDeployment(ctx, env, "publication", desired, observed, nodes); err != nil {
			return core.OutcomeRejected, err.Error()
		}
		if err := t.git.CASUpdate(ctx, ObservedRef(env.Name), observed, desired); err != nil {
			detail := "advance observed ref: " + err.Error()
			if errors.Is(err, core.ErrCASStale) {
				// Only this daemon ever writes an observed ref, so a lost
				// CAS means something outside the model moved it (a second
				// daemon on the same remote, a hand-edited ref). Say so
				// plainly and park rather than force-pushing over it: the
				// ref is a claim about what an environment is RUNNING, and
				// this run no longer knows whose claim is true.
				detail = fmt.Sprintf("advance observed ref: it no longer held %s — another writer moved it", shortSHA(observed))
			}
			return core.OutcomeError, detail
		}
		return core.OutcomeLanded, ""
	}
}

// finishGraph concludes exactly one graph run: it completes the record,
// releases the lane (park bookkeeping and the auto-retry budget included),
// and then emits the terminal event.
//
// The lane is released BEFORE the event goes out, which makes one useful
// thing true: a terminal deploy event means the lane is already free, so
// nothing has to wait a tick to learn that. The cost is a window in which
// a reconcile pass could start the NEXT run between the release and the
// emit, interleaving two runs' events. Each run's own events stay ordered
// and keyed by its own run ID, so a consumer joining on RunID is unaffected
// — and closing the window would mean holding the lane mutex across the
// channel fan-out, which is exactly the kind of lock a slow channel turns
// into a stalled daemon.
//
// The emit context outlives any cancellation: a cancelled run's terminal
// event is precisely what an operator needs told.
func (t *Tracker) finishGraph(ctx context.Context, env Environment, l *lane, rec core.DeployRecord, outcome core.Outcome, detail string) {
	rec.Outcome = outcome
	rec.Detail = detail
	rec.EndedAt = t.now()

	t.releaseLane(env, l, rec, outcome, detail)

	t.emit(ctx, core.Event{
		Kind:        core.EventDeployFinished,
		At:          rec.EndedAt,
		RunID:       rec.RunID,
		DeployEnv:   rec.Env,
		DeploySHA:   rec.DeploySHA,
		DeployedSHA: rec.DeployedSHA,
		Deploy:      &rec,
		Detail:      detail,
	})
	if outcome != core.OutcomeLanded {
		fmt.Fprintf(t.log, "deploy: %s: run %s %s: %s\n", env.Name, rec.RunID, outcomeWord(outcome), detail)
	}
}

// releaseLane marks the lane idle and applies the terminal outcome to its
// park state, synced marker, and auto-retry budget — everything a later
// reconcile pass reads to decide what happens next.
func (t *Tracker) releaseLane(env Environment, l *lane, rec core.DeployRecord, outcome core.Outcome, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	l.running = false
	l.cancel = nil
	l.cancelled = false
	l.cancelReason = ""
	l.run = laneRun{}
	l.last = &LaneResult{
		RunID:       rec.RunID,
		DeploySHA:   rec.DeploySHA,
		DeployedSHA: rec.DeployedSHA,
		Outcome:     outcome,
		Culprit:     rec.Culprit,
		Detail:      detail,
		StartedAt:   rec.StartedAt,
		EndedAt:     rec.EndedAt,
	}

	switch outcome {
	case core.OutcomeLanded:
		l.syncedSHA = rec.DeploySHA
		l.parked = nil
		return
	case core.OutcomeSkipped:
		// Concluded externally: no park. Whatever ended it (a newer
		// desired, a cancel, a drain) is what the next pass reconciles
		// against.
		return
	}

	l.parked = &LanePark{
		SHA:     rec.DeploySHA,
		RunID:   rec.RunID,
		Outcome: outcome,
		Detail:  detail,
		At:      rec.EndedAt,
	}

	// Auto-retry-once, the standing narrow exception (DESIGN.md's ledger,
	// "Auto-retry once on infra-error parks"), with identical semantics to
	// the queue's: OutcomeError ONLY (never a red verdict — that's an
	// author problem), exactly once per (env, desired SHA), fresh budget on
	// a new desired SHA, and never while draining (an auto-retry would put
	// fresh work into a finite drain set). Clearing the park IS the retry:
	// the lane is level-triggered, so the next pass sees drift and re-runs
	// the whole graph.
	if outcome == core.OutcomeError && t.autoRetry && !t.draining && l.autoRetried != rec.DeploySHA {
		l.autoRetried = rec.DeploySHA
		l.parked = nil
		fmt.Fprintf(t.log, "deploy: %s: auto-retry: park cleared (infra error) sha=%s\n", env.Name, shortSHA(rec.DeploySHA))
	}
}

// deployGraph reads the deploy graph the revision at sha declares in its own
// tree and resolves env's `nodes` selection against it. It returns either
// the graph in DECLARATION order or a non-empty spec-rejection reason —
// never both, and never an error a caller has to classify: every failure
// here is the same fail-closed park, distinguished only by its Detail.
//
// The four rejections, in the order they can be detected:
//
//	the spec is unreadable      — no file at CheckSpec in that tree
//	the spec is invalid         — parse or graph validation failed
//	the selection names nothing — `nodes` lists a node the spec lacks
//	the profile is unknown      — a node selects an undefined executor
//	the graph is empty          — the environment expects to deploy and the
//	                              revision declares no deploy nodes at all
func (t *Tracker) deployGraph(ctx context.Context, env Environment, sha string) ([]Node, string) {
	data, err := t.git.ReadFileFromTree(ctx, sha, t.specPath)
	if err != nil {
		return nil, fmt.Sprintf("deploy spec %q: %v", t.specPath, err)
	}
	spec, err := config.ParseChecks(data)
	if err != nil {
		return nil, fmt.Sprintf("deploy spec %q: %v", t.specPath, err)
	}
	selected, err := spec.SelectDeployNodes(env.Nodes)
	if err != nil {
		return nil, err.Error()
	}
	if len(selected) == 0 {
		// An environment is configured, so it expects to deploy something;
		// a revision declaring no deploy nodes cannot satisfy that, and
		// silently "succeeding" would advance the observed ref on the
		// strength of no command at all.
		return nil, "no deploy nodes declared"
	}
	nodes := make([]Node, len(selected))
	for i, d := range selected {
		// The same config-owned known-profile gate the queue applies at
		// spec load, with the queue's own nil semantics: a nil predicate
		// means this daemon defines no named profiles, so ANY non-empty
		// selection is unknown.
		if d.Executor != "" && (t.knownProfile == nil || !t.knownProfile(d.Executor)) {
			return nil, fmt.Sprintf("deploy node %q selects unknown executor profile %q", d.Name, d.Executor)
		}
		nodes[i] = Node{Name: d.Name, After: d.After, Command: d.Command, Executor: d.Executor}
	}
	return nodes, ""
}

// nodeLogPath is one deploy node's full combined-output log file:
// <LogDir>/<deployRunID>/<seq>-<sanitized node>.log.zst — the queue's
// per-check shape verbatim, under the deploy run's own directory. That
// directory layout is what puts deploy logs inside cmd/gauntlet's existing
// retention sweep (pruneLogFiles prunes whole per-run directories by mtime)
// without the sweep needing to know deploys exist. "" when no LogDir is
// configured: the executor then writes no file at all.
func (t *Tracker) nodeLogPath(runID string, idx int, name string) string {
	if t.logDir == "" {
		return ""
	}
	return filepath.Join(t.logDir, runID, fmt.Sprintf("%d-%s.log.zst", idx+1, core.SanitizeName(name)))
}

// nodeStarted/nodeFinished maintain the in-flight node list the Snapshot
// publishes. Both are called from the scheduler's own goroutine (never a
// node's), so the only concurrency here is with a reconcile pass reading it.
func (t *Tracker) nodeStarted(l *lane, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l.run.nodes = append(l.run.nodes, name)
}

func (t *Tracker) nodeFinished(l *lane, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, n := range l.run.nodes {
		if n == name {
			l.run.nodes = append(l.run.nodes[:i], l.run.nodes[i+1:]...)
			return
		}
	}
}

// cancelDetail explains an externally concluded run: the reason whoever
// cancelled it recorded, or the raw context error when the run died with
// the daemon rather than by anyone's decision.
func (t *Tracker) cancelDetail(l *lane, err error) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l.cancelReason != "" {
		return l.cancelReason
	}
	return "run concluded externally: " + err.Error()
}

// emit hands ev to the daemon's channel fan-out, if one was wired. The
// context is stripped of cancellation for the same reason internal/hooks
// strips it: notification delivery must not be short-circuited by the very
// cancellation the event is reporting.
func (t *Tracker) emit(ctx context.Context, ev core.Event) {
	if t.notify == nil {
		return
	}
	t.notify(context.WithoutCancel(ctx), ev)
}

// Retry clears env's park so the next reconcile pass re-runs its whole
// graph, reporting whether there was a park to clear. This is the
// env-addressed operator retry the D3 API/MCP/dashboard surfaces call
// (POST /api/v1/deploy/retry); it is deliberately not a core.Command, since
// a lane has no candidate ref for a ref-addressed command to name.
//
// Retry re-runs the WHOLE graph, never the red suffix: green nodes run
// again and either self-skip through the diff protocol or re-execute
// idempotently. Resuming from recorded per-node results would make history
// rows correctness state, which they are forbidden to be.
//
// It deliberately does NOT refresh the auto-retry-once budget: that budget
// exists to absorb infra flakes without a human, and a human is exactly
// what just arrived. Nil-safe, like every other operator entry point here,
// so cmd can thread it whether or not deployment is configured.
func (t *Tracker) Retry(env string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.lanes[env]
	if l == nil || l.parked == nil {
		return false
	}
	l.parked = nil
	return true
}

// CancelCurrent cancels env's in-flight graph run, reporting whether there
// was one to signal — the deploy twin of hooks.Runner.CancelCurrent, and
// the D3 POST /api/v1/deploy/cancel surface. The cancelled run records
// OutcomeSkipped and does not park, so the lane reconciles fresh on the
// next tick (and, with desired unchanged, deploys the same revision again):
// cancelling is not a way to stop deploying, it is a way to interrupt one
// attempt.
//
// A repeated call while the same run is still winding down returns true
// again: like its hooks counterpart, true means "a cancellation was
// delivered", not "the commands are already dead".
func (t *Tracker) CancelCurrent(env string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.lanes[env]
	if l == nil || !l.running || l.cancel == nil {
		return false
	}
	if !l.cancelled {
		l.cancelled = true
		l.cancelReason = "cancelled by operator"
	}
	l.cancel()
	return true
}

// Drain refuses NEW graph runs while letting in-flight ones finish — the
// deploy half of the daemon's graceful drain (issue #8), and the same
// admission-gate shape the queue uses. It is one-way: nothing resumes
// admission short of a restart, which is what makes a drain a drain.
// Context cancellation remains the hard stop.
func (t *Tracker) Drain() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.draining = true
}

// Wait blocks until no lane has a graph in flight, or ctx is done — the
// "let in-flight finish" half of the drain, called by cmd after Drain and
// after the queue itself has stopped landing. Drain having returned is what
// makes this sound: admission is closed under the same mutex the lane
// counter is incremented under, so by the time Wait runs the counter can
// only fall. Safe to call on a nil Tracker (deployment not configured:
// nothing to wait for).
func (t *Tracker) Wait(ctx context.Context) {
	if t == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// runningState renders the lane's in-flight run for the published Snapshot,
// or nil when the lane is idle. Called with t.mu held; the returned value
// is a fresh copy, never a view onto mutable state.
func (l *lane) runningState() *LaneRun {
	if !l.running {
		return nil
	}
	return &LaneRun{
		RunID:       l.run.runID,
		DeploySHA:   l.run.sha,
		DeployedSHA: l.run.prev,
		StartedAt:   l.run.startedAt,
		Nodes:       append([]string(nil), l.run.nodes...),
	}
}

func (p *LanePark) clone() *LanePark {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func (r *LaneResult) clone() *LaneResult {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// rowFor returns the materialized row for name, or nil — how the culprit's
// own result is recovered to tell a red VERDICT (park, no retry) from a
// daemon-side Err (park, auto-retry eligible).
func rowFor(rows []core.CheckResult, name string) *core.CheckResult {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

// outcomeWord renders an outcome for this package's log lines only.
func outcomeWord(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "deployed"
	case core.OutcomeRejected:
		return "parked (rejected)"
	case core.OutcomeError:
		return "parked (error)"
	case core.OutcomeSkipped:
		return "skipped"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// shortSHA truncates a full OID to git's usual abbreviation length for log
// lines and Detail strings; shorter inputs (test fixtures) pass through.
func shortSHA(sha string) string {
	const n = 8
	if len(sha) > n {
		return sha[:n]
	}
	return sha
}
