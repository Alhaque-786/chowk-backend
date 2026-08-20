-- 0006: marketplace -- merchants, outlets, commission and settlement.
--
-- Turns a single-tenant cloud kitchen into a platform that onboards third-party
-- restaurants at a lower commission than Zomato/Swiggy.
--
-- The model:
--
--   merchant  -- the business: has a contract, a PAN, a bank account, gets paid
--     └── outlet (= a `kitchens` row) -- one physical location: own menu, own
--                                       stock, own hours
--
-- Two levels, not one, because a brand with three locations needs ONE settlement
-- account and THREE independent inventories. Collapsing them breaks one or the
-- other.
--
-- The money model is the part that actually decides whether this business works,
-- and it is built as a double-entry-style ledger rather than a running balance
-- column. A balance you can UPDATE is a balance you can silently corrupt; a
-- ledger of immutable entries can always be re-derived and reconciled against
-- the payment gateway.

-- ---------------------------------------------------------------------------
-- merchants
-- ---------------------------------------------------------------------------

CREATE TABLE merchants (
    id                  UUID PRIMARY KEY,
    legal_name          TEXT NOT NULL,
    -- The customer-facing brand, which often differs from the registered entity
    -- ("Biryani House" vs "Khan Foods Pvt Ltd").
    display_name        TEXT NOT NULL,
    slug                TEXT NOT NULL,

    contact_name         TEXT NOT NULL DEFAULT '',
    contact_phone        TEXT NOT NULL,
    contact_email        TEXT NOT NULL DEFAULT '',

    -- Onboarding lifecycle. A merchant must reach 'active' before their outlets
    -- can accept orders: taking money on behalf of an unverified business is a
    -- compliance problem and a fraud vector.
    onboarding_status   TEXT NOT NULL DEFAULT 'draft',
    rejection_reason    TEXT NOT NULL DEFAULT '',

    -- --- KYC / compliance. Legally required to operate a food marketplace in
    -- India, and required by the payment gateway before it will settle to a
    -- third party's account. ---
    gstin               TEXT,
    pan                 TEXT,
    fssai_license       TEXT,
    fssai_expiry        DATE,
    -- Document URLs live in object storage; only references are stored here.
    kyc_documents       JSONB NOT NULL DEFAULT '[]',
    verified_at         TIMESTAMPTZ,
    verified_by         UUID REFERENCES users (id) ON DELETE SET NULL,

    -- --- Commercials ---
    -- The core competitive lever: Zomato/Swiggy charge roughly 18-30%. Stored per
    -- merchant in basis points because it is negotiated per merchant, and a
    -- platform-wide constant would make that impossible.
    commission_bps      INT NOT NULL DEFAULT 1200,
    -- Some merchants negotiate a flat fee per order instead of, or alongside, a
    -- percentage. Supporting both from the start avoids a schema change during a
    -- negotiation.
    commission_flat_minor BIGINT NOT NULL DEFAULT 0,
    -- Payment gateway fees are a real cost; whether the platform or the merchant
    -- absorbs them is a contract term.
    absorbs_gateway_fee BOOLEAN NOT NULL DEFAULT false,

    -- Settlement cadence. Faster payouts are a genuine selling point against the
    -- incumbents, whose 7-14 day cycles are a common merchant complaint.
    settlement_cycle    TEXT NOT NULL DEFAULT 'weekly',
    -- Amount withheld against future refunds and chargebacks.
    security_deposit_minor BIGINT NOT NULL DEFAULT 0,

    -- --- Payout destination ---
    -- Only the last four digits are stored in the clear; the gateway holds the
    -- real account behind a token, so a database leak cannot redirect payouts.
    bank_account_last4  TEXT NOT NULL DEFAULT '',
    bank_ifsc           TEXT NOT NULL DEFAULT '',
    bank_account_name   TEXT NOT NULL DEFAULT '',
    payout_provider_ref TEXT,

    is_suspended        BOOLEAN NOT NULL DEFAULT false,
    suspension_reason   TEXT NOT NULL DEFAULT '',

    version             INT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT merchants_onboarding_status_check CHECK (onboarding_status IN (
        'draft',            -- partner started signup
        'documents_pending',-- awaiting KYC uploads
        'under_review',     -- our team is verifying
        'active',           -- live, can take orders
        'rejected',
        'churned'
    )),
    CONSTRAINT merchants_commission_check CHECK (commission_bps BETWEEN 0 AND 5000),
    CONSTRAINT merchants_settlement_cycle_check CHECK (
        settlement_cycle IN ('daily', 'weekly', 'biweekly', 'monthly')
    ),
    -- Indian GSTIN is exactly 15 characters; a malformed one fails at settlement
    -- time, which is the worst moment to discover it.
    CONSTRAINT merchants_gstin_check CHECK (gstin IS NULL OR length(gstin) = 15),
    CONSTRAINT merchants_pan_check CHECK (pan IS NULL OR pan ~ '^[A-Z]{5}[0-9]{4}[A-Z]$'),
    -- A merchant cannot go live without the licence that legally permits them to
    -- sell food. Enforced here so no code path can skip it.
    CONSTRAINT merchants_active_requires_kyc_check CHECK (
        onboarding_status <> 'active'
        OR (fssai_license IS NOT NULL AND fssai_license <> '' AND verified_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX merchants_slug_key ON merchants (slug);
CREATE UNIQUE INDEX merchants_gstin_key ON merchants (gstin) WHERE gstin IS NOT NULL;
CREATE UNIQUE INDEX merchants_pan_key ON merchants (pan) WHERE pan IS NOT NULL;
CREATE INDEX merchants_onboarding_idx ON merchants (onboarding_status, created_at DESC);
-- Drives the compliance alert for licences about to lapse. An expired FSSAI
-- means the outlet must stop selling immediately.
CREATE INDEX merchants_fssai_expiry_idx ON merchants (fssai_expiry)
    WHERE onboarding_status = 'active' AND fssai_expiry IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Link kitchens (outlets) to merchants.
--
-- Nullable during migration, then backfilled: existing kitchens belong to our own
-- first-party merchant. Making it NOT NULL immediately would fail against
-- existing rows.
-- ---------------------------------------------------------------------------

ALTER TABLE kitchens
    ADD COLUMN merchant_id UUID REFERENCES merchants (id) ON DELETE RESTRICT,
    -- Distinguishes our own kitchens from partner ones. First-party outlets are
    -- exempt from commission and settlement, and reporting must be able to
    -- separate "our margin" from "platform take rate".
    ADD COLUMN is_first_party BOOLEAN NOT NULL DEFAULT true;

-- Seed our own merchant and attach existing kitchens to it, so the tenant guard
-- has something valid to scope against from the moment it is switched on.
INSERT INTO merchants (
    id, legal_name, display_name, slug, contact_phone,
    onboarding_status, commission_bps, verified_at, fssai_license
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    'First Party Cloud Kitchen', 'Our Kitchen', 'first-party', '+910000000000',
    'active', 0, now(), 'FIRST-PARTY'
) ON CONFLICT (id) DO NOTHING;

UPDATE kitchens
SET merchant_id = '00000000-0000-0000-0000-000000000001'
WHERE merchant_id IS NULL;

-- Now that every row has an owner, make it mandatory. An outlet with no merchant
-- is unscopeable data, which is exactly the leak the tenant guard prevents.
ALTER TABLE kitchens ALTER COLUMN merchant_id SET NOT NULL;

-- The tenant filter's index. Every merchant-facing query starts here, so it must
-- be an index scan.
CREATE INDEX kitchens_merchant_idx ON kitchens (merchant_id) WHERE is_active;

-- ---------------------------------------------------------------------------
-- merchant_users -- who may act on behalf of a merchant
--
-- A join table rather than a column on users, because one person can legitimately
-- manage two merchants (a consultant, or an owner with two brands), and an owner
-- of a three-outlet business needs a different scope from a cook at one of them.
-- ---------------------------------------------------------------------------

CREATE TABLE merchant_users (
    id          UUID PRIMARY KEY,
    merchant_id UUID NOT NULL REFERENCES merchants (id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role        TEXT NOT NULL,
    -- NULL means "all outlets of this merchant" (an owner). A specific outlet
    -- pins a staff member to one location.
    outlet_id   UUID REFERENCES kitchens (id) ON DELETE CASCADE,
    invited_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    accepted_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT merchant_users_role_check CHECK (
        role IN ('owner', 'manager', 'staff', 'accountant')
    )
);

-- A surrogate primary key plus these two unique indexes, rather than a composite
-- PK. Postgres allows expressions in a unique INDEX but not in a PRIMARY KEY
-- constraint, and NULL outlet_id (meaning "all outlets") must be treated as a
-- distinct value -- two NULLs are never equal, so a plain unique constraint would
-- happily allow the same owner row twice.
CREATE UNIQUE INDEX merchant_users_outlet_key
    ON merchant_users (merchant_id, user_id, outlet_id)
    WHERE outlet_id IS NOT NULL;

CREATE UNIQUE INDEX merchant_users_all_outlets_key
    ON merchant_users (merchant_id, user_id)
    WHERE outlet_id IS NULL;

CREATE INDEX merchant_users_user_idx ON merchant_users (user_id);
CREATE INDEX merchant_users_merchant_idx ON merchant_users (merchant_id);

-- ---------------------------------------------------------------------------
-- ledger_entries -- immutable money movement
--
-- The heart of the marketplace. For every order the platform must know exactly:
-- what the customer paid, what the gateway took, what commission we earned, what
-- we owe the merchant, and what has actually been paid out.
--
-- Append-only, double-entry-inspired. Deliberately NOT a `merchant_balance`
-- column: a balance you can UPDATE is a balance a bug can silently corrupt, with
-- no way to discover when or why. Here, the balance is SUM(amount_minor) and can
-- always be recomputed and reconciled against the gateway's own statement.
-- ---------------------------------------------------------------------------

CREATE TABLE ledger_entries (
    id             UUID PRIMARY KEY,
    merchant_id    UUID NOT NULL REFERENCES merchants (id) ON DELETE RESTRICT,
    outlet_id      UUID REFERENCES kitchens (id) ON DELETE SET NULL,
    order_id       UUID REFERENCES orders (id) ON DELETE RESTRICT,

    entry_type     TEXT NOT NULL,
    -- SIGNED, from the MERCHANT's perspective:
    --   positive = platform owes the merchant more (order revenue)
    --   negative = merchant owes the platform (commission, refunds, payouts made)
    -- A single signed column means the balance is a plain SUM with no CASE
    -- statement that a newly added entry_type could silently fall outside of.
    amount_minor   BIGINT NOT NULL,
    currency       TEXT NOT NULL DEFAULT 'INR',

    -- The inputs to the calculation, retained so a merchant dispute can be
    -- answered with arithmetic instead of an apology.
    gross_minor    BIGINT,
    commission_bps INT,

    description    TEXT NOT NULL DEFAULT '',
    -- Links to a settlement once this entry has been paid out. NULL means
    -- unsettled -- which is exactly the query the payout job runs.
    settlement_id  UUID,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_entry_type_check CHECK (entry_type IN (
        'order_revenue',    -- + gross owed to merchant for a delivered order
        'commission',       -- - our take rate
        'gateway_fee',      -- - payment processing cost
        'refund',           -- - customer refunded, claw back merchant revenue
        'chargeback',       -- - disputed payment lost
        'adjustment',       -- +/- manual correction, always with a description
        'payout',           -- - money actually transferred to the merchant
        'penalty',          -- - SLA breach, e.g. repeated late orders
        'incentive'         -- + promotional support the platform funds
    )),
    -- A zero-amount entry carries no information and only pollutes the ledger.
    CONSTRAINT ledger_amount_nonzero_check CHECK (amount_minor <> 0),
    -- A manual adjustment without an explanation is unauditable.
    CONSTRAINT ledger_adjustment_needs_description_check CHECK (
        entry_type <> 'adjustment' OR description <> ''
    )
);

-- The settlement job's query: unsettled entries for a merchant.
CREATE INDEX ledger_unsettled_idx ON ledger_entries (merchant_id, created_at)
    WHERE settlement_id IS NULL;
CREATE INDEX ledger_merchant_created_idx ON ledger_entries (merchant_id, created_at DESC);
CREATE INDEX ledger_order_idx ON ledger_entries (order_id) WHERE order_id IS NOT NULL;
CREATE INDEX ledger_settlement_idx ON ledger_entries (settlement_id) WHERE settlement_id IS NOT NULL;

-- One revenue and one commission entry per order. This unique index is what makes
-- the ledger idempotent: a retried or redelivered order.delivered event cannot
-- double-credit a merchant.
CREATE UNIQUE INDEX ledger_order_entry_type_key ON ledger_entries (order_id, entry_type)
    WHERE order_id IS NOT NULL
      AND entry_type IN ('order_revenue', 'commission', 'gateway_fee');

-- ---------------------------------------------------------------------------
-- settlements -- a batch payout to one merchant
-- ---------------------------------------------------------------------------

CREATE TABLE settlements (
    id              UUID PRIMARY KEY,
    merchant_id     UUID NOT NULL REFERENCES merchants (id) ON DELETE RESTRICT,
    reference       TEXT NOT NULL,

    period_start    TIMESTAMPTZ NOT NULL,
    period_end      TIMESTAMPTZ NOT NULL,

    -- Stored breakdown, not derived at read time: a settlement statement sent to
    -- a merchant must be reproducible byte-for-byte years later, even after
    -- commission terms change.
    gross_minor      BIGINT NOT NULL DEFAULT 0,
    commission_minor BIGINT NOT NULL DEFAULT 0,
    gateway_fee_minor BIGINT NOT NULL DEFAULT 0,
    refunds_minor    BIGINT NOT NULL DEFAULT 0,
    adjustments_minor BIGINT NOT NULL DEFAULT 0,
    -- TDS: Indian tax deducted at source on marketplace payouts. Not optional,
    -- and getting it wrong is a tax liability for the platform.
    tds_minor        BIGINT NOT NULL DEFAULT 0,
    net_payable_minor BIGINT NOT NULL DEFAULT 0,
    currency         TEXT NOT NULL DEFAULT 'INR',

    status          TEXT NOT NULL DEFAULT 'draft',
    -- The payout provider's transfer id, for reconciliation against the bank.
    payout_ref      TEXT,
    payout_utr      TEXT,
    failure_reason  TEXT NOT NULL DEFAULT '',

    initiated_at    TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT settlements_status_check CHECK (status IN (
        'draft', 'pending_approval', 'approved', 'processing', 'paid', 'failed', 'cancelled'
    )),
    CONSTRAINT settlements_period_check CHECK (period_end > period_start),
    -- The net must equal the breakdown. Catches an arithmetic bug at write time
    -- rather than in a merchant's angry email a week later.
    CONSTRAINT settlements_arithmetic_check CHECK (
        net_payable_minor = gross_minor - commission_minor - gateway_fee_minor
                            - refunds_minor - tds_minor + adjustments_minor
    )
);

CREATE UNIQUE INDEX settlements_reference_key ON settlements (reference);
CREATE UNIQUE INDEX settlements_payout_ref_key ON settlements (payout_ref)
    WHERE payout_ref IS NOT NULL;
CREATE INDEX settlements_merchant_idx ON settlements (merchant_id, period_end DESC);
-- Money in flight needs monitoring: a settlement stuck in 'processing' is a
-- merchant who has not been paid.
CREATE INDEX settlements_in_flight_idx ON settlements (status, initiated_at)
    WHERE status IN ('approved', 'processing');

ALTER TABLE ledger_entries
    ADD CONSTRAINT ledger_settlement_fkey
    FOREIGN KEY (settlement_id) REFERENCES settlements (id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- merchant_payout_holds -- withhold a payout pending investigation
--
-- Necessary in practice: a fraud pattern, a food-safety complaint or a licence
-- lapse must be able to freeze money before it leaves, and that decision needs an
-- owner and a paper trail rather than a developer running an UPDATE.
-- ---------------------------------------------------------------------------

CREATE TABLE merchant_payout_holds (
    id          UUID PRIMARY KEY,
    merchant_id UUID NOT NULL REFERENCES merchants (id) ON DELETE CASCADE,
    reason      TEXT NOT NULL,
    amount_minor BIGINT,
    placed_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    released_by UUID REFERENCES users (id) ON DELETE SET NULL,
    released_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX merchant_payout_holds_active_idx ON merchant_payout_holds (merchant_id)
    WHERE released_at IS NULL;
