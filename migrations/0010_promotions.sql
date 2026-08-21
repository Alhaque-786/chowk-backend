-- 0010: coupons and redemptions.
--
-- The hard part here is not the discount arithmetic, it is the usage caps. Two
-- kinds, and they need different mechanisms:
--
--   PER-USER cap  ("one per customer")  -> a unique index. Cheap and absolute.
--   GLOBAL cap    ("first 100 orders")  -> SERIALIZABLE + retry.
--
-- The global cap cannot be enforced with row locks, and that is worth spelling
-- out. Locking the coupon row would work but serialises every checkout using that
-- coupon behind one lock, which is exactly the wrong behaviour during the flash
-- sale the cap exists for. The invariant is "COUNT(redemptions) < limit" -- a
-- predicate over rows that do not exist yet -- and only SERIALIZABLE isolation
-- detects two transactions both counting 99 and both inserting.

CREATE TABLE coupons (
    id          UUID PRIMARY KEY,
    -- Code as the customer types it. Stored uppercase; comparison is exact, so
    -- normalisation happens before the query.
    code        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',

    -- --- What the discount does ---
    discount_type TEXT NOT NULL,
    -- For 'percentage': basis points (1500 = 15%). For 'flat': paise.
    -- One column for both because they are never used together, and two nullable
    -- columns invite the bug where both are set.
    discount_value BIGINT NOT NULL,
    -- Caps a percentage discount. "20% off, max Rs100" is the standard shape;
    -- without a cap a percentage coupon on a large order is unbounded loss.
    max_discount_minor BIGINT,

    -- --- Who and when it applies ---
    min_order_minor BIGINT NOT NULL DEFAULT 0,
    -- Restricting to specific kitchens matters once partners are onboarded: a
    -- platform-funded promotion should not silently discount a partner's food
    -- and leave them short.
    kitchen_ids  UUID[] NOT NULL DEFAULT '{}',
    first_order_only BOOLEAN NOT NULL DEFAULT false,

    valid_from   TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until  TIMESTAMPTZ,

    -- --- Usage caps ---
    -- NULL means unlimited. 0 would mean "unusable", which is a different thing
    -- and a real state (an exhausted coupon left active deliberately).
    max_redemptions          INT,
    max_redemptions_per_user INT NOT NULL DEFAULT 1,
    -- Denormalised counter, maintained in the same transaction as the redemption.
    -- The redemptions table is the truth; this exists so the eligibility check is
    -- one row read rather than a COUNT on every checkout.
    redemption_count INT NOT NULL DEFAULT 0,

    -- Who absorbs the discount. Once partners exist, a platform-funded coupon
    -- must not be deducted from their settlement -- getting this wrong means
    -- underpaying merchants for promotions they never agreed to.
    funded_by TEXT NOT NULL DEFAULT 'platform',

    is_active   BOOLEAN NOT NULL DEFAULT true,
    created_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT coupons_discount_type_check CHECK (
        discount_type IN ('percentage', 'flat', 'free_delivery')
    ),
    CONSTRAINT coupons_discount_value_check CHECK (discount_value >= 0),
    -- A percentage over 100% would produce a negative total.
    CONSTRAINT coupons_percentage_bound_check CHECK (
        discount_type <> 'percentage' OR discount_value <= 10000
    ),
    -- A percentage discount without a cap is unbounded exposure on a large order.
    CONSTRAINT coupons_percentage_needs_cap_check CHECK (
        discount_type <> 'percentage' OR max_discount_minor IS NOT NULL
    ),
    CONSTRAINT coupons_max_redemptions_check CHECK (
        max_redemptions IS NULL OR max_redemptions >= 0
    ),
    CONSTRAINT coupons_per_user_check CHECK (max_redemptions_per_user >= 1),
    CONSTRAINT coupons_validity_window_check CHECK (
        valid_until IS NULL OR valid_until > valid_from
    ),
    CONSTRAINT coupons_funded_by_check CHECK (funded_by IN ('platform', 'merchant', 'shared'))
);

CREATE UNIQUE INDEX coupons_code_key ON coupons (upper(code));

-- The customer-facing "offers for you" list.
CREATE INDEX coupons_active_idx ON coupons (valid_until)
    WHERE is_active;

-- ---------------------------------------------------------------------------
-- coupon_redemptions
--
-- The source of truth for usage. Append-only in spirit: a released redemption is
-- marked, not deleted, so "this customer used and then cancelled" stays visible
-- to fraud review.
-- ---------------------------------------------------------------------------

CREATE TABLE coupon_redemptions (
    id         UUID PRIMARY KEY,
    coupon_id  UUID NOT NULL REFERENCES coupons (id) ON DELETE RESTRICT,
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    order_id   UUID NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    -- The discount actually applied, snapshotted. Recomputing it later from the
    -- coupon would give a different answer once the coupon is edited.
    discount_minor BIGINT NOT NULL,

    -- Released means the order was cancelled and the use was returned. Kept
    -- rather than deleted so repeated order-then-cancel is detectable.
    released_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT coupon_redemptions_discount_check CHECK (discount_minor >= 0)
);

-- One redemption per order, full stop. Makes Redeem idempotent under a retried
-- checkout.
CREATE UNIQUE INDEX coupon_redemptions_order_key ON coupon_redemptions (order_id);

-- THE per-user cap, for the common max_redemptions_per_user = 1 case. A partial
-- unique index on live redemptions means a released one frees the slot, which is
-- the correct behaviour after a cancellation.
--
-- Caps above 1 cannot be expressed as a unique index and are checked in the
-- application under the same SERIALIZABLE transaction as the global cap.
CREATE INDEX coupon_redemptions_user_coupon_idx
    ON coupon_redemptions (coupon_id, user_id)
    WHERE released_at IS NULL;

CREATE INDEX coupon_redemptions_coupon_idx ON coupon_redemptions (coupon_id)
    WHERE released_at IS NULL;

-- ---------------------------------------------------------------------------
-- Seed one launch coupon, so the flow is testable immediately.
-- ---------------------------------------------------------------------------

INSERT INTO coupons (
    id, code, description, discount_type, discount_value, max_discount_minor,
    min_order_minor, first_order_only, max_redemptions_per_user, funded_by
) VALUES (
    gen_random_uuid(), 'WELCOME50',
    '50% off your first order, up to Rs100',
    'percentage', 5000, 10000,
    19900, true, 1, 'platform'
) ON CONFLICT DO NOTHING;
