-- 0017_assisted_vendors
--
-- Why this change:
--
-- Some vendors cannot take an order themselves. A man selling fresh nariyal pani
-- from a cart has no smartphone workflow, no POS, and no interest in acquiring
-- one — and he is exactly the supply the packaged-goods apps do not have. Blinkit
-- will sell you a Real tetra pak; nobody delivers a coconut opened in front of
-- you, because the vendor cannot operate a dashboard.
--
-- The model is that OUR TEAM operates the storefront on his behalf: we take the
-- order, we phone him, our rider collects and delivers. The vendor's only job is
-- the thing he is already good at.
--
-- This is a column, not a new entity. An assisted vendor is still a `merchant`
-- with a `kitchen`; what differs is WHO ACTS ON THE ORDER. Modelling it as a
-- separate table would duplicate menus, hours, pricing, settlement and the whole
-- catalogue for a distinction that is one boolean wide.
--
-- ---------------------------------------------------------------------------
-- FSSAI: registration is not the same thing as a licence
-- ---------------------------------------------------------------------------
--
-- The schema had `fssai_license` only, which cannot describe a cart vendor. Under
-- the FSS Act the tiers are different instruments:
--
--   REGISTRATION  turnover up to ~Rs 12 lakh/year. Form A, roughly Rs 100/year.
--                 This is what a street vendor or a single cart holds.
--   LICENCE       above that. State or Central, materially more paperwork.
--
-- Storing a registration number in a column called `fssai_license` would make
-- every compliance report wrong in the same direction, so the KIND is now
-- explicit. Helping a vendor obtain his registration is also the most useful
-- thing we can do for him at onboarding, and a reason to pick us over an
-- aggregator that only wants a commission.
--
-- ---------------------------------------------------------------------------
-- The compliance control that was never implemented
-- ---------------------------------------------------------------------------
--
-- Migration 0006 added `merchants_fssai_expiry_idx` with the comment "An expired
-- FSSAI means the outlet must stop selling immediately", and nothing in the
-- application has ever read `fssai_expiry`. The index feeds no code. Kitchen
-- domain logic now enforces it, which is why this migration also backfills the
-- fields the enforcement needs rather than letting live rows fail the new check.

-- --- 1. How the storefront is operated -------------------------------------

ALTER TABLE merchants
    ADD COLUMN operating_mode TEXT NOT NULL DEFAULT 'self_serve';

ALTER TABLE merchants
    ADD CONSTRAINT merchants_operating_mode_chk
    CHECK (operating_mode IN ('self_serve', 'assisted'));

COMMENT ON COLUMN merchants.operating_mode IS
    'self_serve: the merchant accepts and prepares through the dashboard. '
    'assisted: our ops team accepts on their behalf and a rider collects. '
    'Drives order routing (ops queue vs kitchen dashboard) and the payment '
    'capture point.';

-- Finding an assisted merchant's orders is the ops queue's hottest query.
CREATE INDEX merchants_assisted_idx ON merchants (operating_mode)
    WHERE operating_mode = 'assisted';

-- --- 2. FSSAI kind ---------------------------------------------------------

ALTER TABLE merchants
    ADD COLUMN fssai_kind TEXT NOT NULL DEFAULT 'license';

ALTER TABLE merchants
    ADD CONSTRAINT merchants_fssai_kind_chk
    CHECK (fssai_kind IN ('license', 'registration'));

COMMENT ON COLUMN merchants.fssai_kind IS
    'Which instrument fssai_license holds: a full licence, or the Form A '
    'registration a cart vendor under ~Rs 12 lakh turnover holds. Different '
    'documents with different renewal rules; conflating them makes every '
    'compliance report wrong in the same direction.';

-- --- 3. The kitchen needs to see its own compliance state ------------------
--
-- Denormalised onto kitchens deliberately. CanAcceptOrder is a PURE DOMAIN
-- FUNCTION on Kitchen and runs on every checkout; making it reach into another
-- module's table would either break the module boundary or add a join to the
-- hottest path in the system. The merchant remains the source of truth and
-- propagates on change.

ALTER TABLE kitchens
    ADD COLUMN fssai_expiry DATE;

COMMENT ON COLUMN kitchens.fssai_expiry IS
    'Copied from the owning merchant so Kitchen.CanAcceptOrder can enforce it '
    'without a cross-module join. NULL means unknown, which is treated as '
    'BLOCKING for a marketplace merchant and permitted for first-party.';

CREATE INDEX kitchens_fssai_expiry_idx ON kitchens (fssai_expiry)
    WHERE is_active AND fssai_expiry IS NOT NULL;

-- --- 4. Backfill, so enforcement does not close every existing kitchen ----
--
-- Existing rows predate the field. Dev fixtures and the first-party merchant get
-- a far-future date; a real marketplace merchant with no expiry recorded is left
-- NULL and WILL be blocked, which is the correct outcome — an outlet whose
-- licence status we cannot state should not be selling.

-- is_first_party lives on KITCHENS, not merchants — checked against
-- information_schema rather than assumed, after the first version of this
-- migration failed on exactly that.
UPDATE kitchens k
SET fssai_expiry = '2099-12-31'
WHERE k.is_first_party
   OR k.fssai_license IN ('DEV-FIXTURE', 'FIRST-PARTY');

UPDATE kitchens k
SET fssai_expiry = m.fssai_expiry
FROM merchants m
WHERE k.merchant_id = m.id
  AND k.fssai_expiry IS NULL
  AND m.fssai_expiry IS NOT NULL;
