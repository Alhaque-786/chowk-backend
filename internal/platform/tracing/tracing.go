// Package tracing sets up OpenTelemetry distributed tracing.
//
// Metrics tell you that p99 checkout latency is 4 seconds. Tracing tells you
// WHICH of the eleven steps inside checkout took 3.8 of them. With a modular
// monolith that is merely convenient; the moment any module becomes its own
// service it is the only way to debug anything, because the latency is now spread
// across processes and no single log file contains the whole story.
//
// Wiring it now rather than "when we split" is deliberate. Retrofitting trace
// propagation across an existing codebase means touching every call site; doing
// it at the seams while there is only one process is nearly free.
//
// OpenTelemetry over a vendor SDK because the exporter is swappable: the same
// instrumentation ships to Jaeger locally, and to Datadog/Honeycomb/Tempo in
// production, without touching application code.
package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const serviceName = "cloud-kitchen-api"

// Config controls the exporter and sampling.
type Config struct {
	Enabled bool
	// Endpoint is an OTLP gRPC collector, e.g. "localhost:4317".
	Endpoint string
	// SampleRatio in [0,1]. Production must sample below 1.0 -- tracing every
	// request at scale costs more in egress and storage than the app costs to run.
	// The ratio applies to ROOT spans only, and the decision propagates, so a
	// sampled trace is complete rather than a scatter of orphaned spans.
	SampleRatio float64
	Environment string
	Version     string
}

// Provider owns the tracer lifecycle.
type Provider struct {
	tp       *sdktrace.TracerProvider
	shutdown func(context.Context) error
}

// Init configures the global tracer.
//
// When disabled it installs a no-op provider rather than leaving the global
// unset. That matters: instrumented code then runs unchanged with effectively
// zero cost, so there is never a nil check at a call site and no separate
// "tracing off" code path that can rot.
func Init(ctx context.Context, cfg Config) (*Provider, error) {
	// Always install the propagator, even when disabled. It is what reads and
	// writes the W3C traceparent header, so an inbound trace id from an upstream
	// service is preserved in logs regardless of whether we export spans.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if !cfg.Enabled || cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return &Provider{shutdown: func(context.Context) error { return nil }}, nil
	}

	// insecure because the collector is normally a sidecar or in-cluster service
	// reached over the pod network. Use TLS credentials for a collector across a
	// trust boundary.
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		// Batch, never sync: a synchronous exporter puts a network call on the
		// request path, so a slow collector would become application latency.
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		),
		// ParentBased keeps a trace whole: if an upstream sampled it, we sample
		// our part too, instead of independently rolling the dice and producing
		// a trace with holes in the middle.
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRatio),
		)),
		sdktrace.WithResource(newResource(cfg)),
	)

	otel.SetTracerProvider(tp)
	return &Provider{tp: tp, shutdown: tp.Shutdown}, nil
}

// Shutdown flushes buffered spans. Must be called on the way out, or the last
// few seconds of traces -- often the ones covering the crash you are
// investigating -- are lost.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Tracer returns a named tracer. Name it after the module, so spans are
// attributable to a bounded context.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// Start begins a span. Always defer the returned end function.
//
//	ctx, end := tracing.Start(ctx, "ordering", "Checkout")
//	defer end()
func Start(ctx context.Context, module, operation string, attrs ...attribute.KeyValue) (context.Context, func()) {
	ctx, span := Tracer(module).Start(ctx, operation, trace.WithAttributes(attrs...))
	return ctx, func() { span.End() }
}

// RecordError marks the active span as failed and attaches the error.
//
// Without this a trace shows a span that took 3 seconds and returned, with no
// indication it errored -- which is the difference between a useful trace and a
// pretty picture.
func RecordError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// TraceIDFrom returns the current trace id, for stamping onto log lines. This is
// the join key between logs and traces: it turns "here is an error" into "here is
// the exact request that produced it".
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SetAttributes annotates the active span with business context -- order id,
// kitchen id, payment method. This is what makes a trace searchable by the thing
// a human actually knows ("find the trace for order CK-8F2K9Q").
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}
