package tracing

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// newResource describes this process to the tracing backend.
//
// These attributes are attached to every span, which is what lets you filter to
// "production, version 1.4.2" and confirm whether a latency regression arrived
// with a specific deploy. Using semconv's standard keys rather than custom ones
// means backends recognise them and light up their built-in service views.
func newResource(cfg Config) *resource.Resource {
	env := cfg.Environment
	if env == "" {
		env = "local"
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}

	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
		attribute.String("deployment.environment", env),
	)
}
