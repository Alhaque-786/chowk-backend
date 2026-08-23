-- 0014_soft_delete
--
-- Why this change:
--
-- POLICY: business records are never hard-deleted. Deleting a row destroys the
-- only evidence that it existed, and in an ordering system that evidence is what
-- answers questions asked months later — what did this dish contain when it made
-- somebody ill, where did we actually deliver order CK-9EVDJPKV, why does last
-- quarter's food cost not reconcile. A DELETE cannot be reviewed, reversed, or
-- explained to an auditor.
--
-- `is_active BOOLEAN` rather than `deleted_at TIMESTAMPTZ`, because this schema
-- already made that choice: kitchens, menu_items, ingredients, device_tokens and
-- coupons all carry is_active. Adding a second convention for two more tables
-- would mean every future reader has to learn which tables use which. Consistency
-- beats the marginally richer timestamp.
--
-- These two tables were the only business records still hard-deleting:
--
--   addresses  DELETE FROM addresses WHERE id = $1 AND user_id = $2
--   recipes    DELETE FROM recipes WHERE menu_item_id = $1 AND ingredient_id = $2
--
-- Addresses matter because orders carry `delivery_address_id`. That column has no
-- foreign key and orders snapshot the address into their own delivery_* columns,
-- so history survived a delete — but the id became a dangling reference, and
-- "which of this customer's addresses was this?" became unanswerable.
--
-- Recipes matter more. A recipe line is a bill of materials, and deleting one
-- erases the record of what a dish USED to contain. That is the exact question an
-- allergen complaint asks.
--
-- STILL HARD-DELETED, deliberately, and each for a stated reason:
--
--   idempotency_keys   a deduplication mechanism with a TTL, not a record. The
--                      row's only job is to be found again within its window.
--   sessions           only rows already past expires_at. An expired session is
--                      not history, it is litter.
--   kitchen_hours      replaced as a SET on update (delete-then-insert). Keeping
--                      superseded rows would need a validity period per row to
--                      be readable at all, which is a bigger change than the
--                      value justifies today. Noted as a known limitation: we
--                      cannot currently answer "what were the opening hours last
--                      Tuesday".
--   test fixtures      tests must not leave rows behind. See the promotion tests.
--
-- THE INDEX TRAP. Both tables had a unique index that must become PARTIAL, or a
-- soft-deleted row permanently blocks its own replacement:
--
--   * one-default-per-user would let a deactivated default keep the slot, so the
--     customer could never set a new default address.
--   * (menu_item, ingredient) would stop a kitchen re-adding an ingredient it had
--     previously removed from a recipe.
--
-- Getting this wrong is the classic soft-delete bug: the delete appears to work
-- and the NEXT insert fails with a confusing constraint violation.

-- --- addresses ------------------------------------------------------------

ALTER TABLE addresses
    ADD COLUMN is_active BOOLEAN NOT NULL DEFAULT true;

COMMENT ON COLUMN addresses.is_active IS
    'False means the customer removed it. Never hard-deleted: orders reference it.';

DROP INDEX IF EXISTS addresses_one_default_per_user_idx;
CREATE UNIQUE INDEX addresses_one_default_per_user_idx
    ON addresses (user_id)
    WHERE is_default AND is_active;

-- Every read is scoped to a user AND to active rows, so the index should be too.
DROP INDEX IF EXISTS addresses_user_id_idx;
CREATE INDEX addresses_user_active_idx ON addresses (user_id) WHERE is_active;

-- --- recipes --------------------------------------------------------------

ALTER TABLE recipes
    ADD COLUMN is_active BOOLEAN NOT NULL DEFAULT true;

COMMENT ON COLUMN recipes.is_active IS
    'False means removed from the recipe. Kept: it records what the dish contained.';

-- Created as a table-level UNIQUE, so it may exist as a constraint, an index, or
-- both depending on how it was declared. Dropping both forms is the only way to
-- be sure, and IF EXISTS makes each attempt safe.
ALTER TABLE recipes DROP CONSTRAINT IF EXISTS recipes_item_ingredient_key;
DROP INDEX IF EXISTS recipes_item_ingredient_key;

CREATE UNIQUE INDEX recipes_item_ingredient_key
    ON recipes (menu_item_id, ingredient_id)
    WHERE is_active;

-- The reverse lookup ("which dishes use this ingredient") only ever wants live
-- lines, and it runs inside the checkout transaction where every millisecond is
-- time spent holding row locks.
DROP INDEX IF EXISTS recipes_ingredient_idx;
CREATE INDEX recipes_ingredient_active_idx ON recipes (ingredient_id) WHERE is_active;
