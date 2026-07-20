// This file adds gauntlet's OTLP metrics signal beside trace.go's spans:
// node-completion histograms (peak RSS, user/sys CPU, duration) and three
// daemon gauges (queue depth, execution-slot occupancy, runs in flight).
// Same "no provider installed ⇒ every instrument is a no-op" contract as
// trace.go's Tracer() — see InstallMeterProvider.
//
// Cardinality (issue #14): the node-completion attribute set is STRICTLY
// target, node name, node kind, and outcome — small, config-bounded, or
// fixed-enum values. NEVER a run ID, a candidate SHA, or a ref: those are
// per-run identifiers with effectively unbounded cardinality, and label
// cardinality blowup is how metrics backends die. The identical per-run
// fact already lives on the check's OTel span (trace.go's EndCheck,
// AttrRunID/AttrCandidateSHA) and in the history row beside it — dropping
// it here loses nothing, it just keeps it off the metric series.
package obs

import (
	"context"
	"errors"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Attribute keys used on gauntlet metric data points. Distinct from
// trace.go's Attr* span keys (AttrCheckName et al.) even where the value is
// the same underlying fact (AttrTarget is reused as-is; a node's name is
// re-keyed under gauntlet.node.* since a node is broader than a "check" —
// it's also an image build or a receipt producer).
const (
	// AttrNodeName is the node's declared name from the check spec: a
	// plain check name, or "image:<name>"/"receipt:<name>" for those two
	// synthetic node kinds. Spec-defined, so bounded — an operator names a
	// handful of checks, never a value per candidate.
	AttrNodeName = "gauntlet.node.name"

	// AttrNodeKind classifies AttrNodeName as NodeKindCheck, NodeKindImage,
	// or NodeKindReceipt — three fixed values, so it adds no cardinality
	// of its own.
	AttrNodeKind = "gauntlet.node.kind"

	// AttrNodeOutcome is the node's terminal verdict: "passed", "failed",
	// "skipped", "blocked", "error", or "unknown" (nodeOutcome's mapping)
	// — a fixed, small set.
	AttrNodeOutcome = "gauntlet.node.outcome"

	// AttrQueueState distinguishes a gauntlet.queue.depth reading's three
	// components — "waiting", "in_flight", "parked" — a fixed three-value
	// set.
	AttrQueueState = "gauntlet.queue.state"
)

// Node kind values for AttrNodeKind, and RecordNode's kind parameter.
const (
	NodeKindCheck   = "check"
	NodeKindImage   = "image"
	NodeKindReceipt = "receipt"
)

// Meter returns gauntlet's shared meter. With no MeterProvider installed
// (the default), every instrument it produces is a no-op and carries no
// cost — mirrors trace.go's Tracer() exactly.
func Meter() metric.Meter { return otel.Meter("gauntlet") }

// InstallMeterProvider installs a real SDK meter provider (OTLP/HTTP
// exporter, periodic export) as the process-wide OTel global, so the
// instruments this package creates (RecordNode's histograms,
// RegisterGauges' gauges) start recording and exporting. Mirrors
// provider.go's InstallProvider exactly: same otlp config block, same
// endpoint/insecure knobs, same absent-endpoint-is-a-no-op contract, same
// "install before anything captures an instrument" ordering caution (see
// cmd/gauntlet/main.go's comment beside its InstallProvider call). Kept as
// a separate install call from the tracer's, rather than folded into one
// function, so the two signals' lifecycles stay independently readable
// even though cmd wires them back-to-back from the same config block.
//
// endpoint == "" installs nothing: it leaves whatever global MeterProvider
// is already registered (the default is none, so every instrument stays a
// no-op) and returns a nil-returning no-op shutdown.
//
// Otherwise it builds an otlpmetrichttp exporter (WithEndpoint(endpoint),
// and WithInsecure() when insecure is set — the metrics analogue of
// otlptracehttp, over the same HTTP transport), wraps it in a
// PeriodicReader (metrics have no natural "span ended" trigger the way
// spans do, so periodic push stands in for the tracer's
// BatchSpanProcessor), and registers a MeterProvider carrying the same
// "service.name"="gauntlet" resource as the tracer provider, as the OTel
// global via otel.SetMeterProvider.
//
// The returned shutdown flushes the reader and shuts the provider down;
// callers should defer it (with a bounded context) on exit — same contract
// as InstallProvider's shutdown.
func InstallMeterProvider(ctx context.Context, endpoint string, insecure bool) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if endpoint == "" {
		return noop, nil
	}

	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	exp, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return noop, err
	}

	res := resource.NewSchemaless(attribute.String("service.name", "gauntlet"))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	return mp.Shutdown, nil
}

// nodeHistograms holds the four node-completion instruments RecordNode
// writes to.
type nodeHistograms struct {
	duration metric.Int64Histogram
	peakRSS  metric.Int64Histogram
	userCPU  metric.Int64Histogram
	sysCPU   metric.Int64Histogram
}

// newNodeHistograms creates the four node-completion histograms against m.
// Split out from RecordNode/defaultNodeHistograms so tests can build their
// own set against a local, manual-reader-backed meter — no global otel
// state, no network — rather than only being able to exercise the
// package-level default bound to Meter().
//
// Instrument creation errors (malformed name/unit — a build-time bug, not
// a runtime condition, since every name/unit here is a static literal) are
// reported via otel.Handle rather than failing the caller: the OTel API
// contract is that Meter methods return a usable (no-op-safe) instrument
// even on error, so there's nothing for a caller to recover from — see
// each SDK method's own doc.
func newNodeHistograms(m metric.Meter) (*nodeHistograms, error) {
	var errs []error
	record := func(h metric.Int64Histogram, err error) metric.Int64Histogram {
		if err != nil {
			errs = append(errs, err)
		}
		return h
	}

	nh := &nodeHistograms{}
	nh.duration = record(m.Int64Histogram("gauntlet.node.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Wall-clock duration of one finished node (check, image build, or receipt producer)."),
	))
	nh.peakRSS = record(m.Int64Histogram("gauntlet.node.peak_rss",
		metric.WithUnit("By"),
		metric.WithDescription("Peak resident-set size of one finished node's process tree; only recorded when measured (core.CheckResult.PeakRSS's zero-means-unmeasured contract)."),
	))
	nh.userCPU = record(m.Int64Histogram("gauntlet.node.user_cpu",
		metric.WithUnit("ms"),
		metric.WithDescription("User CPU time of one finished node's process tree; only recorded when measured."),
	))
	nh.sysCPU = record(m.Int64Histogram("gauntlet.node.sys_cpu",
		metric.WithUnit("ms"),
		metric.WithDescription("System CPU time of one finished node's process tree; only recorded when measured."),
	))

	if len(errs) == 0 {
		return nh, nil
	}
	return nh, errors.Join(errs...)
}

// defaultNodeHistograms is the package-level instance RecordNode uses,
// built once (against whatever MeterProvider is live at first use — the
// no-op default unless InstallMeterProvider already ran, which then
// delegates automatically per the OTel API's own documented behavior for
// instruments obtained before a provider is installed).
var defaultNodeHistograms = sync.OnceValue(func() *nodeHistograms {
	nh, err := newNodeHistograms(Meter())
	if err != nil {
		otel.Handle(err)
	}
	return nh
})

// RecordNode records one finished node's (a check, an image build, or a
// receipt producer — see NodeKind*) resource-usage histograms: duration,
// peak RSS, user CPU, and system CPU. target and kind are supplied by the
// caller (queue's advanceChecks, via its own nodeKind helper) rather than
// derived here, keeping obs ignorant of the "image:"/"receipt:" node-name
// prefix convention that only queue owns.
//
// Attribute set is STRICTLY target, node name, node kind, and outcome —
// see the package doc's cardinality note. Only measured values are
// recorded: result.PeakRSS/UserCPU/SysCPU are zero-means-unmeasured
// (core.CheckResult's own contract — e.g. the container executor never
// measures them), and an unmeasured value is skipped entirely rather than
// recorded as a false zero, so a peak-RSS distribution computed from this
// histogram is never diluted by runs that simply have no reading.
// Duration has no such caveat and is always recorded.
//
// Called from the same site trace.go's EndCheck ends the node's span
// (internal/queue/reconcile.go's advanceChecks), against result BEFORE the
// image/receipt-specific post-validation that can flip a build/receipt
// node's Status — the same pre-validation result EndCheck's span status
// already reflects, so this stays consistent with that existing choice
// rather than introducing a second, differently-timed view of the same
// node.
//
// With no MeterProvider installed, every instrument here is a no-op and
// this call costs a handful of no-op interface calls — no allocation-heavy
// export path, no goroutine, no network.
func RecordNode(ctx context.Context, target, kind string, result core.CheckResult) {
	recordNode(ctx, defaultNodeHistograms(), target, kind, result)
}

// recordNode is RecordNode's testable core: it takes the histogram set
// explicitly so tests can pass one built against a local manual-reader
// meter instead of the global default.
func recordNode(ctx context.Context, nh *nodeHistograms, target, kind string, result core.CheckResult) {
	if nh == nil {
		return
	}
	attrs := metric.WithAttributes(nodeAttributes(target, kind, result)...)

	nh.duration.Record(ctx, result.Duration.Milliseconds(), attrs)
	if result.PeakRSS > 0 {
		nh.peakRSS.Record(ctx, result.PeakRSS, attrs)
	}
	if result.UserCPU > 0 {
		nh.userCPU.Record(ctx, result.UserCPU.Milliseconds(), attrs)
	}
	if result.SysCPU > 0 {
		nh.sysCPU.Record(ctx, result.SysCPU.Milliseconds(), attrs)
	}
}

// nodeAttributes builds one node-completion measurement's attribute set —
// see RecordNode's doc for the strict "these four, never more" contract.
func nodeAttributes(target, kind string, result core.CheckResult) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrTarget, target),
		attribute.String(AttrNodeName, result.Name),
		attribute.String(AttrNodeKind, kind),
		attribute.String(AttrNodeOutcome, nodeOutcome(result)),
	}
}

// nodeOutcome maps a CheckResult onto AttrNodeOutcome's small fixed set:
// "error" when the node itself errored (infra failure, distinct from a red
// verdict), otherwise trace.go's checkStatusString mapping (passed/failed/
// skipped/blocked/unknown) — reused so the span and the metric never
// disagree about what a given result's terminal shape was called.
func nodeOutcome(result core.CheckResult) string {
	if result.Err != nil {
		return "error"
	}
	return checkStatusString(result.Status)
}

// QueueDepth is one target's live queue occupancy, as needed by the
// gauntlet.queue.depth gauge. obs deliberately never imports package
// queue — queue already imports obs (EndCheck/StartCheck/...), so the
// reverse import would cycle — so this is the minimal projection cmd
// wiring supplies from queue.Daemon.Snapshot() at each collection; see
// RegisterGauges.
type QueueDepth struct {
	Target                    string
	Waiting, InFlight, Parked int
}

// RegisterGauges registers the daemon's three observable (async) gauges —
// queue depth per target, execution-slot occupancy, and runs in flight —
// against Meter(). Call once, at daemon start, from cmd wiring (mirrors
// InstallMeterProvider's "install/register once" lifecycle) — there is no
// polling goroutine here of gauntlet's own; each callback below is invoked
// fresh by the SDK (or never, under the no-op meter) at its own collection
// cadence.
//
// depth is called once per collection and must return every target's
// current (waiting, in-flight, parked) counts; cmd wiring supplies this
// straight from queue.Daemon.Snapshot().Targets — the very same source
// the history depth sampler already reads
// (cmd/gauntlet/dashboard.go's startDepthSampler), so this adds no new
// sampling path of its own.
//
// slotsInUse reports the daemon-wide execution cap's current occupancy;
// ok is false when no cap is configured (a nil core.Slots, i.e. unlimited
// executions), in which case the gauge simply observes nothing that
// collection — an unconfigured cap has no "in use" reading to give.
//
// runsInFlight is the daemon-wide count of runs currently occupying a
// lane — queue.Snapshot's own ActiveRuns field (already computed for the
// drain-lifecycle status surface), reused rather than resampled.
func RegisterGauges(
	depth func() []QueueDepth,
	slotsInUse func() (n int, ok bool),
	runsInFlight func() int,
) (metric.Registration, error) {
	return registerGauges(Meter(), depth, slotsInUse, runsInFlight)
}

// registerGauges is RegisterGauges' testable core: it takes the meter
// explicitly so tests can pass one built against a local manual-reader
// meter instead of the global default.
func registerGauges(
	m metric.Meter,
	depth func() []QueueDepth,
	slotsInUse func() (n int, ok bool),
	runsInFlight func() int,
) (metric.Registration, error) {
	var errs []error
	depthGauge, err := m.Int64ObservableGauge("gauntlet.queue.depth",
		metric.WithUnit("{candidate}"),
		metric.WithDescription("Queue occupancy per target: waiting, in-flight, or parked candidates (gauntlet.queue.state)."),
	)
	if err != nil {
		errs = append(errs, err)
	}
	slotsGauge, err := m.Int64ObservableGauge("gauntlet.slots.in_use",
		metric.WithUnit("{slot}"),
		metric.WithDescription("Execution slots currently held against the daemon-wide max-executions cap; unobserved when no cap is configured."),
	)
	if err != nil {
		errs = append(errs, err)
	}
	runsGauge, err := m.Int64ObservableGauge("gauntlet.runs.in_flight",
		metric.WithUnit("{run}"),
		metric.WithDescription("Runs currently occupying a target's pipeline, daemon-wide."),
	)
	if err != nil {
		errs = append(errs, err)
	}

	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, d := range depth() {
			o.ObserveInt64(depthGauge, int64(d.Waiting), metric.WithAttributes(
				attribute.String(AttrTarget, d.Target), attribute.String(AttrQueueState, "waiting")))
			o.ObserveInt64(depthGauge, int64(d.InFlight), metric.WithAttributes(
				attribute.String(AttrTarget, d.Target), attribute.String(AttrQueueState, "in_flight")))
			o.ObserveInt64(depthGauge, int64(d.Parked), metric.WithAttributes(
				attribute.String(AttrTarget, d.Target), attribute.String(AttrQueueState, "parked")))
		}
		if n, ok := slotsInUse(); ok {
			o.ObserveInt64(slotsGauge, int64(n))
		}
		o.ObserveInt64(runsGauge, int64(runsInFlight()))
		return nil
	}, depthGauge, slotsGauge, runsGauge)
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return reg, nil
	}
	return reg, errors.Join(errs...)
}
