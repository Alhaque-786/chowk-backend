// Package server is the composition root.
//
// This is the ONLY package permitted to know about every module at once. Modules
// do not import each other; they meet here, and here alone. If you ever find
// yourself wanting to import one module from another, the dependency belongs in
// internal/contracts and gets injected from this file instead.
package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
)

// Health, readiness and status endpoints, and the types they return.
//
// Split out of server.go, which had grown to 856 lines doing six unrelated jobs.
// These three answer three different questions and the distinction is documented on
// handleStatus: alive, should-take-traffic, and what-is-degraded.

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
// readinessTTL memoises the dependency check for a moment.
//
// The probes sit OUTSIDE rate limiting on purpose -- an orchestrator must always be
// able to reach them -- which also makes /ready an unauthenticated endpoint that
// performs a Postgres round trip and a Redis round trip on every request. That is a
// cheap amplifier: a few thousand requests a second to a public URL becomes a few
// thousand pings a second against the database that is already struggling, which is
// exactly when a probe is being hammered.
//
// One second is short enough that a real outage is still detected within a probe
// interval (orchestrators poll at 5-10s) and long enough that a flood collapses into
// a single check. Not configurable, because there is no useful reason to raise it and
// lowering it re-opens the hole.

// handleReady is the readiness probe: can this instance serve traffic? It does
// check dependencies, so a pod with a dead pool is pulled from the load balancer
// without being killed.
// readinessTTL memoises the dependency check for a moment.
//
// The probes sit OUTSIDE rate limiting on purpose -- an orchestrator must always be
// able to reach them -- which also makes /ready an unauthenticated endpoint that
// performs a Postgres round trip and a Redis round trip on every request. That is a
// cheap amplifier: a few thousand requests a second to a public URL becomes a few
// thousand pings a second against the database that is already struggling, which is
// exactly when a probe is being hammered.
//
// One second is short enough that a real outage is still detected within a probe
// interval (orchestrators poll at 5-10s) and long enough that a flood collapses into
// a single check. Not configurable, because there is no useful reason to raise it and
// lowering it re-opens the hole.
const readinessTTL = time.Second

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	result := s.checkReadiness(r.Context())

	status := http.StatusOK
	if !result.Ready {
		status = http.StatusServiceUnavailable
	}
	// Probes must never be cached by an intermediary. Without this a CDN or a
	// corporate proxy can serve a stale "ok" long after the process is gone.
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, status, result)
}

func (s *Server) checkReadiness(ctx context.Context) readyResult {
	s.readiness.mu.Lock()
	defer s.readiness.mu.Unlock()

	if time.Since(s.readiness.at) < readinessTTL && s.readiness.result.Checks != nil {
		return s.readiness.result
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	/*
		THE TWO CHECKS RUN CONCURRENTLY, and that is a fix rather than a flourish.

		They used to run in sequence under one shared 3-second deadline, so a
		Postgres ping that took 2.9s left Redis with 100ms and reported Redis as
		"unavailable" when Redis was fine. A readiness probe that blames the wrong
		dependency is worse than one that simply fails: it sends an operator to
		restart the healthy component.

		Concurrent means the 3 seconds is a per-check budget, which is what the
		number was always meant to express.
	*/
	var wg sync.WaitGroup
	var pgErr, redisErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		pgErr = s.db.Health(ctx)
	}()
	go func() {
		defer wg.Done()
		redisErr = s.cache.Health(ctx)
	}()
	wg.Wait()

	result := readyResult{Ready: true, Checks: map[string]string{"postgres": "ok", "redis": "ok"}}

	// The caller is told WHICH dependency is down but never why. The error text can
	// carry a DSN, a hostname or a driver internal, and this endpoint is public.
	if pgErr != nil {
		result.Checks["postgres"] = "unavailable"
		result.Ready = false
		s.log.Error("readiness: postgres check failed", "error", pgErr)
	}
	if redisErr != nil {
		result.Checks["redis"] = "unavailable"
		result.Ready = false
		s.log.Error("readiness: redis check failed", "error", redisErr)
	}

	s.readiness.at = time.Now()
	s.readiness.result = result
	return result
}

/*
handleStatus is the OPERATOR view: what is degraded, not whether to route traffic.

It exists because the codebase already wanted it. `Guard.State()` carries the
comment "exposes the breaker state for the health endpoint, so an operator can see
which dependency is tripped without reading logs" -- and it was called from nowhere,
because guards are built deep inside the adapters that use them and nothing upstream
held a reference. The intention was written down and never wired up, which reads as a
working feature in review and is discovered at 2am.

DELIBERATELY SEPARATE FROM /ready, and the separation is the whole design:

  - /health answers "is this process alive". No dependency checks at all, because
    failing liveness makes an orchestrator RESTART the process, and restarting your
    API does not fix someone else's database. It turns a dependency blip into a
    crash loop.
  - /ready answers "should traffic come here". Postgres and Redis only, because
    those are the two things without which no request can be served.
  - /status answers "what is degraded". Nothing here influences routing. An open
    payment breaker means payments are failing while menus, carts and order history
    all still work -- pulling the instance out of the load balancer for that would
    convert a partial degradation into a total outage.

A STALLED OUTBOX IS THE CASE THIS UNLOCKS. The relay's own comment calls the outbox
gauges "the most important operational signal in the system: if the relay stalls, the
API stays perfectly healthy while every downstream side effect silently stops --
notifications, stock release, analytics." Until now that signal existed only as a
Prometheus gauge, so anyone without Prometheus had no way to see it.

GATED like /metrics, and for the same reason: breaker states and backlog depths
describe internal topology and traffic shape. Restrict it at the ingress or bind it
to a separate port. It is not authenticated, because the thing that scrapes it is an
agent with no user token -- which is precisely why it must not be internet-reachable.
*/
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.outboxSample.mu.Lock()
	outbox := s.outboxSample.snap
	s.outboxSample.mu.Unlock()

	body := map[string]any{
		"env":      string(s.cfg.Env),
		"ready":    s.checkReadiness(r.Context()),
		"breakers": s.guards.Snapshot(),
		"outbox":   outbox,
		"db_pool":  poolStats(s.db),
		// A relay that has never reported is different from a relay with an empty
		// backlog, and only one of those is fine.
		"outbox_sampled": outbox.SampledAt != nil,
	}

	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, body)
}

func poolStats(db *database.DB) map[string]int32 {
	stat := db.Pool().Stat()
	return map[string]int32{
		"acquired": stat.AcquiredConns(),
		"idle":     stat.IdleConns(),
		"total":    stat.TotalConns(),
		"max":      stat.MaxConns(),
	}
}

// handleListEventHandlers exposes the live event wiring, so you can verify in
// production that a subscriber is actually registered rather than inferring it
// from the code.

// handleListEventHandlers exposes the live event wiring, so you can verify in
// production that a subscriber is actually registered rather than inferring it
// from the code.
func (s *Server) handleListEventHandlers(w http.ResponseWriter, _ *http.Request) {
	httpx.OK(w, s.dispatcher.HandlerNames())
}

// Run starts the outbox relay and the HTTP listener, and blocks until ctx is
// cancelled, then shuts down gracefully.
