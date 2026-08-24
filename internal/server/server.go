// Package server is the composition root.
//
// This is the ONLY package permitted to know about every module at once. Modules
// do not import each other; they meet here, and here alone. If you ever find
// yourself wanting to import one module from another, the dependency belongs in
// internal/contracts and gets injected from this file instead.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery"
	"github.com/zevoxa/chowk-backend/internal/modules/identity"
	"github.com/zevoxa/chowk-backend/internal/modules/inventory"
	"github.com/zevoxa/chowk-backend/internal/modules/notification"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering"
	"github.com/zevoxa/chowk-backend/internal/modules/payment"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/lock"
	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
	"github.com/zevoxa/chowk-backend/internal/platform/ratelimit"
	"github.com/zevoxa/chowk-backend/internal/platform/resilience"
	"github.com/zevoxa/chowk-backend/internal/platform/token"
	"github.com/zevoxa/chowk-backend/internal/platform/tracing"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
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
	identity     *identity.Module
	catalog      *catalog.Module
	ordering     *ordering.Module
	payment      *payment.Module
	inventory    *inventory.Module
	delivery     *delivery.Module
	notification *notification.Module
	promotion    *promotion.Module

	// idempotency guards write endpoints against client retries.
	idempotency *database.IdempotencyStore
	// locker ensures exactly one worker replica runs each periodic job.
	locker *lock.Locker

	// guards collects every circuit breaker in the process so /status can report
	// which dependency is tripped. Populated during module construction.
	guards *resilience.Registry

	// readiness memoises the last dependency check. See handleReady.
	readiness struct {
		mu     sync.Mutex
		at     time.Time
		result readyResult
	}

	// outbox holds the most recent backlog sample, written by the metrics
	// collector that already queries it every 15s. Cached rather than re-queried
	// so /status costs nothing.
	outboxSample struct {
		mu   sync.Mutex
		at   time.Time
		snap outboxSnapshot
	}
}

// readyResult is the memoised outcome of a readiness check.
type readyResult struct {
	Ready  bool              `json:"ready"`
	Checks map[string]string `json:"checks"`
}

// outboxSnapshot is the relay's backlog at a point in time.
type outboxSnapshot struct {
	Pending    int64   `json:"pending"`
	DeadLetter int64   `json:"dead_letter"`
	LagSeconds float64 `json:"lag_seconds"`
	// A POINTER so it serialises as null rather than 0001-01-01T00:00:00Z before
	// the first sample. Any dashboard that formats this field would otherwise
	// render "year 1" and look broken — and, worse, zeros from "never measured"
	// would be indistinguishable from zeros from "nothing pending". Those are
	// different facts and only one of them is good news.
	SampledAt *time.Time `json:"sampled_at"`
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

	// Emitted once, at startup, next to everything else an operator reads when a
	// deployment comes up. A TLS misconfiguration is exactly the kind of thing that
	// is obvious in a startup line and invisible forever afterwards.
	for _, w := range db.Warnings() {
		log.Warn("database configuration", "warning", w)
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

	guards := resilience.NewRegistry()

	srv := &Server{
		cfg:        cfg,
		log:        log,
		db:         db,
		cache:      redis,
		dispatcher: dispatcher,
		relay:      relay,
		tracer:     tracer,
		guards:     guards,

		idempotency: database.NewIdempotencyStore(db),
		locker:      lock.New(redis),

		identity: identity.New(identity.Deps{
			Config: cfg, DB: db, Cache: redis, Events: outbox, Tokens: tokens, Clock: clk,
		}),
		catalog: catalog.New(catalog.Deps{
			Config: cfg, DB: db, Cache: redis, Events: outbox, Clock: clk,
		}),
	}

	// These three are built before ordering, which depends on all their contracts.
	srv.inventory = inventory.New(inventory.Deps{
		Config: cfg, DB: db, Events: outbox, Clock: clk,
	})
	srv.delivery = delivery.New(delivery.Deps{
		Config: cfg, DB: db, Events: outbox, Clock: clk,
		Users: srv.identity.UserDirectory(),
	})

	// Notification subscribes to the order lifecycle and is subscribed to by
	// nothing. Deleting it would not change a line of ordering, payment or
	// inventory -- which is the property that makes it independently deployable.
	srv.notification = notification.New(notification.Deps{
		Config: cfg, DB: db, Log: log, Clock: clk,
		Users: srv.identity.UserDirectory(),
	})

	/*
	  A GENUINE CYCLE between two modules, resolved by deferring the lookup.

	  ordering needs promotion's DiscountCalculator, and promotion needs ordering's
	  CustomerHistory for the first-order-only coupon rule. Neither can be
	  constructed second, so construction order cannot resolve it.

	  The fix is that promotion does not need the dependency at CONSTRUCTION time —
	  only when a request arrives, by which point both modules exist. `lazyHistory`
	  closes over srv and resolves on call.

	  Deliberately not the two alternatives. Setter injection after both are built
	  leaves a window where the field is nil and nothing enforces that it is filled.
	  Merging the modules to remove the cycle would delete the seam this
	  architecture exists to keep — and the cycle is between two RULES that
	  genuinely belong to different owners: promotion owns "first order only",
	  ordering owns what an order is.
	*/
	srv.promotion = promotion.New(promotion.Deps{
		Config: cfg, DB: db, Events: outbox, Clock: clk,
		History: lazyHistory{srv: srv},
	})

	// Payment before ordering: ordering depends on its PaymentInitiator.
	srv.payment = payment.New(payment.Deps{
		Config: cfg, DB: db, Events: outbox, Clock: clk,
		Users:  srv.identity.UserDirectory(),
		Guards: guards,
	})

	// Ordering is built last because it depends on every other module's contract.
	//
	// Every dependency is now a real implementation -- no null adapters remain.
	// The adapters in contracts/null.go are kept for tests and for the next module
	// that is stubbed before it is built.
	srv.ordering = ordering.New(ordering.Deps{
		Config: cfg, DB: db, Cache: redis, Events: outbox, Clock: clk,

		Prices:   srv.catalog.PriceQuoter(),
		Kitchens: srv.catalog.KitchenAvailability(),
		Users:    srv.identity.UserDirectory(),

		Payments:  srv.payment.PaymentInitiator(),
		Stock:     srv.inventory.StockReserver(),
		Riders:    srv.delivery.RiderDispatcher(),
		Discounts: srv.promotion.DiscountCalculator(),
	})

	// Every module registers its event handlers before the relay starts, so no
	// event can be dispatched to an unregistered consumer and dead-letter.
	srv.identity.RegisterSubscribers(dispatcher)
	srv.catalog.RegisterSubscribers(dispatcher)
	srv.ordering.RegisterSubscribers(dispatcher)
	srv.payment.RegisterSubscribers(dispatcher)
	srv.inventory.RegisterSubscribers(dispatcher)
	srv.delivery.RegisterSubscribers(dispatcher)
	srv.notification.RegisterSubscribers(dispatcher)
	srv.promotion.RegisterSubscribers(dispatcher)

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
		// Same gate, same warning: /status reports breaker states and backlog
		// depths, which describe internal topology.
		r.Get("/status", s.handleStatus)
	}

	// Payment webhooks are public by necessity (a gateway has no JWT) and
	// authenticated by HMAC signature inside the handler. Mounted OUTSIDE the
	// rate-limited /api/v1 subtree on purpose: throttling a gateway means
	// dropping payment notifications, and a dropped capture is an order the
	// customer paid for that nobody cooks.
	r.Mount("/webhooks/payment", s.payment.WebhookRoutes())
	r.Mount("/webhooks/delivery", s.delivery.WebhookRoutes())

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
			// Offers live here, not in the authenticated group below: browsing
			// promotions must work before signing up. The module applies
			// Authenticate itself to the one route that needs a user.
			r.Mount("/offers", s.promotion.CustomerRoutes(httpx.Authenticate(tokens)))
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
			r.Mount("/deliveries", s.delivery.CustomerRoutes())
			r.Mount("/notifications", s.notification.CustomerRoutes())
			r.Mount("/addresses", s.identity.AddressRoutes())
		})

		// --- Kitchen-facing: the staff app ---
		r.Group(func(r chi.Router) {
			r.Use(httpx.Authenticate(tokens))
			r.Use(httpx.RequireRole(httpx.RoleKitchenStaff, httpx.RoleKitchenAdmin))
			r.Mount("/kitchen/catalog", s.catalog.KitchenRoutes())
			r.Mount("/kitchen", s.ordering.KitchenRoutes())
			r.Mount("/kitchen/inventory", s.inventory.KitchenRoutes())
			r.Mount("/kitchen/delivery", s.delivery.KitchenRoutes())
		})

		// --- Admin dashboard ---
		r.Group(func(r chi.Router) {
			r.Use(httpx.Authenticate(tokens))
			r.Use(httpx.RequireRole(httpx.RolePlatformAdmin))
			r.Get("/admin/events", s.handleListEventHandlers)
			r.Mount("/admin/coupons", s.promotion.AdminRoutes())
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, notFoundErr())
	})
	return r
}

// lazyHistory defers resolving ordering's CustomerHistory until a call arrives.
//
// See the cycle note at the promotion construction above. It reaches through srv
// on every call rather than caching the resolved value, which costs one pointer
// hop and removes any question of when it was captured.
type lazyHistory struct{ srv *Server }

func (l lazyHistory) HasEverOrdered(ctx context.Context, userID id.ID) (bool, error) {
	return l.srv.ordering.CustomerHistory().HasEverOrdered(ctx, userID)
}

// Handler exposes the fully-wired HTTP handler.
//
// Exported for tests, which is worth stating plainly: the whole middleware chain
// -- auth, rate limiting, idempotency, the error envelope -- only exists as a
// composed whole, and testing handlers in isolation would skip exactly the part
// most likely to be wrong. A route accidentally mounted outside Authenticate is
// an auth bypass that no unit test can see.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Dispatcher is exposed so the worker binary can reuse the same wiring.
func (s *Server) Dispatcher() *eventbus.Dispatcher { return s.dispatcher }

// Relay is exposed so the worker binary can run it standalone.
func (s *Server) Relay() *eventbus.Relay { return s.relay }
