-- 0002: catalog -- kitchens, menu structure, and pricing.

-- ---------------------------------------------------------------------------
-- kitchens
-- ---------------------------------------------------------------------------

CREATE TABLE kitchens (
    id                    UUID PRIMARY KEY,
    name                  TEXT NOT NULL,
    slug                  TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    -- Cuisine tags drive both filtering and the "Biryani near you" browse rails.
    cuisines              TEXT[] NOT NULL DEFAULT '{}',
    phone                 TEXT NOT NULL DEFAULT '',
    email                 TEXT NOT NULL DEFAULT '',

    address_line1         TEXT NOT NULL DEFAULT '',
    city                  TEXT NOT NULL DEFAULT '',
    pincode               TEXT NOT NULL DEFAULT '',
    latitude              NUMERIC(10, 7) NOT NULL,
    longitude             NUMERIC(10, 7) NOT NULL,
    -- Straight-line radius in metres. A crude proxy for road distance, but it is
    -- one arithmetic comparison instead of a paid routing API call on every menu
    -- view. Swap for isochrones once volume justifies it.
    delivery_radius_m     INT NOT NULL DEFAULT 5000,

    -- is_accepting_orders is the kill switch: staff flip it when the kitchen is
    -- swamped. Separate from is_active (a business-level on/off) and from opening
    -- hours, because all three suppress ordering for different reasons and the
    -- customer needs a different message for each.
    is_active             BOOLEAN NOT NULL DEFAULT true,
    is_accepting_orders   BOOLEAN NOT NULL DEFAULT true,
    pause_reason          TEXT NOT NULL DEFAULT '',

    -- Commission the platform takes, in basis points. Stored per kitchen because
    -- it is negotiated per kitchen.
    commission_bps        INT NOT NULL DEFAULT 0,
    packaging_fee_minor   BIGINT NOT NULL DEFAULT 0,
    min_order_minor       BIGINT NOT NULL DEFAULT 0,
    avg_prep_minutes      INT NOT NULL DEFAULT 20,

    -- Denormalised rating aggregates. Recomputed by the review module rather than
    -- averaged live: a menu listing must not run AVG() over every review.
    rating_average        NUMERIC(3, 2) NOT NULL DEFAULT 0,
    rating_count          INT NOT NULL DEFAULT 0,

    fssai_license         TEXT NOT NULL DEFAULT '',
    gstin                 TEXT NOT NULL DEFAULT '',

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT kitchens_latitude_check CHECK (latitude BETWEEN -90 AND 90),
    CONSTRAINT kitchens_longitude_check CHECK (longitude BETWEEN -180 AND 180),
    CONSTRAINT kitchens_radius_check CHECK (delivery_radius_m BETWEEN 100 AND 50000),
    CONSTRAINT kitchens_commission_check CHECK (commission_bps BETWEEN 0 AND 10000),
    CONSTRAINT kitchens_money_nonneg_check CHECK (
        packaging_fee_minor >= 0 AND min_order_minor >= 0
    ),
    CONSTRAINT kitchens_rating_check CHECK (rating_average BETWEEN 0 AND 5)
);

CREATE UNIQUE INDEX kitchens_slug_key ON kitchens (slug);

-- Bounding-box prefilter for "kitchens near me". This is not a true radius
-- search -- the app narrows by box on the index, then filters exactly in Go.
-- Upgrade to PostGIS + GiST when the kitchen count makes the box too coarse.
CREATE INDEX kitchens_location_idx ON kitchens (latitude, longitude)
    WHERE is_active;

CREATE INDEX kitchens_cuisines_idx ON kitchens USING GIN (cuisines);

-- ---------------------------------------------------------------------------
-- kitchen_hours -- opening schedule
--
-- One row per day per kitchen, allowing split shifts (lunch and dinner) as two
-- rows for the same weekday. A single open/close pair per day could not express
-- a kitchen that shuts between 15:00 and 19:00.
-- ---------------------------------------------------------------------------

CREATE TABLE kitchen_hours (
    id          UUID PRIMARY KEY,
    kitchen_id  UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    -- 0 = Sunday, matching Go's time.Weekday.
    day_of_week SMALLINT NOT NULL,
    -- TIME, not TIMESTAMPTZ: this is a wall-clock rule ("opens at 11:00"), not a
    -- moment in time. Interpreted in Asia/Kolkata.
    opens_at    TIME NOT NULL,
    closes_at   TIME NOT NULL,

    CONSTRAINT kitchen_hours_day_check CHECK (day_of_week BETWEEN 0 AND 6),
    -- closes_at > opens_at forbids a shift crossing midnight; such a shift must
    -- be stored as two rows so every query stays a simple range check.
    CONSTRAINT kitchen_hours_range_check CHECK (closes_at > opens_at)
);

CREATE INDEX kitchen_hours_kitchen_day_idx ON kitchen_hours (kitchen_id, day_of_week);

-- ---------------------------------------------------------------------------
-- menu_categories
-- ---------------------------------------------------------------------------

CREATE TABLE menu_categories (
    id            UUID PRIMARY KEY,
    kitchen_id    UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    -- Explicit ordering: the kitchen decides that Starters come before Biryani,
    -- and alphabetical would be wrong.
    display_order INT NOT NULL DEFAULT 0,
    is_active     BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX menu_categories_kitchen_idx ON menu_categories (kitchen_id, display_order)
    WHERE is_active;

-- ---------------------------------------------------------------------------
-- menu_items
-- ---------------------------------------------------------------------------

CREATE TABLE menu_items (
    id                UUID PRIMARY KEY,
    kitchen_id        UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    category_id       UUID REFERENCES menu_categories (id) ON DELETE SET NULL,
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    image_url         TEXT NOT NULL DEFAULT '',

    price_minor       BIGINT NOT NULL,
    -- Struck-through "was" price for showing a discount. NULL means no offer.
    compare_price_minor BIGINT,
    -- Per-item GST in basis points. Item-level because rates genuinely differ:
    -- 5% on prepared food, 18% on packaged drinks. One blended rate on the
    -- order subtotal would be wrong and would fail a GST audit.
    tax_bps           INT NOT NULL DEFAULT 500,
    packaging_fee_minor BIGINT NOT NULL DEFAULT 0,

    -- Veg/non-veg is a legal labelling requirement in India, not a preference,
    -- and drives the mandatory green/red mark in the UI.
    food_type         TEXT NOT NULL DEFAULT 'veg',
    spice_level       SMALLINT NOT NULL DEFAULT 0,
    allergens         TEXT[] NOT NULL DEFAULT '{}',
    -- Free-text nutrition, since customers ask and the format varies by dish.
    calories          INT,
    serves            SMALLINT NOT NULL DEFAULT 1,

    -- is_available is the manual toggle; is_sold_out is set automatically by the
    -- inventory module when ingredients run out. Two flags because the kitchen
    -- must be able to see which one hid the dish.
    is_available      BOOLEAN NOT NULL DEFAULT true,
    is_sold_out       BOOLEAN NOT NULL DEFAULT false,
    prep_minutes      INT NOT NULL DEFAULT 15,
    display_order     INT NOT NULL DEFAULT 0,
    is_recommended    BOOLEAN NOT NULL DEFAULT false,

    rating_average    NUMERIC(3, 2) NOT NULL DEFAULT 0,
    rating_count      INT NOT NULL DEFAULT 0,
    -- Denormalised popularity, refreshed by the analytics module. Sorting a menu
    -- by "most ordered" must not aggregate order_lines on every page view.
    order_count       INT NOT NULL DEFAULT 0,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT menu_items_price_check CHECK (price_minor > 0),
    -- A compare price below the real price would render as a price increase
    -- dressed up as a discount.
    CONSTRAINT menu_items_compare_price_check CHECK (
        compare_price_minor IS NULL OR compare_price_minor > price_minor
    ),
    CONSTRAINT menu_items_tax_check CHECK (tax_bps BETWEEN 0 AND 10000),
    CONSTRAINT menu_items_food_type_check CHECK (
        food_type IN ('veg', 'non_veg', 'egg', 'vegan')
    ),
    CONSTRAINT menu_items_spice_check CHECK (spice_level BETWEEN 0 AND 3)
);

-- The customer-facing menu query: one kitchen's orderable items, in display
-- order. Partial so hidden items do not bloat the index.
CREATE INDEX menu_items_kitchen_available_idx
    ON menu_items (kitchen_id, display_order)
    WHERE is_available AND NOT is_sold_out;

CREATE INDEX menu_items_category_idx ON menu_items (category_id, display_order);
CREATE INDEX menu_items_name_trgm_idx ON menu_items USING GIN (name gin_trgm_ops);
CREATE INDEX menu_items_popularity_idx ON menu_items (kitchen_id, order_count DESC);

-- ---------------------------------------------------------------------------
-- modifier_groups / modifiers -- "extra cheese", "make it a large"
--
-- Two tables rather than JSON on the item, because the same group ("Choose a
-- size") is reused across many items and a modifier's price must be joinable and
-- reportable.
-- ---------------------------------------------------------------------------

CREATE TABLE modifier_groups (
    id            UUID PRIMARY KEY,
    kitchen_id    UUID NOT NULL REFERENCES kitchens (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    -- min/max selections express every real rule: required single-choice is
    -- (1,1), optional add-ons is (0,n).
    min_select    SMALLINT NOT NULL DEFAULT 0,
    max_select    SMALLINT NOT NULL DEFAULT 1,
    display_order INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT modifier_groups_select_check CHECK (
        min_select >= 0 AND max_select >= min_select
    )
);

CREATE TABLE modifiers (
    id            UUID PRIMARY KEY,
    group_id      UUID NOT NULL REFERENCES modifier_groups (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    -- Can be 0 (a free choice like "no onions") but never negative.
    price_minor   BIGINT NOT NULL DEFAULT 0,
    is_available  BOOLEAN NOT NULL DEFAULT true,
    display_order INT NOT NULL DEFAULT 0,

    CONSTRAINT modifiers_price_check CHECK (price_minor >= 0)
);

CREATE INDEX modifiers_group_idx ON modifiers (group_id, display_order);

-- Join table: which groups apply to which item.
CREATE TABLE menu_item_modifier_groups (
    menu_item_id UUID NOT NULL REFERENCES menu_items (id) ON DELETE CASCADE,
    group_id     UUID NOT NULL REFERENCES modifier_groups (id) ON DELETE CASCADE,
    PRIMARY KEY (menu_item_id, group_id)
);
