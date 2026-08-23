// Package postgres implements the inventory module's repository ports.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/inventory/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

type IngredientRepository struct {
	db *database.DB
}

func NewIngredientRepository(db *database.DB) *IngredientRepository {
	return &IngredientRepository{db: db}
}

const ingredientColumns = `
	id, kitchen_id, name, sku, unit, on_hand, reserved,
	reorder_level, reorder_qty, unit_cost_minor,
	supplier_name, shelf_life_days, is_active, version, created_at, updated_at`

func (r *IngredientRepository) Create(ctx context.Context, i *domain.Ingredient) error {
	const q = `
		INSERT INTO ingredients (
			id, kitchen_id, name, sku, unit, on_hand, reserved,
			reorder_level, reorder_qty, unit_cost_minor,
			supplier_name, shelf_life_days, is_active, version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,0,$7,$8,$9,$10,$11,$12,1,$13,$14)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		i.ID, i.KitchenID, i.Name, i.SKU, string(i.Unit), i.OnHand,
		i.ReorderLevel, i.ReorderQty, i.UnitCost.Minor(),
		i.SupplierName, i.ShelfLifeDays, i.IsActive, i.CreatedAt, i.UpdatedAt,
	); err != nil {
		if database.IsUniqueViolation(err, "ingredients_kitchen_sku_key") {
			return apperr.Conflict("SKU_EXISTS", "An ingredient with that SKU already exists.")
		}
		return apperr.Internal(fmt.Errorf("inventory: create ingredient: %w", err))
	}
	return nil
}

// Update writes descriptive fields only.
//
// on_hand and reserved are deliberately NOT settable here: they move exclusively
// through ApplyDelta inside a locked transaction. Allowing a plain UPDATE of a
// balance would let a caller clobber a concurrent reservation, which is the whole
// class of bug this module exists to prevent.
func (r *IngredientRepository) Update(ctx context.Context, i *domain.Ingredient) error {
	const q = `
		UPDATE ingredients SET
			name = $2, sku = $3, unit = $4, reorder_level = $5, reorder_qty = $6,
			unit_cost_minor = $7, supplier_name = $8, shelf_life_days = $9,
			is_active = $10, version = version + 1, updated_at = $11
		WHERE id = $1`

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		i.ID, i.Name, i.SKU, string(i.Unit), i.ReorderLevel, i.ReorderQty,
		i.UnitCost.Minor(), i.SupplierName, i.ShelfLifeDays, i.IsActive, i.UpdatedAt,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("inventory: update ingredient: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("INGREDIENT_NOT_FOUND", "That ingredient could not be found.")
	}
	return nil
}

// LockForUpdate reads ingredients with a row lock, ORDERED BY ID.
//
// THE `ORDER BY id` IS LOAD-BEARING. Without it, two concurrent checkouts needing
// rice and chicken can lock them in opposite orders and deadlock: A holds rice
// waiting for chicken, B holds chicken waiting for rice. Postgres detects it and
// kills one transaction, so checkout fails intermittently under exactly the load
// where it must not. A consistent lock order makes one transaction simply wait.
//
// The lock is held until the enclosing transaction commits, which is what
// serialises the read-check-write sequence in Reserve.
func (r *IngredientRepository) LockForUpdate(ctx context.Context, ingredientIDs []id.ID) (map[id.ID]*domain.Ingredient, error) {
	if len(ingredientIDs) == 0 {
		return map[id.ID]*domain.Ingredient{}, nil
	}

	q := `SELECT ` + ingredientColumns + ` FROM ingredients
		WHERE id = ANY($1)
		ORDER BY id
		FOR UPDATE`

	rows, err := r.db.Q(ctx).Query(ctx, q, ingredientIDs)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("inventory: lock ingredients: %w", err))
	}
	defer rows.Close()

	out := make(map[id.ID]*domain.Ingredient, len(ingredientIDs))
	for rows.Next() {
		i, err := scanIngredient(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("inventory: scan ingredient: %w", err))
		}
		out[i.ID] = i
	}
	return out, rows.Err()
}

// ApplyDelta adjusts on_hand and reserved by signed amounts.
//
// A relative UPDATE, not an absolute SET, on purpose. `SET reserved = $value`
// computed in Go would overwrite a concurrent change; `SET reserved = reserved +
// $delta` is applied by the database against whatever the current value is. The
// row lock makes this deterministic, and the arithmetic being in SQL means even
// an un-locked call cannot lose a concurrent update.
//
// The database's CHECK constraints (on_hand >= 0, reserved >= 0, reserved <=
// on_hand) are the final backstop: if application logic ever tries to
// over-reserve, the write fails rather than silently producing a negative
// balance.
func (r *IngredientRepository) ApplyDelta(ctx context.Context, ingredientID id.ID, onHandDelta, reservedDelta float64) error {
	const q = `
		UPDATE ingredients
		SET on_hand = on_hand + $2, reserved = reserved + $3, updated_at = now()
		WHERE id = $1`

	tag, err := r.db.Q(ctx).Exec(ctx, q, ingredientID, onHandDelta, reservedDelta)
	if err != nil {
		if database.IsCheckViolation(err) {
			// Reaching here means the application-level check was bypassed or
			// raced. Surface it as a conflict so the caller retries rather than a
			// 500, but it is worth investigating.
			return apperr.Conflict("STOCK_CONSTRAINT_VIOLATED",
				"That stock change would leave an invalid balance.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("inventory: apply delta to %s: %w", ingredientID, err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("INGREDIENT_NOT_FOUND", "That ingredient could not be found.")
	}
	return nil
}

func (r *IngredientRepository) FindByID(ctx context.Context, ingredientID id.ID) (*domain.Ingredient, error) {
	q := `SELECT ` + ingredientColumns + ` FROM ingredients WHERE id = $1`

	i, err := scanIngredient(r.db.Q(ctx).QueryRow(ctx, q, ingredientID))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("INGREDIENT_NOT_FOUND", "That ingredient could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("inventory: find ingredient: %w", err))
	}
	return i, nil
}

func (r *IngredientRepository) ListForKitchen(ctx context.Context, kitchenID id.ID, activeOnly bool) ([]*domain.Ingredient, error) {
	q := `SELECT ` + ingredientColumns + ` FROM ingredients WHERE kitchen_id = $1`
	if activeOnly {
		q += ` AND is_active`
	}
	q += ` ORDER BY name`
	return r.queryIngredients(ctx, q, kitchenID)
}

// ListLowStock backs the "what do I need to buy" dashboard. Matches the partial
// index, so it is an index scan rather than a table scan.
func (r *IngredientRepository) ListLowStock(ctx context.Context, kitchenID id.ID) ([]*domain.Ingredient, error) {
	q := `SELECT ` + ingredientColumns + ` FROM ingredients
		WHERE kitchen_id = $1 AND is_active AND on_hand <= reorder_level
		ORDER BY name`
	return r.queryIngredients(ctx, q, kitchenID)
}

func (r *IngredientRepository) queryIngredients(ctx context.Context, q string, args ...any) ([]*domain.Ingredient, error) {
	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("inventory: query ingredients: %w", err))
	}
	defer rows.Close()

	var out []*domain.Ingredient
	for rows.Next() {
		i, err := scanIngredient(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("inventory: scan ingredient: %w", err))
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (r *IngredientRepository) AppendMovement(ctx context.Context, m *domain.StockMovement) error {
	const q = `
		INSERT INTO stock_movements (
			id, ingredient_id, kitchen_id, quantity, movement_type, balance_after,
			reference_type, reference_id, unit_cost_minor, note, actor_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		m.ID, m.IngredientID, m.KitchenID, m.Quantity, string(m.Type), m.BalanceAfter,
		m.ReferenceType, m.ReferenceID, m.UnitCost.Minor(), m.Note, m.ActorID, m.CreatedAt,
	); err != nil {
		return apperr.Internal(fmt.Errorf("inventory: append movement: %w", err))
	}
	return nil
}

func (r *IngredientRepository) ListMovements(ctx context.Context, ingredientID id.ID, limit int) ([]*domain.StockMovement, error) {
	const q = `
		SELECT id, ingredient_id, kitchen_id, quantity, movement_type, balance_after,
		       reference_type, reference_id, unit_cost_minor, note, actor_id, created_at
		FROM stock_movements WHERE ingredient_id = $1
		ORDER BY created_at DESC LIMIT $2`

	rows, err := r.db.Q(ctx).Query(ctx, q, ingredientID, limit)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("inventory: list movements: %w", err))
	}
	defer rows.Close()

	var out []*domain.StockMovement
	for rows.Next() {
		var (
			m        domain.StockMovement
			mType    string
			unitCost int64
		)
		if err := rows.Scan(&m.ID, &m.IngredientID, &m.KitchenID, &m.Quantity, &mType,
			&m.BalanceAfter, &m.ReferenceType, &m.ReferenceID, &unitCost, &m.Note,
			&m.ActorID, &m.CreatedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("inventory: scan movement: %w", err))
		}
		m.Type = domain.MovementType(mType)
		m.UnitCost = money.FromMinor(unitCost, money.INR)
		out = append(out, &m)
	}
	return out, rows.Err()
}

func scanIngredient(row scanner) (*domain.Ingredient, error) {
	var (
		i        domain.Ingredient
		unit     string
		unitCost int64
	)
	if err := row.Scan(
		&i.ID, &i.KitchenID, &i.Name, &i.SKU, &unit, &i.OnHand, &i.Reserved,
		&i.ReorderLevel, &i.ReorderQty, &unitCost,
		&i.SupplierName, &i.ShelfLifeDays, &i.IsActive, &i.Version, &i.CreatedAt, &i.UpdatedAt,
	); err != nil {
		return nil, err
	}
	i.Unit = domain.Unit(unit)
	i.UnitCost = money.FromMinor(unitCost, money.INR)
	return &i, nil
}

// ---------------------------------------------------------------------------
// Recipes
// ---------------------------------------------------------------------------

type RecipeRepository struct {
	db *database.DB
}

func NewRecipeRepository(db *database.DB) *RecipeRepository {
	return &RecipeRepository{db: db}
}

// Upsert sets a recipe line, replacing any existing one for the same
// (dish, ingredient) pair.
//
// ON CONFLICT rather than delete-then-insert: listing the same ingredient twice
// for a dish would double-count consumption, and the unique index prevents it.
//
// `is_active = true` in the DO UPDATE is what makes re-adding a removed
// ingredient REACTIVATE its existing line rather than insert a second one. The
// unique index is deliberately full rather than partial so that this works — see
// migration 0015 for why the partial version broke conflict inference entirely.
func (r *RecipeRepository) Upsert(ctx context.Context, line *domain.RecipeLine) error {
	const q = `
		INSERT INTO recipes (id, menu_item_id, ingredient_id, quantity, wastage_bps, is_optional, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (menu_item_id, ingredient_id)
		DO UPDATE SET quantity = $4, wastage_bps = $5, is_optional = $6,
		              is_active = true`

	lineID := line.ID
	if id.IsNil(lineID) {
		lineID = id.New()
	}
	if _, err := r.db.Q(ctx).Exec(ctx, q,
		lineID, line.MenuItemID, line.IngredientID, line.Quantity, line.WastageBps, line.IsOptional,
	); err != nil {
		if database.IsCheckViolation(err) {
			return apperr.Invalid("INVALID_RECIPE",
				"Recipe quantity must be positive and wastage between 0 and 100%.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("inventory: upsert recipe: %w", err))
	}
	return nil
}

// Delete deactivates a recipe line. It does NOT remove the row.
//
// A recipe line is a bill of materials, and the row is the only record of what a
// dish USED to contain. That is exactly the question an allergen complaint asks
// six months later, and a DELETE makes it unanswerable.
//
// Not an error when it is already inactive, or absent: removing an ingredient
// that is not in the recipe has achieved what the caller wanted, and failing here
// would make an idempotent menu edit fail on its second attempt.
func (r *RecipeRepository) Delete(ctx context.Context, menuItemID, ingredientID id.ID) error {
	const q = `
		UPDATE recipes SET is_active = false
		 WHERE menu_item_id = $1 AND ingredient_id = $2 AND is_active`

	if _, err := r.db.Q(ctx).Exec(ctx, q, menuItemID, ingredientID); err != nil {
		return apperr.Internal(fmt.Errorf("inventory: deactivate recipe line: %w", err))
	}
	return nil
}

// LoadForMenuItems batches by dish, so exploding a ten-line cart is ONE query.
//
// Joins the ingredient name and unit so error messages ("Rice has just run out")
// need no second lookup.
func (r *RecipeRepository) LoadForMenuItems(ctx context.Context, menuItemIDs []id.ID) (map[id.ID][]domain.RecipeLine, error) {
	if len(menuItemIDs) == 0 {
		return map[id.ID][]domain.RecipeLine{}, nil
	}

	const q = `
		SELECT r.id, r.menu_item_id, r.ingredient_id, r.quantity, r.wastage_bps,
		       r.is_optional, i.name, i.unit
		FROM recipes r
		JOIN ingredients i ON i.id = r.ingredient_id
		WHERE r.menu_item_id = ANY($1) AND r.is_active AND i.is_active`

	rows, err := r.db.Q(ctx).Query(ctx, q, menuItemIDs)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("inventory: load recipes: %w", err))
	}
	defer rows.Close()

	out := make(map[id.ID][]domain.RecipeLine)
	for rows.Next() {
		var (
			line domain.RecipeLine
			unit string
		)
		if err := rows.Scan(&line.ID, &line.MenuItemID, &line.IngredientID, &line.Quantity,
			&line.WastageBps, &line.IsOptional, &line.IngredientName, &unit); err != nil {
			return nil, apperr.Internal(fmt.Errorf("inventory: scan recipe: %w", err))
		}
		line.Unit = domain.Unit(unit)
		out[line.MenuItemID] = append(out[line.MenuItemID], line)
	}
	return out, rows.Err()
}

// MenuItemsUsingAny returns every dish using any of these ingredients.
//
// DISTINCT because a dish can use several of them, and the caller wants a set.
// One placeholder via ANY($1) rather than a generated IN list, so Postgres caches
// the plan instead of recompiling per distinct ingredient count.
func (r *RecipeRepository) MenuItemsUsingAny(ctx context.Context, ingredientIDs []id.ID) ([]id.ID, error) {
	if len(ingredientIDs) == 0 {
		return nil, nil
	}

	const q = `
		SELECT DISTINCT menu_item_id FROM recipes
		 WHERE ingredient_id = ANY($1) AND is_active`

	rows, err := r.db.Q(ctx).Query(ctx, q, ingredientIDs)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("inventory: menu items using ingredient: %w", err))
	}
	defer rows.Close()

	var out []id.ID
	for rows.Next() {
		var itemID id.ID
		if err := rows.Scan(&itemID); err != nil {
			return nil, apperr.Internal(fmt.Errorf("inventory: scan menu item id: %w", err))
		}
		out = append(out, itemID)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Reservations
// ---------------------------------------------------------------------------

type ReservationRepository struct {
	db *database.DB
}

func NewReservationRepository(db *database.DB) *ReservationRepository {
	return &ReservationRepository{db: db}
}

// Create records a hold.
//
// The partial unique index on (order_id, ingredient_id) WHERE status='held' makes
// this idempotent: a retried checkout cannot double-hold the same ingredient for
// the same order.
func (r *ReservationRepository) Create(ctx context.Context, res *domain.Reservation) error {
	const q = `
		INSERT INTO stock_reservations (id, order_id, ingredient_id, kitchen_id,
		                                quantity, status, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		res.ID, res.OrderID, res.IngredientID, res.KitchenID,
		res.Quantity, string(res.Status), res.ExpiresAt, res.CreatedAt,
	); err != nil {
		if database.IsUniqueViolation(err, "stock_reservations_order_ingredient_held_key") {
			return apperr.Conflict("ALREADY_RESERVED",
				"Stock for this order is already reserved.")
		}
		return apperr.Internal(fmt.Errorf("inventory: create reservation: %w", err))
	}
	return nil
}

func (r *ReservationRepository) ListHeldForOrder(ctx context.Context, orderID id.ID) ([]*domain.Reservation, error) {
	const q = `
		SELECT id, order_id, ingredient_id, kitchen_id, quantity, status,
		       expires_at, released_at, consumed_at, created_at
		FROM stock_reservations
		WHERE order_id = $1 AND status = 'held'
		ORDER BY ingredient_id`
	return r.queryReservations(ctx, q, orderID)
}

// FindExpired backs the reaper. Matches the partial index on held reservations.
func (r *ReservationRepository) FindExpired(ctx context.Context, before time.Time, limit int) ([]*domain.Reservation, error) {
	const q = `
		SELECT id, order_id, ingredient_id, kitchen_id, quantity, status,
		       expires_at, released_at, consumed_at, created_at
		FROM stock_reservations
		WHERE status = 'held' AND expires_at < $1
		ORDER BY expires_at LIMIT $2`
	return r.queryReservations(ctx, q, before, limit)
}

func (r *ReservationRepository) queryReservations(ctx context.Context, q string, args ...any) ([]*domain.Reservation, error) {
	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("inventory: query reservations: %w", err))
	}
	defer rows.Close()

	var out []*domain.Reservation
	for rows.Next() {
		var (
			res    domain.Reservation
			status string
		)
		if err := rows.Scan(&res.ID, &res.OrderID, &res.IngredientID, &res.KitchenID,
			&res.Quantity, &status, &res.ExpiresAt, &res.ReleasedAt, &res.ConsumedAt,
			&res.CreatedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("inventory: scan reservation: %w", err))
		}
		res.Status = domain.ReservationStatus(status)
		out = append(out, &res)
	}
	return out, rows.Err()
}

func (r *ReservationRepository) MarkStatus(ctx context.Context, reservationID id.ID, status domain.ReservationStatus, at time.Time) error {
	// Only a held reservation may change state, so a double-consume is a no-op
	// rather than corrupting the ledger.
	const q = `
		UPDATE stock_reservations
		SET status = $2,
		    consumed_at = CASE WHEN $2 = 'consumed' THEN $3 ELSE consumed_at END,
		    released_at = CASE WHEN $2 IN ('released','expired') THEN $3 ELSE released_at END
		WHERE id = $1 AND status = 'held'`

	if _, err := r.db.Q(ctx).Exec(ctx, q, reservationID, string(status), at); err != nil {
		return apperr.Internal(fmt.Errorf("inventory: mark reservation %s: %w", status, err))
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

var (
	_ domain.IngredientRepository  = (*IngredientRepository)(nil)
	_ domain.RecipeRepository      = (*RecipeRepository)(nil)
	_ domain.ReservationRepository = (*ReservationRepository)(nil)
)
