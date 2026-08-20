package postgres

import (
	"context"
	"fmt"

	"github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

type MenuRepository struct {
	db *database.DB
}

func NewMenuRepository(db *database.DB) *MenuRepository {
	return &MenuRepository{db: db}
}

const itemColumns = `
	id, kitchen_id, category_id, name, description, image_url,
	price_minor, compare_price_minor, tax_bps, packaging_fee_minor,
	food_type, spice_level, allergens, calories, serves,
	is_available, is_sold_out, prep_minutes, display_order, is_recommended,
	rating_average, rating_count, order_count, version, created_at, updated_at`

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

func (r *MenuRepository) CreateCategory(ctx context.Context, c *domain.Category) error {
	const q = `
		INSERT INTO menu_categories (id, kitchen_id, name, description, display_order, is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	if _, err := r.db.Q(ctx).Exec(ctx, q,
		c.ID, c.KitchenID, c.Name, c.Description, c.DisplayOrder, c.IsActive, c.CreatedAt, c.UpdatedAt,
	); err != nil {
		return apperr.Internal(fmt.Errorf("catalog: create category: %w", err))
	}
	return nil
}

func (r *MenuRepository) UpdateCategory(ctx context.Context, c *domain.Category) error {
	const q = `
		UPDATE menu_categories
		SET name = $2, description = $3, display_order = $4, is_active = $5, updated_at = $6
		WHERE id = $1`
	tag, err := r.db.Q(ctx).Exec(ctx, q, c.ID, c.Name, c.Description, c.DisplayOrder, c.IsActive, c.UpdatedAt)
	if err != nil {
		return apperr.Internal(fmt.Errorf("catalog: update category: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CATEGORY_NOT_FOUND", "That menu category could not be found.")
	}
	return nil
}

func (r *MenuRepository) ListCategories(ctx context.Context, kitchenID id.ID, activeOnly bool) ([]domain.Category, error) {
	q := `SELECT id, kitchen_id, name, description, display_order, is_active, created_at, updated_at
		FROM menu_categories WHERE kitchen_id = $1`
	if activeOnly {
		q += ` AND is_active`
	}
	q += ` ORDER BY display_order, name`

	rows, err := r.db.Q(ctx).Query(ctx, q, kitchenID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: list categories: %w", err))
	}
	defer rows.Close()

	var out []domain.Category
	for rows.Next() {
		var c domain.Category
		if err := rows.Scan(&c.ID, &c.KitchenID, &c.Name, &c.Description,
			&c.DisplayOrder, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan category: %w", err))
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------------

func (r *MenuRepository) CreateItem(ctx context.Context, m *domain.MenuItem) error {
	const q = `
		INSERT INTO menu_items (
			id, kitchen_id, category_id, name, description, image_url,
			price_minor, compare_price_minor, tax_bps, packaging_fee_minor,
			food_type, spice_level, allergens, calories, serves,
			is_available, is_sold_out, prep_minutes, display_order, is_recommended,
			version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,false,$17,$18,$19,1,$20,$21)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		m.ID, m.KitchenID, m.CategoryID, m.Name, m.Description, m.ImageURL,
		m.Price.Minor(), comparePriceMinor(m), m.TaxBps, m.PackagingFee.Minor(),
		string(m.FoodType), m.SpiceLevel, m.Allergens, m.Calories, m.Serves,
		m.IsAvailable, m.PrepMinutes, m.DisplayOrder, m.IsRecommended,
		m.CreatedAt, m.UpdatedAt,
	); err != nil {
		if database.IsCheckViolation(err) {
			return apperr.Invalid("INVALID_MENU_ITEM",
				"Those menu item values are not valid. Check price and tax rate.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("catalog: create item: %w", err))
	}
	return nil
}

// UpdateItem applies optimistic locking. is_sold_out is deliberately NOT
// touched: it is owned by inventory, and an admin editing a price must not
// accidentally mark a sold-out dish available again.
func (r *MenuRepository) UpdateItem(ctx context.Context, m *domain.MenuItem) error {
	const q = `
		UPDATE menu_items SET
			category_id = $2, name = $3, description = $4, image_url = $5,
			price_minor = $6, compare_price_minor = $7, tax_bps = $8, packaging_fee_minor = $9,
			food_type = $10, spice_level = $11, allergens = $12, calories = $13, serves = $14,
			is_available = $15, prep_minutes = $16, display_order = $17, is_recommended = $18,
			version = version + 1, updated_at = $19
		WHERE id = $1 AND version = $20`

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		m.ID, m.CategoryID, m.Name, m.Description, m.ImageURL,
		m.Price.Minor(), comparePriceMinor(m), m.TaxBps, m.PackagingFee.Minor(),
		string(m.FoodType), m.SpiceLevel, m.Allergens, m.Calories, m.Serves,
		m.IsAvailable, m.PrepMinutes, m.DisplayOrder, m.IsRecommended,
		m.UpdatedAt, m.Version,
	)
	if err != nil {
		if database.IsCheckViolation(err) {
			return apperr.Invalid("INVALID_MENU_ITEM",
				"Those menu item values are not valid.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("catalog: update item %s: %w", m.ID, err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("MENU_ITEM_MODIFIED",
			"This item was changed by someone else. Please reload and try again.")
	}
	m.Version++
	return nil
}

func (r *MenuRepository) FindItemByID(ctx context.Context, itemID id.ID) (*domain.MenuItem, error) {
	q := `SELECT ` + itemColumns + ` FROM menu_items WHERE id = $1`

	item, err := scanItem(r.db.Q(ctx).QueryRow(ctx, q, itemID))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("MENU_ITEM_NOT_FOUND", "That item is not on the menu.")
		}
		return nil, apperr.Internal(fmt.Errorf("catalog: find item: %w", err))
	}
	return item, nil
}

// FindItemsByIDs is the batch read checkout uses: one query for the whole cart.
func (r *MenuRepository) FindItemsByIDs(ctx context.Context, itemIDs []id.ID) ([]domain.MenuItem, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	q := `SELECT ` + itemColumns + ` FROM menu_items WHERE id = ANY($1)`
	return r.queryItems(ctx, q, itemIDs)
}

func (r *MenuRepository) ListItems(ctx context.Context, kitchenID id.ID, orderableOnly bool) ([]domain.MenuItem, error) {
	q := `SELECT ` + itemColumns + ` FROM menu_items WHERE kitchen_id = $1`
	if orderableOnly {
		// Matches the partial index menu_items_kitchen_available_idx, so the
		// customer menu read is an index scan.
		q += ` AND is_available AND NOT is_sold_out`
	}
	q += ` ORDER BY display_order, name`
	return r.queryItems(ctx, q, kitchenID)
}

// SearchItems uses the trigram index for typo-tolerant matching.
func (r *MenuRepository) SearchItems(ctx context.Context, kitchenID id.ID, query string, limit int) ([]domain.MenuItem, error) {
	// similarity() ranks by trigram closeness so "biryani" also finds "biriyani".
	q := `SELECT ` + itemColumns + ` FROM menu_items
		WHERE kitchen_id = $1
		  AND is_available AND NOT is_sold_out
		  AND (name ILIKE '%' || $2 || '%' OR description ILIKE '%' || $2 || '%')
		ORDER BY similarity(name, $2) DESC, order_count DESC
		LIMIT $3`
	return r.queryItems(ctx, q, kitchenID, query, limit)
}

// SetSoldOut is called by inventory's event handler. It bypasses optimistic
// locking because it is a system reaction to a stock fact, not a human edit
// competing with another human edit.
func (r *MenuRepository) SetSoldOut(ctx context.Context, itemID id.ID, soldOut bool) error {
	const q = `UPDATE menu_items SET is_sold_out = $2, updated_at = now() WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q, itemID, soldOut); err != nil {
		return apperr.Internal(fmt.Errorf("catalog: set sold out: %w", err))
	}
	return nil
}

func (r *MenuRepository) queryItems(ctx context.Context, q string, args ...any) ([]domain.MenuItem, error) {
	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: query items: %w", err))
	}
	defer rows.Close()

	var out []domain.MenuItem
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan item: %w", err))
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

func scanItem(row scanner) (*domain.MenuItem, error) {
	var (
		m            domain.MenuItem
		categoryID   *id.ID
		priceMinor   int64
		comparePrice *int64
		packagingFee int64
		foodType     string
		ratingAvg    float64
	)
	if err := row.Scan(
		&m.ID, &m.KitchenID, &categoryID, &m.Name, &m.Description, &m.ImageURL,
		&priceMinor, &comparePrice, &m.TaxBps, &packagingFee,
		&foodType, &m.SpiceLevel, &m.Allergens, &m.Calories, &m.Serves,
		&m.IsAvailable, &m.IsSoldOut, &m.PrepMinutes, &m.DisplayOrder, &m.IsRecommended,
		&ratingAvg, &m.RatingCount, &m.OrderCount, &m.Version, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}

	m.CategoryID = categoryID
	m.Price = money.FromMinor(priceMinor, money.INR)
	m.PackagingFee = money.FromMinor(packagingFee, money.INR)
	m.FoodType = domain.FoodType(foodType)
	m.RatingAverage = ratingAvg
	if comparePrice != nil {
		cp := money.FromMinor(*comparePrice, money.INR)
		m.ComparePrice = &cp
	}
	return &m, nil
}

func comparePriceMinor(m *domain.MenuItem) *int64 {
	if m.ComparePrice == nil {
		return nil
	}
	v := m.ComparePrice.Minor()
	return &v
}

// ---------------------------------------------------------------------------
// Modifiers
// ---------------------------------------------------------------------------

func (r *MenuRepository) CreateModifierGroup(ctx context.Context, g *domain.ModifierGroup) error {
	const q = `
		INSERT INTO modifier_groups (id, kitchen_id, name, min_select, max_select, display_order, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,now())`
	if _, err := r.db.Q(ctx).Exec(ctx, q, g.ID, g.KitchenID, g.Name, g.MinSelect, g.MaxSelect, g.DisplayOrder); err != nil {
		return apperr.Internal(fmt.Errorf("catalog: create modifier group: %w", err))
	}
	return nil
}

func (r *MenuRepository) CreateModifier(ctx context.Context, m *domain.Modifier) error {
	const q = `
		INSERT INTO modifiers (id, group_id, name, price_minor, is_available, display_order)
		VALUES ($1,$2,$3,$4,$5,$6)`
	if _, err := r.db.Q(ctx).Exec(ctx, q, m.ID, m.GroupID, m.Name, m.Price.Minor(), m.IsAvailable, m.DisplayOrder); err != nil {
		return apperr.Internal(fmt.Errorf("catalog: create modifier: %w", err))
	}
	return nil
}

func (r *MenuRepository) AttachModifierGroup(ctx context.Context, itemID, groupID id.ID) error {
	// Idempotent: re-attaching an already-attached group is a no-op rather than
	// an error, so a retried admin save does not fail.
	const q = `
		INSERT INTO menu_item_modifier_groups (menu_item_id, group_id)
		VALUES ($1,$2) ON CONFLICT DO NOTHING`
	if _, err := r.db.Q(ctx).Exec(ctx, q, itemID, groupID); err != nil {
		return apperr.Internal(fmt.Errorf("catalog: attach modifier group: %w", err))
	}
	return nil
}

// LoadModifierGroups fetches groups and their modifiers for many items in TWO
// queries total, regardless of item count.
//
// The naive version is one query per item per group, which for a 40-dish menu
// with 3 groups each is 120 round trips — the classic N+1 that makes a menu
// endpoint take two seconds.
func (r *MenuRepository) LoadModifierGroups(ctx context.Context, itemIDs []id.ID) (map[id.ID][]domain.ModifierGroup, error) {
	if len(itemIDs) == 0 {
		return map[id.ID][]domain.ModifierGroup{}, nil
	}

	const groupQ = `
		SELECT mimg.menu_item_id, g.id, g.kitchen_id, g.name, g.min_select, g.max_select, g.display_order
		FROM menu_item_modifier_groups mimg
		JOIN modifier_groups g ON g.id = mimg.group_id
		WHERE mimg.menu_item_id = ANY($1)
		ORDER BY g.display_order, g.name`

	rows, err := r.db.Q(ctx).Query(ctx, groupQ, itemIDs)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: load modifier groups: %w", err))
	}
	defer rows.Close()

	out := make(map[id.ID][]domain.ModifierGroup)
	distinctGroups := make(map[id.ID]struct{})

	for rows.Next() {
		var (
			itemID id.ID
			g      domain.ModifierGroup
		)
		if err := rows.Scan(&itemID, &g.ID, &g.KitchenID, &g.Name, &g.MinSelect, &g.MaxSelect, &g.DisplayOrder); err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan modifier group: %w", err))
		}
		out[itemID] = append(out[itemID], g)
		distinctGroups[g.ID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	if len(distinctGroups) == 0 {
		return out, nil
	}

	groupIDs := make([]id.ID, 0, len(distinctGroups))
	for gID := range distinctGroups {
		groupIDs = append(groupIDs, gID)
	}

	const modQ = `
		SELECT id, group_id, name, price_minor, is_available, display_order
		FROM modifiers
		WHERE group_id = ANY($1)
		ORDER BY display_order, name`

	modRows, err := r.db.Q(ctx).Query(ctx, modQ, groupIDs)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: load modifiers: %w", err))
	}
	defer modRows.Close()

	// Collect modifiers keyed by group, THEN attach.
	//
	// Do not be tempted to hold &out[itemID][n] pointers while still appending to
	// that slice: append reallocates the backing array once it outgrows capacity,
	// which orphans every pointer taken before the growth. Writes through those
	// stale pointers land in a dead array and vanish silently -- the symptom is
	// that only the LAST group of each item ends up populated. Indexing after the
	// slice is final has no such hazard.
	modsByGroup := make(map[id.ID][]domain.Modifier, len(groupIDs))
	for modRows.Next() {
		var (
			m          domain.Modifier
			priceMinor int64
		)
		if err := modRows.Scan(&m.ID, &m.GroupID, &m.Name, &priceMinor, &m.IsAvailable, &m.DisplayOrder); err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan modifier: %w", err))
		}
		m.Price = money.FromMinor(priceMinor, money.INR)
		modsByGroup[m.GroupID] = append(modsByGroup[m.GroupID], m)
	}
	if err := modRows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}

	for itemID := range out {
		groups := out[itemID]
		for i := range groups {
			groups[i].Modifiers = modsByGroup[groups[i].ID]
		}
	}
	return out, nil
}

func (r *MenuRepository) FindModifiersByIDs(ctx context.Context, modifierIDs []id.ID) ([]domain.Modifier, error) {
	if len(modifierIDs) == 0 {
		return nil, nil
	}
	const q = `
		SELECT id, group_id, name, price_minor, is_available, display_order
		FROM modifiers WHERE id = ANY($1)`

	rows, err := r.db.Q(ctx).Query(ctx, q, modifierIDs)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: find modifiers: %w", err))
	}
	defer rows.Close()

	var out []domain.Modifier
	for rows.Next() {
		var (
			m          domain.Modifier
			priceMinor int64
		)
		if err := rows.Scan(&m.ID, &m.GroupID, &m.Name, &priceMinor, &m.IsAvailable, &m.DisplayOrder); err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan modifier: %w", err))
		}
		m.Price = money.FromMinor(priceMinor, money.INR)
		out = append(out, m)
	}
	return out, rows.Err()
}

var _ domain.MenuRepository = (*MenuRepository)(nil)
