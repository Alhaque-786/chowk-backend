// Package resilience protects the API from its own dependencies.
//
// The failure mode this exists to prevent: the payment gateway starts taking 30
// seconds instead of 300ms. Every checkout request now holds a goroutine and a
// database connection for 30 seconds. The pool exhausts. Requests that have
// nothing to do with payments start timing out. A degraded third party has become
// a full outage of your own service.
//
// Three patterns, each solving a distinct part of that:
//
//   - Circuit breaker: stop calling a dependency that is clearly broken. Failing
//     in 1µs is far better than failing in 30s, and it lets the dependency
//     recover instead of being hammered while it is down.
//   - Retry with jitter: recover from transient blips -- but ONLY for idempotent
//     operations, and never in lockstep, or every client retries at the same
//     instant and the recovering service is immediately re-flattened.
//   - Bulkhead: cap concurrency per dependency, so slowness in one cannot consume
//     every worker in the process.
package resilience

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/metrics"
)

// State is the breaker's current position.
type State int

const (
	// StateClosed is normal operation: calls pass through.
	StateClosed State = iota
	// StateHalfOpen allows a limited trial of calls to test recovery.
	StateHalfOpen
	// StateOpen rejects immediately without calling the dependency.
	StateOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half_open"
	default:
		return "open"
	}
}

// ErrCircuitOpen is returned when the breaker is rejecting calls. Callers should
// treat it as a temporary unavailability and degrade, not as a hard failure.
var ErrCircuitOpen = errors.New("resilience: circuit open")

// BreakerConfig tunes one breaker.
type BreakerConfig struct {
	Name string

	// FailureThreshold is the consecutive-failure count that trips the breaker.
	FailureThreshold int
	// FailureRateThreshold trips on a ratio instead, once MinimumRequests have
	// been seen in the window. A rate catches "40% of calls fail" -- which a
	// consecutive counter never sees, because successes keep resetting it.
	FailureRateThreshold float64
	MinimumRequests      int
	// Window over which the failure rate is measured.
	Window time.Duration

	// OpenTimeout is how long to stay open before trialling recovery.
	OpenTimeout time.Duration
	// HalfOpenMaxCalls limits concurrent trial calls, so a still-broken
	// dependency gets a trickle rather than the full flood on the first probe.
	HalfOpenMaxCalls int
	// HalfOpenSuccessesToClose is how many trial calls must succeed to close.
	// More than one, because a single lucky response is not recovery.
	HalfOpenSuccessesToClose int
}

// DefaultBreakerConfig is tuned for a payment gateway: quick to trip (money
// operations failing is urgent), patient about recovery (gateways take real time
// to come back, and probing aggressively slows that down).
func DefaultBreakerConfig(name string) BreakerConfig {
	return BreakerConfig{
		Name:                     name,
		FailureThreshold:         5,
		FailureRateThreshold:     0.5,
		MinimumRequests:          10,
		Window:                   30 * time.Second,
		OpenTimeout:              15 * time.Second,
		HalfOpenMaxCalls:         3,
		HalfOpenSuccessesToClose: 2,
	}
}

// Breaker is a circuit breaker for one dependency.
//
// One breaker per dependency, never one shared: a failing SMS provider must not
// block payments. Safe for concurrent use.
type Breaker struct {
	cfg BreakerConfig

	mu    sync.Mutex
	state State

	consecutiveFailures int
	// Counts within the current rolling window.
	windowStart time.Time
	requests    int
	failures    int

	openedAt        time.Time
	halfOpenCalls   int
	halfOpenSuccess int
}

func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = 15 * time.Second
	}
	if cfg.Window <= 0 {
		cfg.Window = 30 * time.Second
	}
	if cfg.HalfOpenMaxCalls <= 0 {
		cfg.HalfOpenMaxCalls = 3
	}
	if cfg.HalfOpenSuccessesToClose <= 0 {
		cfg.HalfOpenSuccessesToClose = 2
	}

	b := &Breaker{cfg: cfg, state: StateClosed, windowStart: time.Now()}
	metrics.SetCircuitState(cfg.Name, int(StateClosed))
	return b
}

// Do runs fn under the breaker.
//
// Returns ErrCircuitOpen without invoking fn when the circuit is open. That fast
// failure is the entire point: it frees the caller immediately instead of making
// it wait out a timeout that is already known to be coming.
func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	if err := b.beforeCall(); err != nil {
		return err
	}

	err := fn(ctx)

	// A caller-cancelled context is not the dependency's fault. Counting it as a
	// failure would let a wave of client disconnects trip a breaker on a
	// perfectly healthy service.
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		b.afterCall(true)
		return err
	}

	b.afterCall(err == nil)
	return err
}

// beforeCall decides whether the call may proceed.
func (b *Breaker) beforeCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	switch b.state {
	case StateOpen:
		if now.Sub(b.openedAt) < b.cfg.OpenTimeout {
			return ErrCircuitOpen
		}
		// Cooldown elapsed: trial recovery.
		b.transitionTo(StateHalfOpen, now)
		b.halfOpenCalls = 1
		return nil

	case StateHalfOpen:
		// Admit only a few probes; the rest fail fast so a still-broken
		// dependency is not flooded the instant the timer expires.
		if b.halfOpenCalls >= b.cfg.HalfOpenMaxCalls {
			return ErrCircuitOpen
		}
		b.halfOpenCalls++
		return nil

	default: // StateClosed
		b.rollWindowLocked(now)
		return nil
	}
}

// afterCall records the outcome and may change state.
func (b *Breaker) afterCall(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.rollWindowLocked(now)
	b.requests++
	if !success {
		b.failures++
	}

	switch b.state {
	case StateHalfOpen:
		if !success {
			// Any failure during trial re-opens immediately. Being conservative
			// here is correct: a half-recovered dependency degrades worse under
			// load than one that is left alone.
			b.transitionTo(StateOpen, now)
			return
		}
		b.halfOpenSuccess++
		if b.halfOpenSuccess >= b.cfg.HalfOpenSuccessesToClose {
			b.transitionTo(StateClosed, now)
		}

	case StateClosed:
		if success {
			b.consecutiveFailures = 0
			return
		}
		b.consecutiveFailures++

		if b.consecutiveFailures >= b.cfg.FailureThreshold {
			b.transitionTo(StateOpen, now)
			return
		}
		// Rate check catches partial failure, which a consecutive counter misses
		// entirely because interleaved successes keep resetting it.
		if b.cfg.FailureRateThreshold > 0 && b.requests >= b.cfg.MinimumRequests {
			if float64(b.failures)/float64(b.requests) >= b.cfg.FailureRateThreshold {
				b.transitionTo(StateOpen, now)
			}
		}
	}
}

// transitionTo changes state and resets the counters that belong to it.
// Caller must hold the mutex.
func (b *Breaker) transitionTo(next State, now time.Time) {
	if b.state == next {
		return
	}
	b.state = next
	metrics.SetCircuitState(b.cfg.Name, int(next))

	switch next {
	case StateOpen:
		b.openedAt = now
		b.halfOpenCalls = 0
		b.halfOpenSuccess = 0
	case StateHalfOpen:
		b.halfOpenCalls = 0
		b.halfOpenSuccess = 0
	case StateClosed:
		b.consecutiveFailures = 0
		b.halfOpenCalls = 0
		b.halfOpenSuccess = 0
		b.resetWindowLocked(now)
	}
}

// rollWindowLocked discards counts older than the window.
//
// A simple reset rather than a true sliding window: the imprecision at a
// boundary does not matter for a trip decision, and it avoids keeping a
// timestamp per request. Caller must hold the mutex.
func (b *Breaker) rollWindowLocked(now time.Time) {
	if now.Sub(b.windowStart) >= b.cfg.Window {
		b.resetWindowLocked(now)
	}
}

func (b *Breaker) resetWindowLocked(now time.Time) {
	b.windowStart = now
	b.requests = 0
	b.failures = 0
}

// State reports the current state, for health endpoints and tests.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
