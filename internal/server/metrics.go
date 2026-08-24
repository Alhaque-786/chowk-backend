// Package server is the composition root.
//
// This is the ONLY package permitted to know about every module at once. Modules
// do not import each other; they meet here, and here alone. If you ever find
// yourself wanting to import one module from another, the dependency belongs in
// internal/contracts and gets injected from this file instead.
package server

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
)

// Gauges that must be SAMPLED rather than incremented.
//
// Split out of server.go. Pool occupancy and outbox backlog cannot be counted at
// the call site — nothing increments "how many connections are checked out" — so
// they are polled on a ticker.

// collectRuntimeMetrics periodically publishes gauges that must be sampled
// rather than incremented: pool occupancy and outbox backlog.
//
// The outbox gauges are the most important operational signal in the system. If
// the relay stalls, the API stays perfectly healthy while every downstream side
// effect silently stops -- notifications, stock release, analytics. Without these
// gauges that failure is invisible until a customer complains.
func (s *Server) collectRuntimeMetrics(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stat := s.db.Pool().Stat()
			metrics.DBPoolStats.WithLabelValues("acquired").Set(float64(stat.AcquiredConns()))
			metrics.DBPoolStats.WithLabelValues("idle").Set(float64(stat.IdleConns()))
			metrics.DBPoolStats.WithLabelValues("total").Set(float64(stat.TotalConns()))
			metrics.DBPoolStats.WithLabelValues("max").Set(float64(stat.MaxConns()))

			s.collectOutboxMetrics(ctx)
		}
	}
}

func (s *Server) collectOutboxMetrics(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// One query for all three gauges. FILTER is cheaper than three separate
	// scans, and this runs on every replica every 15s.
	const q = `
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'dead_letter'),
			COALESCE(EXTRACT(EPOCH FROM (now() - min(occurred_at) FILTER (WHERE status = 'pending'))), 0)
		FROM outbox_events
		WHERE status IN ('pending', 'dead_letter')`

	var pending, deadLetter int64
	var lagSeconds float64

	if err := s.db.Q(queryCtx).QueryRow(queryCtx, q).Scan(&pending, &deadLetter, &lagSeconds); err != nil {
		s.log.Error("collecting outbox metrics failed", "error", err)
		return
	}

	metrics.OutboxPendingGauge.Set(float64(pending))
	metrics.OutboxDeadLetterGauge.Set(float64(deadLetter))
	metrics.OutboxLagSeconds.Set(lagSeconds)

	// Kept for /status, so an operator without Prometheus can see the same signal.
	// Free: this query already runs every 15s for the gauges above.
	s.outboxSample.mu.Lock()
	sampledAt := time.Now()
	s.outboxSample.snap = outboxSnapshot{
		Pending:    pending,
		DeadLetter: deadLetter,
		LagSeconds: lagSeconds,
		SampledAt:  &sampledAt,
	}
	s.outboxSample.mu.Unlock()

	// Dead letters are money and customer trust sitting in a table doing
	// nothing. Log loudly so an alert can fire on the log too, not only on the
	// metric.
	if deadLetter > 0 {
		s.log.Error("outbox has dead-lettered events requiring manual review", "count", deadLetter)
	}
}
