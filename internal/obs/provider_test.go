package obs

import (
	"context"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// withRestoredProvider saves the process-wide global TracerProvider and
// restores it via t.Cleanup. InstallProvider mutates unscoped global state
// (otel.SetTracerProvider); without this, a test that installs a real SDK
// provider would leak into whichever test runs next in this binary —
// notably trace_test.go's TestNoopTracerProducesNonRecordingSpans, which
// asserts on the *default* (no-op) global. go test runs this package's
// files in filename order (provider_test.go before trace_test.go), so the
// leak would in practice bite the very next test if it weren't restored.
func withRestoredProvider(t *testing.T) {
	t.Helper()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
}

// deadLocalAddr returns a "host:port" nothing is listening on: it binds an
// ephemeral port and immediately closes it. Used as the OTLP endpoint so the
// exporter's (lazy) dial has somewhere local and inert to fail against,
// without a live collector or any network beyond that failing dial.
func deadLocalAddr(t *testing.T) string {
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

func TestInstallProviderEmptyEndpointIsNoop(t *testing.T) {
	withRestoredProvider(t)

	shutdown, err := InstallProvider(context.Background(), "", false)
	if err != nil {
		t.Fatalf(`InstallProvider("") error: %v`, err)
	}
	if shutdown == nil {
		t.Fatal(`InstallProvider("") returned nil shutdown`)
	}

	_, span := Tracer().Start(context.Background(), "span")
	if span.IsRecording() {
		t.Error("span IsRecording() = true with empty endpoint, want false (no-op preserved)")
	}
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
}

func TestInstallProviderConfiguredEndpointInstalls(t *testing.T) {
	withRestoredProvider(t)

	endpoint := deadLocalAddr(t) // nothing listens here

	shutdown, err := InstallProvider(context.Background(), endpoint, true)
	if err != nil {
		t.Fatalf("InstallProvider(%q) error: %v", endpoint, err)
	}
	if shutdown == nil {
		t.Fatal("InstallProvider returned nil shutdown")
	}

	_, span := Tracer().Start(context.Background(), "span")
	if !span.IsRecording() {
		t.Fatal("span IsRecording() = false with configured endpoint, want true (real SDK provider installed)")
	}
	span.End() // queues onto the batch processor; flushed (and dropped) on shutdown

	// otlptracehttp.New/Start do no I/O (confirmed in the module source:
	// client.Start only lazily initializes instrumentation), so InstallProvider
	// itself never touches the network. Shutdown does try to flush the span
	// queued above, which triggers exactly one HTTP POST attempt to the dead
	// local port above; that fails immediately with connection-refused
	// rather than blocking, so Shutdown returns promptly even with no
	// collector ever having been present. We bound it anyway so a future
	// regression here fails the test instead of hanging the suite.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- shutdown(shutdownCtx) }()
	select {
	case err := <-done:
		// Exporting to a dead endpoint is expected to error; we only assert
		// that Shutdown returned promptly, not that the export succeeded.
		t.Logf("shutdown returned (err=%v)", err)
	case <-shutdownCtx.Done():
		t.Fatal("shutdown did not return within 5s against a dead (non-listening) endpoint")
	}
}
