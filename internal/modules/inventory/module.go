// Package inventory is the module's assembly file and only public surface.
//
// Inventory implements contracts.StockReserver, which is what closes the
// overselling window: two customers buying the last portion simultaneously now
// serialise on a row lock instead of both succeeding.
package inventory

import (
	"context"
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	inventoryhttp "github.com/zevoxa/chowk-backend/internal/modules/inventory/adapter/http"
	inventorypg "github.com/zevoxa/chowk-backend/internal/modules/inventory/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/inventory/app"
	"github.com/zevoxa/chowk-backend/internal/modules/inventory/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

type Deps struct {
	Config *config.Config
	DB     *database.DB
	Events eventbus.Publisher
	Clock  clock.Clock
}

type Module struct {
	deps Deps

	ingredients  domain.IngredientRepository
	recipes      domain.RecipeRepository
	reservations domain.ReservationRepository

	service *app.InventoryService
	handler *inventoryhttp.Handler
}

func New(deps Deps) *Module {
	ingredients := inventorypg.NewIngredientRepository(deps.DB)
	recipes := inventorypg.NewRecipeRepository(deps.DB)
	reservations := inventorypg.NewReservationRepository(deps.DB)

	service := app.NewInventoryService(deps.DB, ingredients, recipes, reservations, deps.Events, deps.Clock)

	return &Module{
		deps:         deps,
		ingredients:  ingredients,
		recipes:      recipes,
		reservations: reservations,
		service:      service,
		handler:      inventoryhttp.NewHandler(service),
	}
}

func (m *Module) Name() string { return "inventory" }

// KitchenRoutes are staff stock management. Mount behind Authenticate +
// RequireRole.
func (m *Module) KitchenRoutes() http.Handler { return m.handler.KitchenRoutes() }

// RegisterSubscribers wires inventory's event handlers.
//
// Consumption is driven by order.preparing rather than being called directly by
// ordering. That is the inversion that matters: ordering announces that cooking
// started, and inventory decides what that means for stock. Ordering does not
// need to know inventory exists.
//
// Reserve and Release are the exception -- they are synchronous, because checkout
// genuinely cannot proceed without knowing whether stock exists, and must be in
// the same transaction as the order.
func (m *Module) RegisterSubscribers(sub eventbus.Subscriber) {
	sub.Subscribe(contracts.EventOrderPreparing, "inventory.consume_stock",
		func(ctx context.Context, e eventbus.Event) error {
			payload, err := eventbus.Decode[contracts.OrderStatusChangedPayload](e)
			if err != nil {
				return err
			}
			// Idempotent: Consume finds no held reservations on a redelivery and
			// returns success, which matters because delivery is at-least-once.
			return m.deps.DB.InTx(ctx, func(ctx context.Context) error {
				return m.service.Consume(ctx, payload.OrderID)
			})
		})
}

// StockReserver is the contract ordering depends on. Replacing
// contracts.NullStockReserver with this in the composition root is what switches
// oversell protection on.
func (m *Module) StockReserver() contracts.StockReserver { return m.service }

// Service is exposed for the worker (reservation reaper) and the seeder.
func (m *Module) Service() *app.InventoryService { return m.service }

// Repos are exposed ONLY for the seeder, which creates ingredients and recipes
// directly. Application code goes through Service.
func (m *Module) IngredientRepo() domain.IngredientRepository { return m.ingredients }
func (m *Module) RecipeRepo() domain.RecipeRepository         { return m.recipes }
