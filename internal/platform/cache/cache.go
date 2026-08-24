// Package cache wraps Redis behind a narrow interface.
//
// Modules depend on the Cache interface, not on go-redis. That is not
// ceremony: it means a unit test uses the in-memory implementation with no
// container, and swapping Redis for Valkey or an in-process LRU later is a
// one-file change.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zevoxa/chowk-backend/internal/config"
)

// ErrMiss is returned when a key is absent. Callers must treat a miss as normal
// control flow, never as a failure.
var ErrMiss = errors.New("cache: miss")

// Cache is the contract every module codes against.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// SetNX sets only if absent, reporting whether it won. This is the primitive
	// behind distributed locks and OTP send throttling.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Delete(ctx context.Context, keys ...string) error
	// Incr atomically increments and applies ttl on first touch. Used for rate
	// limit counters where read-modify-write would race.
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	Close() error
	Health(ctx context.Context) error
}

// Redis is the production implementation.
type Redis struct {
	client *redis.Client
}

// Connect opens the Redis connection.
//
// TWO WAYS IN, because local development and a hosted provider want different
// things. REDIS_URL wins when set: hosted Redis hands you a single connection
// string, and the scheme is the only place TLS can be expressed. Otherwise the
// discrete Addr/Password/DB fields apply, so a local container stays
// REDIS_ADDR=localhost:6379 with nothing else to configure.
//
// TLS IS NOT A FLAG HERE, deliberately. `rediss://` already means "TLS", so a
// separate REDIS_TLS boolean would create a second source of truth that can
// disagree with the URL — and the failure when they disagree is a connection that
// hangs on a handshake rather than a clear error. go-redis reads the scheme and
// builds the tls.Config itself.
func Connect(ctx context.Context, cfg config.RedisConfig) (*Redis, error) {
	opts, err := options(cfg)
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		// The address, never the password. This error reaches logs, and a URL-form
		// credential would be pasted into an issue tracker within the week.
		return nil, fmt.Errorf("cache: ping redis at %s: %w", redacted(opts.Addr), err)
	}
	return &Redis{client: client}, nil
}

func options(cfg config.RedisConfig) (*redis.Options, error) {
	timeouts := func(o *redis.Options) *redis.Options {
		o.DialTimeout = 5 * time.Second
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.PoolSize = 20
		return o
	}

	if cfg.URL == "" {
		return timeouts(&redis.Options{
			Addr:     cfg.Addr,
			Password: cfg.Password,
			DB:       cfg.DB,
		}), nil
	}

	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		// Deliberately does NOT wrap the URL into the message: it contains the
		// password. A parse failure is about shape, and the shape is describable
		// without the secret.
		return nil, fmt.Errorf("cache: REDIS_URL is not a valid redis:// or rediss:// connection string: %w", err)
	}

	/*
		A hosted Redis over plaintext is a mistake worth refusing rather than
		warning about. `redis://` to anything that is not loopback sends the
		password in the clear on the first command, and the cache holds session
		and OTP-throttling keys.

		Localhost is exempt because that is the local container, where there is no
		network to sniff and requiring certificates would make development worse
		for no gain.
	*/
	if opts.TLSConfig == nil && !isLoopback(opts.Addr) {
		return nil, fmt.Errorf(
			"cache: REDIS_URL points at %s over plaintext redis://; use rediss:// so the password is not sent in the clear",
			redacted(opts.Addr))
	}

	return timeouts(opts), nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redacted keeps the host and port and drops anything else, so an error can name
// where it was trying to reach without naming how to get in.
func redacted(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		return host + ":" + port
	}
	return addr
}

func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, fmt.Errorf("cache: get %q: %w", key, err)
	}
	return b, nil
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := r.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("cache: set %q: %w", key, err)
	}
	return nil
}

func (r *Redis) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	ok, err := r.client.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("cache: setnx %q: %w", key, err)
	}
	return ok, nil
}

func (r *Redis) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("cache: delete: %w", err)
	}
	return nil
}

func (r *Redis) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	// Pipeline so the increment and its expiry are one round trip. Without the
	// conditional TTL, a hot key would have its window extended forever.
	pipe := r.client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("cache: incr %q: %w", key, err)
	}
	return incr.Val(), nil
}

func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) Health(ctx context.Context) error { return r.client.Ping(ctx).Err() }

// Client exposes the raw driver for the few places that legitimately need more
// than the interface -- pub/sub for live order tracking, for instance.
func (r *Redis) Client() *redis.Client { return r.client }

// GetJSON and SetJSON are generic helpers so call sites do not repeat marshal
// boilerplate. They live as functions rather than methods because Go does not
// allow type parameters on methods.
func GetJSON[T any](ctx context.Context, c Cache, key string) (T, error) {
	var out T
	raw, err := c.Get(ctx, key)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// Treat a corrupt entry as a miss so a bad write cannot wedge a
		// code path permanently.
		return out, ErrMiss
	}
	return out, nil
}

func SetJSON(ctx context.Context, c Cache, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cache: marshal %q: %w", key, err)
	}
	return c.Set(ctx, key, raw, ttl)
}

// Memory is an in-process Cache for tests and local runs without Redis.
type Memory struct {
	mu    sync.Mutex
	items map[string]memEntry
}

type memEntry struct {
	value     []byte
	expiresAt time.Time
}

func NewMemory() *Memory { return &Memory{items: make(map[string]memEntry)} }

func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.items[key]
	if !ok || (!e.expiresAt.IsZero() && time.Now().After(e.expiresAt)) {
		delete(m.items, key)
		return nil, ErrMiss
	}
	return e.value, nil
}

func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = memEntry{value: value, expiresAt: expiry(ttl)}
	return nil
}

func (m *Memory) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.items[key]; ok && (e.expiresAt.IsZero() || time.Now().Before(e.expiresAt)) {
		return false, nil
	}
	m.items[key] = memEntry{value: value, expiresAt: expiry(ttl)}
	return true, nil
}

func (m *Memory) Delete(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.items, k)
	}
	return nil
}

func (m *Memory) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var n int64
	if e, ok := m.items[key]; ok && (e.expiresAt.IsZero() || time.Now().Before(e.expiresAt)) {
		_ = json.Unmarshal(e.value, &n)
		n++
		m.items[key] = memEntry{value: []byte(fmt.Sprint(n)), expiresAt: e.expiresAt}
		return n, nil
	}
	m.items[key] = memEntry{value: []byte("1"), expiresAt: expiry(ttl)}
	return 1, nil
}

func (m *Memory) Close() error                 { return nil }
func (m *Memory) Health(context.Context) error { return nil }

func expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// Compile-time proof both implementations satisfy the interface.
var (
	_ Cache = (*Redis)(nil)
	_ Cache = (*Memory)(nil)
)
