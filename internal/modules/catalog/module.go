// Package catalog is the module's assembly file and only public surface.
//
// Catalog owns kitchens, menus and prices. It is the module ordering leans on
// most: it implements contracts.PriceQuoter (server-side pricing, never trust
// the client) and contracts.KitchenAvailability (can this kitchen take an order
// right now).
package catalog

import (
	"context"
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	cataloghttp "github.com/zevoxa/chowk-backend/internal/modules/catalog/adapter/http"
	catalogpg "github.com/zevoxa/chowk-backend/internal/modules/catalog/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog/app"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type Deps struct {
	Config *config.Config
	DB     *database.DB
	Cache  cache.Cache
	Events eventbus.Publisher
	Clock  clock.Clock
}

type Module struct {
	deps Deps

	kitchens domain.KitchenRepository
	menus    domain.MenuRepository

	service *app.CatalogService
	handler *cataloghttp.Handler
}

func New(deps Deps) *Module {
	kitchens := catalogpg.NewKitchenRepository(deps.DB)
	menus := catalogpg.NewMenuRepository(deps.DB)

	// The stampede-protected loader, not the raw cache: the menu is the hottest
	// read in the system and a cache expiry during the lunch rush would otherwise
	// send every concurrent request straight at Postgres.
	loader := cache.NewLoader(deps.Cache)

	service := app.NewCatalogService(deps.DB, kitchens, menus, loader, deps.Cache, deps.Events, deps.Clock)

	return &Module{
		deps:     deps,
		kitchens: kitchens,
		menus:    menus,
		service:  service,
		handler:  cataloghttp.NewHandler(service),
	}
}

func (m *Module) Name() string { return "catalog" }

// PublicRoutes: browse kitchens and menus without signing in.
func (m *Module) PublicRoutes() http.Handler { return m.handler.PublicRoutes() }

// KitchenRoutes: staff menu management. Mount behind Authenticate + RequireRole.
func (m *Module) KitchenRoutes() http.Handler { return m.handler.KitchenRoutes() }

// RegisterSubscribers wires catalog's event handlers.
//
// menu_item.sold_out comes from inventory. Catalog reacts by flipping the
// is_sold_out flag and invalidating the cached menu — it does NOT reach into
// inventory to ask about stock. That inversion is what keeps the two modules
// independently deployable.
func (m *Module) RegisterSubscribers(sub eventbus.Subscriber) {
	sub.Subscribe(contracts.EventItemSoldOut, "catalog.mark_sold_out",
		func(ctx context.Context, e eventbus.Event) error {
			payload, err := eventbus.Decode[contracts.ItemSoldOutPayload](e)
			if err != nil {
				return err
			}
			// Idempotent: setting sold_out on an already-sold-out item is a no-op,
			// which matters because outbox delivery is at-least-once.
			if err := m.menus.SetSoldOut(ctx, payload.MenuItemID, true); err != nil {
				return err
			}
			return m.service.InvalidateMenu(ctx, payload.KitchenID)
		})
}

// PriceQuoter is the contract ordering depends on for authoritative pricing.
func (m *Module) PriceQuoter() contracts.PriceQuoter { return m.service }

// KitchenAvailability answers "can this kitchen accept an order right now".
func (m *Module) KitchenAvailability() contracts.KitchenAvailability { return m.service }

// Service is exposed for the seeder and for admin tooling that legitimately
// needs more than the narrow contracts.
func (m *Module) Service() *app.CatalogService { return m.service }

// KitchenRepo and MenuRepo are exposed ONLY for the seed command, which has to
// create kitchens and menu items directly. Application code must go through
// Service.
func (m *Module) KitchenRepo() domain.KitchenRepository { return m.kitchens }
func (m *Module) MenuRepo() domain.MenuRepository       { return m.menus }

// FirstPartyMerchantID is the merchant seeded by migration 0006 — Khansama.
// Kept here so the seeder does not hardcode a UUID literal in two places.
var FirstPartyMerchantID = id.MustParse("00000000-0000-0000-0000-000000000001")
