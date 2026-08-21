-- 0011: index the foreign keys that actually matter.
--
-- Postgres does NOT create an index for a foreign key automatically (unlike
-- MySQL). That leaves two costs:
--
--   1. Every DELETE or key UPDATE on the parent must find referencing rows. With
--      no index that is a sequential scan PER DELETED ROW -- quadratic on a bulk
--      delete.
--   2. Any query filtering or joining on the FK column scans.
--
-- 17 unindexed FKs exist. Only 7 are indexed here. The rest are deliberately
-- left alone, because an index is not free: it slows every INSERT and UPDATE on
-- the table and consumes cache that the hot indexes want. Indexing an FK whose
-- parent is never deleted and which nothing queries is pure overhead.
--
-- The list of what is skipped, and why, is at the bottom.

-- ---------------------------------------------------------------------------
-- 1. sessions.rotated_to -- THE REAL BUG
--
-- The cleanup worker HARD-DELETES expired sessions in bulk. This FK is
-- self-referencing with ON DELETE SET NULL, so for every row deleted Postgres
-- must locate rows whose rotated_to points at it. Unindexed, that is a full scan
-- of `sessions` per deleted row: O(n*m) on a job designed to delete thousands.
--
-- Invisible at 27 rows. At 100k sessions pruning 10k, roughly a billion row
-- visits, and the cleanup job silently starts timing out.
-- ---------------------------------------------------------------------------
CREATE INDEX sessions_rotated_to_idx ON sessions (rotated_to)
    WHERE rotated_to IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. purchase_order_lines.ingredient_id -- ON DELETE RESTRICT
--
-- Every attempt to delete or deactivate an ingredient must prove no PO line
-- references it. Unindexed, that scans every PO line ever created.
-- ---------------------------------------------------------------------------
CREATE INDEX po_lines_ingredient_idx ON purchase_order_lines (ingredient_id);

-- ---------------------------------------------------------------------------
-- 3. ledger_entries.outlet_id -- a core marketplace report
--
-- "Revenue by outlet" is one of the first things a multi-outlet merchant asks
-- for, and the ledger is the table that grows fastest once partners are live.
-- ---------------------------------------------------------------------------
CREATE INDEX ledger_outlet_idx ON ledger_entries (outlet_id, created_at DESC)
    WHERE outlet_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 4. modifier_groups.kitchen_id -- cascade + admin listing
--
-- ON DELETE CASCADE from kitchens, and the menu admin lists groups per kitchen.
-- ---------------------------------------------------------------------------
CREATE INDEX modifier_groups_kitchen_idx ON modifier_groups (kitchen_id);

-- ---------------------------------------------------------------------------
-- 5. stock_reservations.kitchen_id -- cascade
--
-- ON DELETE CASCADE from kitchens, and this table sees heavy churn from
-- checkout, so an offboarding could otherwise scan a large table.
-- ---------------------------------------------------------------------------
CREATE INDEX stock_reservations_kitchen_idx ON stock_reservations (kitchen_id);

-- ---------------------------------------------------------------------------
-- 6. idempotency_keys.user_id -- cascade on user delete
--
-- ON DELETE CASCADE from users. The table is on the write path of every
-- checkout, so it is never small between prunes.
-- ---------------------------------------------------------------------------
CREATE INDEX idempotency_user_idx ON idempotency_keys (user_id);

-- ---------------------------------------------------------------------------
-- 7. order_status_events.actor_id -- a real support query
--
-- "What did this staff member do?" during an incident or a dispute. Partial,
-- because system transitions have no actor and there is no point indexing NULLs.
-- ---------------------------------------------------------------------------
CREATE INDEX order_status_events_actor_idx ON order_status_events (actor_id, created_at DESC)
    WHERE actor_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- DELIBERATELY NOT INDEXED, and why:
--
--   orders.payment_id            Nothing queries orders BY payment; payments are
--                                never deleted (financial records).
--   orders.cancelled_by          Audit column. Users are SOFT-deleted, so the
--   refunds.initiated_by         SET NULL path never fires. audit_log already
--   stock_movements.actor_id     covers "what did this actor do" with its own
--   coupons.created_by           index, and these tables are queried by their
--   purchase_orders.created_by   own entity, never by actor.
--   merchants.verified_by
--   merchant_users.invited_by
--   merchant_payout_holds.placed_by / released_by
--
-- Each of those would cost write throughput on a hot table to speed up a delete
-- path that does not exist and a query nobody runs. Revisit if users ever become
-- hard-deletable (a GDPR-style erasure request would change this calculus).
-- ---------------------------------------------------------------------------
