-- 0015_recipes_upsert_fix
--
-- Why this change:
--
-- 0014 made recipes_item_ingredient_key a PARTIAL unique index (`WHERE
-- is_active`), which broke every recipe write. Postgres can only infer a conflict
-- target from an index whose predicate the statement repeats, so
--
--     ON CONFLICT (menu_item_id, ingredient_id) DO UPDATE ...
--
-- began failing with "there is no unique or exclusion constraint matching the ON
-- CONFLICT specification". Reproduced directly in psql before writing this.
--
-- Two ways out, and the partial index is the worse one. Keeping it would mean
-- writing `ON CONFLICT (menu_item_id, ingredient_id) WHERE is_active` — which
-- works, but also means a soft-deleted line no longer blocks an INSERT, so
-- re-adding an ingredient accumulates a second row. Over time a dish would carry
-- several rows per ingredient, one live and several dead, and every reader would
-- have to know that.
--
-- So: back to a FULL unique index, exactly one row per (dish, ingredient) forever,
-- and the upsert sets is_active = true. Re-adding an ingredient REACTIVATES the
-- line it had before. Nothing is deleted, nothing duplicates, and the conflict
-- target is inferable again.
--
-- The lesson generalises, and it is why addresses kept its partial index while
-- this one did not: make a unique index partial when the excluded rows must NOT
-- reserve the slot (a removed address must not hold the one-default place), and
-- keep it full when the row IS the identity and should be reused (a dish's line
-- for an ingredient).

DROP INDEX IF EXISTS recipes_item_ingredient_key;

CREATE UNIQUE INDEX recipes_item_ingredient_key
    ON recipes (menu_item_id, ingredient_id);

COMMENT ON INDEX recipes_item_ingredient_key IS
    'Full, not partial: a soft-deleted line is reactivated by the upsert, never duplicated.';
