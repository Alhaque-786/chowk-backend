// Package server is the composition root.
//
// This is the ONLY package permitted to know about every module at once. Modules
// do not import each other; they meet here, and here alone. If you ever find
// yourself wanting to import one module from another, the dependency belongs in
// internal/contracts and gets injected from this file instead.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/lock"
)

// The background worker: periodic jobs and the outbox relay.
//
// Split out of server.go. This runs as a SEPARATE PROCESS from the API in
// production (same binary, different entrypoint), so having its own file makes that
// boundary visible in the file listing rather than only in cmd/.

// RunWorker runs the background half of the system with no HTTP listener.
//
// It reuses the exact same module wiring as the API, which is the point: a
// subscriber registered in a module is live in the worker automatically, with no
// second wiring path to forget to update.
//
// Deploying this as a separate process matters under load. Sending a push
// notification or draining a thousand outbox rows must not compete for the same
// goroutines and connection pool that are serving checkout.
func (s *Server) RunWorker(ctx context.Context) error {
	s.log.Info("worker starting", "env", string(s.cfg.Env))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.relay.Run(ctx); err != nil {
			s.log.Error("outbox relay exited", "error", err)
		}
	}()

	// Periodic maintenance. Kept in-process rather than as cron entries so it
	// ships and scales with the app and cannot silently stop existing.
	//
	// Every job runs under a distributed lock, so with several worker replicas
	// exactly one executes each tick. Without it, three replicas would each try to
	// reap the same expired reservations and contend on the same rows.
	go s.runPeriodic(ctx, "session-cleanup", time.Hour, s.cleanupExpiredSessions)

	// Releases stock held by orders abandoned mid-payment. Without this, one
	// customer who opens checkout and walks away keeps the last portion reserved
	// indefinitely -- and because the order was never paid, nobody is watching it.
	go s.runPeriodic(ctx, "abandoned-order-reaper", 5*time.Minute, func(ctx context.Context) error {
		n, err := s.ordering.KitchenService().ReapAbandonedOrders(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			s.log.Info("cancelled abandoned orders and released their stock", "count", n)
		}
		return nil
	})

	// Backstop for holds whose order vanished some other way. Runs more often than
	// the order reaper because stock sitting reserved is directly lost revenue.
	go s.runPeriodic(ctx, "reservation-reaper", 2*time.Minute, func(ctx context.Context) error {
		n, err := s.inventory.Service().ReapExpiredReservations(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			s.log.Info("released expired stock reservations", "orders", n)
		}
		return nil
	})

	// Money that was authorized but never captured is money at risk, and a payment
	// stuck in `pending` for an hour means a webhook was missed. Read-only: it
	// reports, it does not guess.
	go s.runPeriodic(ctx, "payment-reconciliation", 15*time.Minute, func(ctx context.Context) error {
		_, err := s.payment.Service().ReconcilePending(ctx, time.Hour)
		return err
	})

	// Idempotency keys only need to outlive a client's retry window. Keeping them
	// forever grows a table that is on the write path of every checkout.
	go s.runPeriodic(ctx, "idempotency-prune", 6*time.Hour, func(ctx context.Context) error {
		n, err := s.idempotency.PruneOlderThan(ctx, time.Now().UTC().Add(-24*time.Hour))
		if err != nil {
			return err
		}
		if n > 0 {
			s.log.Info("pruned idempotency keys", "count", n)
		}
		return nil
	})

	<-ctx.Done()
	s.log.Info("worker draining")
	<-done

	if err := s.cache.Close(); err != nil {
		s.log.Error("closing redis failed", "error", err)
	}
	s.db.Close()
	s.log.Info("worker stopped")
	return nil
}

// runPeriodic runs job on an interval until ctx is cancelled. A failing job logs
// and retries on the next tick rather than killing the ticker -- one bad run must
// not stop maintenance forever.

// runPeriodic runs job on an interval until ctx is cancelled. A failing job logs
// and retries on the next tick rather than killing the ticker -- one bad run must
// not stop maintenance forever.
func (s *Server) runPeriodic(ctx context.Context, name string, every time.Duration, job func(context.Context) error) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			start := time.Now()

			// One replica per tick. The lock TTL is generous relative to the job so
			// a slow run is not preempted, but bounded so a crashed holder does not
			// block the job forever.
			err := s.locker.WithLock(ctx, "job:"+name, every+time.Minute, job)
			switch {
			case errors.Is(err, lock.ErrNotAcquired):
				// Another replica is running it. Not a problem, and not worth an
				// Info line every tick.
				continue
			case err != nil:
				s.log.Error("periodic job failed", "job", name, "error", err)
				continue
			}
			s.log.Debug("periodic job completed", "job", name, "duration_ms", time.Since(start).Milliseconds())
		}
	}
}

// cleanupExpiredSessions prunes dead session rows. An unbounded sessions table
// slows the refresh-token lookup that every client hits on token expiry.

// cleanupExpiredSessions prunes dead session rows. An unbounded sessions table
// slows the refresh-token lookup that every client hits on token expiry.
func (s *Server) cleanupExpiredSessions(ctx context.Context) error {
	// Keep a grace window rather than deleting exactly at expiry, so a session
	// that just lapsed can still be diagnosed if a customer reports being logged
	// out unexpectedly.
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	const q = `DELETE FROM sessions WHERE expires_at < $1`
	tag, err := s.db.Q(ctx).Exec(ctx, q, cutoff)
	if err != nil {
		return fmt.Errorf("cleanup sessions: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		s.log.Info("pruned expired sessions", "count", n)
	}
	return nil
}
