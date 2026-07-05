package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// InstallProvider installs a real SDK tracer provider (OTLP/HTTP exporter,
// batched) as the process-wide OTel global, so the spans trace.go already
// emits (via Tracer()) start recording and exporting — trace.go itself is
// unchanged and unaware whether a provider is installed.
//
// endpoint == "" installs nothing: it leaves whatever global provider is
// already registered (the phase-1 default is none, so spans stay no-op) and
// returns a nil-returning no-op shutdown.
//
// Otherwise it builds an otlptracehttp exporter (WithEndpoint(endpoint), and
// WithInsecure() when insecure is set), wraps it in a BatchSpanProcessor, and
// registers a TracerProvider carrying a "service.name"="gauntlet" resource as
// the OTel global via otel.SetTracerProvider.
//
// The returned shutdown flushes the batch processor and shuts the provider
// down; callers should defer it (with a bounded context) on exit.
func InstallProvider(ctx context.Context, endpoint string, insecure bool) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if endpoint == "" {
		return noop, nil
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return noop, err
	}

	res := resource.NewSchemaless(attribute.String("service.name", "gauntlet"))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}
