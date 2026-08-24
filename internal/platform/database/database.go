// Package database owns the Postgres connection pool and the transaction
// boundary.
//
// The central idea: a repository never knows whether it is inside a
// transaction. It calls db.Q(ctx) to get a Querier, which resolves to the
// ambient transaction if one is running and to the pool otherwise. That means a
// use case can wrap five repositories from three different modules in one
// atomic commit -- "decrement stock, create order, reserve coupon, write outbox
// event" -- without any of them taking a *pgx.Tx parameter.
//
// This is what keeps the modules independently extractable: when `ordering`
// becomes its own service, its repositories compile unchanged.
package database

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// Querier is the subset of pgx satisfied by both *pgxpool.Pool and pgx.Tx.
// Repositories depend on this, never on the concrete pool.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB wraps the pool and exposes the transaction boundary.
type DB struct {
	pool *pgxpool.Pool

	// unverifiedTLS records that the connection is encrypted but NOT
	// authenticated. Held rather than logged inside Connect, which has no logger,
	// and surfaced through Warnings() so the composition root can emit it with the
	// rest of startup. A warning nobody can reach is the same as no warning — see
	// Guard.State(), which was documented as feeding a health endpoint and called
	// from nowhere.
	unverifiedTLS bool
	host          string
}

type txKey struct{}

// Connect opens the pool and verifies it with a ping, so a bad DSN fails at
// boot rather than on the first customer request.
func Connect(ctx context.Context, cfg config.PostgresConfig) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("database: parse DSN: %w", err)
	}

	if err := rejectTransactionPooler(poolCfg.ConnConfig.Port, poolCfg.ConnConfig.DefaultQueryExecMode); err != nil {
		return nil, err
	}
	if err := requireTLSToRemoteHost(poolCfg.ConnConfig.Host, poolCfg.ConnConfig.TLSConfig); err != nil {
		return nil, err
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return &DB{
		pool:          pool,
		host:          poolCfg.ConnConfig.Host,
		unverifiedTLS: unverifiedTLS(poolCfg.ConnConfig.Host, poolCfg.ConnConfig.TLSConfig),
	}, nil
}

func (db *DB) Close() { db.pool.Close() }

func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Health verifies the pool can still serve queries, for the readiness probe.
/*
rejectTransactionPooler refuses a DSN pointed at a transaction-mode connection
pooler, because the failure it causes is one of the nastiest kinds.

pgx uses the extended protocol and caches prepared statements per connection.
A transaction-mode pooler hands a different backend connection to each
TRANSACTION, so a statement prepared on one connection is unknown on the next.
The result is "prepared statement \"lrupsc_1_0\" does not exist" — appearing only
under concurrency, only sometimes, and looking like database corruption rather
than a configuration mistake. It passes every local test, because locally there
is no pooler.

6543 is Supabase's Supavisor transaction-mode port; 6432 is the PgBouncer
convention. A persistent Go service wants SESSION mode instead (Supabase serves
that on 5432 via the pooler host, which also gives IPv4 — the direct connection
is IPv6-only without the paid add-on). Session mode keeps one backend connection
per client connection, so prepared statements behave normally and this whole class
of bug does not exist.

Refused rather than auto-corrected: silently rewriting someone's connection
string is worse than telling them, and the fix is one character in an env var.
The escape hatch is to disable pgx's statement cache
(`?default_query_exec_mode=exec`), which is why the check looks for that first --
someone who has genuinely chosen transaction mode has already said so.
*/
func rejectTransactionPooler(port uint16, mode pgx.QueryExecMode) error {
	if port != 6543 && port != 6432 {
		return nil
	}
	// The deliberate opt-out. `?default_query_exec_mode=exec` tells pgx to send
	// statements unprepared, which is exactly what makes transaction pooling safe --
	// so someone who set it has already made this decision and does not need to be
	// stopped.
	if mode == pgx.QueryExecModeExec || mode == pgx.QueryExecModeSimpleProtocol {
		return nil
	}
	return fmt.Errorf(
		"database: POSTGRES_DSN points at port %d, a transaction-mode pooler. "+
			"pgx caches prepared statements per connection and transaction pooling "+
			"reassigns connections per transaction, which fails intermittently under "+
			"load with \"prepared statement does not exist\". Use the session-mode "+
			"port (5432) for a long-lived service, or add "+
			"?default_query_exec_mode=exec to opt out deliberately", port)
}

/*
requireTLSToRemoteHost refuses an unencrypted connection to anything that is not
loopback.

`sslmode=disable` is the one people reach for when a connection will not work, and
against a hosted database it sends the password and then every row — orders,
addresses, payment references — in clear text across the public internet. It also
succeeds, which is why it survives: the app starts, the tests pass, and nothing
ever complains.

Loopback is exempt. The local container has no certificates and no network to
intercept, and requiring TLS there would make development worse for nothing.

A NOTE ON `require`, WHICH IS THE MORE INTERESTING TRAP. Measured with pgx v5:

	(no sslmode)         TLSConfig set, InsecureSkipVerify=true
	sslmode=disable      TLSConfig nil          <- refused here
	sslmode=prefer       TLSConfig set, InsecureSkipVerify=true
	sslmode=require      TLSConfig set, InsecureSkipVerify=true
	sslmode=verify-full  TLSConfig set, InsecureSkipVerify=false

So `require` means ENCRYPTED, not VERIFIED. It accepts any certificate at all, so
anyone able to answer for the host can present their own and read and rewrite the
whole session. Only `verify-full` checks the chain and the hostname.

That is a warning rather than a refusal, deliberately: Supabase signs its Postgres
certificates with its own CA ("Supabase Intermediate 2021 CA", verified by
handshake — the chain is self-signed and does NOT validate against the system
trust store), so `verify-full` needs their CA file and refusing without it would
block a working deployment on a setting the operator may not be able to satisfy
yet. The fix is to download the CA from the dashboard and use
`?sslmode=verify-full&sslrootcert=/path/to/prod-ca.crt`.
*/
func requireTLSToRemoteHost(host string, tlsCfg *tls.Config) error {
	if tlsCfg != nil || isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf(
		"database: POSTGRES_DSN connects to %s with TLS disabled; the password and "+
			"every row would cross the network in clear text. Use sslmode=require at "+
			"minimum, or sslmode=verify-full with sslrootcert=<ca.crt> to also verify "+
			"the certificate", host)
}

// unverifiedTLS reports a connection that is encrypted but not authenticated.
func unverifiedTLS(host string, tlsCfg *tls.Config) bool {
	return tlsCfg != nil && tlsCfg.InsecureSkipVerify && !isLoopbackHost(host)
}

// Warnings returns configuration problems that are not fatal but that somebody
// should see. Emitted by the caller at startup, once, rather than per query.
func (db *DB) Warnings() []string {
	if !db.unverifiedTLS {
		return nil
	}
	return []string{fmt.Sprintf(
		"postgres connection to %s is encrypted but NOT verified (sslmode=require or "+
			"prefer sets InsecureSkipVerify): any host able to answer for that name can "+
			"present its own certificate and read the session. Download the provider CA "+
			"and use sslmode=verify-full with sslrootcert=<ca.crt>", db.host)}
}

func isLoopbackHost(host string) bool {
	// A unix socket directory, which pgx passes through as the host.
	if strings.HasPrefix(host, "/") {
		return true
	}
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (db *DB) Health(ctx context.Context) error { return db.pool.Ping(ctx) }

// Q returns the Querier a repository should use: the ambient transaction if the
// caller opened one, otherwise the pool.
func (db *DB) Q(ctx context.Context) Querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok && tx != nil {
		return tx
	}
	return db.pool
}

// InTx runs fn inside a transaction, committing on nil error and rolling back
// on error or panic.
//
// Nested calls join the outer transaction rather than opening a second one --
// otherwise a use case calling another use case would deadlock against itself
// on a row it already locked.
func (db *DB) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.InTxOpts(ctx, pgx.TxOptions{}, fn)
}

// InTxSerializable is for invariants that must hold against concurrent writers
// where row locks are not enough -- e.g. "this coupon may be redeemed 100 times
// in total". Callers must be prepared to retry on serialization failure.
func (db *DB) InTxSerializable(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.InTxOpts(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
}

func (db *DB) InTxOpts(ctx context.Context, opts pgx.TxOptions, fn func(ctx context.Context) error) error {
	// Already in a transaction: participate in it, do not nest.
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok && tx != nil {
		return fn(ctx)
	}

	tx, err := db.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("database: begin tx: %w", err)
	}

	// Roll back unconditionally on the way out. A Rollback after a successful
	// Commit is a documented no-op in pgx, so this is safe and guarantees no
	// connection is leaked on a panic.
	committed := false
	defer func() {
		if !committed {
			// Use a detached context: if the request context is already
			// cancelled, the rollback would fail and leak the connection.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	committed = true
	return nil
}

// RetrySerializable runs fn under SERIALIZABLE, retrying on serialization
// failure. Postgres reports these as retryable by contract, so the caller does
// not have to distinguish them.
//
// TWO THINGS HERE ARE NOT INCIDENTAL.
//
// JITTER. The backoff was `(i+1) * 10ms` with no randomness, which is the
// thundering-herd shape: every racer that loses a serialization conflict sleeps
// for the SAME duration and they all retry into each other again. Six callers
// contending for one coupon's last slots would collide, back off in lockstep,
// and collide again, so a caller could burn every attempt without the contention
// ever thinning out. Randomising each sleep across a window is what actually
// spreads them, and it is the same reasoning as the jittered retry in
// internal/platform/resilience -- this one just predates it.
//
// EXHAUSTION IS A CONFLICT, NOT AN INTERNAL ERROR. Running out of attempts means
// "too many people are doing this at once", which is a true and actionable thing
// to tell a caller. Returning the raw pg error made it an INTERNAL_ERROR, so a
// customer racing for the last coupon slot saw "Something went wrong on our end"
// -- blaming us for what is ordinary contention, and giving the client no reason
// to retry. Translating it here rather than at each call site means no service
// can forget to.
func (db *DB) RetrySerializable(ctx context.Context, attempts int, fn func(ctx context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		err = db.InTxSerializable(ctx, fn)
		if err == nil || !IsSerializationFailure(err) {
			return err
		}

		// Exponential base with full jitter: sleep uniformly in [0, base). Full
		// jitter rather than base+jitter because the aim is to SPREAD colliding
		// racers, and a fixed floor keeps them bunched at that floor.
		base := time.Duration(1<<uint(i)) * 10 * time.Millisecond
		if base > maxSerializableBackoff {
			base = maxSerializableBackoff
		}
		delay := time.Duration(rand.Int64N(int64(base)))

		select {
		case <-ctx.Done():
			// The caller's deadline wins. Returning ctx.Err() rather than the
			// serialization error, because "you ran out of time" is the more
			// accurate description of what stopped us.
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	// Every attempt hit contention. Report it as such.
	return apperr.Conflict("TOO_BUSY",
		"Too many people are doing this at once. Please try again.").WithCause(err)
}

// maxSerializableBackoff caps the jitter window. Without a cap, a generous
// attempts value would push the last sleep into the seconds, which for a request
// sitting in a checkout is worse than failing.
const maxSerializableBackoff = 200 * time.Millisecond

// Postgres error codes we branch on. Repositories translate these into
// apperr kinds so services never see a driver-specific error.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeSerializationFailed = "40001"
	codeDeadlockDetected    = "40P01"
	codeLockNotAvailable    = "55P03"
)

// IsNotFound reports whether a query returned no rows.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsUniqueViolation reports a duplicate-key conflict, and if constraint is
// non-empty, only for that specific constraint. Passing the constraint name
// matters: an order insert can violate several unique indexes, and each means
// something different to the caller.
//
// HAZARD -- read before using this inside a transaction.
//
// In Postgres, ANY error raised inside a transaction poisons it: every subsequent
// statement fails, and COMMIT reports "commit unexpectedly resulted in rollback".
// So this helper is safe for deciding WHICH ERROR TO RETURN, and unsafe for
// deciding to CARRY ON.
//
//	// SAFE: translate and return. The caller aborts the transaction.
//	if database.IsUniqueViolation(err, "users_phone_key") {
//	    return apperr.Conflict("PHONE_TAKEN", "...")
//	}
//
//	// BROKEN: the transaction is already dead. The next statement fails and the
//	// commit reports a rollback.
//	if database.IsUniqueViolation(err, "x_key") {
//	    return nil // then keep going...
//	}
//
// For idempotent inserts -- "create it unless it already exists" -- use
// INSERT ... ON CONFLICT DO NOTHING RETURNING id and check whether a row came
// back. That raises no error at all, so the transaction stays usable. This bug
// shipped once in promotion's redemption insert and made retried checkouts return
// 500 instead of succeeding idempotently.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != codeUniqueViolation {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

func IsForeignKeyViolation(err error) bool { return hasCode(err, codeForeignKeyViolation) }
func IsCheckViolation(err error) bool      { return hasCode(err, codeCheckViolation) }
func IsLockNotAvailable(err error) bool    { return hasCode(err, codeLockNotAvailable) }

// IsSerializationFailure covers both serialization conflicts and deadlocks;
// both are safe to retry from the top of the transaction.
func IsSerializationFailure(err error) bool {
	return hasCode(err, codeSerializationFailed) || hasCode(err, codeDeadlockDetected)
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// ConstraintName returns the violated constraint, or "" if the error is not a
// constraint violation. Useful for mapping a specific index to a specific
// user-facing message.
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
