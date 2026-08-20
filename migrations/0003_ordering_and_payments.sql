-- 0003: orders, order lines, the status audit trail, and payments.

-- ---------------------------------------------------------------------------
-- orders
--
-- Every price is a snapshot taken at checkout, never a live join to menu_items.
-- If a kitchen raises a price tomorrow, yesterday's invoice must not change --
-- that is both a correctness requirement and a GST record-keeping one.
-- ---------------------------------------------------------------------------

CREATE TABLE orders (
    id                  UUID PRIMARY KEY,
    -- Human-readable, e.g. "CK-8F2K9Q". What the customer quotes to support.
    number              TEXT NOT NULL,
    user_id             UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    kitchen_id          UUID NOT NULL REFERENCES kitchens (id) ON DELETE RESTRICT,
    status              TEXT NOT NULL DEFAULT 'pending_payment',

    -- Money breakdown. Each component is stored rather than derived so a
    -- historical invoice reproduces exactly even after tax rules change.
    subtotal_minor      BIGINT NOT NULL DEFAULT 0,
    packaging_fee_minor BIGINT NOT NULL DEFAULT 0,
    delivery_fee_minor  BIGINT NOT NULL DEFAULT 0,
    discount_minor      BIGINT NOT NULL DEFAULT 0,
    tax_minor           BIGINT NOT NULL DEFAULT 0,
    tip_minor           BIGINT NOT NULL DEFAULT 0,
    total_minor         BIGINT NOT NULL DEFAULT 0,
    currency            TEXT NOT NULL DEFAULT 'INR',

    payment_method      TEXT NOT NULL,
    payment_id          UUID,
    coupon_id           UUID,
    coupon_code         TEXT NOT NULL DEFAULT '',

    -- Delivery snapshot. Copied, not referenced: if the customer edits or deletes
    -- that saved address, the order must still record where the food went.
    delivery_address_id UUID,
    delivery_line1      TEXT NOT NULL DEFAULT '',
    delivery_line2      TEXT NOT NULL DEFAULT '',
    delivery_landmark   TEXT NOT NULL DEFAULT '',
    delivery_city       TEXT NOT NULL DEFAULT '',
    delivery_pincode    TEXT NOT NULL DEFAULT '',
    delivery_latitude   NUMERIC(10, 7),
    delivery_longitude  NUMERIC(10, 7),
    receiver_name       TEXT NOT NULL DEFAULT '',
    receiver_phone      TEXT NOT NULL DEFAULT '',

    rider_id            UUID,

    customer_note       TEXT NOT NULL DEFAULT '',
    kitchen_note        TEXT NOT NULL DEFAULT '',

    promised_at         TIMESTAMPTZ,
    confirmed_at        TIMESTAMPTZ,
    prepared_at         TIMESTAMPTZ,
    picked_up_at        TIMESTAMPTZ,
    delivered_at        TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    cancel_reason       TEXT NOT NULL DEFAULT '',
    cancelled_by        UUID REFERENCES users (id) ON DELETE SET NULL,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The status list must stay in lockstep with ordering/domain.Status. A value
    -- the Go state machine does not know would be unreachable and unfixable.
    CONSTRAINT orders_status_check CHECK (status IN (
        'draft', 'pending_payment', 'placed', 'confirmed', 'preparing',
        'ready', 'picked_up', 'delivered', 'cancelled', 'rejected', 'refunded'
    )),
    CONSTRAINT orders_payment_method_check CHECK (
        payment_method IN ('upi', 'card', 'netbanking', 'wallet', 'cod')
    ),
    -- Money invariants enforced at the storage layer, because a bug that writes a
    -- negative total is a bug that hands out money.
    CONSTRAINT orders_amounts_nonneg_check CHECK (
        subtotal_minor >= 0 AND packaging_fee_minor >= 0 AND delivery_fee_minor >= 0
        AND discount_minor >= 0 AND tax_minor >= 0 AND tip_minor >= 0
        AND total_minor >= 0
    ),
    -- A discount larger than the subtotal would mean paying the customer to eat.
    CONSTRAINT orders_discount_bound_check CHECK (discount_minor <= subtotal_minor),
    -- The total must equal the sum of its parts. This catches any arithmetic
    -- divergence between the cart preview and the charged amount at write time,
    -- rather than in a reconciliation report a month later.
    CONSTRAINT orders_total_consistency_check CHECK (
        total_minor = subtotal_minor + packaging_fee_minor + delivery_fee_minor
                      + tax_minor + tip_minor - discount_minor
    ),
    -- A cancelled order must record why and when; an audit with a blank reason is
    -- useless in a payment dispute.
    CONSTRAINT orders_cancel_fields_check CHECK (
        (status NOT IN ('cancelled', 'rejected'))
        OR (cancelled_at IS NOT NULL AND cancel_reason <> '')
    )
);

CREATE UNIQUE INDEX orders_number_key ON orders (number);

-- The customer's order history, keyset-paginated.
CREATE INDEX orders_user_created_idx ON orders (user_id, created_at DESC, id DESC);

-- The kitchen dashboard's primary query: live orders for one kitchen, oldest
-- first so the queue is FIFO. Partial index keeps it tiny -- a kitchen has a
-- handful of live orders against a table that grows forever.
CREATE INDEX orders_kitchen_active_idx ON orders (kitchen_id, created_at)
    WHERE status IN ('placed', 'confirmed', 'preparing', 'ready', 'picked_up');

-- Lets the reaper worker find orders abandoned mid-payment and release the stock
-- they are holding.
CREATE INDEX orders_stale_pending_idx ON orders (created_at)
    WHERE status = 'pending_payment';

CREATE INDEX orders_rider_idx ON orders (rider_id) WHERE rider_id IS NOT NULL;
-- Backs the admin revenue reports.
CREATE INDEX orders_created_at_idx ON orders (created_at DESC);

-- ---------------------------------------------------------------------------
-- order_lines
-- ---------------------------------------------------------------------------

CREATE TABLE order_lines (
    id            UUID PRIMARY KEY,
    order_id      UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    -- ON DELETE RESTRICT: a menu item referenced by any order can never be hard
    -- deleted, only hidden. Losing the link would orphan the invoice.
    menu_item_id  UUID NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    -- Snapshotted, so a renamed or delisted dish still prints correctly.
    name          TEXT NOT NULL,
    quantity      INT NOT NULL,
    unit_price_minor BIGINT NOT NULL,
    line_total_minor BIGINT NOT NULL,
    tax_bps       INT NOT NULL DEFAULT 0,
    tax_minor     BIGINT NOT NULL DEFAULT 0,
    -- Chosen modifiers, denormalised as JSONB. They are never queried
    -- independently of their line, and a child table would add a join to every
    -- order read for no gain.
    modifiers     JSONB NOT NULL DEFAULT '[]',
    note          TEXT NOT NULL DEFAULT '',

    CONSTRAINT order_lines_quantity_check CHECK (quantity > 0 AND quantity <= 100),
    CONSTRAINT order_lines_price_check CHECK (
        unit_price_minor >= 0 AND line_total_minor >= 0 AND tax_minor >= 0
    )
);

CREATE INDEX order_lines_order_idx ON order_lines (order_id);
-- Backs "most ordered dishes" analytics.
CREATE INDEX order_lines_menu_item_idx ON order_lines (menu_item_id);

-- ---------------------------------------------------------------------------
-- order_status_events -- append-only audit trail
--
-- For any payment dispute or complaint, "who moved this order to cancelled, when,
-- and why" must be answerable months later. There is no UPDATE or DELETE path.
-- ---------------------------------------------------------------------------

CREATE TABLE order_status_events (
    id          UUID PRIMARY KEY,
    order_id    UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    from_status TEXT NOT NULL,
    to_status   TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    actor_id    UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Role is snapshotted: if a staff member is later promoted, the trail must
    -- still show the authority they acted under at the time.
    actor_role  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX order_status_events_order_idx ON order_status_events (order_id, created_at);

-- ---------------------------------------------------------------------------
-- payments
-- ---------------------------------------------------------------------------

CREATE TABLE payments (
    id               UUID PRIMARY KEY,
    order_id         UUID NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    user_id          UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    amount_minor     BIGINT NOT NULL,
    currency         TEXT NOT NULL DEFAULT 'INR',
    status           TEXT NOT NULL DEFAULT 'created',
    method           TEXT NOT NULL,

    provider         TEXT NOT NULL DEFAULT 'razorpay',
    -- The gateway's own id. Unique, because a webhook arriving twice must update
    -- the existing row rather than insert a duplicate payment.
    provider_ref     TEXT,
    provider_order_id TEXT,
    -- Raw gateway payload, kept verbatim for reconciliation and disputes. Never
    -- parsed for business logic -- the provider can change its shape.
    provider_payload JSONB,

    failure_code     TEXT NOT NULL DEFAULT '',
    failure_reason   TEXT NOT NULL DEFAULT '',

    -- The client's idempotency key, so a retried "create payment" cannot produce
    -- a second charge.
    idempotency_key  TEXT,

    authorized_at    TIMESTAMPTZ,
    captured_at      TIMESTAMPTZ,
    failed_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT payments_status_check CHECK (status IN (
        'created', 'pending', 'authorized', 'captured', 'failed', 'refunded', 'partially_refunded'
    )),
    CONSTRAINT payments_amount_check CHECK (amount_minor > 0)
);

CREATE UNIQUE INDEX payments_provider_ref_key ON payments (provider_ref)
    WHERE provider_ref IS NOT NULL;
CREATE UNIQUE INDEX payments_idempotency_key ON payments (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX payments_order_idx ON payments (order_id);
CREATE INDEX payments_user_created_idx ON payments (user_id, created_at DESC);

-- Money that is authorized but never captured is money at risk; this index backs
-- the reconciliation alert.
CREATE INDEX payments_pending_idx ON payments (created_at)
    WHERE status IN ('created', 'pending', 'authorized');

ALTER TABLE orders
    ADD CONSTRAINT orders_payment_id_fkey
    FOREIGN KEY (payment_id) REFERENCES payments (id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- refunds
--
-- A separate table rather than columns on payments, because one payment can be
-- refunded in several parts (two of five items were wrong) and each part has its
-- own gateway lifecycle.
-- ---------------------------------------------------------------------------

CREATE TABLE refunds (
    id            UUID PRIMARY KEY,
    payment_id    UUID NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    order_id      UUID NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    amount_minor  BIGINT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'initiated',
    provider_ref  TEXT,
    initiated_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    completed_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT refunds_status_check CHECK (
        status IN ('initiated', 'processing', 'completed', 'failed')
    ),
    CONSTRAINT refunds_amount_check CHECK (amount_minor > 0)
);

CREATE INDEX refunds_payment_idx ON refunds (payment_id);
CREATE INDEX refunds_order_idx ON refunds (order_id);

-- ---------------------------------------------------------------------------
-- webhook_events -- inbound gateway callbacks
--
-- Persisted before processing, for two reasons: gateways retry aggressively and
-- the unique provider_event_id makes duplicate delivery a no-op rather than a
-- double capture; and if processing has a bug, the raw event can be replayed.
-- ---------------------------------------------------------------------------

CREATE TABLE webhook_events (
    id                UUID PRIMARY KEY,
    provider          TEXT NOT NULL,
    provider_event_id TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    payload           JSONB NOT NULL,
    signature_valid   BOOLEAN NOT NULL DEFAULT false,
    processed_at      TIMESTAMPTZ,
    process_error     TEXT NOT NULL DEFAULT '',
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX webhook_events_provider_event_key
    ON webhook_events (provider, provider_event_id);
CREATE INDEX webhook_events_unprocessed_idx ON webhook_events (received_at)
    WHERE processed_at IS NULL;
