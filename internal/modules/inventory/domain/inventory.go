// Package domain holds the inventory module's entities and ports.
//
// Inventory solves the hardest concurrency problem in the system: two customers
// buying the last portion of biryani at the same instant must not both succeed.
// Everything here is shaped by that.
package domain

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Unit is the measure an ingredient is tracked in.
type Unit string

const (
	UnitGram       Unit = "g"
	UnitKilogram   Unit = "kg"
	UnitMillilitre Unit = "ml"
	UnitLitre      Unit = "l"
	UnitPiece      Unit = "piece"
	UnitPacket     Unit = "packet"
	UnitDozen      Unit = "dozen"
)

func (u Unit) Valid() bool {
	switch u {
	case UnitGram, UnitKilogram, UnitMillilitre, UnitLitre, UnitPiece, UnitPacket, UnitDozen:
		return true
	}
	return false
}

// Ingredient is a tracked stock item.
//
// Quantities are float64, unlike money. That is a deliberate difference: money
// has an exact smallest unit (the paise) and must never drift, whereas ingredient
// quantities are genuinely continuous (0.25 kg of flour) with no natural atom
// across flour, milk and eggs. The database stores NUMERIC(14,3), so precision is
// gram-level and the rounding that matters happens there.
type Ingredient struct {
	ID        id.ID
	KitchenID id.ID
	Name      string
	SKU       string
	Unit      Unit

	// OnHand is what physically exists. Reserved is the part already promised to
	// open orders. Available is the difference, and it is the only number a
	// reservation may draw against.
	OnHand   float64
	Reserved float64

	ReorderLevel float64
	ReorderQty   float64
	UnitCost     money.Amount

	SupplierName  string
	ShelfLifeDays *int32
	IsActive      bool

	Version   int32
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Available is what can still be promised.
func (i *Ingredient) Available() float64 { return i.OnHand - i.Reserved }

// NeedsReorder reports whether stock has fallen to the reorder point.
func (i *Ingredient) NeedsReorder() bool {
	return i.ReorderLevel > 0 && i.OnHand <= i.ReorderLevel
}

func (i *Ingredient) Validate() error {
	err := apperr.Invalid("INVALID_INGREDIENT", "Please check the ingredient fields.")
	bad := false

	if i.Name == "" {
		err.WithField("name", "required")
		bad = true
	}
	if !i.Unit.Valid() {
		err.WithField("unit", "invalid")
		bad = true
	}
	if i.OnHand < 0 {
		err.WithField("on_hand", "must_not_be_negative")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

// RecipeLine is one ingredient in a dish's bill of materials.
type RecipeLine struct {
	ID           id.ID
	MenuItemID   id.ID
	IngredientID id.ID
	// Quantity consumed per single serving, in the ingredient's unit.
	Quantity float64
	// WastageBps is expected loss: peeling, trimming, spillage. Ignoring it makes
	// every food-cost figure optimistic and every reorder point too low.
	WastageBps int32
	// IsOptional means running out must NOT make the dish unorderable. A missing
	// garnish should not take biryani off the menu.
	IsOptional bool

	// Denormalised for display and for building error messages without a join.
	IngredientName string
	Unit           Unit
}

// RequiredFor returns how much this line consumes for a given serving count,
// including expected wastage.
func (r *RecipeLine) RequiredFor(servings int32) float64 {
	base := r.Quantity * float64(servings)
	return base * (1 + float64(r.WastageBps)/10000)
}

// MovementType classifies a stock ledger entry.
type MovementType string

const (
	MovementPurchase    MovementType = "purchase"
	MovementConsumption MovementType = "consumption"
	MovementWastage     MovementType = "wastage"
	MovementAdjustment  MovementType = "adjustment"
	MovementReturn      MovementType = "return"
	MovementTransferIn  MovementType = "transfer_in"
	MovementTransferOut MovementType = "transfer_out"
)

// StockMovement is one entry in the append-only ledger.
//
// Quantity is SIGNED: positive for receipts, negative for consumption and
// wastage. One signed column means the balance is a plain SUM, with no CASE over
// movement types that a newly added type could silently fall outside of.
type StockMovement struct {
	ID           id.ID
	IngredientID id.ID
	KitchenID    id.ID
	Quantity     float64
	Type         MovementType
	// BalanceAfter lets the ledger be audited without replaying it from the
	// beginning.
	BalanceAfter float64

	ReferenceType string
	ReferenceID   *id.ID

	UnitCost  money.Amount
	Note      string
	ActorID   *id.ID
	CreatedAt time.Time
}

// ReservationStatus is the lifecycle of a stock hold.
type ReservationStatus string

const (
	ReservationHeld     ReservationStatus = "held"
	ReservationConsumed ReservationStatus = "consumed"
	ReservationReleased ReservationStatus = "released"
	ReservationExpired  ReservationStatus = "expired"
)

// Reservation is stock promised to an order but not yet consumed.
//
// The two-phase hold is what prevents overselling. Without it, stock is only
// decremented when the kitchen starts cooking, and everything ordered in the gap
// oversells.
type Reservation struct {
	ID           id.ID
	OrderID      id.ID
	IngredientID id.ID
	KitchenID    id.ID
	Quantity     float64
	Status       ReservationStatus
	// ExpiresAt bounds the hold. A customer who opens checkout and walks away
	// would otherwise keep the last portion forever.
	ExpiresAt  time.Time
	ReleasedAt *time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

// Requirement is one aggregated ingredient need for a whole order.
type Requirement struct {
	IngredientID   id.ID
	IngredientName string
	Quantity       float64
	Unit           Unit
	IsOptional     bool
}

// ExplodeBOM converts order lines into aggregated ingredient requirements.
//
// AGGREGATION IS THE POINT. Two separate order lines can both contain biryani —
// one plain, one with extra raita — and both need rice. Checking each line
// against stock independently passes twice against the same portion and
// oversells. This is the single most common inventory bug, and summing first is
// the whole fix.
//
// recipesByItem must contain every item in lines; a missing recipe means the dish
// has no BOM and is treated as needing nothing, which is correct for something
// like a bottled drink that is stocked as a finished good.
func ExplodeBOM(lines map[id.ID]int32, recipesByItem map[id.ID][]RecipeLine) []Requirement {
	// Accumulate into a map keyed by ingredient, so the same ingredient appearing
	// via several dishes sums rather than competing.
	totals := make(map[id.ID]*Requirement)

	for menuItemID, servings := range lines {
		if servings <= 0 {
			continue
		}
		for _, recipe := range recipesByItem[menuItemID] {
			needed := recipe.RequiredFor(servings)

			existing, ok := totals[recipe.IngredientID]
			if !ok {
				totals[recipe.IngredientID] = &Requirement{
					IngredientID:   recipe.IngredientID,
					IngredientName: recipe.IngredientName,
					Quantity:       needed,
					Unit:           recipe.Unit,
					IsOptional:     recipe.IsOptional,
				}
				continue
			}
			existing.Quantity += needed
			// If ANY dish needs this ingredient non-optionally, the aggregate need
			// is non-optional. Otherwise a garnish for one dish could excuse a
			// shortage that blocks another.
			if !recipe.IsOptional {
				existing.IsOptional = false
			}
		}
	}

	out := make([]Requirement, 0, len(totals))
	for _, req := range totals {
		out = append(out, *req)
	}
	return out
}

// ServingsAvailable computes how many servings of a dish can still be made.
//
// The MINIMUM across required ingredients: a dish needs all of them, so the
// scarcest one is the limit. Optional ingredients are skipped — running out of a
// garnish must not report the dish as unavailable.
//
// This is what drives automatic sold-out detection, so a dish disappears from the
// menu BEFORE it is oversold rather than after a customer complains.
func ServingsAvailable(recipes []RecipeLine, stock map[id.ID]*Ingredient) int32 {
	if len(recipes) == 0 {
		// No BOM: availability is not governed by ingredients. Effectively
		// unlimited as far as inventory is concerned.
		return -1
	}

	limit := int32(-1)
	for _, recipe := range recipes {
		if recipe.IsOptional {
			continue
		}
		ingredient, ok := stock[recipe.IngredientID]
		if !ok {
			// A recipe referencing an unknown ingredient is a data error. Report
			// zero rather than guessing, so it surfaces as "sold out" and gets
			// investigated instead of silently overselling.
			return 0
		}

		perServing := recipe.RequiredFor(1)
		if perServing <= 0 {
			continue
		}

		possible := int32(ingredient.Available() / perServing)
		if possible < 0 {
			possible = 0
		}
		if limit == -1 || possible < limit {
			limit = possible
		}
	}
	return limit
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

type IngredientRepository interface {
	Create(ctx context.Context, i *Ingredient) error
	Update(ctx context.Context, i *Ingredient) error
	FindByID(ctx context.Context, ingredientID id.ID) (*Ingredient, error)
	ListForKitchen(ctx context.Context, kitchenID id.ID, activeOnly bool) ([]*Ingredient, error)
	ListLowStock(ctx context.Context, kitchenID id.ID) ([]*Ingredient, error)

	// LockForUpdate reads ingredients with a row lock, ORDERED BY ID.
	//
	// The ordering is not cosmetic. Two concurrent orders locking the same
	// ingredients in different sequences deadlock; a consistent order makes one
	// simply wait for the other. This is the primitive the whole no-oversell
	// guarantee rests on.
	LockForUpdate(ctx context.Context, ingredientIDs []id.ID) (map[id.ID]*Ingredient, error)

	// ApplyDelta adjusts on_hand and/or reserved by signed amounts. Must run
	// inside the transaction that locked the row.
	ApplyDelta(ctx context.Context, ingredientID id.ID, onHandDelta, reservedDelta float64) error

	AppendMovement(ctx context.Context, m *StockMovement) error
	ListMovements(ctx context.Context, ingredientID id.ID, limit int) ([]*StockMovement, error)
}

type RecipeRepository interface {
	Upsert(ctx context.Context, r *RecipeLine) error
	Delete(ctx context.Context, menuItemID, ingredientID id.ID) error
	// LoadForMenuItems batches by item, so exploding a ten-line cart is one query.
	LoadForMenuItems(ctx context.Context, menuItemIDs []id.ID) (map[id.ID][]RecipeLine, error)
	// MenuItemsUsing finds every dish affected by an ingredient, for recomputing
	// sold-out status after stock changes.
	MenuItemsUsing(ctx context.Context, ingredientID id.ID) ([]id.ID, error)
}

type ReservationRepository interface {
	Create(ctx context.Context, r *Reservation) error
	// ListHeldForOrder returns the open holds for an order, for consume/release.
	ListHeldForOrder(ctx context.Context, orderID id.ID) ([]*Reservation, error)
	MarkStatus(ctx context.Context, reservationID id.ID, status ReservationStatus, at time.Time) error
	// FindExpired backs the reaper that frees abandoned holds.
	FindExpired(ctx context.Context, before time.Time, limit int) ([]*Reservation, error)
}
