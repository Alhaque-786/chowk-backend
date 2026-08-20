package httpx

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// Observe records metrics and opens a trace span for each request.
//
// Two details that make or break this middleware:
//
//  1. The metric label is chi's ROUTE PATTERN ("/orders/{orderID}"), never the
//     concrete path. Using the real path creates one time series per order id --
//     a cardinality explosion that kills Prometheus long before it kills the app.
//  2. The pattern is only known AFTER the handler runs, because chi resolves
//     routing as it descends. So the route is read on the way out, not the way in.
func Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		// Continue an upstream trace if the caller sent traceparent, so a trace
		// spans the gateway and the API instead of restarting here.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		// Span name is provisional; renamed to the route pattern once known,
		// otherwise every order id becomes its own operation in the trace UI.
		ctx, span := otel.Tracer("http").Start(ctx, r.Method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("url.path", r.URL.Path),
				attribute.String("user_agent.original", r.UserAgent()),
			),
		)
		defer span.End()

		metrics.HTTPRequestsInFlight.Inc()
		defer metrics.HTTPRequestsInFlight.Dec()

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r.WithContext(ctx))

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		route := routePattern(r)
		span.SetName(r.Method + " " + route)
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", status),
		)
		if p, ok := PrincipalFrom(r.Context()); ok {
			span.SetAttributes(attribute.String("enduser.id", p.UserID.String()))
		}

		metrics.ObserveHTTP(route, r.Method, status, time.Since(start))
	})
}

// routePattern extracts chi's matched pattern, falling back to a constant for
// unmatched requests so a URL scanner cannot mint new time series.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}

// RecordError counts an application error by kind and stable code.
//
// Called from Error(), so every error response is counted exactly once and
// "PHONE_ALREADY_REGISTERED spiked 50x after the deploy" becomes a graph rather
// than a log-grep exercise.
func RecordError(err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		metrics.ErrorsTotal.WithLabelValues(string(apperr.KindInternal), "UNCLASSIFIED").Inc()
		return
	}
	metrics.ErrorsTotal.WithLabelValues(string(appErr.Kind), appErr.Code).Inc()
}
