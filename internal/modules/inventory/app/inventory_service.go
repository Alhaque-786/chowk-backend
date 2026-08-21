// Package app holds the inventory module's use cases.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/inventory/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// reservationTTL bounds how long a hold survives.
//
// Must comfortably exceed how long a UPI or 3-D Secure flow can legitimately
// take, or the reaper would release stock for orders that are about to pay.
const reservationTTL = 30 * time.Minute

// InventoryService implements contracts.StockReserver plus staff stock
// management.
type InventoryService struct {
	db           *database.DB
	ingredients  domain.IngredientRepository
	recipes      domain.RecipeRepository
	reservations domain.ReservationRepository
	events       eventbus.Publisher
	clock        clock.Clock
}

func NewInventoryService(
	db *database.DB,
	ingredients domain.IngredientRepository,
	recipes domain.RecipeRepository,
	reservations domain.ReservationRepository,
	events eventbus.Publisher,
	clk clock.Clock,
) *InventoryService {
	return &InventoryService{
		db: db, ingredients: ingredients, recipes: recipes,
		reservations: reservations, events: events, clock: clk,
	}
}

// Reserve holds stock for an order.
//
// THIS IS THE NO-OVERSELL GUARANTEE. Called from inside ordering's checkout
// transaction, so it participates in that commit: if anything later in checkout
// fails, the reservation rolls back with it and no stock is orphaned.
//
// The algorithm, and why each step is necessary:
//
//  1. Explode the bill of materials and AGGREGATE per ingredient. Two order lines
//     of the same dish both need rice; checking them independently passes twice
//     against the same portion.
//  2. Lock every needed ingredient row FOR UPDATE, ORDERED BY ID. The lock
//     serialises concurrent checkouts; the ordering prevents them deadlocking
//     against each other.
//  3. Only THEN compare against available stock. Checking before locking is a
//     read of a value that can change before the write — which is exactly the
//     race this exists to close.
//  4. Increment `reserved` and write the hold rows.
func (s *InventoryService) Reserve(ctx context.Context, orderID, kitchenID id.ID, lines []contracts.ReservationLine) error {
	if len(lines) == 0 {
		return nil
	}

	// Collapse duplicate menu items from the caller before exploding, so the
	// aggregation is correct even if ordering passes the same item twice.
	servingsByItem := make(map[id.ID]int32, len(lines))
	itemIDs := make([]id.ID, 0, len(lines))
	for _, line := range lines {
		if _, seen := servingsByItem[line.MenuItemID]; !seen {
			itemIDs = append(itemIDs, line.MenuItemID)
		}
		servingsByItem[line.MenuItemID] += line.Quantity
	}

	recipesByItem, err := s.recipes.LoadForMenuItems(ctx, itemIDs)
	if err != nil {
		return err
	}

	requirements := domain.ExplodeBOM(servingsByItem, recipesByItem)
	if len(requirements) == 0 {
		// No dish in this order has a BOM (all stocked as finished goods). Nothing
		// to reserve, and that is a success, not an error.
		return nil
	}

	ingredientIDs := make([]id.ID, 0, len(requirements))
	for _, req := range requirements {
		ingredientIDs = append(ingredientIDs, req.IngredientID)
	}

	// --- The critical section ---
	stock, err := s.ingredients.LockForUpdate(ctx, ingredientIDs)
	if err != nil {
		return err
	}

	now := s.clock.Now()
	expiresAt := now.Add(reservationTTL)

	for _, req := range requirements {
		ingredient, ok := stock[req.IngredientID]
		if !ok {
			// A recipe pointing at a missing ingredient is a data error. Refuse
			// rather than silently skipping it, which would oversell.
			return apperr.Internal(fmt.Errorf(
				"inventory: recipe references unknown ingredient %s", req.IngredientID))
		}

		if ingredient.Available() < req.Quantity {
			// An optional ingredient must not block the order. Reserve what exists
			// and carry on: the garnish is missing, the meal is not.
			if req.IsOptional {
				continue
			}
			metrics.StockReservationTotal.WithLabelValues("insufficient").Inc()
			// Name the ingredient. "Out of stock" forces the kitchen to guess what
			// to restock.
			return apperr.Conflict("INSUFFICIENT_STOCK",
				fmt.Sprintf("%s has just run out. Please adjust your order.", req.IngredientName))
		}

		if err := s.ingredients.ApplyDelta(ctx, req.IngredientID, 0, req.Quantity); err != nil {
			return err
		}

		if err := s.reservations.Create(ctx, &domain.Reservation{
			ID:           id.New(),
			OrderID:      orderID,
			IngredientID: req.IngredientID,
			KitchenID:    kitchenID,
			Quantity:     req.Quantity,
			Status:       domain.ReservationHeld,
			ExpiresAt:    expiresAt,
			CreatedAt:    now,
		}); err != nil {
			return err
		}
	}

	metrics.StockReservationTotal.WithLabelValues("reserved").Inc()
	return nil
}

// Consume converts held reservations into actual consumption.
//
// Called when the kitchen starts cooking. At that point the ingredients are
// genuinely spent: on_hand drops, the hold is released, and a ledger entry
// records it. Doing this at `placed` would be too early (the order may still be
// cancelled) and at `ready` too late (stock would read as available while it is
// in a pan).
func (s *InventoryService) Consume(ctx context.Context, orderID id.ID) error {
	held, err := s.reservations.ListHeldForOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		// Idempotent: already consumed, or the order had no BOM. Either way there
		// is nothing to do and that is success — this runs from an at-least-once
		// event handler.
		return nil
	}

	ingredientIDs := make([]id.ID, 0, len(held))
	for _, r := range held {
		ingredientIDs = append(ingredientIDs, r.IngredientID)
	}

	stock, err := s.ingredients.LockForUpdate(ctx, ingredientIDs)
	if err != nil {
		return err
	}

	now := s.clock.Now()
	var lowStock []*domain.Ingredient

	for _, reservation := range held {
		ingredient, ok := stock[reservation.IngredientID]
		if !ok {
			continue
		}

		// on_hand down (it is used), reserved down (the hold is discharged). Both
		// in one delta so the DB's `reserved <= on_hand` constraint never sees an
		// intermediate state that violates it.
		if err := s.ingredients.ApplyDelta(ctx, reservation.IngredientID,
			-reservation.Quantity, -reservation.Quantity); err != nil {
			return err
		}

		balanceAfter := ingredient.OnHand - reservation.Quantity
		orderRef := reservation.OrderID

		if err := s.ingredients.AppendMovement(ctx, &domain.StockMovement{
			ID:            id.New(),
			IngredientID:  reservation.IngredientID,
			KitchenID:     reservation.KitchenID,
			Quantity:      -reservation.Quantity, // signed: consumption is negative
			Type:          domain.MovementConsumption,
			BalanceAfter:  balanceAfter,
			ReferenceType: "order",
			ReferenceID:   &orderRef,
			UnitCost:      ingredient.UnitCost,
			CreatedAt:     now,
		}); err != nil {
			return err
		}

		if err := s.reservations.MarkStatus(ctx, reservation.ID, domain.ReservationConsumed, now); err != nil {
			return err
		}

		// Snapshot the post-consumption state for the alerts below.
		ingredient.OnHand = balanceAfter
		ingredient.Reserved -= reservation.Quantity
		if ingredient.NeedsReorder() {
			lowStock = append(lowStock, ingredient)
		}
	}

	// Alerts are published, not acted on here. Inventory does not decide what a
	// low-stock warning means -- notification and analytics subscribe.
	for _, ingredient := range lowStock {
		if err := s.publishStockLow(ctx, ingredient); err != nil {
			return err
		}
	}

	// Recompute sold-out AFTER the balances have moved, so the decision is based
	// on what is actually left.
	return s.refreshSoldOut(ctx, held)
}

// Release returns held stock, on cancellation or expiry.
func (s *InventoryService) Release(ctx context.Context, orderID id.ID) error {
	held, err := s.reservations.ListHeldForOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		return nil // idempotent
	}

	ingredientIDs := make([]id.ID, 0, len(held))
	for _, r := range held {
		ingredientIDs = append(ingredientIDs, r.IngredientID)
	}
	if _, err := s.ingredients.LockForUpdate(ctx, ingredientIDs); err != nil {
		return err
	}

	now := s.clock.Now()
	for _, reservation := range held {
		// Only `reserved` moves. on_hand never changed, because the food was never
		// cooked -- so no ledger entry either. The ledger records physical
		// movement, and a released hold is not one.
		if err := s.ingredients.ApplyDelta(ctx, reservation.IngredientID, 0, -reservation.Quantity); err != nil {
			return err
		}
		if err := s.reservations.MarkStatus(ctx, reservation.ID, domain.ReservationReleased, now); err != nil {
			return err
		}
	}

	metrics.StockReservationTotal.WithLabelValues("released").Inc()

	// Stock is available again, so a dish marked sold out may now be orderable.
	return s.refreshSoldOut(ctx, held)
}

// refreshSoldOut recomputes availability for every dish touched by these
// reservations and publishes menu_item.sold_out where servings have hit zero.
//
// Catalog subscribes and flips its flag. Inventory does NOT write to menu_items —
// that would reach across a module boundary. The event is the seam.
func (s *InventoryService) refreshSoldOut(ctx context.Context, reservations []*domain.Reservation) error {
	// One query for every affected dish, not one per ingredient. This runs inside
	// the checkout and consume transactions, so each avoided round trip is time
	// not spent holding row locks that concurrent checkouts are blocked on.
	ingredientIDs := make([]id.ID, 0, len(reservations))
	seen := make(map[id.ID]struct{}, len(reservations))
	for _, r := range reservations {
		if _, dup := seen[r.IngredientID]; dup {
			continue
		}
		seen[r.IngredientID] = struct{}{}
		ingredientIDs = append(ingredientIDs, r.IngredientID)
	}

	itemIDs, err := s.recipes.MenuItemsUsingAny(ctx, ingredientIDs)
	if err != nil {
		return err
	}
	if len(itemIDs) == 0 {
		return nil
	}

	recipesByItem, err := s.recipes.LoadForMenuItems(ctx, itemIDs)
	if err != nil {
		return err
	}

	// Gather the ingredient rows once for all affected dishes.
	needed := make(map[id.ID]struct{})
	for _, recipes := range recipesByItem {
		for _, r := range recipes {
			needed[r.IngredientID] = struct{}{}
		}
	}
	lookupIDs := make([]id.ID, 0, len(needed))
	for ingID := range needed {
		lookupIDs = append(lookupIDs, ingID)
	}

	// Already locked by the caller within this transaction, so this is a cheap
	// re-read of rows we hold.
	stock, err := s.ingredients.LockForUpdate(ctx, lookupIDs)
	if err != nil {
		return err
	}

	for itemID, recipes := range recipesByItem {
		servings := domain.ServingsAvailable(recipes, stock)
		// -1 means "not governed by ingredients"; only zero is sold out.
		if servings != 0 {
			continue
		}
		kitchenID := id.Nil
		if len(recipes) > 0 {
			if ing, ok := stock[recipes[0].IngredientID]; ok {
				kitchenID = ing.KitchenID
			}
		}

		evt, err := eventbus.New(contracts.EventItemSoldOut, itemID, contracts.ItemSoldOutPayload{
			KitchenID:  kitchenID,
			MenuItemID: itemID,
			Reason:     "ingredients exhausted",
		})
		if err != nil {
			return err
		}
		evt.TraceID = httpx.TraceIDFrom(ctx)
		if err := s.events.Publish(ctx, evt); err != nil {
			return err
		}
	}
	return nil
}

func (s *InventoryService) publishStockLow(ctx context.Context, i *domain.Ingredient) error {
	evt, err := eventbus.New(contracts.EventStockLow, i.ID, contracts.StockLowPayload{
		KitchenID:    i.KitchenID,
		IngredientID: i.ID,
		Name:         i.Name,
		OnHand:       i.OnHand,
		ReorderLevel: i.ReorderLevel,
		Unit:         string(i.Unit),
	})
	if err != nil {
		return err
	}
	evt.TraceID = httpx.TraceIDFrom(ctx)
	return s.events.Publish(ctx, evt)
}

// ---------------------------------------------------------------------------
// Staff-facing stock management
// ---------------------------------------------------------------------------

type ReceiveStockInput struct {
	IngredientID  id.ID
	Quantity      float64
	UnitCostMinor int64
	Note          string
	ActorID       id.ID
}

// ReceiveStock records a delivery from a supplier.
//
// Also updates the weighted-average unit cost, which is the number that decides
// whether a dish is actually profitable. Using the latest purchase price instead
// would make food-cost swing with every delivery.
func (s *InventoryService) ReceiveStock(ctx context.Context, in ReceiveStockInput) error {
	if in.Quantity <= 0 {
		return apperr.Invalid("INVALID_QUANTITY", "Received quantity must be positive.")
	}

	return s.db.InTx(ctx, func(ctx context.Context) error {
		stock, err := s.ingredients.LockForUpdate(ctx, []id.ID{in.IngredientID})
		if err != nil {
			return err
		}
		ingredient, ok := stock[in.IngredientID]
		if !ok {
			return apperr.NotFound("INGREDIENT_NOT_FOUND", "That ingredient could not be found.")
		}

		newCost := money.FromMinor(in.UnitCostMinor, money.INR)
		if in.UnitCostMinor > 0 && ingredient.OnHand > 0 {
			// Weighted average: (existing value + incoming value) / total quantity.
			existingValue := ingredient.UnitCost.Minor() * int64(ingredient.OnHand*1000) / 1000
			incomingValue := in.UnitCostMinor * int64(in.Quantity*1000) / 1000
			totalQty := ingredient.OnHand + in.Quantity
			if totalQty > 0 {
				newCost = money.FromMinor(int64(float64(existingValue+incomingValue)/totalQty), money.INR)
			}
		}

		if err := s.ingredients.ApplyDelta(ctx, in.IngredientID, in.Quantity, 0); err != nil {
			return err
		}

		ingredient.UnitCost = newCost
		ingredient.UpdatedAt = s.clock.Now()
		if err := s.ingredients.Update(ctx, ingredient); err != nil {
			return err
		}

		actor := in.ActorID
		return s.ingredients.AppendMovement(ctx, &domain.StockMovement{
			ID:            id.New(),
			IngredientID:  in.IngredientID,
			KitchenID:     ingredient.KitchenID,
			Quantity:      in.Quantity, // positive: a receipt
			Type:          domain.MovementPurchase,
			BalanceAfter:  ingredient.OnHand + in.Quantity,
			ReferenceType: "manual_receipt",
			UnitCost:      newCost,
			Note:          in.Note,
			ActorID:       &actor,
			CreatedAt:     s.clock.Now(),
		})
	})
}

type AdjustStockInput struct {
	IngredientID id.ID
	// NewOnHand is the counted quantity, not a delta. Physical stock counts are
	// absolute: staff count what is on the shelf, they do not compute a
	// difference.
	NewOnHand float64
	Reason    string
	IsWastage bool
	ActorID   id.ID
}

// AdjustStock corrects the balance after a physical count or records wastage.
func (s *InventoryService) AdjustStock(ctx context.Context, in AdjustStockInput) error {
	if in.NewOnHand < 0 {
		return apperr.Invalid("INVALID_QUANTITY", "Counted stock cannot be negative.")
	}
	if in.Reason == "" {
		// An unexplained adjustment is unauditable, and adjustments are exactly
		// where shrinkage hides.
		return apperr.Invalid("REASON_REQUIRED", "Please give a reason for this adjustment.")
	}

	return s.db.InTx(ctx, func(ctx context.Context) error {
		stock, err := s.ingredients.LockForUpdate(ctx, []id.ID{in.IngredientID})
		if err != nil {
			return err
		}
		ingredient, ok := stock[in.IngredientID]
		if !ok {
			return apperr.NotFound("INGREDIENT_NOT_FOUND", "That ingredient could not be found.")
		}

		delta := in.NewOnHand - ingredient.OnHand
		if delta == 0 {
			return nil
		}
		// Cannot count below what is already promised to open orders, or the DB's
		// reserved <= on_hand constraint fails with a confusing error.
		if in.NewOnHand < ingredient.Reserved {
			return apperr.Conflict("BELOW_RESERVED",
				fmt.Sprintf("%.3f %s is already reserved for open orders; stock cannot be set below that.",
					ingredient.Reserved, ingredient.Unit))
		}

		if err := s.ingredients.ApplyDelta(ctx, in.IngredientID, delta, 0); err != nil {
			return err
		}

		movementType := domain.MovementAdjustment
		if in.IsWastage && delta < 0 {
			movementType = domain.MovementWastage
		}

		actor := in.ActorID
		if err := s.ingredients.AppendMovement(ctx, &domain.StockMovement{
			ID:            id.New(),
			IngredientID:  in.IngredientID,
			KitchenID:     ingredient.KitchenID,
			Quantity:      delta,
			Type:          movementType,
			BalanceAfter:  in.NewOnHand,
			ReferenceType: "manual_adjustment",
			UnitCost:      ingredient.UnitCost,
			Note:          in.Reason,
			ActorID:       &actor,
			CreatedAt:     s.clock.Now(),
		}); err != nil {
			return err
		}

		ingredient.OnHand = in.NewOnHand
		if ingredient.NeedsReorder() {
			if err := s.publishStockLow(ctx, ingredient); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *InventoryService) ListForKitchen(ctx context.Context, kitchenID id.ID) ([]*domain.Ingredient, error) {
	return s.ingredients.ListForKitchen(ctx, kitchenID, true)
}

func (s *InventoryService) ListLowStock(ctx context.Context, kitchenID id.ID) ([]*domain.Ingredient, error) {
	return s.ingredients.ListLowStock(ctx, kitchenID)
}

func (s *InventoryService) ListMovements(ctx context.Context, ingredientID id.ID, limit int) ([]*domain.StockMovement, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.ingredients.ListMovements(ctx, ingredientID, limit)
}

// ReapExpiredReservations frees holds from abandoned checkouts.
//
// Runs in the worker. Without it one customer who opens checkout and walks away
// keeps the last portion reserved indefinitely.
func (s *InventoryService) ReapExpiredReservations(ctx context.Context) (int, error) {
	expired, err := s.reservations.FindExpired(ctx, s.clock.Now(), 200)
	if err != nil {
		return 0, err
	}

	// Group by order so each release is one transaction, matching how Release
	// works and keeping lock scope small.
	byOrder := make(map[id.ID]struct{})
	for _, r := range expired {
		byOrder[r.OrderID] = struct{}{}
	}

	reaped := 0
	for orderID := range byOrder {
		err := s.db.InTx(ctx, func(ctx context.Context) error {
			return s.Release(ctx, orderID)
		})
		if err != nil {
			// One problematic order must not stop the rest.
			logger.From(ctx).Error("releasing expired reservation failed",
				"order_id", orderID, "error", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

var _ contracts.StockReserver = (*InventoryService)(nil)
