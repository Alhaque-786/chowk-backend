-- 0007: name the first-party merchant.
--
-- The platform is Chowk; the first kitchen on it is Khansama. Two names on
-- purpose: a partner restaurant will not list on a marketplace named after a
-- competing kitchen, and "Chowk" (the market square) belongs to nobody, which is
-- exactly the neutrality a marketplace needs.
--
-- This is a NEW migration rather than an edit to 0006, because 0006 has already
-- been applied. The migration runner checksums applied files and refuses to start
-- if one changed -- that edit would already be live elsewhere, so the
-- environments would have silently diverged. Applied migrations are immutable.

UPDATE merchants
SET legal_name   = 'Khansama Foods',
    display_name = 'Khansama',
    slug         = 'khansama',
    updated_at   = now()
WHERE id = '00000000-0000-0000-0000-000000000001';

-- Structurally, Khansama is now just another row in `merchants` with
-- is_first_party = true. It gets no special-casing anywhere in the code -- which
-- is what makes self-preferencing auditable rather than baked in. See
-- docs/MARKETPLACE.md.
