-- 0004: inventory -- ingredients, recipes, stock movements, reservations.
--
-- This is the module the admin dashboard's "inventory management" requirement
-- maps onto, and it is the one with the hardest concurrency problem in the system:
-- two customers buying the last portion at the same instant must not both
-- succeed.
--
-- The design: stock is never a single mutable number. It is a ledger of movements
-- plus a cached balance, and reservations are rows. That means the truth is always
-- reconstructible, and "why did we run out" is answerable.

-- ---------------------------------------------------------------------------
-- ingredients
-- ---------------------------------------------------------------------------

CREATE TABLE ingredients (
    id              UUID PRIMARY KEY,
    kitchen_id      UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    sku             TEXT NOT NULL DEFAULT '',
    -- The unit every quantity for this ingredient is expressed in. Stored once
    -- here rather than per movement, so a recipe in grams can never be applied
    -- against a balance in kilograms.
    unit            TEXT NOT NULL,

    -- NUMERIC, not BIGINT: unlike money, ingredient quantities are genuinely
    -- fractional (0.25 kg) and there is no natural smallest unit across flour,
    -- milk and eggs. 3 decimal places = gram precision on a kg balance.
    on_hand         NUMERIC(14, 3) NOT NULL DEFAULT 0,
    -- Sum of open reservations. Cached here so an availability check is one row
    -- read instead of an aggregate over the reservations table.
    reserved        NUMERIC(14, 3) NOT NULL DEFAULT 0,

    reorder_level   NUMERIC(14, 3) NOT NULL DEFAULT 0,
    reorder_qty     NUMERIC(14, 3) NOT NULL DEFAULT 0,
    -- Weighted-average cost per unit, in paise. Needed for food-cost reporting,
    -- which is the number that decides whether a dish is actually profitable.
    unit_cost_minor BIGINT NOT NULL DEFAULT 0,

    supplier_name   TEXT NOT NULL DEFAULT '',
    -- Perishables need a shelf-life so the wastage report can flag what expired
    -- rather than what was eaten.
    shelf_life_days INT,
    is_active       BOOLEAN NOT NULL DEFAULT true,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ingredients_unit_check CHECK (
        unit IN ('g', 'kg', 'ml', 'l', 'piece', 'packet', 'dozen')
    ),
    -- on_hand may not go negative: you cannot have less than zero flour. Any code
    -- path that would breach this is a bug, and failing the write is far better
    -- than silently carrying a negative balance into a food-cost report.
    CONSTRAINT ingredients_on_hand_check CHECK (on_hand >= 0),
    CONSTRAINT ingredients_reserved_check CHECK (reserved >= 0),
    -- Reserved can never exceed what physically exists.
    CONSTRAINT ingredients_reserved_bound_check CHECK (reserved <= on_hand)
);

CREATE UNIQUE INDEX ingredients_kitchen_sku_key ON ingredients (kitchen_id, sku)
    WHERE sku <> '';
CREATE INDEX ingredients_kitchen_idx ON ingredients (kitchen_id) WHERE is_active;

-- The "what do I need to buy" dashboard query. Expressing the comparison in the
-- index predicate means the low-stock list is an index-only scan.
CREATE INDEX ingredients_low_stock_idx ON ingredients (kitchen_id, name)
    WHERE is_active AND on_hand <= reorder_level;

-- ---------------------------------------------------------------------------
-- recipes -- the bill of materials linking a dish to its ingredients
--
-- This is what makes automatic sold-out detection possible: knowing a biryani
-- needs 300g rice means the system can compute how many biryanis are left,
-- instead of waiting for staff to notice and toggle a flag.
-- ---------------------------------------------------------------------------

CREATE TABLE recipes (
    id            UUID PRIMARY KEY,
    menu_item_id  UUID NOT NULL REFERENCES menu_items (id) ON DELETE CASCADE,
    ingredient_id UUID NOT NULL REFERENCES ingredients (id) ON DELETE RESTRICT,
    -- Quantity consumed per single serving of the dish, in the ingredient's unit.
    quantity      NUMERIC(14, 3) NOT NULL,
    -- Expected loss in basis points (peeling, trimming, spillage). Ignoring it
    -- makes every food-cost figure optimistic and the reorder point too low.
    wastage_bps   INT NOT NULL DEFAULT 0,
    -- An optional ingredient (a garnish) must not make the dish unorderable when
    -- it runs out.
    is_optional   BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT recipes_quantity_check CHECK (quantity > 0),
    CONSTRAINT recipes_wastage_check CHECK (wastage_bps BETWEEN 0 AND 10000)
);

-- One row per (dish, ingredient): listing the same ingredient twice for a dish
-- would double-count consumption.
CREATE UNIQUE INDEX recipes_item_ingredient_key ON recipes (menu_item_id, ingredient_id);
CREATE INDEX recipes_ingredient_idx ON recipes (ingredient_id);

-- ---------------------------------------------------------------------------
-- stock_movements -- the append-only ledger
--
-- Every change to on_hand writes a row here. The balance on `ingredients` is a
-- cache of this ledger's sum. Without the ledger, "we should have 4kg but the
-- shelf has 1kg" is unanswerable; with it, every gram is attributable.
-- ---------------------------------------------------------------------------

CREATE TABLE stock_movements (
    id            UUID PRIMARY KEY,
    ingredient_id UUID NOT NULL REFERENCES ingredients (id) ON DELETE CASCADE,
    kitchen_id    UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    -- Signed: positive for receipts, negative for consumption and wastage. A
    -- single signed column means the balance is a plain SUM, with no CASE over
    -- movement types that a new type could silently fall outside of.
    quantity      NUMERIC(14, 3) NOT NULL,
    movement_type TEXT NOT NULL,
    -- Balance immediately after this movement, so the ledger can be audited
    -- without replaying it from the beginning.
    balance_after NUMERIC(14, 3) NOT NULL,

    -- What caused it: an order, a purchase, a stock count. Not a foreign key,
    -- because the referent is polymorphic and a FK per type would mean six
    -- nullable columns.
    reference_type TEXT NOT NULL DEFAULT '',
    reference_id   UUID,

    unit_cost_minor BIGINT NOT NULL DEFAULT 0,
    note          TEXT NOT NULL DEFAULT '',
    actor_id      UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT stock_movements_type_check CHECK (movement_type IN (
        'purchase',      -- stock received from a supplier
        'consumption',   -- used to cook an order
        'wastage',       -- spoiled, expired, dropped
        'adjustment',    -- physical stock count correction
        'return',        -- returned to supplier
        'transfer_in',   -- moved from another kitchen
        'transfer_out'
    )),
    -- A zero-quantity movement is meaningless and would pollute the ledger.
    CONSTRAINT stock_movements_quantity_check CHECK (quantity <> 0)
);

CREATE INDEX stock_movements_ingredient_idx ON stock_movements (ingredient_id, created_at DESC);
CREATE INDEX stock_movements_kitchen_created_idx ON stock_movements (kitchen_id, created_at DESC);
CREATE INDEX stock_movements_reference_idx ON stock_movements (reference_type, reference_id)
    WHERE reference_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- stock_reservations -- the two-phase hold during checkout
--
-- Phase 1: reserve at checkout (increments ingredients.reserved).
-- Phase 2: consume when cooking starts (writes a movement, decrements both), or
--          release on cancellation / expiry.
--
-- Without this, stock is only decremented when the kitchen starts cooking, and
-- everything ordered in between oversells.
-- ---------------------------------------------------------------------------

CREATE TABLE stock_reservations (
    id            UUID PRIMARY KEY,
    order_id      UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    ingredient_id UUID NOT NULL REFERENCES ingredients (id) ON DELETE RESTRICT,
    kitchen_id    UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    quantity      NUMERIC(14, 3) NOT NULL,
    status        TEXT NOT NULL DEFAULT 'held',
    -- A reservation must expire. A customer who opens checkout and abandons it
    -- would otherwise hold the last portion forever; the reaper worker releases
    -- anything past this.
    expires_at    TIMESTAMPTZ NOT NULL,
    released_at   TIMESTAMPTZ,
    consumed_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT stock_reservations_status_check CHECK (
        status IN ('held', 'consumed', 'released', 'expired')
    ),
    CONSTRAINT stock_reservations_quantity_check CHECK (quantity > 0)
);

-- One held reservation per (order, ingredient): a retried checkout must not
-- double-hold. This unique index is what makes Reserve idempotent.
CREATE UNIQUE INDEX stock_reservations_order_ingredient_held_key
    ON stock_reservations (order_id, ingredient_id)
    WHERE status = 'held';

CREATE INDEX stock_reservations_order_idx ON stock_reservations (order_id);

-- The reaper's query: held reservations past their expiry.
CREATE INDEX stock_reservations_expiring_idx ON stock_reservations (expires_at)
    WHERE status = 'held';

-- ---------------------------------------------------------------------------
-- purchase_orders -- procurement
-- ---------------------------------------------------------------------------

CREATE TABLE purchase_orders (
    id              UUID PRIMARY KEY,
    kitchen_id      UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    number          TEXT NOT NULL,
    supplier_name   TEXT NOT NULL,
    supplier_phone  TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'draft',
    total_minor     BIGINT NOT NULL DEFAULT 0,
    expected_at     TIMESTAMPTZ,
    received_at     TIMESTAMPTZ,
    note            TEXT NOT NULL DEFAULT '',
    created_by      UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT purchase_orders_status_check CHECK (
        status IN ('draft', 'sent', 'partially_received', 'received', 'cancelled')
    ),
    CONSTRAINT purchase_orders_total_check CHECK (total_minor >= 0)
);

CREATE UNIQUE INDEX purchase_orders_number_key ON purchase_orders (number);
CREATE INDEX purchase_orders_kitchen_idx ON purchase_orders (kitchen_id, created_at DESC);

CREATE TABLE purchase_order_lines (
    id                UUID PRIMARY KEY,
    purchase_order_id UUID NOT NULL REFERENCES purchase_orders (id) ON DELETE CASCADE,
    ingredient_id     UUID NOT NULL REFERENCES ingredients (id) ON DELETE RESTRICT,
    quantity_ordered  NUMERIC(14, 3) NOT NULL,
    -- Tracked separately because short deliveries are the norm, not the exception,
    -- and the difference is what drives the supplier reliability report.
    quantity_received NUMERIC(14, 3) NOT NULL DEFAULT 0,
    unit_cost_minor   BIGINT NOT NULL DEFAULT 0,

    CONSTRAINT po_lines_quantity_check CHECK (quantity_ordered > 0),
    CONSTRAINT po_lines_received_check CHECK (quantity_received >= 0)
);

CREATE INDEX po_lines_po_idx ON purchase_order_lines (purchase_order_id);
