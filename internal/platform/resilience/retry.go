package resilience

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// RetryConfig tunes a retry loop.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// Multiplier for exponential growth. 2.0 doubles each attempt.
	Multiplier float64
	// JitterFactor in [0,1] randomises each delay by ±factor.
	//
	// This is not a nicety. Without jitter, every client that failed at the same
	// instant retries at the same instant, so a service coming back up is
	// immediately flattened by a synchronised thundering herd -- and the pattern
	// repeats on the next attempt, and the next. Jitter spreads the load out.
	JitterFactor float64
	// Retryable decides whether an error is worth retrying. Defaults to
	// "everything except context cancellation", which is usually too broad --
	// pass a real predicate.
	Retryable func(error) bool
}

func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:  3,
		BaseDelay:    100 * time.Millisecond,
		MaxDelay:     5 * time.Second,
		Multiplier:   2.0,
		JitterFactor: 0.3,
	}
}

// Do runs fn, retrying on retryable errors with exponential backoff and jitter.
//
// CRITICAL: only wrap IDEMPOTENT operations. Retrying "charge this card" without
// an idempotency key charges twice. Reads and any write carrying an idempotency
// key are safe; a bare POST that creates something is not.
func Do(ctx context.Context, cfg RetryConfig, fn func(context.Context) error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.Multiplier <= 1 {
		cfg.Multiplier = 2.0
	}
	retryable := cfg.Retryable
	if retryable == nil {
		retryable = func(err error) bool {
			// Never retry a cancelled or timed-out context: the caller has
			// already given up, and retrying just burns resources on a response
			// nobody will read.
			return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
		}
	}

	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(cfg, attempt)
			// Wait on the context as well as the timer, so a cancelled request
			// aborts during the backoff instead of sleeping pointlessly.
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("resilience: retry aborted after %d attempts: %w", attempt, ctx.Err())
			case <-timer.C:
			}
		}

		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) {
			return lastErr
		}
		// A tripped breaker means the dependency is known-down. Retrying is
		// pointless and just delays the inevitable failure response.
		if errors.Is(lastErr, ErrCircuitOpen) {
			return lastErr
		}
	}
	return fmt.Errorf("resilience: exhausted %d attempts: %w", cfg.MaxAttempts, lastErr)
}

// backoffDelay computes the delay before the given attempt (1-indexed for delay
// purposes), applying exponential growth, a ceiling, and symmetric jitter.
func backoffDelay(cfg RetryConfig, attempt int) time.Duration {
	delay := float64(cfg.BaseDelay) * math.Pow(cfg.Multiplier, float64(attempt-1))

	if cfg.MaxDelay > 0 && delay > float64(cfg.MaxDelay) {
		delay = float64(cfg.MaxDelay)
	}

	if cfg.JitterFactor > 0 {
		// Full symmetric jitter: delay × (1 ± factor). Spreading both earlier and
		// later avoids the retry wave that "always add jitter" alone still leaves
		// when every client adds the same positive offset.
		jitter := delay * cfg.JitterFactor
		delay = delay - jitter + (rand.Float64() * 2 * jitter)
	}

	if delay < 0 {
		delay = 0
	}
	return time.Duration(delay)
}

// DoWithResult is Do for a function returning a value.
//
// A separate generic function because Go does not allow type parameters on
// methods, and threading a result through a closure at every call site is noise.
func DoWithResult[T any](ctx context.Context, cfg RetryConfig, fn func(context.Context) (T, error)) (T, error) {
	var result T
	err := Do(ctx, cfg, func(ctx context.Context) error {
		var err error
		result, err = fn(ctx)
		return err
	})
	return result, err
}
