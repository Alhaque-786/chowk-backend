-- 0001: extensions, identity tables, and the outbox.
--
-- Conventions used throughout the schema:
--
--   * Primary keys are UUID, generated in Go as UUIDv7 (time-ordered), so inserts
--     append to the right edge of the index instead of fragmenting it.
--   * Money is BIGINT in paise. Never NUMERIC (slow arithmetic, and still lets a
--     bad cast through) and absolutely never FLOAT.
--   * Timestamps are TIMESTAMPTZ, always. TIMESTAMP without a zone silently
--     reinterprets under a server tz change, which corrupts every order history.
--   * Every status column has a CHECK constraint rather than a Postgres ENUM.
--     ENUMs cannot have a value removed and adding one used to require an
--     exclusive lock; a CHECK is a one-line migration.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";
-- Trigram index support, for the admin's "search users by name" box. Without it
-- an ILIKE '%foo%' is a full table scan.
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id                UUID PRIMARY KEY,
    -- E.164, normalised in Go before it ever reaches here. The unique index is
    -- the real guard against duplicate accounts; application checks race.
    phone             TEXT NOT NULL,
    email             TEXT,
    full_name         TEXT NOT NULL DEFAULT '',
    -- NULL for OTP-only accounts, which is a legitimate state, not missing data.
    password_hash     TEXT,
    status            TEXT NOT NULL DEFAULT 'pending_verification',
    -- An array rather than a join table: roles are few, always read together with
    -- the user, and never queried independently. A join table here would add a
    -- round trip to every single authenticated request for no benefit.
    roles             TEXT[] NOT NULL DEFAULT ARRAY['customer'],
    kitchen_id        UUID,
    phone_verified_at TIMESTAMPTZ,
    email_verified_at TIMESTAMPTZ,
    last_login_at     TIMESTAMPTZ,
    referral_code     TEXT NOT NULL,
    referred_by       TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_status_check CHECK (
        status IN ('pending_verification', 'active', 'suspended', 'deleted')
    ),
    CONSTRAINT users_phone_format_check CHECK (phone ~ '^\+91[6-9][0-9]{9}$'),
    CONSTRAINT users_roles_not_empty_check CHECK (cardinality(roles) > 0)
);

-- Named explicitly because the Go code branches on this constraint name to turn
-- a duplicate-key error into "this number is already registered".
CREATE UNIQUE INDEX users_phone_key ON users (phone);

-- Partial unique index: many users have no email, and NULLs are not equal to
-- each other, so this permits unlimited NULLs while keeping real emails unique.
CREATE UNIQUE INDEX users_email_key ON users (lower(email)) WHERE email IS NOT NULL;
CREATE UNIQUE INDEX users_referral_code_key ON users (referral_code);

-- GIN index makes `'kitchen_admin' = ANY(roles)` an index scan instead of a seq
-- scan, which matters for the admin staff list.
CREATE INDEX users_roles_idx ON users USING GIN (roles);

-- Supports the admin user browser's keyset pagination. Partial, because deleted
-- users are never listed and there is no reason to index them.
CREATE INDEX users_created_at_id_idx ON users (created_at DESC, id DESC)
    WHERE status <> 'deleted';

CREATE INDEX users_full_name_trgm_idx ON users USING GIN (full_name gin_trgm_ops);
CREATE INDEX users_kitchen_id_idx ON users (kitchen_id) WHERE kitchen_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- sessions -- one row per logged-in device
-- ---------------------------------------------------------------------------

CREATE TABLE sessions (
    id                 UUID PRIMARY KEY,
    user_id            UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 hex of the refresh token. The plaintext is never stored, so a
    -- database leak yields no usable sessions.
    refresh_token_hash TEXT NOT NULL,
    user_agent         TEXT,
    ip_address         TEXT,
    device_name        TEXT,
    expires_at         TIMESTAMPTZ NOT NULL,
    revoked_at         TIMESTAMPTZ,
    -- Set when this token is rotated. Presenting a token whose row has this set
    -- means replay: either a network duplicate or theft, and we cannot tell, so
    -- the app revokes every session for that user.
    rotated_to         UUID REFERENCES sessions (id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sessions_refresh_token_hash_key ON sessions (refresh_token_hash);

-- Covers the "my devices" screen: only live sessions, newest first.
CREATE INDEX sessions_user_active_idx ON sessions (user_id, last_used_at DESC)
    WHERE revoked_at IS NULL;

-- Lets the cleanup worker find prunable rows without scanning the table.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- addresses
-- ---------------------------------------------------------------------------

CREATE TABLE addresses (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type           TEXT NOT NULL DEFAULT 'home',
    label          TEXT NOT NULL DEFAULT '',
    line1          TEXT NOT NULL,
    line2          TEXT NOT NULL DEFAULT '',
    landmark       TEXT NOT NULL DEFAULT '',
    city           TEXT NOT NULL,
    state          TEXT NOT NULL DEFAULT '',
    pincode        TEXT NOT NULL,
    -- Geocoded at save time so checkout never makes a blocking third-party call.
    -- NUMERIC not FLOAT: coordinates are compared for equality in dedupe logic.
    latitude       NUMERIC(10, 7) NOT NULL,
    longitude      NUMERIC(10, 7) NOT NULL,
    is_default     BOOLEAN NOT NULL DEFAULT false,
    receiver_name  TEXT NOT NULL DEFAULT '',
    receiver_phone TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT addresses_type_check CHECK (type IN ('home', 'work', 'other')),
    CONSTRAINT addresses_pincode_check CHECK (pincode ~ '^[1-9][0-9]{5}$'),
    CONSTRAINT addresses_latitude_check CHECK (latitude BETWEEN -90 AND 90),
    CONSTRAINT addresses_longitude_check CHECK (longitude BETWEEN -180 AND 180),
    -- Rejects the (0,0) an un-geocoded address would carry. That point is in the
    -- Gulf of Guinea and would pass a naive distance check.
    CONSTRAINT addresses_geocoded_check CHECK (NOT (latitude = 0 AND longitude = 0))
);

CREATE INDEX addresses_user_id_idx ON addresses (user_id);

-- Enforces "at most one default per user" in the database rather than in
-- application code, where two concurrent requests could both win.
CREATE UNIQUE INDEX addresses_one_default_per_user_idx ON addresses (user_id)
    WHERE is_default;

-- ---------------------------------------------------------------------------
-- outbox_events -- the transactional outbox
--
-- Written in the same transaction as the state change that caused it, so an
-- order and its "order placed" event either both exist or neither does.
-- ---------------------------------------------------------------------------

CREATE TABLE outbox_events (
    id              UUID PRIMARY KEY,
    event_name      TEXT NOT NULL,
    aggregate_id    UUID NOT NULL,
    payload         JSONB NOT NULL,
    trace_id        TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending',
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at    TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbox_status_check CHECK (
        status IN ('pending', 'processing', 'processed', 'dead_letter')
    )
);

-- The relay's hot path: claim pending rows whose backoff has elapsed, oldest
-- first. Partial index so the millions of already-processed rows cost nothing to
-- skip -- without WHERE, this index would grow forever and the claim query would
-- slow down as history accumulates.
CREATE INDEX outbox_pending_idx ON outbox_events (next_attempt_at, occurred_at)
    WHERE status = 'pending';

-- Dead letters need a human. Indexed so the admin alert query is instant.
CREATE INDEX outbox_dead_letter_idx ON outbox_events (updated_at DESC)
    WHERE status = 'dead_letter';

CREATE INDEX outbox_aggregate_idx ON outbox_events (aggregate_id, occurred_at);

-- ---------------------------------------------------------------------------
-- idempotency_keys -- makes retried writes safe
--
-- Mobile clients retry on flaky networks. Without this, a retried POST /checkout
-- creates a second order and a second charge.
-- ---------------------------------------------------------------------------

CREATE TABLE idempotency_keys (
    key          TEXT PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    endpoint     TEXT NOT NULL,
    -- The first request's response, replayed verbatim to every retry so the
    -- client sees a consistent answer.
    response     JSONB,
    status       TEXT NOT NULL DEFAULT 'in_progress',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,

    CONSTRAINT idempotency_status_check CHECK (status IN ('in_progress', 'completed'))
);

-- Keys are only useful for a short window; the cleanup worker prunes by age.
CREATE INDEX idempotency_created_at_idx ON idempotency_keys (created_at);
