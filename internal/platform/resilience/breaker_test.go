package resilience

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func failing(context.Context) error    { return errBoom }
func succeeding(context.Context) error { return nil }

func testConfig() BreakerConfig {
	return BreakerConfig{
		Name:                     "test",
		FailureThreshold:         3,
		Window:                   time.Minute,
		OpenTimeout:              50 * time.Millisecond,
		HalfOpenMaxCalls:         2,
		HalfOpenSuccessesToClose: 2,
	}
}

func TestBreakerTripsAfterConsecutiveFailures(t *testing.T) {
	b := NewBreaker(testConfig())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := b.Do(ctx, failing); !errors.Is(err, errBoom) {
			t.Fatalf("call %d: got %v, want errBoom", i, err)
		}
	}

	if got := b.State(); got != StateOpen {
		t.Fatalf("state after 3 failures = %v, want open", got)
	}

	// The whole point: once open, the dependency is not called at all. Failing in
	// microseconds instead of waiting out a 30s timeout is what stops a degraded
	// third party from exhausting our connection pool.
	called := false
	err := b.Do(ctx, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("open breaker returned %v, want ErrCircuitOpen", err)
	}
	if called {
		t.Error("open breaker invoked the dependency; it must fail fast without calling")
	}
}

func TestBreakerSuccessResetsConsecutiveFailures(t *testing.T) {
	b := NewBreaker(testConfig())
	ctx := context.Background()

	// Two failures, then a success, then two more failures. Consecutive count
	// never reaches 3, so the breaker must stay closed.
	_ = b.Do(ctx, failing)
	_ = b.Do(ctx, failing)
	_ = b.Do(ctx, succeeding)
	_ = b.Do(ctx, failing)
	_ = b.Do(ctx, failing)

	if got := b.State(); got != StateClosed {
		t.Errorf("state = %v, want closed (a success resets the counter)", got)
	}
}

// TestBreakerTripsOnFailureRate covers the case a consecutive counter cannot
// see: a dependency failing 60% of the time, where interleaved successes keep
// resetting the consecutive count forever.
func TestBreakerTripsOnFailureRate(t *testing.T) {
	b := NewBreaker(BreakerConfig{
		Name: "rate",
		// Set high so only the rate rule can trip it.
		FailureThreshold:     1000,
		FailureRateThreshold: 0.5,
		MinimumRequests:      10,
		Window:               time.Minute,
		OpenTimeout:          time.Second,
	})
	ctx := context.Background()

	// Alternate fail/succeed. Consecutive failures never exceed 1.
	for i := 0; i < 12; i++ {
		if i%2 == 0 {
			_ = b.Do(ctx, failing)
		} else {
			_ = b.Do(ctx, succeeding)
		}
	}

	if got := b.State(); got != StateOpen {
		t.Errorf("state = %v, want open (50%% failure rate should trip)", got)
	}
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	cfg := testConfig()
	b := NewBreaker(cfg)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.Do(ctx, failing)
	}
	if b.State() != StateOpen {
		t.Fatal("breaker should be open")
	}

	// Wait out the cooldown.
	time.Sleep(cfg.OpenTimeout + 20*time.Millisecond)

	// First trial call is admitted and succeeds -> half-open, not yet closed,
	// because one lucky response is not proof of recovery.
	if err := b.Do(ctx, succeeding); err != nil {
		t.Fatalf("first trial call: %v", err)
	}
	if got := b.State(); got != StateHalfOpen {
		t.Errorf("state after 1 trial success = %v, want half_open", got)
	}

	// Second success reaches HalfOpenSuccessesToClose -> closed.
	if err := b.Do(ctx, succeeding); err != nil {
		t.Fatalf("second trial call: %v", err)
	}
	if got := b.State(); got != StateClosed {
		t.Errorf("state after 2 trial successes = %v, want closed", got)
	}
}

// TestBreakerReopensOnHalfOpenFailure asserts the conservative recovery policy:
// any failure during a trial sends it straight back to open. Being eager to
// re-close would hammer a still-recovering dependency and slow its recovery.
func TestBreakerReopensOnHalfOpenFailure(t *testing.T) {
	cfg := testConfig()
	b := NewBreaker(cfg)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.Do(ctx, failing)
	}
	time.Sleep(cfg.OpenTimeout + 20*time.Millisecond)

	// A trial call that fails.
	_ = b.Do(ctx, failing)

	if got := b.State(); got != StateOpen {
		t.Errorf("state after failed trial = %v, want open", got)
	}
}

// TestBreakerIgnoresCallerCancellation is important: a wave of client
// disconnects (users backgrounding the app, a mobile network dropping) must not
// trip a breaker on a perfectly healthy dependency.
func TestBreakerIgnoresCallerCancellation(t *testing.T) {
	b := NewBreaker(testConfig())

	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancelled before the call
		_ = b.Do(ctx, func(ctx context.Context) error { return ctx.Err() })
	}

	if got := b.State(); got != StateClosed {
		t.Errorf("state = %v, want closed -- caller cancellations are not dependency failures", got)
	}
}

// TestBreakerIsRaceFree runs with -race and is the real reason this test file
// exists. A breaker is shared mutable state on the hot path of every outbound
// call; an unsynchronised counter here is a heisenbug that only appears under
// production load.
func TestBreakerIsRaceFree(t *testing.T) {
	b := NewBreaker(testConfig())
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if (i+j)%3 == 0 {
					_ = b.Do(ctx, failing)
				} else {
					_ = b.Do(ctx, succeeding)
				}
				_ = b.State()
			}
		}(i)
	}
	wg.Wait()
}

func TestBackoffGrowsAndIsJittered(t *testing.T) {
	cfg := RetryConfig{
		BaseDelay:    100 * time.Millisecond,
		MaxDelay:     10 * time.Second,
		Multiplier:   2.0,
		JitterFactor: 0.3,
	}

	// Growth: attempt 3's delay should exceed attempt 1's, jitter notwithstanding.
	// Compare bounds rather than exact values, since jitter is random.
	first := backoffDelay(cfg, 1) // ~100ms ±30%  -> [70, 130]
	third := backoffDelay(cfg, 3) // ~400ms ±30%  -> [280, 520]

	if first > 130*time.Millisecond || first < 70*time.Millisecond {
		t.Errorf("attempt 1 delay %v outside jittered range [70ms, 130ms]", first)
	}
	if third < 280*time.Millisecond || third > 520*time.Millisecond {
		t.Errorf("attempt 3 delay %v outside jittered range [280ms, 520ms]", third)
	}

	// Jitter must actually vary. Identical delays across many samples would mean
	// every client retries in lockstep -- the thundering herd jitter exists to
	// prevent.
	seen := make(map[time.Duration]bool)
	for i := 0; i < 20; i++ {
		seen[backoffDelay(cfg, 2)] = true
	}
	if len(seen) < 5 {
		t.Errorf("only %d distinct delays across 20 samples; jitter is not working", len(seen))
	}
}

func TestBackoffRespectsMaxDelay(t *testing.T) {
	cfg := RetryConfig{
		BaseDelay:    time.Second,
		MaxDelay:     2 * time.Second,
		Multiplier:   2.0,
		JitterFactor: 0, // no jitter, so the cap is exact
	}

	// Attempt 10 would be 512s uncapped.
	if got := backoffDelay(cfg, 10); got != 2*time.Second {
		t.Errorf("delay = %v, want the 2s cap", got)
	}
}

func TestRetryStopsOnNonRetryableError(t *testing.T) {
	permanent := errors.New("permanent")
	cfg := RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		Multiplier:  2,
		Retryable:   func(err error) bool { return !errors.Is(err, permanent) },
	}

	attempts := 0
	err := Do(context.Background(), cfg, func(context.Context) error {
		attempts++
		return permanent
	})

	if attempts != 1 {
		t.Errorf("made %d attempts, want 1 -- a non-retryable error must not be retried", attempts)
	}
	if !errors.Is(err, permanent) {
		t.Errorf("got %v, want the permanent error unwrapped", err)
	}
}

func TestRetryDoesNotRetryOpenCircuit(t *testing.T) {
	cfg := RetryConfig{MaxAttempts: 5, BaseDelay: time.Millisecond, Multiplier: 2}

	attempts := 0
	err := Do(context.Background(), cfg, func(context.Context) error {
		attempts++
		return ErrCircuitOpen
	})

	// A tripped breaker means the dependency is known-down. Retrying only delays
	// the inevitable failure response.
	if attempts != 1 {
		t.Errorf("made %d attempts against an open circuit, want 1", attempts)
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("got %v, want ErrCircuitOpen", err)
	}
}

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	cfg := RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, Multiplier: 2}

	attempts := 0
	err := Do(context.Background(), cfg, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errBoom
		}
		return nil
	})

	if err != nil {
		t.Errorf("got %v, want success on the third attempt", err)
	}
	if attempts != 3 {
		t.Errorf("made %d attempts, want 3", attempts)
	}
}

func TestRetryAbortsOnContextCancellation(t *testing.T) {
	cfg := RetryConfig{MaxAttempts: 10, BaseDelay: 50 * time.Millisecond, Multiplier: 2}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	attempts := 0
	err := Do(ctx, cfg, func(context.Context) error {
		attempts++
		return errBoom
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when the context expires")
	}
	// Must abort during the backoff rather than sleeping out all 10 attempts.
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v; retry should abort promptly on cancellation", elapsed)
	}
}

func TestBulkheadRejectsWhenFull(t *testing.T) {
	bh := NewBulkhead("test", 2)
	ctx := context.Background()

	if err := bh.Acquire(ctx); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := bh.Acquire(ctx); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if got := bh.InFlight(); got != 2 {
		t.Errorf("InFlight = %d, want 2", got)
	}

	// Third must be rejected rather than queue unboundedly, which is what stops a
	// slow dependency consuming every goroutine in the process.
	shortCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := bh.Acquire(shortCtx); !errors.Is(err, ErrBulkheadFull) {
		t.Errorf("third acquire returned %v, want ErrBulkheadFull", err)
	}

	bh.Release()
	if err := bh.Acquire(ctx); err != nil {
		t.Errorf("acquire after release: %v", err)
	}
}
