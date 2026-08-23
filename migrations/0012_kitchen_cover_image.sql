-- 0012_kitchen_cover_image
--
-- Why this change:
--
-- A kitchen had no image. menu_items has had `image_url` since 0002, so a dish
-- could carry a photograph, but the kitchen card -- the single largest element on
-- any listing page, and the thing a customer's eye lands on before they read a
-- word -- had nothing to show. The frontend was deriving a gradient tile from the
-- kitchen NAME to fill the space, which is a reasonable stand-in and not a
-- substitute.
--
-- TEXT NOT NULL DEFAULT '' rather than nullable, matching menu_items.image_url in
-- the same schema. One representation of "no image" instead of two: every caller
-- checks `!= ''` and none of them has to also handle NULL. A nullable column here
-- would be a second way to say the same thing, and the bug it produces is a
-- template rendering the literal string "null" into a src attribute.
--
-- No index: this is never a search predicate, only ever projected.

ALTER TABLE kitchens
    ADD COLUMN cover_image_url TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN kitchens.cover_image_url IS
    'Listing card image. Empty means the UI falls back to a generated tile.';
