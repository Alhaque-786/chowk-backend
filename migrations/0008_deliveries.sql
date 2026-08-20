-- 0008: deliveries.
--
-- One row per attempt to move an order from kitchen to customer.
--
-- Deliberately thin: no riders table, no shifts, no location history. Delivery is
-- a per-drop cost paid to a third party (or handled by the kitchen's own delivery
-- boy), not a fleet we operate. See internal/modules/delivery/domain for why that
-- is an economics decision rather than a shortcut.

CREATE TABLE deliveries (
    id             UUID PRIMARY KEY,
    order_id       UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    kitchen_id     UUID NOT NULL REFERENCES kitchens (id) ON DELETE RESTRICT,
    mode           TEXT NOT NULL DEFAULT 'third_party',
    status         TEXT NOT NULL DEFAULT 'requested',

    provider       TEXT NOT NULL DEFAULT 'manual',
    provider_ref   TEXT,
    -- The provider's own tracking page. Reusing theirs rather than building a map
    -- view is most of why this module stays small.
    tracking_url   TEXT NOT NULL DEFAULT '',

    rider_name     TEXT NOT NULL DEFAULT '',
    rider_phone    TEXT NOT NULL DEFAULT '',

    -- What the PROVIDER charges us, in paise. Distinct from the delivery FEE the
    -- customer pays (on orders); the gap between them is margin or subsidy, and
    -- keeping both visible is how that stays a deliberate decision.
    cost_minor     BIGINT NOT NULL DEFAULT 0,

    pickup_eta     TIMESTAMPTZ,
    drop_eta       TIMESTAMPTZ,
    assigned_at    TIMESTAMPTZ,
    picked_up_at   TIMESTAMPTZ,
    delivered_at   TIMESTAMPTZ,
    failure_reason TEXT NOT NULL DEFAULT '',

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT deliveries_mode_check CHECK (mode IN ('third_party', 'self', 'pickup')),
    CONSTRAINT deliveries_status_check CHECK (status IN (
        'requested', 'assigned', 'at_pickup', 'picked_up', 'delivered', 'cancelled', 'failed'
    )),
    CONSTRAINT deliveries_cost_check CHECK (cost_minor >= 0),
    -- A failed delivery must say why. "The food did not arrive" with no reason is
    -- unanswerable when the customer asks for a refund.
    CONSTRAINT deliveries_failure_reason_check CHECK (
        status <> 'failed' OR failure_reason <> ''
    )
);

-- One delivery per order. A second booking would mean paying twice for one drop,
-- so the constraint enforces the idempotency the service also checks for.
CREATE UNIQUE INDEX deliveries_order_key ON deliveries (order_id);

CREATE UNIQUE INDEX deliveries_provider_ref_key ON deliveries (provider, provider_ref)
    WHERE provider_ref IS NOT NULL;

-- The staff dispatch board: what is still out. Partial, so completed deliveries
-- cost nothing to skip as history grows.
CREATE INDEX deliveries_active_idx ON deliveries (kitchen_id, created_at)
    WHERE status IN ('requested', 'assigned', 'at_pickup', 'picked_up');

-- Deliveries stuck in `requested` need a human to call a rider.
CREATE INDEX deliveries_unassigned_idx ON deliveries (created_at)
    WHERE status = 'requested';
