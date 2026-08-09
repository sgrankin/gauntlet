package obs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newRecordingRecorder builds a DeployRecorder whose spans land in a
// local SpanRecorder and whose instruments write to a local ManualReader —
// no global OTel state, no network, so these tests can assert on real
// emitted spans and data points rather than only on attribute builders.
func newRecordingRecorder(t *testing.T) (*DeployRecorder, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	rdr, meter := newTestMeter(t)
	hist, err := newDeployHistograms(meter)
	if err != nil {
		t.Fatalf("newDeployHistograms: %v", err)
	}
	r := &DeployRecorder{tracer: tp.Tracer("gauntlet-test"), hist: hist, runs: map[string]deployRun{}}
	return r, sr, rdr
}

// assertExactHistogramAttrs checks that mt has exactly one data point and
// that its attribute set is EXACTLY want — no extras. The exactness is the
// point: this is the cardinality guard, and an attribute nobody asserted the
// absence of is exactly how a run ID or a SHA would sneak onto a series.
func assertExactHistogramAttrs(t *testing.T, name string, mt metricdata.Metrics, want map[string]string) {
	t.Helper()
	hist, ok := mt.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Errorf("%s: Data is %T, want Histogram[int64]", name, mt.Data)
		return
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("%s: got %d data points, want 1", name, len(hist.DataPoints))
	}
	dp := hist.DataPoints[0]
	for key, wantVal := range want {
		if got := attrString(t, dp.Attributes, key); got != wantVal {
			t.Errorf("%s: %s = %q, want %q", name, key, got, wantVal)
		}
	}
	if dp.Attributes.Len() != len(want) {
		t.Errorf("%s: got %d attributes, want exactly %d (%v): %s",
			name, dp.Attributes.Len(), len(want), want, dp.Attributes.Encoded(attribute.DefaultEncoder()))
	}
	for _, forbidden := range []string{AttrRunID, AttrDeploySHA, AttrDeployedSHA, AttrTarget} {
		for _, kv := range dp.Attributes.ToSlice() {
			if string(kv.Key) == forbidden {
				t.Errorf("%s: metric carries %s = %v; per-deploy identifiers belong on the span, never on a metric series", name, forbidden, kv.Value)
			}
		}
	}
}

// gaugeValue returns the single data point of an int64 gauge.
func gaugeValue(t *testing.T, mt metricdata.Metrics) int64 {
	t.Helper()
	g, ok := mt.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s: Data is %T, want Gauge[int64]", mt.Name, mt.Data)
	}
	if len(g.DataPoints) != 1 {
		t.Fatalf("%s: got %d data points, want 1", mt.Name, len(g.DataPoints))
	}
	return g.DataPoints[0].Value
}

// spanNamed returns the single recorded span with the given name.
func spanNamed(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("recorded %d spans named %q, want exactly 1", len(found), name)
	}
	return found[0]
}

func spanAttr(t *testing.T, s sdktrace.ReadOnlySpan, key string) attribute.Value {
	t.Helper()
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	t.Fatalf("span %q has no attribute %q (has %v)", s.Name(), key, s.Attributes())
	return attribute.Value{}
}

// TestDeployRecorder_SpanTree is the linkage this type exists for: the
// node span is a CHILD of the run's root span, even though the two arrive
// as independent events with nothing but a run ID in common. It also pins
// the reconstructed interval — a node's span covers [At-Duration, At], not
// the instant the event happened to be observed.
func TestDeployRecorder_SpanTree(t *testing.T) {
	r, sr, _ := newRecordingRecorder(t)
	ctx := context.Background()
	start := time.Now().Add(-5 * time.Minute)

	r.Observe(ctx, core.Event{
		Kind: core.EventDeployStarted, At: start, RunID: "deploy-1",
		DeployEnv: "prod", DeploySHA: "sha-new", DeployedSHA: "sha-old",
	})
	nodeEnd := start.Add(30 * time.Second)
	r.Observe(ctx, core.Event{
		Kind: core.EventDeployNodeFinished, At: nodeEnd, RunID: "deploy-1",
		DeployEnv: "prod", DeploySHA: "sha-new", CheckName: "migrate",
		Check: &core.CheckResult{Name: "migrate", Status: core.CheckPassed, Duration: 22 * time.Second},
	})
	end := start.Add(80 * time.Second)
	r.Observe(ctx, core.Event{
		Kind: core.EventDeployFinished, At: end, RunID: "deploy-1",
		DeployEnv: "prod", DeploySHA: "sha-new", DeployedSHA: "sha-old",
		Deploy: &core.DeployRecord{
			RunID: "deploy-1", Env: "prod", DeploySHA: "sha-new", DeployedSHA: "sha-old",
			Nodes: []core.CheckResult{
				{Name: "migrate", Status: core.CheckPassed, Duration: 22 * time.Second},
				{Name: "app1", Status: core.CheckSkipped},
			},
			Outcome: core.OutcomeLanded, StartedAt: start, EndedAt: end,
		},
	})

	root := spanNamed(t, sr, "deploy")
	node := spanNamed(t, sr, "deploy-node")

	if node.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("deploy-node parent = %s, want the deploy root %s", node.Parent().SpanID(), root.SpanContext().SpanID())
	}
	if node.Parent().TraceID() != root.SpanContext().TraceID() {
		t.Error("deploy-node is in a different trace from its run's root span")
	}
	if !node.StartTime().Equal(nodeEnd.Add(-22 * time.Second)) {
		t.Errorf("node span start = %s, want the reconstructed %s", node.StartTime(), nodeEnd.Add(-22*time.Second))
	}
	if !node.EndTime().Equal(nodeEnd) {
		t.Errorf("node span end = %s, want %s", node.EndTime(), nodeEnd)
	}
	if !root.StartTime().Equal(start) || !root.EndTime().Equal(end) {
		t.Errorf("root span interval = [%s, %s], want [%s, %s]", root.StartTime(), root.EndTime(), start, end)
	}

	// Root summary attributes: the per-run drill-down that deliberately
	// does NOT exist on the metrics.
	if got := spanAttr(t, root, AttrRunID).AsString(); got != "deploy-1" {
		t.Errorf("root run id = %q, want deploy-1", got)
	}
	if got := spanAttr(t, root, AttrDeploySHA).AsString(); got != "sha-new" {
		t.Errorf("root deploy sha = %q, want sha-new", got)
	}
	if got := spanAttr(t, root, AttrDeployedSHA).AsString(); got != "sha-old" {
		t.Errorf("root deployed sha = %q, want sha-old", got)
	}
	if got := spanAttr(t, root, AttrOutcome).AsString(); got != "landed" {
		t.Errorf("root outcome = %q, want landed", got)
	}
	if got := spanAttr(t, root, AttrDeployNodesTotal).AsInt64(); got != 2 {
		t.Errorf("root nodes total = %d, want 2 (blocked/skipped rows counted)", got)
	}
	if got := spanAttr(t, node, AttrDeployNode).AsString(); got != "migrate" {
		t.Errorf("node name attr = %q, want migrate", got)
	}

	// The map is empty again: a long-running daemon must not hold a span
	// per deploy it has already finished.
	if n := r.InFlight(); n != 0 {
		t.Errorf("InFlight() after terminal = %d, want 0", n)
	}
}

// TestDeployRecorder_BareTerminalSynthesizesRoot: a spec rejection parks the
// lane before any node runs, so no started event ever arrives. The terminal
// must still produce a (childless) root span from the record's own interval
// — a park that traces as nothing is a park nobody can find.
func TestDeployRecorder_BareTerminalSynthesizesRoot(t *testing.T) {
	r, sr, _ := newRecordingRecorder(t)
	start := time.Now().Add(-time.Minute)
	end := start.Add(2 * time.Second)

	r.Observe(context.Background(), core.Event{
		Kind: core.EventDeployFinished, At: end, RunID: "deploy-reject",
		DeployEnv: "prod2", DeploySHA: "sha-x",
		Deploy: &core.DeployRecord{
			RunID: "deploy-reject", Env: "prod2", DeploySHA: "sha-x",
			Outcome: core.OutcomeRejected, Detail: "revision declares no deploy nodes",
			StartedAt: start, EndedAt: end,
		},
	})

	root := spanNamed(t, sr, "deploy")
	if len(sr.Ended()) != 1 {
		t.Errorf("recorded %d spans, want just the synthesized childless root", len(sr.Ended()))
	}
	if !root.StartTime().Equal(start) || !root.EndTime().Equal(end) {
		t.Errorf("synthesized root interval = [%s, %s], want the record's [%s, %s]", root.StartTime(), root.EndTime(), start, end)
	}
	if got := spanAttr(t, root, AttrDetail).AsString(); got != "revision declares no deploy nodes" {
		t.Errorf("synthesized root detail = %q", got)
	}
	if got := spanAttr(t, root, AttrDeployNodesTotal).AsInt64(); got != 0 {
		t.Errorf("synthesized root nodes total = %d, want 0", got)
	}
	if r.InFlight() != 0 {
		t.Error("a bare terminal left an entry in the in-flight map")
	}
}

// TestDeployRecorder_NodeSpanStatusAndUsage: a failing node's span carries
// an error status; usage attributes appear only when measured (the
// zero-means-unmeasured contract every gauntlet surface shares).
func TestDeployRecorder_NodeSpanStatusAndUsage(t *testing.T) {
	kvs := deployNodeAttributes("prod", core.CheckResult{
		Name: "migrate", Status: core.CheckFailed, Duration: 24 * time.Second,
	})
	for _, key := range []string{AttrCheckPeakRSS, AttrCheckUserCPU, AttrCheckSysCPU} {
		for _, kv := range kvs {
			if string(kv.Key) == key {
				t.Errorf("unmeasured %s is present (%v); an unmeasured field must be absent, never a recorded zero", key, kv.Value)
			}
		}
	}

	measured := deployNodeAttributes("prod", core.CheckResult{
		Name: "migrate", Status: core.CheckPassed, Duration: time.Second,
		PeakRSS: 4096, UserCPU: 700 * time.Millisecond, SysCPU: 300 * time.Millisecond,
	})
	if got := attr(t, measured, AttrCheckPeakRSS).Value.AsInt64(); got != 4096 {
		t.Errorf("peak rss = %d, want 4096", got)
	}
	if got := attr(t, measured, AttrCheckUserCPU).Value.AsInt64(); got != 700 {
		t.Errorf("user cpu = %d ms, want 700", got)
	}

	r, sr, _ := newRecordingRecorder(t)
	now := time.Now()
	r.Observe(context.Background(), core.Event{
		Kind: core.EventDeployStarted, At: now.Add(-time.Minute), RunID: "d", DeployEnv: "prod", DeploySHA: "s",
	})
	r.Observe(context.Background(), core.Event{
		Kind: core.EventDeployNodeFinished, At: now, RunID: "d", DeployEnv: "prod", DeploySHA: "s",
		CheckName: "migrate",
		Check:     &core.CheckResult{Name: "migrate", Status: core.CheckFailed, Duration: time.Second},
	})
	node := spanNamed(t, sr, "deploy-node")
	if node.Status().Code.String() != "Error" {
		t.Errorf("failed node span status = %s, want Error", node.Status().Code)
	}
}

// TestDeployRecorder_MetricAttributesAreExhaustive is the cardinality guard,
// stated as an EXACT attribute set rather than a spot check: a run ID or a
// SHA on a metric series is how a metrics backend dies, and both facts are
// already on the spans above. The set is checked exhaustively so an added
// attribute fails here rather than in production.
func TestDeployRecorder_MetricAttributesAreExhaustive(t *testing.T) {
	r, _, m := newRecordingRecorder(t)
	ctx := context.Background()
	start := time.Now().Add(-2 * time.Minute)

	r.Observe(ctx, core.Event{
		Kind: core.EventDeployStarted, At: start, RunID: "deploy-1", DeployEnv: "prod", DeploySHA: "sha-new",
	})
	r.Observe(ctx, core.Event{
		Kind: core.EventDeployNodeFinished, At: start.Add(time.Minute), RunID: "deploy-1",
		DeployEnv: "prod", DeploySHA: "sha-new", CheckName: "migrate",
		Check: &core.CheckResult{
			Name: "migrate", Status: core.CheckFailed, Duration: 24 * time.Second,
			PeakRSS: 4096, UserCPU: 700 * time.Millisecond, SysCPU: 300 * time.Millisecond,
		},
	})
	r.Observe(ctx, core.Event{
		Kind: core.EventDeployFinished, At: start.Add(2 * time.Minute), RunID: "deploy-1",
		DeployEnv: "prod", DeploySHA: "sha-new",
		Deploy: &core.DeployRecord{
			RunID: "deploy-1", Env: "prod", DeploySHA: "sha-new",
			Outcome: core.OutcomeRejected, Culprit: "migrate",
			StartedAt: start, EndedAt: start.Add(2 * time.Minute),
		},
	})

	rm := collect(t, m)

	nodeWant := map[string]string{
		AttrDeployEnv:   "prod",
		AttrDeployNode:  "migrate",
		AttrNodeOutcome: "failed",
	}
	for _, name := range []string{
		"gauntlet.deploy.node.duration",
		"gauntlet.deploy.node.peak_rss",
		"gauntlet.deploy.node.user_cpu",
		"gauntlet.deploy.node.sys_cpu",
	} {
		mt, ok := findMetric(rm, name)
		if !ok {
			t.Errorf("no %s metric recorded", name)
			continue
		}
		assertExactHistogramAttrs(t, name, mt, nodeWant)
	}

	runMetric, ok := findMetric(rm, "gauntlet.deploy.duration")
	if !ok {
		t.Fatal("no gauntlet.deploy.duration metric recorded")
	}
	assertExactHistogramAttrs(t, "gauntlet.deploy.duration", runMetric, map[string]string{
		AttrDeployEnv: "prod",
		AttrOutcome:   "rejected",
	})
}

// TestDeployRecorder_UnmeasuredUsageRecordsNothing: an unmeasured node
// records only its duration, so a peak-RSS distribution is never diluted by
// nodes that simply have no reading.
func TestDeployRecorder_UnmeasuredUsageRecordsNothing(t *testing.T) {
	r, _, m := newRecordingRecorder(t)
	now := time.Now()
	r.Observe(context.Background(), core.Event{
		Kind: core.EventDeployNodeFinished, At: now, RunID: "d", DeployEnv: "dev", DeploySHA: "s",
		CheckName: "app1",
		Check:     &core.CheckResult{Name: "app1", Status: core.CheckSkipped, Duration: time.Second},
	})
	rm := collect(t, m)
	if _, ok := findMetric(rm, "gauntlet.deploy.node.duration"); !ok {
		t.Error("duration is always recorded, measured or not")
	}
	for _, name := range []string{
		"gauntlet.deploy.node.peak_rss",
		"gauntlet.deploy.node.user_cpu",
		"gauntlet.deploy.node.sys_cpu",
	} {
		if _, ok := findMetric(rm, name); ok {
			t.Errorf("%s recorded a data point for an unmeasured node", name)
		}
	}
}

// TestRegisterDeployGauge observes the daemon-wide in-flight lane count,
// separately from RegisterGauges' three queue gauges (a deployless daemon
// must not report a permanent zero for a subsystem it never runs).
func TestRegisterDeployGauge(t *testing.T) {
	rdr, meter := newTestMeter(t)
	inFlight := 3
	reg, err := registerDeployGauge(meter, func() int { return inFlight })
	if err != nil {
		t.Fatalf("registerDeployGauge: %v", err)
	}
	defer reg.Unregister()

	rm := collect(t, rdr)
	g, ok := findMetric(rm, "gauntlet.deploy.lanes_in_flight")
	if !ok {
		t.Fatal("no gauntlet.deploy.lanes_in_flight metric")
	}
	if got := gaugeValue(t, g); got != 3 {
		t.Errorf("lanes_in_flight = %d, want 3", got)
	}
}

// TestDeployRecorder_NilSafeAndNonDeployEventsIgnored: cmd threads one
// unconditionally, and hands it the whole event stream.
func TestDeployRecorder_NilSafeAndNonDeployEventsIgnored(t *testing.T) {
	var nilRec *DeployRecorder
	nilRec.Observe(context.Background(), core.Event{Kind: core.EventDeployStarted})
	if nilRec.InFlight() != 0 {
		t.Error("nil recorder reported in-flight runs")
	}

	r, sr, _ := newRecordingRecorder(t)
	r.Observe(context.Background(), core.Event{Kind: core.EventLanded, Target: "main", Record: &core.RunRecord{}})
	r.Observe(context.Background(), core.Event{Kind: core.EventCheckFinished, Check: &core.CheckResult{Name: "test"}})
	r.Observe(context.Background(), core.Event{Kind: core.EventDeployFinished, RunID: "x"}) // no record
	if n := len(sr.Ended()); n != 0 {
		t.Errorf("non-deploy events produced %d spans, want 0", n)
	}
	if r.InFlight() != 0 {
		t.Error("non-deploy events left in-flight state behind")
	}
}

// TestDeployRecorder_ErroredNodeOutcome: a daemon-side Err is "error" on the
// metric, distinct from a red verdict — the same mapping the queue's node
// histograms use, reused rather than restated.
func TestDeployRecorder_ErroredNodeOutcome(t *testing.T) {
	r, _, m := newRecordingRecorder(t)
	r.Observe(context.Background(), core.Event{
		Kind: core.EventDeployNodeFinished, At: time.Now(), RunID: "d", DeployEnv: "dev", DeploySHA: "s",
		CheckName: "app1",
		Check:     &core.CheckResult{Name: "app1", Status: core.CheckPassed, Err: errors.New("executor unreachable")},
	})
	rm := collect(t, m)
	mt, ok := findMetric(rm, "gauntlet.deploy.node.duration")
	if !ok {
		t.Fatal("no duration metric recorded")
	}
	assertExactHistogramAttrs(t, "gauntlet.deploy.node.duration", mt, map[string]string{
		AttrDeployEnv:   "dev",
		AttrDeployNode:  "app1",
		AttrNodeOutcome: "error",
	})
}
