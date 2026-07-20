package obs

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMeter builds an isolated SDK MeterProvider backed by a
// ManualReader — no global otel state, no network — and returns its Meter
// plus the reader tests collect from. Every histogram/gauge test below uses
// one of these rather than the package-level Meter()/defaultNodeHistograms,
// so tests never depend on (or contend over) the process-wide global
// MeterProvider withRestoredMeterProvider below has to manage separately.
func newTestMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return rdr, mp.Meter("gauntlet-test")
}

// collect runs a synchronous Collect against rdr and fails the test on
// error, returning the resulting ResourceMetrics.
func collect(t *testing.T, rdr *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

// findMetric returns the named metric from rm's first scope carrying it,
// and whether it was found at all — a metric with zero recorded data
// points is sometimes omitted entirely by the SDK rather than reported
// empty, so callers that want "no measurement" should treat !ok the same
// as ok-with-zero-datapoints.
func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func attrString(t *testing.T, attrs attribute.Set, key string) string {
	t.Helper()
	v, ok := attrs.Value(attribute.Key(key))
	if !ok {
		t.Errorf("attribute %q not present in %s", key, attrs.Encoded(attribute.DefaultEncoder()))
		return ""
	}
	return v.AsString()
}

func TestRecordNodeHistogramExactAttributes(t *testing.T) {
	rdr, meter := newTestMeter(t)
	nh, err := newNodeHistograms(meter)
	if err != nil {
		t.Fatalf("newNodeHistograms: %v", err)
	}

	result := core.CheckResult{
		Name:     "test",
		Status:   core.CheckPassed,
		Duration: 2500 * time.Millisecond,
		PeakRSS:  123456,
		UserCPU:  500 * time.Millisecond,
		SysCPU:   25 * time.Millisecond,
	}
	recordNode(context.Background(), nh, "main", NodeKindCheck, result)

	rm := collect(t, rdr)

	cases := []struct {
		metric string
		want   int64
	}{
		{"gauntlet.node.duration", 2500},
		{"gauntlet.node.peak_rss", 123456},
		{"gauntlet.node.user_cpu", 500},
		{"gauntlet.node.sys_cpu", 25},
	}
	for _, c := range cases {
		m, ok := findMetric(rm, c.metric)
		if !ok {
			t.Errorf("%s: metric not recorded", c.metric)
			continue
		}
		hist, ok := m.Data.(metricdata.Histogram[int64])
		if !ok {
			t.Errorf("%s: Data is %T, want Histogram[int64]", c.metric, m.Data)
			continue
		}
		if len(hist.DataPoints) != 1 {
			t.Fatalf("%s: got %d data points, want 1", c.metric, len(hist.DataPoints))
		}
		dp := hist.DataPoints[0]
		if dp.Count != 1 || dp.Sum != c.want {
			t.Errorf("%s: got count=%d sum=%d, want count=1 sum=%d", c.metric, dp.Count, dp.Sum, c.want)
		}
		if got := attrString(t, dp.Attributes, AttrTarget); got != "main" {
			t.Errorf("%s: %s = %q, want %q", c.metric, AttrTarget, got, "main")
		}
		if got := attrString(t, dp.Attributes, AttrNodeName); got != "test" {
			t.Errorf("%s: %s = %q, want %q", c.metric, AttrNodeName, got, "test")
		}
		if got := attrString(t, dp.Attributes, AttrNodeKind); got != NodeKindCheck {
			t.Errorf("%s: %s = %q, want %q", c.metric, AttrNodeKind, got, NodeKindCheck)
		}
		if got := attrString(t, dp.Attributes, AttrNodeOutcome); got != "passed" {
			t.Errorf("%s: %s = %q, want %q", c.metric, AttrNodeOutcome, got, "passed")
		}
		if dp.Attributes.Len() != 4 {
			t.Errorf("%s: got %d attributes, want exactly 4 (target, node name, node kind, outcome): %s",
				c.metric, dp.Attributes.Len(), dp.Attributes.Encoded(attribute.DefaultEncoder()))
		}
	}
}

func TestRecordNodeSkipsUnmeasuredValues(t *testing.T) {
	rdr, meter := newTestMeter(t)
	nh, err := newNodeHistograms(meter)
	if err != nil {
		t.Fatalf("newNodeHistograms: %v", err)
	}

	// PeakRSS/UserCPU/SysCPU all zero ("not measured" — core.CheckResult's
	// own contract, e.g. a container-executor result); Duration is never
	// zero-means-unmeasured and must still be recorded.
	result := core.CheckResult{
		Name:     "image:ci",
		Status:   core.CheckFailed,
		Err:      errors.New("boom"),
		Duration: time.Second,
	}
	recordNode(context.Background(), nh, "main", NodeKindImage, result)

	rm := collect(t, rdr)

	if m, ok := findMetric(rm, "gauntlet.node.duration"); !ok {
		t.Error("gauntlet.node.duration: not recorded, want the always-recorded duration point")
	} else if hist, ok := m.Data.(metricdata.Histogram[int64]); !ok || len(hist.DataPoints) != 1 {
		t.Errorf("gauntlet.node.duration: got %#v, want exactly 1 histogram data point", m.Data)
	} else if got := attrString(t, hist.DataPoints[0].Attributes, AttrNodeOutcome); got != "error" {
		t.Errorf("gauntlet.node.duration: %s = %q, want %q (result.Err != nil)", AttrNodeOutcome, got, "error")
	}

	for _, name := range []string{"gauntlet.node.peak_rss", "gauntlet.node.user_cpu", "gauntlet.node.sys_cpu"} {
		m, ok := findMetric(rm, name)
		if !ok {
			continue // absent entirely is the expected "never recorded" shape
		}
		if hist, ok := m.Data.(metricdata.Histogram[int64]); ok && len(hist.DataPoints) != 0 {
			t.Errorf("%s: got %d data points for an unmeasured (zero) value, want 0", name, len(hist.DataPoints))
		}
	}
}

func TestNodeOutcomeMapping(t *testing.T) {
	cases := []struct {
		name   string
		result core.CheckResult
		want   string
	}{
		{"passed", core.CheckResult{Status: core.CheckPassed}, "passed"},
		{"failed", core.CheckResult{Status: core.CheckFailed}, "failed"},
		{"skipped", core.CheckResult{Status: core.CheckSkipped}, "skipped"},
		{"blocked", core.CheckResult{Status: core.CheckBlocked}, "blocked"},
		{"err overrides status", core.CheckResult{Status: core.CheckPassed, Err: errors.New("infra")}, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nodeOutcome(c.result); got != c.want {
				t.Errorf("nodeOutcome(%+v) = %q, want %q", c.result, got, c.want)
			}
		})
	}
}

func TestRecordNodeNilHistogramsDoesNotPanic(t *testing.T) {
	// defaultNodeHistograms() can, in principle, hand back nil if instrument
	// creation itself failed; recordNode must tolerate that rather than
	// panic on a nil receiver.
	recordNode(context.Background(), nil, "main", NodeKindCheck, core.CheckResult{Name: "test"})
}

func TestGaugesObserveWiredSources(t *testing.T) {
	rdr, meter := newTestMeter(t)

	depth := []QueueDepth{{Target: "main", Waiting: 2, InFlight: 1, Parked: 3}}
	reg, err := registerGauges(meter,
		func() []QueueDepth { return depth },
		func() (int, bool) { return 4, true },
		func() int { return 5 },
	)
	if err != nil {
		t.Fatalf("registerGauges: %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	rm := collect(t, rdr)

	depthMetric, ok := findMetric(rm, "gauntlet.queue.depth")
	if !ok {
		t.Fatal("gauntlet.queue.depth: not observed")
	}
	gauge, ok := depthMetric.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("gauntlet.queue.depth: Data is %T, want Gauge[int64]", depthMetric.Data)
	}
	if len(gauge.DataPoints) != 3 {
		t.Fatalf("gauntlet.queue.depth: got %d data points, want 3 (waiting/in_flight/parked)", len(gauge.DataPoints))
	}
	got := map[string]int64{}
	for _, dp := range gauge.DataPoints {
		if target := attrString(t, dp.Attributes, AttrTarget); target != "main" {
			t.Errorf("gauntlet.queue.depth: %s = %q, want %q", AttrTarget, target, "main")
		}
		got[attrString(t, dp.Attributes, AttrQueueState)] = dp.Value
	}
	want := map[string]int64{"waiting": 2, "in_flight": 1, "parked": 3}
	for state, wantVal := range want {
		if got[state] != wantVal {
			t.Errorf("gauntlet.queue.depth[%s] = %d, want %d", state, got[state], wantVal)
		}
	}

	slotsMetric, ok := findMetric(rm, "gauntlet.slots.in_use")
	if !ok {
		t.Fatal("gauntlet.slots.in_use: not observed")
	}
	slotsGauge := slotsMetric.Data.(metricdata.Gauge[int64])
	if len(slotsGauge.DataPoints) != 1 || slotsGauge.DataPoints[0].Value != 4 {
		t.Errorf("gauntlet.slots.in_use: got %+v, want one data point with value 4", slotsGauge.DataPoints)
	}

	runsMetric, ok := findMetric(rm, "gauntlet.runs.in_flight")
	if !ok {
		t.Fatal("gauntlet.runs.in_flight: not observed")
	}
	runsGauge := runsMetric.Data.(metricdata.Gauge[int64])
	if len(runsGauge.DataPoints) != 1 || runsGauge.DataPoints[0].Value != 5 {
		t.Errorf("gauntlet.runs.in_flight: got %+v, want one data point with value 5", runsGauge.DataPoints)
	}
}

func TestGaugesSkipUnconfiguredSlots(t *testing.T) {
	rdr, meter := newTestMeter(t)

	reg, err := registerGauges(meter,
		func() []QueueDepth { return nil },
		func() (int, bool) { return 0, false }, // no cap configured
		func() int { return 0 },
	)
	if err != nil {
		t.Fatalf("registerGauges: %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	rm := collect(t, rdr)

	if m, ok := findMetric(rm, "gauntlet.slots.in_use"); ok {
		if gauge, ok := m.Data.(metricdata.Gauge[int64]); ok && len(gauge.DataPoints) != 0 {
			t.Errorf("gauntlet.slots.in_use: got %d data points with no cap configured, want 0 (unobserved)", len(gauge.DataPoints))
		}
	}
}

// withRestoredMeterProvider saves the process-wide global MeterProvider and
// restores it via t.Cleanup — mirrors provider_test.go's
// withRestoredProvider (same rationale: InstallMeterProvider mutates
// unscoped global state via otel.SetMeterProvider).
func withRestoredMeterProvider(t *testing.T) {
	t.Helper()
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
}

func deadLocalAddrForMetrics(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func TestInstallMeterProviderEmptyEndpointIsNoop(t *testing.T) {
	withRestoredMeterProvider(t)

	shutdown, err := InstallMeterProvider(context.Background(), "", false)
	if err != nil {
		t.Fatalf(`InstallMeterProvider("") error: %v`, err)
	}
	if shutdown == nil {
		t.Fatal(`InstallMeterProvider("") returned nil shutdown`)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
}

func TestInstallMeterProviderConfiguredEndpointInstalls(t *testing.T) {
	withRestoredMeterProvider(t)

	endpoint := deadLocalAddrForMetrics(t) // nothing listens here

	shutdown, err := InstallMeterProvider(context.Background(), endpoint, true)
	if err != nil {
		t.Fatalf("InstallMeterProvider(%q) error: %v", endpoint, err)
	}
	if shutdown == nil {
		t.Fatal("InstallMeterProvider returned nil shutdown")
	}

	// otlpmetrichttp.New does no I/O at construction (mirrors
	// otlptracehttp, per provider_test.go's own comment); the exporter is
	// only ever touched by a periodic/manual Collect+export cycle, which
	// this test never triggers, so there is nothing further to assert here
	// beyond "installing against a dead endpoint doesn't itself error."
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Logf("shutdown returned (err=%v)", err) // best-effort export flush against a dead endpoint; not asserted
	}
}

// Under the disabled-otlp default (no MeterProvider ever installed in this
// test — see withRestoredMeterProvider's absence here, deliberate), every
// instrument RecordNode/RegisterGauges touch must be a costless no-op:
// nothing panics, nothing blocks, nothing requires a provider.
func TestDisabledOTLPMetricsAreNoop(t *testing.T) {
	RecordNode(context.Background(), "main", NodeKindCheck, core.CheckResult{
		Name: "test", Status: core.CheckPassed, Duration: time.Second, PeakRSS: 1024,
	})

	reg, err := RegisterGauges(
		func() []QueueDepth { return []QueueDepth{{Target: "main", Waiting: 1}} },
		func() (int, bool) { return 1, true },
		func() int { return 1 },
	)
	if err != nil {
		t.Fatalf("RegisterGauges under no-op meter: %v", err)
	}
	if reg != nil {
		_ = reg.Unregister()
	}
}
