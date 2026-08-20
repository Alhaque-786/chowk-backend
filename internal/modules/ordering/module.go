// Package ordering is the module's assembly file and only public surface.
//
// Ordering is the spine of the system: the only module permitted to create an
// order, and the publisher of the order.* events every other module reacts to.
//
// Note what Deps contains: five contracts interfaces, not five modules. Ordering
// does not import catalog, inventory, promotion, payment or identity. The
// composition root injects implementations — real ones where the module exists,
// null adapters where it does not yet. That is what makes each of them
// independently replaceable, including by a gRPC client later.
package ordering

import (
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	orderinghttp "github.com/zevoxa/chowk-backend/internal/modules/ordering/adapter/http"
	orderingpg "github.com/zevoxa/chowk-backend/internal/modules/ordering/adapter/postgres"
	orderingredis "github.com/zevoxa/chowk-backend/internal/modules/ordering/adapter/redis"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/app"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

type Deps struct {
	Config *config.Config
	DB     *database.DB
	Cache  cache.Cache
	Events eventbus.Publisher
	Clock  clock.Clock

	// Cross-module dependencies, as narrow interfaces.
	Prices    contracts.PriceQuoter
	Kitchens  contracts.KitchenAvailability
	Stock     contracts.StockReserver
	Discounts contracts.DiscountCalculator
	Payments  contracts.PaymentInitiator
	Riders    contracts.RiderDispatcher
	Users     contracts.UserDirectory
}

type Module struct {
	deps Deps

	orders domain.OrderRepository
	carts  domain.CartRepository

	cartService     *app.CartService
	checkoutService *app.CheckoutService
	kitchenService  *app.KitchenOrderService
	handler         *orderinghttp.Handler
}

func New(deps Deps) *Module {
	orders := orderingpg.NewOrderRepository(deps.DB)
	// Carts live in Redis, not Postgres: high-write, short-lived, mostly
	// abandoned, and they want to simply expire.
	carts := orderingredis.NewCartRepository(deps.Cache)

	cartService := app.NewCartService(carts, deps.Prices, deps.Kitchens, deps.Clock)

	checkoutService := app.NewCheckoutService(
		deps.DB, orders, carts,
		deps.Prices, deps.Kitchens, deps.Stock, deps.Discounts, deps.Payments, deps.Users,
		deps.Events, deps.Clock,
	)

	kitchenService := app.NewKitchenOrderService(
		deps.DB, orders, deps.Stock, deps.Riders, deps.Payments, deps.Events, deps.Clock,
	)

	return &Module{
		deps:            deps,
		orders:          orders,
		carts:           carts,
		cartService:     cartService,
		checkoutService: checkoutService,
		kitchenService:  kitchenService,
		handler:         orderinghttp.NewHandler(cartService, checkoutService, kitchenService),
	}
}

func (m *Module) Name() string { return "ordering" }

// CustomerRoutes: cart, checkout, order tracking. Mount behind Authenticate.
func (m *Module) CustomerRoutes() http.Handler { return m.handler.CustomerRoutes() }

// KitchenRoutes: the staff queue and status transitions.
func (m *Module) KitchenRoutes() http.Handler { return m.handler.KitchenRoutes() }

// RegisterSubscribers wires ordering's event handlers.
//
// Ordering consumes payment events to advance an order past pending_payment. It
// does NOT call the payment module to ask — payment tells it, which is what keeps
// the dependency one-directional.
func (m *Module) RegisterSubscribers(_ eventbus.Subscriber) {
	// payment.captured -> placed and payment.failed -> cancelled are registered
	// once the payment module exists. Declaring the hook now means adding them is
	// a one-line change here rather than new plumbing through the server.
}

// KitchenService is exposed so the worker can run the abandoned-order reaper.
func (m *Module) KitchenService() *app.KitchenOrderService { return m.kitchenService }
