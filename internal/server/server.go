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
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog"
	"github.com/zevoxa/chowk-backend/internal/modules/identity"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering"
	"github.com/zevoxa/chowk-backend/internal/modules/payment"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
	"github.com/zevoxa/chowk-backend/internal/platform/ratelimit"
	"github.com/zevoxa/chowk-backend/internal/platform/token"
	"github.com/zevoxa/chowk-backend/internal/platform/tracing"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

// Server owns the HTTP listener and every long-lived dependency.
type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	db    *database.DB
	cache cache.Cache
	http  *http.Server

	dispatcher *eventbus.Dispatcher
	relay      *eventbus.Relay
	tracer     *tracing.Provider

	// Modules. Add each new module here and to routes() -- those are the only
	// two places the composition root grows.
	identity *identity.Module
	catalog  *catalog.Module
	ordering *ordering.Module
	payment  *payment.Module

	// idempotency guards write endpoints against client retries.
	idempotency *database.IdempotencyStore
}

// New wires everything together. Order matters: infrastructure, then the event
// bus, then modules, then routes.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	// Tracing first, so spans cover the rest of startup and a slow migration or
	// pool warm-up is visible rather than invisible.
	tracer, err := tracing.Init(ctx, tracing.Config{
		Enabled:     cfg.Observ.TracingEnabled,
		Endpoint:    cfg.Observ.TracingEndpoint,
		SampleRatio: cfg.Observ.TracingSampleRatio,
		Environment: string(cfg.Env),
		Version:     cfg.Observ.Version,
	})
	if err != nil {
		return nil, err
	}

	db, err := database.Connect(ctx, cfg.Postgres)
	if err != nil {
		return nil, err
	}

	redis, err := cache.Connect(ctx, cfg.Redis)
	if err != nil {
		db.Close()
		return nil, err
	}

	// The outbox is the Publisher every module receives. Modules therefore
	// publish transactionally without knowing that is what is happening.
	dispatcher := eventbus.NewDispatcher(log)
	outbox := eventbus.NewOutbox(db)
	relay := eventbus.NewRelay(db, dispatcher, log)

	tokens := token.NewManager(cfg.Auth)
	clk := clock.System{}

	srv := &Server{
		cfg:        cfg,
		log:        log,
		db:         db,
		cache:      redis,
		dispatcher: dispatcher,
		relay:      relay,
		tracer:     tracer,

		idempotency: database.NewIdempotencyStore(db),

		identity: identity.New(identity.Deps{
			Config: cfg, DB: db, Cache: redis, Events: outbox, Tokens: tokens, Clock: clk,
		}),
		catalog: catalog.New(catalog.Deps{
			Config: cfg, DB: db, Cache: redis, Events: outbox, Clock: clk,
		}),
	}

	// Payment before ordering: ordering depends on its PaymentInitiator.
	srv.payment = payment.New(payment.Deps{
		Config: cfg, DB: db, Events: outbox, Clock: clk,
		Users: srv.identity.UserDirectory(),
	})

	// Ordering is built last because it depends on the others' contracts.
	//
	// Null adapters stand in for modules that do not exist yet (see
	// contracts/null.go). Each is honest about its absence: stock and rider
	// dispatch degrade silently because that is safe, while payment FAILS LOUDLY
	// because a silent success would mark orders paid without taking money.
	// Replacing an entry here is what "that module is done" means.
	srv.ordering = ordering.New(ordering.Deps{
		Config: cfg, DB: db, Cache: redis, Events: outbox, Clock: clk,

		Prices:   srv.catalog.PriceQuoter(),
		Kitchens: srv.catalog.KitchenAvailability(),
		Users:    srv.identity.UserDirectory(),

		Payments: srv.payment.PaymentInitiator(),

		Stock:     contracts.NullStockReserver{},      // TODO: inventory module
		Discounts: contracts.NullDiscountCalculator{}, // TODO: promotion module
		Riders:    contracts.NullRiderDispatcher{},    // TODO: delivery module
	})

	// Every module registers its event handlers before the relay starts, so no
	// event can be dispatched to an unregistered consumer and dead-letter.
	srv.identity.RegisterSubscribers(dispatcher)
	srv.catalog.RegisterSubscribers(dispatcher)
	srv.ordering.RegisterSubscribers(dispatcher)
	srv.payment.RegisterSubscribers(dispatcher)

	srv.http = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      srv.routes(tokens, ratelimit.New(redis)),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
		// Bound the header read separately: a slowloris attack sends headers one
		// byte at a time and would otherwise hold a connection for the full
		// ReadTimeout.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv, nil
}

// routes builds the full HTTP surface.
//
// The URL structure is versioned from day one (/api/v1). Retrofitting a version
// prefix later means either breaking every mobile client in the wild or running
// a redirect layer forever -- and shipped mobile apps cannot be forced to update.
func (s *Server) routes(tokens *token.Manager, limiter *ratelimit.Limiter) http.Handler {
	r := chi.NewRouter()

	// Global middleware, outermost first. Order is deliberate:
	// RequestID before RequestLogger so every log line has a trace id;
	// Recoverer inside the logger so a panic is still logged as a request.
	r.Use(httpx.RequestID)
	r.Use(httpx.SecurityHeaders)
	// Observe sits outside the logger so its span wraps the whole request, and
	// outside Recoverer so a panicking handler is still counted as a 5xx rather
	// than vanishing from the metrics entirely.
	r.Use(httpx.Observe)
	r.Use(httpx.RequestLogger(s.log))
	r.Use(httpx.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   s.cfg.Server.AllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Request-ID", "X-Device-Name", "Idempotency-Key"},
		ExposedHeaders:   []string{"X-Request-ID", "X-RateLimit-Remaining"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(httpx.Timeout(s.cfg.Server.WriteTimeout - time.Second))

	// Probes are outside /api/v1 and outside rate limiting, because the
	// orchestrator must always be able to reach them.
	r.Get("/health", s.handleHealth)
	r.Get("/ready", s.handleReady)

	// /metrics is unauthenticated but must NEVER be internet-reachable: it
	// exposes internal topology and traffic shape. Restrict it at the ingress or
	// bind it to a separate port in production.
	if s.cfg.Observ.MetricsEnabled {
		r.Handle("/metrics", metrics.Handler())
	}

	// Payment webhooks are public by necessity (a gateway has no JWT) and
	// authenticated by HMAC signature inside the handler. Mounted OUTSIDE the
	// rate-limited /api/v1 subtree on purpose: throttling a gateway means
	// dropping payment notifications, and a dropped capture is an order the
	// customer paid for that nobody cooks.
	r.Mount("/webhooks/payment", s.payment.WebhookRoutes())

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(limiter.Middleware(ratelimit.RuleGlobal))

		// --- Auth: /auth is one subtree that mixes public and authenticated
		// routes, so the module receives the auth middleware and applies it to
		// just the routes that need it. The tighter RuleAuth limit covers the
		// whole subtree, since login and OTP are the endpoints worth attacking.
		r.Group(func(r chi.Router) {
			r.Use(limiter.Middleware(ratelimit.RuleAuth))
			r.Mount("/auth", s.identity.AuthRoutes(httpx.Authenticate(tokens)))
		})

		// --- Public browse: a customer must be able to see the menu before
		// signing in, or the funnel is broken before it starts.
		r.Group(func(r chi.Router) {
			// OptionalAuth, not Authenticate: signed-in customers can be shown
			// personalised content, but anonymous browsing must still work.
			r.Use(httpx.OptionalAuth(tokens))
			r.Mount("/kitchens", s.catalog.PublicRoutes())
		})

		// --- Customer: cart, checkout, orders, addresses ---
		r.Group(func(r chi.Router) {
			r.Use(httpx.Authenticate(tokens))
			// Idempotency on writes. Mobile clients retry on flaky networks, and a
			// retried checkout without this is a second order and a second charge.
			// Not required, so cart edits work without a key; checkout clients
			// should always send one.
			r.Use(httpx.Idempotency(s.idempotency, false))
			r.Use(limiter.Middleware(ratelimit.RuleCheckout))

			r.Mount("/", s.ordering.CustomerRoutes())
			r.Mount("/payments", s.payment.CustomerRoutes())
			r.Mount("/addresses", s.identity.AddressRoutes())
		})

		// --- Kitchen-facing: the staff app ---
		r.Group(func(r chi.Router) {
			r.Use(httpx.Authenticate(tokens))
			r.Use(httpx.RequireRole(httpx.RoleKitchenStaff, httpx.RoleKitchenAdmin))
			r.Mount("/kitchen/catalog", s.catalog.KitchenRoutes())
			r.Mount("/kitchen", s.ordering.KitchenRoutes())
		})

		// --- Admin dashboard ---
		r.Group(func(r chi.Router) {
			r.Use(httpx.Authenticate(tokens))
			r.Use(httpx.RequireRole(httpx.RolePlatformAdmin))
			r.Get("/admin/events", s.handleListEventHandlers)
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, notFoundErr())
	})
	return r
}

// handleHealth is the liveness probe: is the process up? It must not touch
// dependencies -- a Redis blip should not cause Kubernetes to restart a
// perfectly healthy pod.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"env":    string(s.cfg.Env),
	})
}

// handleReady is the readiness probe: can this instance serve traffic? It does
// check dependencies, so a pod with a dead pool is pulled from the load balancer
// without being killed.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{"postgres": "ok", "redis": "ok"}
	ready := true

	if err := s.db.Health(ctx); err != nil {
		checks["postgres"] = "unavailable"
		ready = false
		s.log.Error("readiness: postgres check failed", "error", err)
	}
	if err := s.cache.Health(ctx); err != nil {
		checks["redis"] = "unavailable"
		ready = false
		s.log.Error("readiness: redis check failed", "error", err)
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	httpx.JSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

// handleListEventHandlers exposes the live event wiring, so you can verify in
// production that a subscriber is actually registered rather than inferring it
// from the code.
func (s *Server) handleListEventHandlers(w http.ResponseWriter, _ *http.Request) {
	httpx.OK(w, s.dispatcher.HandlerNames())
}

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

	// Dead letters are money and customer trust sitting in a table doing
	// nothing. Log loudly so an alert can fire on the log too, not only on the
	// metric.
	if deadLetter > 0 {
		s.log.Error("outbox has dead-lettered events requiring manual review", "count", deadLetter)
	}
}

// Dispatcher is exposed so the worker binary can reuse the same wiring.
func (s *Server) Dispatcher() *eventbus.Dispatcher { return s.dispatcher }

// Relay is exposed so the worker binary can run it standalone.
func (s *Server) Relay() *eventbus.Relay { return s.relay }

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
	go s.runPeriodic(ctx, "session-cleanup", time.Hour, s.cleanupExpiredSessions)

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
func (s *Server) runPeriodic(ctx context.Context, name string, every time.Duration, job func(context.Context) error) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			start := time.Now()
			if err := job(ctx); err != nil {
				s.log.Error("periodic job failed", "job", name, "error", err)
				continue
			}
			s.log.Info("periodic job completed", "job", name, "duration_ms", time.Since(start).Milliseconds())
		}
	}
}

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
