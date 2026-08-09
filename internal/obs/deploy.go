package obs

// Deploy observability (docs/design/deployment.md, "Logs and history"): a
// `deploy` root span per environment graph run with a `deploy-node` child
// per node, plus terminal metrics — the same shape the queue's run tree and
// node histograms already have, over the deploy event stream.
//
// The shape difference that drives this whole file: the queue OWNS its
// spans. reconcile.go holds the root span in the run struct and hands the
// derived context to each check, so parenting is a local variable. Nothing
// like that exists here — internal/deploy publishes EVENTS and knows
// nothing about OTel (it would not compile against it), and cmd/gauntlet
// fans those events out to channels. So the linkage a caller would
// otherwise hold has to live somewhere, and that somewhere is
// DeployRecorder: a runID -> (context, span) map opened by the started
// event, read by node-finished events, and closed (deleted) by the terminal
// one. That is the entire reason this type exists; it holds no other state.
//
// Both spans are recorded with EXPLICIT TIMESTAMPS rather than "now": an
// event reaches this recorder after passing through the daemon's channel
// fan-out, and a node's own interval is already a finished fact by the time
// its event exists. Reconstructing [At-Duration, At] from the event is the
// only way the span tree shows what actually happened rather than when the
// notification arrived.
//
// Cardinality (metrics.go's package-doc rule, restated because this file
// adds a second family of instruments): the deploy metric attribute set is
// STRICTLY environment, node name, and outcome — all config- or
// spec-bounded, exactly as the design promised. NEVER a run ID and NEVER a
// SHA: those are per-deploy identifiers with unbounded cardinality. Both
// already ride the SPANS here, which is where a per-run drill-down belongs.

import (
	"context"
	"errors"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys used on deploy spans and (the first three) deploy metric
// data points.
const (
	// AttrDeployEnv is the environment being deployed — daemon config
	// names it, so it is bounded by the operator's own environment list.
	AttrDeployEnv = "gauntlet.deploy.env"

	// AttrDeployNode is the deploy node's declared name from the deployed
	// revision's spec — bounded the same way AttrNodeName is.
	AttrDeployNode = "gauntlet.deploy.node"

	// AttrDeploySHA and AttrDeployedSHA are the revision being deployed and
	// the observed ref's value before this run ("" on a first-ever deploy).
	// SPAN-ONLY, deliberately: see this file's cardinality note.
	AttrDeploySHA   = "gauntlet.deploy.sha"
	AttrDeployedSHA = "gauntlet.deploy.deployed_sha"

	// AttrDeployCulprit names the node whose non-green result failed the
	// graph; AttrDeployNodesTotal counts the declared nodes of the run's
	// graph, blocked rows included. Span-only, like the SHAs.
	AttrDeployCulprit    = "gauntlet.deploy.culprit"
	AttrDeployNodesTotal = "gauntlet.deploy.nodes.total"
)

// deployRun is one in-flight graph run's span linkage: the context to parent
// node spans from, and the root span itself to end at terminal.
type deployRun struct {
	ctx  context.Context
	span trace.Span
}

// DeployRecorder turns the deploy event stream into spans and metrics. It is
// safe for concurrent use: lanes run their graphs on their own goroutines
// and cmd's fan-out calls Observe from whichever one emitted.
//
// A nil *DeployRecorder is a valid no-op receiver, so cmd/gauntlet can
// thread one unconditionally even when deployment isn't configured — the
// same nil-safety internal/deploy's own operator entry points have.
type DeployRecorder struct {
	tracer trace.Tracer
	hist   *deployHistograms

	mu   sync.Mutex
	runs map[string]deployRun
}

// NewDeployRecorder builds a recorder against gauntlet's shared tracer and
// meter. Call once, at daemon start, and only when deployment is actually
// configured — with no provider installed every span and instrument it
// produces is a no-op, but there is no reason to keep a map for a subsystem
// that will never emit.
func NewDeployRecorder() *DeployRecorder {
	h, err := newDeployHistograms(Meter())
	if err != nil {
		otel.Handle(err)
	}
	return &DeployRecorder{tracer: Tracer(), hist: h, runs: make(map[string]deployRun)}
}

// Observe records ev. It is called at the TOP of cmd/gauntlet's deploy Emit
// closure, before the channel fan-out, so a slow or blocked channel can
// never delay or reorder the span tree — and so the span tree exists even
// for a daemon with no channels configured at all.
//
// Every non-deploy event is ignored, so a caller may hand it the whole
// stream without filtering.
func (r *DeployRecorder) Observe(ctx context.Context, ev core.Event) {
	if r == nil {
		return
	}
	switch ev.Kind {
	case core.EventDeployStarted:
		r.startRun(ctx, ev)
	case core.EventDeployNodeFinished:
		r.recordNodeEvent(ev)
	case core.EventDeployFinished:
		r.finishRun(ctx, ev)
	}
}

// startRun opens the root span for one graph run and remembers it under the
// run's ID.
//
// ctx is the parent for the root span, and it is deliberately whatever the
// emitter passed — a deploy lane is not inside any request or run trace, so
// in practice this is the daemon's own context and the span is a true root.
func (r *DeployRecorder) startRun(ctx context.Context, ev core.Event) {
	runCtx, span := r.tracer.Start(ctx, "deploy",
		trace.WithTimestamp(ev.At),
		trace.WithAttributes(
			attribute.String(AttrRunID, ev.RunID),
			attribute.String(AttrDeployEnv, ev.DeployEnv),
			attribute.String(AttrDeploySHA, ev.DeploySHA),
			attribute.String(AttrDeployedSHA, ev.DeployedSHA),
		))

	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, exists := r.runs[ev.RunID]; exists {
		// A duplicated started event would otherwise orphan the first root
		// forever (nothing else ever ends it): end the loser immediately so
		// the map holds exactly one live span per run ID.
		prev.span.End(trace.WithTimestamp(ev.At))
	}
	r.runs[ev.RunID] = deployRun{ctx: runCtx, span: span}
}

// recordNodeEvent emits one node's child span and its metrics.
//
// The span's interval is RECONSTRUCTED as [At-Duration, At] rather than
// started and ended around a live call: by the time this event exists the
// node has already finished, so its true interval is only derivable from
// the result it carries. A node with no recorded duration collapses to an
// instant at At, which is the honest rendering of "we know when it ended and
// nothing more".
//
// A node whose run has no root span (this recorder started mid-graph, or the
// started event was lost) records its METRICS anyway and skips the span: a
// parentless `deploy-node` span floating at the trace root would claim a
// linkage that isn't there, whereas the measurement is true regardless.
func (r *DeployRecorder) recordNodeEvent(ev core.Event) {
	if ev.Check == nil {
		return
	}
	res := *ev.Check
	r.recordNodeMetrics(ev.DeployEnv, res)

	r.mu.Lock()
	run, ok := r.runs[ev.RunID]
	r.mu.Unlock()
	if !ok {
		return
	}

	_, span := r.tracer.Start(run.ctx, "deploy-node",
		trace.WithTimestamp(ev.At.Add(-res.Duration)),
		trace.WithAttributes(deployNodeAttributes(ev.DeployEnv, res)...))
	setNodeStatus(span, res)
	span.End(trace.WithTimestamp(ev.At))
}

// finishRun ends the root span with the terminal record's summary and drops
// the run from the map.
//
// The BARE TERMINAL case — no root span for this run ID — is not an error
// and not a drop: a spec rejection parks the lane before any node runs and
// emits no started event at all, and that park is exactly the fact worth
// tracing. A root is synthesized from the record's own StartedAt/EndedAt so
// the trace shows a real (if childless) deploy attempt rather than nothing.
func (r *DeployRecorder) finishRun(ctx context.Context, ev core.Event) {
	rec := ev.Deploy
	if rec == nil {
		return
	}
	r.recordRunMetrics(rec)

	r.mu.Lock()
	run, ok := r.runs[ev.RunID]
	delete(r.runs, ev.RunID)
	r.mu.Unlock()

	span := run.span
	if !ok {
		_, span = r.tracer.Start(ctx, "deploy",
			trace.WithTimestamp(rec.StartedAt),
			trace.WithAttributes(
				attribute.String(AttrRunID, rec.RunID),
				attribute.String(AttrDeployEnv, rec.Env),
				attribute.String(AttrDeploySHA, rec.DeploySHA),
				attribute.String(AttrDeployedSHA, rec.DeployedSHA),
			))
	}

	span.SetAttributes(
		attribute.String(AttrOutcome, outcomeString(rec.Outcome)),
		attribute.String(AttrDeployCulprit, rec.Culprit),
		attribute.String(AttrDetail, rec.Detail),
		attribute.Int(AttrDeployNodesTotal, len(rec.Nodes)),
	)
	code, desc := outcomeStatus(rec.Outcome, rec.Detail)
	span.SetStatus(code, desc)
	span.End(trace.WithTimestamp(rec.EndedAt))
}

// InFlight reports how many graph runs this recorder currently holds a root
// span for — the same number RegisterDeployGauge observes. Exported so cmd
// can wire the gauge to the recorder it already built rather than reaching
// into internal/deploy for a second, differently-timed count.
func (r *DeployRecorder) InFlight() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// deployNodeAttributes builds one deploy node's SPAN attribute set: the
// lane, the node, its verdict and duration, plus resource usage when it was
// actually measured — the same zero-means-unmeasured gate checkAttributes
// applies, for the same reason (a recorded zero would misread as a measured
// zero).
func deployNodeAttributes(env string, res core.CheckResult) []attribute.KeyValue {
	kvs := []attribute.KeyValue{
		attribute.String(AttrDeployEnv, env),
		attribute.String(AttrDeployNode, res.Name),
		attribute.String(AttrCheckStatus, checkStatusString(res.Status)),
		attribute.Int64(AttrCheckDuration, res.Duration.Milliseconds()),
	}
	if res.PeakRSS > 0 {
		kvs = append(kvs, attribute.Int64(AttrCheckPeakRSS, res.PeakRSS))
	}
	if res.UserCPU.Milliseconds() > 0 {
		kvs = append(kvs, attribute.Int64(AttrCheckUserCPU, res.UserCPU.Milliseconds()))
	}
	if res.SysCPU.Milliseconds() > 0 {
		kvs = append(kvs, attribute.Int64(AttrCheckSysCPU, res.SysCPU.Milliseconds()))
	}
	return kvs
}

// setNodeStatus maps a node's result onto its span status, mirroring
// EndCheck's mapping exactly so a deploy node and a check never disagree
// about what a given result's terminal shape was called.
func setNodeStatus(span trace.Span, res core.CheckResult) {
	switch {
	case res.Err != nil:
		span.SetStatus(codes.Error, res.Err.Error())
	case res.Status == core.CheckFailed:
		span.SetStatus(codes.Error, res.Name+" failed")
	case res.Status == core.CheckSkipped, res.Status == core.CheckBlocked:
		span.SetStatus(codes.Unset, "")
	default:
		span.SetStatus(codes.Ok, "")
	}
}

// --- metrics ----------------------------------------------------------------

// deployHistograms holds the deploy-side instruments: four per-node, one
// per-run. Deliberately separate from nodeHistograms rather than reusing
// gauntlet.node.* — those carry gauntlet.target, and a deploy has no target;
// forcing an empty or synthetic target onto them would corrupt every
// existing per-target query for the sake of not adding four names.
type deployHistograms struct {
	nodeDuration metric.Int64Histogram
	nodePeakRSS  metric.Int64Histogram
	nodeUserCPU  metric.Int64Histogram
	nodeSysCPU   metric.Int64Histogram
	runDuration  metric.Int64Histogram
}

func newDeployHistograms(m metric.Meter) (*deployHistograms, error) {
	var errs []error
	record := func(h metric.Int64Histogram, err error) metric.Int64Histogram {
		if err != nil {
			errs = append(errs, err)
		}
		return h
	}

	dh := &deployHistograms{}
	dh.nodeDuration = record(m.Int64Histogram("gauntlet.deploy.node.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Wall-clock duration of one finished deploy node."),
	))
	dh.nodePeakRSS = record(m.Int64Histogram("gauntlet.deploy.node.peak_rss",
		metric.WithUnit("By"),
		metric.WithDescription("Peak resident-set size of one finished deploy node's process tree; only recorded when measured."),
	))
	dh.nodeUserCPU = record(m.Int64Histogram("gauntlet.deploy.node.user_cpu",
		metric.WithUnit("ms"),
		metric.WithDescription("User CPU time of one finished deploy node's process tree; only recorded when measured."),
	))
	dh.nodeSysCPU = record(m.Int64Histogram("gauntlet.deploy.node.sys_cpu",
		metric.WithUnit("ms"),
		metric.WithDescription("System CPU time of one finished deploy node's process tree; only recorded when measured."),
	))
	dh.runDuration = record(m.Int64Histogram("gauntlet.deploy.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Wall-clock duration of one finished deploy graph run, by environment and outcome."),
	))

	if len(errs) == 0 {
		return dh, nil
	}
	return dh, errors.Join(errs...)
}

// recordNodeMetrics records one finished node's histograms. Attribute set is
// STRICTLY environment, node name, and outcome — see this file's cardinality
// note. Only measured usage values are recorded, so a peak-RSS distribution
// is never diluted by nodes that simply have no reading.
func (r *DeployRecorder) recordNodeMetrics(env string, res core.CheckResult) {
	if r.hist == nil {
		return
	}
	ctx := context.Background()
	attrs := metric.WithAttributes(
		attribute.String(AttrDeployEnv, env),
		attribute.String(AttrDeployNode, res.Name),
		attribute.String(AttrNodeOutcome, nodeOutcome(res)),
	)
	r.hist.nodeDuration.Record(ctx, res.Duration.Milliseconds(), attrs)
	if res.PeakRSS > 0 {
		r.hist.nodePeakRSS.Record(ctx, res.PeakRSS, attrs)
	}
	if res.UserCPU.Milliseconds() > 0 {
		r.hist.nodeUserCPU.Record(ctx, res.UserCPU.Milliseconds(), attrs)
	}
	if res.SysCPU.Milliseconds() > 0 {
		r.hist.nodeSysCPU.Record(ctx, res.SysCPU.Milliseconds(), attrs)
	}
}

// recordRunMetrics records one finished graph run's duration, attributed by
// environment and outcome only.
func (r *DeployRecorder) recordRunMetrics(rec *core.DeployRecord) {
	if r.hist == nil {
		return
	}
	r.hist.runDuration.Record(context.Background(),
		rec.EndedAt.Sub(rec.StartedAt).Milliseconds(),
		metric.WithAttributes(
			attribute.String(AttrDeployEnv, rec.Env),
			attribute.String(AttrOutcome, outcomeString(rec.Outcome)),
		))
}

// RegisterDeployGauge registers the one deploy gauge:
// gauntlet.deploy.lanes_in_flight, the daemon-wide count of environment
// lanes running a graph right now.
//
// Deliberately its own registration rather than a fourth callback inside
// RegisterGauges: that function's three gauges are the QUEUE's, wired
// unconditionally from cmd, while this one only exists when deployment is
// configured. Folding it in would force every deployless daemon to observe a
// permanent zero for a subsystem it doesn't run — a series that looks like
// data and is really just wiring.
func RegisterDeployGauge(inFlight func() int) (metric.Registration, error) {
	return registerDeployGauge(Meter(), inFlight)
}

// registerDeployGauge is RegisterDeployGauge's testable core, taking the
// meter explicitly the way registerGauges does.
func registerDeployGauge(m metric.Meter, inFlight func() int) (metric.Registration, error) {
	gauge, err := m.Int64ObservableGauge("gauntlet.deploy.lanes_in_flight",
		metric.WithUnit("{lane}"),
		metric.WithDescription("Environment lanes running a deploy graph right now, daemon-wide."),
	)
	if err != nil {
		return nil, err
	}
	return m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(gauge, int64(inFlight()))
		return nil
	}, gauge)
}
