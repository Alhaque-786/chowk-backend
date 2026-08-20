package resilience

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// ErrBulkheadFull is returned when a dependency's concurrency budget is exhausted.
var ErrBulkheadFull = errors.New("resilience: bulkhead full")

// Bulkhead caps concurrent calls to one dependency.
//
// Named after ship compartments: a hull breach floods one compartment, not the
// vessel. Here, a slow SMS provider can occupy at most N goroutines, so it cannot
// consume every worker in the process and starve checkout.
//
// A buffered channel is the whole implementation -- a semaphore where acquiring
// is a send and releasing is a receive.
type Bulkhead struct {
	slots chan struct{}
	name  string
}

func NewBulkhead(name string, maxConcurrent int) *Bulkhead {
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	return &Bulkhead{slots: make(chan struct{}, maxConcurrent), name: name}
}

// Acquire takes a slot, waiting up to the context deadline.
func (b *Bulkhead) Acquire(ctx context.Context) error {
	select {
	case b.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %s", ErrBulkheadFull, b.name)
	}
}

func (b *Bulkhead) Release() {
	select {
	case <-b.slots:
	default:
		// Unbalanced Release. Ignoring it beats panicking in production, and the
		// bug shows up as a slowly shrinking concurrency budget.
	}
}

// InFlight reports current occupancy, for metrics and health output.
func (b *Bulkhead) InFlight() int { return len(b.slots) }

// Guard composes all three patterns plus a timeout into the single wrapper that
// every outbound call should go through.
//
// Order of composition is deliberate and not interchangeable:
//
//	bulkhead → breaker → timeout → retry → the call
//
//	1. Bulkhead outermost, so a flood is rejected before it consumes anything.
//	2. Breaker next, so a known-down dependency fails in microseconds.
//	3. Timeout inside the breaker, so a hang becomes a countable failure that
//	   can actually trip the breaker. Outside it, hangs would never be recorded.
//	4. Retry innermost, so each attempt gets its own fresh timeout rather than
//	   all attempts sharing one deadline the first slow call already consumed.
type Guard struct {
	name     string
	breaker  *Breaker
	bulkhead *Bulkhead
	timeout  time.Duration
	retry    RetryConfig
}

type GuardConfig struct {
	Name          string
	Timeout       time.Duration
	MaxConcurrent int
	Breaker       BreakerConfig
	Retry         RetryConfig
	// DisableRetry must be set for non-idempotent operations. Defaulting retry
	// ON and requiring an explicit opt-out would be the wrong default for
	// anything that moves money, so callers doing writes must think about it.
	DisableRetry bool
}

func NewGuard(cfg GuardConfig) *Guard {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Breaker.Name == "" {
		cfg.Breaker = DefaultBreakerConfig(cfg.Name)
	}
	retry := cfg.Retry
	if cfg.DisableRetry {
		retry.MaxAttempts = 1
	} else if retry.MaxAttempts == 0 {
		retry = DefaultRetryConfig()
	}

	return &Guard{
		name:     cfg.Name,
		breaker:  NewBreaker(cfg.Breaker),
		bulkhead: NewBulkhead(cfg.Name, cfg.MaxConcurrent),
		timeout:  cfg.Timeout,
		retry:    retry,
	}
}

// Call runs fn through the full protection stack and records metrics.
//
// It translates infrastructure failures into apperr.KindUnavailable, so a handler
// returns 503 (retry later) rather than 500 (we are broken) — a distinction that
// matters to both clients and your error budget.
func (g *Guard) Call(ctx context.Context, operation string, fn func(context.Context) error) error {
	start := time.Now()

	// 1. Bulkhead: reject rather than queue unboundedly.
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, 1*time.Second)
	err := g.bulkhead.Acquire(acquireCtx)
	cancelAcquire()
	if err != nil {
		metrics.ObserveExternalCall(g.name, operation, "bulkhead_full", time.Since(start))
		return apperr.Unavailable("DEPENDENCY_SATURATED",
			"We are experiencing high load. Please try again in a moment.").WithCause(err)
	}
	defer g.bulkhead.Release()

	// 2. Breaker, wrapping 3. timeout and 4. retry.
	err = g.breaker.Do(ctx, func(ctx context.Context) error {
		return Do(ctx, g.retry, func(ctx context.Context) error {
			// Fresh timeout per attempt.
			callCtx, cancel := context.WithTimeout(ctx, g.timeout)
			defer cancel()
			return fn(callCtx)
		})
	})

	elapsed := time.Since(start)

	switch {
	case err == nil:
		metrics.ObserveExternalCall(g.name, operation, "success", elapsed)
		return nil

	case errors.Is(err, ErrCircuitOpen):
		metrics.ObserveExternalCall(g.name, operation, "circuit_open", elapsed)
		return apperr.Unavailable("DEPENDENCY_UNAVAILABLE",
			"That service is temporarily unavailable. Please try again shortly.").WithCause(err)

	case errors.Is(err, context.DeadlineExceeded):
		metrics.ObserveExternalCall(g.name, operation, "timeout", elapsed)
		return apperr.Unavailable("DEPENDENCY_TIMEOUT",
			"That request took too long. Please try again.").WithCause(err)

	default:
		metrics.ObserveExternalCall(g.name, operation, "error", elapsed)
		return err
	}
}

// State exposes the breaker state for the health endpoint, so an operator can see
// which dependency is tripped without reading logs.
func (g *Guard) State() string { return g.breaker.State().String() }

func (g *Guard) Name() string { return g.name }

// InFlight reports current bulkhead occupancy.
func (g *Guard) InFlight() int { return g.bulkhead.InFlight() }
