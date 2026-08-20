// Package lock provides a distributed mutex on Redis.
//
// When to reach for this instead of a database row lock: the critical section
// spans more than one system, or touches no single row you could lock.
//
//	Row lock (SELECT ... FOR UPDATE)  -> "decrement this ingredient's stock"
//	Distributed lock                  -> "only one replica may run the nightly
//	                                      settlement job", "only one worker may
//	                                      reconcile this payment with the gateway"
//
// Prefer the database lock when one exists. It is transactional, it releases
// automatically on rollback, and it cannot be lost. This is for the cases where
// there is no row to lock.
//
// Honest limitation: this is a single-instance Redis lock, and it is NOT safe
// against Redis failover — a lock held on a primary that dies can be re-acquired
// on a replica that had not yet replicated it. That is acceptable for the jobs
// above, where a rare double-run is merely wasteful. It is NOT acceptable for
// anything where a double-run moves money; use a database transaction there.
// (Redlock across independent nodes is the usual next step, and is itself
// contested; the real answer is to make the protected work idempotent.)
package lock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/cache"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/id"
)

// ErrNotAcquired means someone else holds the lock.
var ErrNotAcquired = errors.New("lock: not acquired")

// Locker acquires named distributed locks.
type Locker struct {
	cache cache.Cache
}

func New(c cache.Cache) *Locker { return &Locker{cache: c} }

// Lock is a held lock. Always defer Release.
type Lock struct {
	locker *Locker
	key    string
	// token proves ownership. Without it, a slow holder whose TTL expired would
	// delete the lock a DIFFERENT holder has since acquired -- releasing someone
	// else's mutex and letting a third party in. The token makes release
	// conditional on still being the owner.
	token string
	ttl   time.Duration
}

// Acquire tries once and returns ErrNotAcquired immediately if the lock is held.
//
// Use for "only one replica should do this" work, where the losers should simply
// skip rather than queue up behind the winner.
func (l *Locker) Acquire(ctx context.Context, name string, ttl time.Duration) (*Lock, error) {
	if ttl <= 0 {
		// A lock with no expiry is a deadlock waiting to happen: the holder
		// crashes and nothing is ever released.
		ttl = 30 * time.Second
	}

	key := "lock:" + name
	token := id.New().String()

	ok, err := l.cache.SetNX(ctx, key, []byte(token), ttl)
	if err != nil {
		return nil, fmt.Errorf("lock: acquire %q: %w", name, err)
	}
	if !ok {
		return nil, ErrNotAcquired
	}
	return &Lock{locker: l, key: key, token: token, ttl: ttl}, nil
}

// AcquireWait polls until the lock is free or the context expires.
//
// Use when the work must happen and the caller can wait. Polls with a small
// interval rather than a blocking primitive, because Redis has no native
// blocking lock wait and busy-waiting on a short interval is cheap enough.
func (l *Locker) AcquireWait(ctx context.Context, name string, ttl, pollInterval time.Duration) (*Lock, error) {
	if pollInterval <= 0 {
		pollInterval = 50 * time.Millisecond
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		lk, err := l.Acquire(ctx, name, ttl)
		if err == nil {
			return lk, nil
		}
		if !errors.Is(err, ErrNotAcquired) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("lock: wait for %q: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Release frees the lock, but only if we still own it.
//
// The ownership check is the important part. If our TTL expired mid-work,
// another holder now owns this key, and an unconditional DEL would hand the
// critical section to a third party while the second holder is still inside it.
func (lk *Lock) Release(ctx context.Context) error {
	if lk == nil {
		return nil
	}

	current, err := lk.locker.cache.Get(ctx, lk.key)
	if err != nil {
		if errors.Is(err, cache.ErrMiss) {
			// Already expired. Nothing to release, and nothing to report: the
			// work still completed.
			return nil
		}
		return fmt.Errorf("lock: release %q: %w", lk.key, err)
	}
	// Not ours any more -- our TTL lapsed and someone else took it. Deleting here
	// would be the bug this check exists to prevent.
	if string(current) != lk.token {
		return nil
	}
	return lk.locker.cache.Delete(ctx, lk.key)
}

// Extend renews the TTL for long-running work.
//
// Call this periodically from work that may outlive its lease. The alternative --
// taking a very long TTL up front -- means a crashed holder blocks everyone for
// that entire duration.
func (lk *Lock) Extend(ctx context.Context) error {
	current, err := lk.locker.cache.Get(ctx, lk.key)
	if err != nil {
		return fmt.Errorf("lock: extend %q: %w", lk.key, err)
	}
	if string(current) != lk.token {
		return ErrNotAcquired
	}
	return lk.locker.cache.Set(ctx, lk.key, []byte(lk.token), lk.ttl)
}

// WithLock runs fn while holding the named lock, releasing it afterwards even on
// panic. Returns ErrNotAcquired without running fn if the lock is taken.
func (l *Locker) WithLock(ctx context.Context, name string, ttl time.Duration, fn func(context.Context) error) error {
	lk, err := l.Acquire(ctx, name, ttl)
	if err != nil {
		return err
	}
	defer func() {
		// Detached context: if the request context is already cancelled, the
		// release would fail and the lock would linger for its full TTL.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = lk.Release(releaseCtx)
	}()

	return fn(ctx)
}
