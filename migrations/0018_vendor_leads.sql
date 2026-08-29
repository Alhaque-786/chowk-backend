-- 0018_vendor_leads
--
-- Why this change:
--
-- The supply side has no front door. A customer who knows a good juice cart has
-- nowhere to tell us, and the cart itself will never fill in a web form — that is
-- the whole premise of assisted vendors in 0017. The people we most want to sign
-- up are exactly the people least able to sign themselves up, so the lead has to
-- come from somebody else: a customer, or our own team after walking a market.
--
-- PHONE IS DELIBERATELY NOT UNIQUE. The obvious schema puts a unique index on
-- contact_phone and calls a second submission a duplicate. That would throw away
-- the most valuable signal this table can carry: three separate customers naming
-- the same nariyal pani cart in Hazratganj is not noise to be deduplicated, it is
-- the strongest possible argument for going there first. The admin list groups by
-- phone and shows the count. Spam is handled by the rate limiter, which is the
-- right tool for it.
--
-- Two ways of saying no, kept apart because they mean different things:
--
--   status = 'rejected'   a real vendor we decided against. Stays in the funnel
--                         numbers, and somebody may revisit it next quarter.
--   is_active = FALSE     junk. Never was a lead, should not be counted as one.
--
-- Collapsing them would make the rejection rate meaningless the first week
-- somebody submits "asdfgh".
--
-- is_active rather than deleted_at, matching the choice migration 0014 made for
-- the whole schema. A second convention would mean every future reader has to
-- learn which tables use which.

CREATE TABLE vendor_leads (
    id            UUID PRIMARY KEY,

    -- Who is telling us. A referral is somebody vouching for a vendor they buy
    -- from; a self_signup is the vendor themselves. They convert at very
    -- different rates and the outreach script is not the same, so the queue has
    -- to tell them apart.
    kind          TEXT NOT NULL CHECK (kind IN ('referral', 'self_signup')),

    business_name TEXT NOT NULL,
    -- Blank is normal and allowed: a customer referring the cart outside their
    -- office knows the stall, not the owner's name. Demanding it would cost more
    -- leads than the field is worth.
    contact_name  TEXT NOT NULL DEFAULT '',
    contact_phone TEXT NOT NULL,

    -- Free text, not a foreign key to serving areas. A lead may well be somewhere
    -- we do not serve yet, and that is useful to know — it is demand data. A
    -- constrained column would silently reject the most interesting rows.
    area          TEXT NOT NULL,

    vendor_kind   TEXT NOT NULL CHECK (vendor_kind IN (
        'juice_cart', 'tiffin', 'cloud_kitchen', 'restaurant', 'sweet_shop', 'other'
    )),

    note          TEXT NOT NULL DEFAULT '',

    status        TEXT NOT NULL DEFAULT 'new' CHECK (status IN (
        'new', 'contacted', 'visiting', 'onboarding', 'converted', 'rejected'
    )),

    -- Set when a signed-in customer submits. Nullable because the form works
    -- signed out on purpose: requiring an account to tell us about a juice cart
    -- would lose most of the leads this table exists to collect.
    -- ON DELETE SET NULL, not CASCADE: the lead outlives the account that named
    -- it, and losing a vendor because a customer closed their account would be
    -- absurd.
    referred_by   UUID REFERENCES users (id) ON DELETE SET NULL,

    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Same format as users.phone, so the two can be joined and compared without
    -- normalising at query time. Enforced here as well as in Go because the
    -- database is the last line that cannot be bypassed by a new caller.
    CONSTRAINT vendor_leads_phone_format_check CHECK (contact_phone ~ '^\+91[6-9][0-9]{9}$'),
    CONSTRAINT vendor_leads_business_name_check CHECK (length(btrim(business_name)) > 0),
    CONSTRAINT vendor_leads_area_check          CHECK (length(btrim(area)) > 0)
);

-- The admin queue: open leads, oldest first. Partial on is_active because junk is
-- never worth scanning past, and status leads the key because every query filters
-- on it before ordering.
CREATE INDEX vendor_leads_queue_idx
    ON vendor_leads (status, created_at)
    WHERE is_active;

-- Grouping by phone to count how many people named the same vendor. This is the
-- index that makes the "three customers said this" signal cheap to compute.
CREATE INDEX vendor_leads_phone_idx
    ON vendor_leads (contact_phone)
    WHERE is_active;

COMMENT ON TABLE vendor_leads IS
    'Vendors somebody wants on the platform. Referrals from customers and signups from vendors. Phone is intentionally not unique -- repeat mentions are signal.';
