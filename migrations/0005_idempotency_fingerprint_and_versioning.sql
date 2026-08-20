-- 0005: idempotency fingerprinting + optimistic locking.
--
-- Two additions the first pass missed.

-- ---------------------------------------------------------------------------
-- 1. Body fingerprint on idempotency keys
--
-- Without it, a client that reuses one key for two different requests silently
-- receives the first response for the second request. That hides a real client
-- bug behind a plausible-looking success -- the customer sees an order
-- confirmation for a cart they changed. Storing a hash of the body lets the
-- middleware distinguish "same request retried" (replay it) from "key reused for
-- different content" (reject it, loudly).
-- ---------------------------------------------------------------------------

ALTER TABLE idempotency_keys
    ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';

-- Response status is stored inside the response JSONB, so no column is needed.
-- Recording which endpoint issued the key stays useful for debugging.
CREATE INDEX idempotency_endpoint_idx ON idempotency_keys (endpoint, created_at DESC);

-- ---------------------------------------------------------------------------
-- 2. Optimistic locking via a version column
--
-- The problem it solves: two kitchen staff open the same order on two tablets.
-- Both read version 3. One marks it preparing, the other marks it cancelled.
-- Last write wins and one action silently vanishes -- with no error, and no way
-- to know it happened.
--
-- With a version column, every UPDATE carries `AND version = $expected` and
-- increments it. The second writer matches zero rows, gets a conflict, and the
-- UI can reload and re-present the choice. The user is told, instead of being
-- lied to.
--
-- Why this AND row locks: `SELECT ... FOR UPDATE` (used for stock) serialises
-- writers inside one short transaction. Optimistic locking covers the much longer
-- window where a human is holding stale data on a screen -- you cannot hold a
-- database lock for the ninety seconds someone spends deciding.
-- ---------------------------------------------------------------------------

ALTER TABLE orders          ADD COLUMN version INT NOT NULL DEFAULT 1;
ALTER TABLE menu_items      ADD COLUMN version INT NOT NULL DEFAULT 1;
ALTER TABLE ingredients     ADD COLUMN version INT NOT NULL DEFAULT 1;
ALTER TABLE kitchens        ADD COLUMN version INT NOT NULL DEFAULT 1;
ALTER TABLE purchase_orders ADD COLUMN version INT NOT NULL DEFAULT 1;

-- Sessions, addresses and the append-only tables are deliberately excluded:
-- concurrent edits there are either impossible or harmless.

-- ---------------------------------------------------------------------------
-- 3. Admin audit log
--
-- Every privileged action needs to be attributable. "Who marked this order
-- refunded", "who changed this dish's price at 2am", "who suspended this
-- account" are questions that arise during disputes and incidents, and the
-- answer must survive long after the actor's role has changed.
--
-- Separate from order_status_events, which tracks one aggregate. This tracks the
-- ACTOR across all of them.
-- ---------------------------------------------------------------------------

CREATE TABLE audit_log (
    id           UUID PRIMARY KEY,
    actor_id     UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Snapshotted: if a staff member is later promoted or demoted, the record
    -- must still show the authority they acted under at the time.
    actor_role   TEXT NOT NULL DEFAULT '',
    action       TEXT NOT NULL,
    -- Polymorphic target, so one table covers every entity type. Not a foreign
    -- key, because a FK per type would mean a dozen nullable columns.
    entity_type  TEXT NOT NULL,
    entity_id    UUID,
    -- Before/after snapshots make the change itself reviewable, not just the
    -- fact that something changed.
    before_state JSONB,
    after_state  JSONB,
    ip_address   TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT '',
    trace_id     TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- "What did this person do?" -- the question asked during an investigation.
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, created_at DESC);
-- "What happened to this order?" -- the question asked during a dispute.
CREATE INDEX audit_log_entity_idx ON audit_log (entity_type, entity_id, created_at DESC);
CREATE INDEX audit_log_action_idx ON audit_log (action, created_at DESC);
