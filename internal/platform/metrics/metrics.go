// Package metrics defines the application's Prometheus instruments.
//
// Why this exists at all: you cannot scale what you cannot see. Logs tell you
// what happened in one request; metrics tell you the shape of all of them. When
// checkout starts failing at 2% you need a rate, a percentile and a breakdown by
// reason -- grepping logs cannot give you that in time to act.
//
// Two rules that keep this cheap:
//
//  1. Label values must be BOUNDED. Every distinct combination of label values
//     creates a new time series held in memory, in Prometheus, and forever in
//     storage. Labelling by user_id or order_id is a cardinality explosion that
//     will take down your monitoring before it takes down your app -- so route
//     templates ("/api/v1/orders/{id}"), never concrete paths.
//  2. Histograms are pre-bucketed and the buckets are chosen per instrument.
//     Default buckets are tuned for seconds; an API that should answer in 20ms
//     needs buckets that can actually resolve 20ms.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// Registry is this process's collector registry.
//
// A private registry rather than prometheus.DefaultRegisterer, so a linked
// library cannot silently inject series into our namespace and tests can build a
// clean registry without global state leaking between them.
var Registry = prometheus.NewRegistry()

// latencyBuckets are tuned for a food-ordering API: most reads should land in
// single-digit milliseconds, writes under 100ms, and anything past a second is
// already a user-visible problem. The tail buckets exist to make timeouts
// visible, not to measure them precisely.
var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

var (
	// --- HTTP ---

	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total HTTP requests by route, method and status class.",
		},
		// status_class not status: 2xx/4xx/5xx is what alerts fire on, and it
		// keeps cardinality at 3 instead of 40.
		[]string{"route", "method", "status_class"},
	)

	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ck",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency by route and method.",
			Buckets:   latencyBuckets,
		},
		[]string{"route", "method"},
	)

	HTTPRequestsInFlight = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "ck",
			Subsystem: "http",
			Name:      "requests_in_flight",
			Help:      "Requests currently being served. Rising with flat throughput means saturation.",
		},
	)

	// --- Application errors ---

	// ErrorsTotal is keyed by our stable apperr code, which is what makes
	// "PHONE_ALREADY_REGISTERED spiked 50x" a graphable signal instead of a
	// log-grep exercise.
	ErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Name:      "errors_total",
			Help:      "Application errors by kind and stable error code.",
		},
		[]string{"kind", "code"},
	)

	// --- Database ---

	DBQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ck",
			Subsystem: "db",
			Name:      "query_duration_seconds",
			Help:      "Database query latency by operation.",
			Buckets:   latencyBuckets,
		},
		[]string{"operation"},
	)

	// DBPoolStats is a gauge vec rather than separate gauges so a dashboard can
	// plot acquired/idle/total on one axis. Pool exhaustion is the single most
	// common cause of a Go API falling over under load, and it is invisible
	// without this.
	DBPoolStats = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "ck",
			Subsystem: "db",
			Name:      "pool_connections",
			Help:      "Connection pool state.",
		},
		[]string{"state"}, // acquired | idle | total | max
	)

	DBTxTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Subsystem: "db",
			Name:      "transactions_total",
			Help:      "Transactions by outcome. Watch serialization_failure for contention.",
		},
		[]string{"outcome"}, // committed | rolled_back | serialization_failure
	)

	// --- Cache ---

	CacheOpsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Subsystem: "cache",
			Name:      "operations_total",
			Help:      "Cache operations by result. hit/(hit+miss) is the ratio that matters.",
		},
		[]string{"operation", "result"}, // result: hit | miss | error
	)

	// --- Event bus / outbox ---

	// OutboxPendingGauge is the single most important operational metric in the
	// system. If it grows without bound the relay is stuck, and every downstream
	// side effect -- notifications, stock release, analytics -- has silently
	// stopped while the API still looks perfectly healthy.
	OutboxPendingGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "ck",
			Subsystem: "outbox",
			Name:      "pending_events",
			Help:      "Events awaiting delivery. Sustained growth means the relay is stuck.",
		},
	)

	OutboxDeadLetterGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "ck",
			Subsystem: "outbox",
			Name:      "dead_letter_events",
			Help:      "Events that exhausted retries. Any non-zero value needs a human.",
		},
	)

	// OutboxLagSeconds is the age of the oldest pending event. More actionable
	// than a queue depth: 10 events stuck for an hour is worse than 10,000 that
	// arrived a second ago.
	OutboxLagSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "ck",
			Subsystem: "outbox",
			Name:      "oldest_pending_age_seconds",
			Help:      "Age of the oldest undelivered event.",
		},
	)

	EventHandlerDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ck",
			Subsystem: "events",
			Name:      "handler_duration_seconds",
			Help:      "Event handler latency by event and handler.",
			Buckets:   latencyBuckets,
		},
		[]string{"event", "handler"},
	)

	EventHandlerTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Subsystem: "events",
			Name:      "handled_total",
			Help:      "Event handler invocations by outcome.",
		},
		[]string{"event", "handler", "outcome"}, // success | failure | panic
	)

	// --- External dependencies ---

	// ExternalCallDuration covers payment gateways, SMS providers, maps APIs.
	// These are the calls that hang, and a hanging dependency is what turns into
	// pool exhaustion and a full outage.
	ExternalCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ck",
			Subsystem: "external",
			Name:      "call_duration_seconds",
			Help:      "Third-party call latency by dependency and operation.",
			// Wider buckets than internal calls: a payment gateway legitimately
			// takes seconds, and we need to see 30s hangs distinctly.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		},
		[]string{"dependency", "operation"},
	)

	ExternalCallTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Subsystem: "external",
			Name:      "calls_total",
			Help:      "Third-party calls by outcome.",
		},
		[]string{"dependency", "operation", "outcome"}, // success|error|timeout|circuit_open
	)

	CircuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "ck",
			Subsystem: "external",
			Name:      "circuit_breaker_state",
			Help:      "Circuit breaker state: 0=closed, 1=half_open, 2=open.",
		},
		[]string{"dependency"},
	)

	// --- Business metrics ---
	//
	// These are deliberately here alongside the technical ones. A deploy that
	// breaks checkout often shows a perfectly healthy 200 rate while orders
	// quietly stop -- because the bug is in the client, or a validation rule got
	// stricter. Order rate is the real "is the business working" signal, and it
	// belongs on the same dashboard as latency.

	OrdersTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Subsystem: "business",
			Name:      "orders_total",
			Help:      "Orders by terminal outcome and payment method.",
		},
		[]string{"outcome", "payment_method"},
	)

	OrderValueRupees = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "ck",
			Subsystem: "business",
			Name:      "order_value_rupees",
			Help:      "Order value distribution, for AOV tracking.",
			Buckets:   []float64{50, 100, 200, 300, 500, 750, 1000, 1500, 2500, 5000},
		},
	)

	// OrderPrepDuration measures kitchen performance, which is what the promised
	// ETA should be derived from rather than a hardcoded guess.
	OrderPrepDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ck",
			Subsystem: "business",
			Name:      "order_prep_duration_seconds",
			Help:      "Time from confirmed to ready, by kitchen.",
			Buckets:   []float64{60, 300, 600, 900, 1200, 1800, 2700, 3600},
		},
		[]string{"kitchen_id"},
	)

	// StockReservationTotal surfaces oversell pressure: a rising conflict rate
	// means customers are hitting sold-out items at checkout, which is a
	// forecasting problem, not a code problem.
	StockReservationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ck",
			Subsystem: "business",
			Name:      "stock_reservations_total",
			Help:      "Stock reservation attempts by outcome.",
		},
		[]string{"outcome"}, // reserved | insufficient | conflict
	)
)

func init() {
	Registry.MustRegister(
		HTTPRequestsTotal, HTTPRequestDuration, HTTPRequestsInFlight,
		ErrorsTotal,
		DBQueryDuration, DBPoolStats, DBTxTotal,
		CacheOpsTotal,
		OutboxPendingGauge, OutboxDeadLetterGauge, OutboxLagSeconds,
		EventHandlerDuration, EventHandlerTotal,
		ExternalCallDuration, ExternalCallTotal, CircuitBreakerState,
		OrdersTotal, OrderValueRupees, OrderPrepDuration, StockReservationTotal,
	)

	// Go runtime and process collectors: goroutine count, GC pause, heap, open
	// file descriptors. A goroutine leak is invisible without these and shows up
	// only as a pod OOMing days later.
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Handler serves the /metrics endpoint.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		// Report scrape errors rather than serving a partial response, so a
		// broken collector is visible instead of silently dropping series.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// StatusClass buckets a status code, keeping label cardinality at 3 instead of
// one series per distinct code.
func StatusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// ObserveHTTP records one request. Called from middleware.
func ObserveHTTP(route, method string, status int, d time.Duration) {
	if route == "" {
		// An unmatched route must not be labelled with its raw path, or a
		// scanner probing random URLs creates a new time series per probe.
		route = "unmatched"
	}
	HTTPRequestsTotal.WithLabelValues(route, method, StatusClass(status)).Inc()
	HTTPRequestDuration.WithLabelValues(route, method).Observe(d.Seconds())
}

// ObserveExternalCall records a third-party call.
func ObserveExternalCall(dependency, operation, outcome string, d time.Duration) {
	ExternalCallTotal.WithLabelValues(dependency, operation, outcome).Inc()
	ExternalCallDuration.WithLabelValues(dependency, operation).Observe(d.Seconds())
}

// SetCircuitState publishes breaker state as a number, since Prometheus gauges
// cannot hold strings.
func SetCircuitState(dependency string, state int) {
	CircuitBreakerState.WithLabelValues(dependency).Set(float64(state))
}

// intToStr avoids strconv appearing at every call site.
func intToStr(n int) string { return strconv.Itoa(n) }

var _ = intToStr
