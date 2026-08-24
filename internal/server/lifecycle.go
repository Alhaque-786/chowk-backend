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
	"net/http"
)

// Starting and stopping the process.
//
// Split out of server.go. Run and shutdown are the only places that care about
// signal handling and drain ordering, and keeping them together makes the ordering
// reviewable in one screen.

// Run starts the outbox relay and the HTTP listener, and blocks until ctx is
// cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	relayCtx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()

	go func() {
		if err := s.relay.Run(relayCtx); err != nil {
			s.log.Error("outbox relay exited", "error", err)
		}
	}()

	go s.collectRuntimeMetrics(relayCtx)

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening",
			"addr", s.http.Addr, "env", string(s.cfg.Env))
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server: listen: %w", err)
	case <-ctx.Done():
		return s.shutdown()
	}
}

// shutdown drains in-flight requests before closing dependencies.
//
// The ordering is what makes a deploy invisible to customers: stop accepting new
// requests, let existing ones finish, stop the relay, then close the pools. Doing
// it the other way round fails every request that was mid-flight.

// shutdown drains in-flight requests before closing dependencies.
//
// The ordering is what makes a deploy invisible to customers: stop accepting new
// requests, let existing ones finish, stop the relay, then close the pools. Doing
// it the other way round fails every request that was mid-flight.
/*
shutdown drains the server within the grace period.

TAKES NO CONTEXT ON PURPOSE, and contextcheck's advice here would be a bug rather
than an improvement. shutdown runs BECAUSE the parent context was cancelled — that
is the signal to stop. Threading that context in would hand every step below an
already-cancelled deadline, so http.Shutdown would return immediately, in-flight
requests would be cut off mid-response, and the grace period would be decorative.

A fresh context from Background with an explicit timeout is the only correct source
of a deadline for work that must outlive the thing that triggered it.
*/
//nolint:contextcheck // the parent ctx is cancelled; shutdown must outlive it
func (s *Server) shutdown() error {
	s.log.Info("shutting down", "grace_period", s.cfg.Server.ShutdownTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout)
	defer cancel()

	var shutdownErr error
	if err := s.http.Shutdown(ctx); err != nil {
		shutdownErr = fmt.Errorf("server: graceful shutdown: %w", err)
		s.log.Error("graceful shutdown failed, forcing close", "error", err)
		_ = s.http.Close()
	}

	// Flush spans before closing anything else, or the traces covering this very
	// shutdown -- often the ones you need after a bad deploy -- are lost.
	if err := s.tracer.Shutdown(ctx); err != nil {
		s.log.Error("flushing traces failed", "error", err)
	}

	if err := s.cache.Close(); err != nil {
		s.log.Error("closing redis failed", "error", err)
	}
	s.db.Close()

	s.log.Info("shutdown complete")
	return shutdownErr
}

// collectRuntimeMetrics periodically publishes gauges that must be sampled
// rather than incremented: pool occupancy and outbox backlog.
//
// The outbox gauges are the most important operational signal in the system. If
// the relay stalls, the API stays perfectly healthy while every downstream side
// effect silently stops -- notifications, stock release, analytics. Without these
// gauges that failure is invisible until a customer complains.
