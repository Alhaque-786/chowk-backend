-- 0013_kitchen_cost_for_two
--
-- Why this change:
--
-- Every listing card on Zomato and Swiggy carries a "cost for two" figure, and it
-- does real work: it is the only number on the card that answers "can I afford
-- this place" before tapping in. Without it a customer compares six kitchens on
-- rating and delivery time alone, and finds out about price one screen later.
--
-- STORED, NOT DERIVED, and that is a deliberate choice. It could be computed from
-- the menu -- roughly twice the median main-course price -- and that would always
-- be present with no merchant effort. But it would also be wrong in the cases
-- that matter: a kitchen selling one 800-rupee family biryani alongside 60-rupee
-- breads has a median that describes neither, and a thali place where one plate
-- genuinely feeds two would be overstated by exactly 2x. The merchant knows what
-- two people spend; the median does not.
--
-- Minor units (paise) as BIGINT, like every other money column in this schema.
-- Never a float, and never rupees -- see internal/shared/money.
--
-- DEFAULT 0 means "not set", and the API omits the field entirely in that case so
-- the UI shows nothing rather than "₹0 for two". A nullable column would express
-- the same thing with a second representation, which is what the existing money
-- columns here already avoid.

ALTER TABLE kitchens
    ADD COLUMN cost_for_two_minor BIGINT NOT NULL DEFAULT 0
        CONSTRAINT kitchens_cost_for_two_non_negative CHECK (cost_for_two_minor >= 0);

COMMENT ON COLUMN kitchens.cost_for_two_minor IS
    'Typical spend for two people, in paise. 0 means not set; the API omits it.';
