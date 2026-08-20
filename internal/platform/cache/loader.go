package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
)

// Loader implements cache-aside with stampede protection.
//
// The problem it solves — cache stampede (a.k.a. thundering herd, dogpiling):
//
// A popular kitchen's menu is cached. The key expires at 12:30, right in the
// lunch rush. Two thousand concurrent requests all miss, and all two thousand
// run the same expensive menu query against Postgres simultaneously. The
// database saturates, queries slow down, the pool exhausts, and the site falls
// over — at the exact moment it is busiest.
//
// Two mitigations, both applied here:
//
//  1. singleflight — of N concurrent requests for the same key, exactly ONE
//     executes the loader; the rest wait and receive its result. N database
//     queries collapse to 1. This is per-process; with M replicas you get M
//     queries instead of N×M, which is the difference between fine and fatal.
//  2. TTL jitter — expiry is randomised by ±10%, so keys populated together do
//     not all expire together. Without it, a warm-up that caches 500 menus at
//     once creates a synchronised expiry cliff 5 minutes later.
type Loader struct {
	cache Cache
	group singleflight.Group
}

func NewLoader(c Cache) *Loader {
	return &Loader{cache: c}
}

// GetOrLoad returns the cached value, or loads and caches it.
//
// Generic over T so call sites are typed rather than juggling []byte. It is a
// method on a struct holding the singleflight group so that dedupe is shared
// across calls — a package-level function would need a global group.
func GetOrLoad[T any](
	ctx context.Context,
	l *Loader,
	key string,
	ttl time.Duration,
	load func(context.Context) (T, error),
) (T, error) {
	var zero T

	// Fast path: a hit needs no coordination at all.
	raw, err := l.cache.Get(ctx, key)
	if err == nil {
		var out T
		if json.Unmarshal(raw, &out) == nil {
			metrics.CacheOpsTotal.WithLabelValues("get", "hit").Inc()
			return out, nil
		}
		// Corrupt entry: fall through and reload rather than serving garbage or
		// wedging this key permanently.
		metrics.CacheOpsTotal.WithLabelValues("get", "corrupt").Inc()
	} else if !errors.Is(err, ErrMiss) {
		// Redis is down. Degrade to the origin rather than failing the request:
		// a slow site beats a broken one.
		metrics.CacheOpsTotal.WithLabelValues("get", "error").Inc()
		return load(ctx)
	} else {
		metrics.CacheOpsTotal.WithLabelValues("get", "miss").Inc()
	}

	// Slow path: exactly one caller per key runs the loader.
	result, err, _ := l.group.Do(key, func() (any, error) {
		// Re-check inside the flight. The winner of a race may have populated the
		// cache while we were queueing, and re-reading is far cheaper than
		// re-running the query.
		if raw, err := l.cache.Get(ctx, key); err == nil {
			var out T
			if json.Unmarshal(raw, &out) == nil {
				return out, nil
			}
		}

		value, err := load(ctx)
		if err != nil {
			return zero, err
		}

		encoded, err := json.Marshal(value)
		if err != nil {
			return zero, fmt.Errorf("cache: marshal %q: %w", key, err)
		}
		// Best-effort write: a failed cache write must not fail the request,
		// since we already have the value the caller asked for.
		if err := l.cache.Set(ctx, key, encoded, jitter(ttl)); err != nil {
			metrics.CacheOpsTotal.WithLabelValues("set", "error").Inc()
		}
		return value, nil
	})
	if err != nil {
		return zero, err
	}

	typed, ok := result.(T)
	if !ok {
		// Only reachable if the same key is used with two different types, which
		// is a programming error worth surfacing rather than papering over.
		return zero, fmt.Errorf("cache: type mismatch for key %q", key)
	}
	return typed, nil
}

// Invalidate drops keys. Call on every write to the underlying data.
//
// Explicit invalidation over short TTLs: a menu price change must be visible
// immediately, and "wait 60 seconds for the TTL" is not acceptable when a
// customer is being charged the old price.
func (l *Loader) Invalidate(ctx context.Context, keys ...string) error {
	return l.cache.Delete(ctx, keys...)
}

// jitter spreads expiry by ±10% so keys written together do not expire together.
func jitter(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	spread := float64(ttl) * 0.1
	return ttl + time.Duration((rand.Float64()*2-1)*spread)
}

// Key builders. Centralised so the same logical entity always maps to the same
// key, and so invalidating "everything about kitchen X" is a knowable operation
// rather than a grep.
const (
	keyPrefixMenu    = "menu"
	keyPrefixKitchen = "kitchen"
	keyPrefixCart    = "cart"
)

func MenuKey(kitchenID string) string    { return keyPrefixMenu + ":" + kitchenID }
func KitchenKey(kitchenID string) string { return keyPrefixKitchen + ":" + kitchenID }
func CartKey(userID string) string       { return keyPrefixCart + ":" + userID }
