// Package delivery is the module's assembly file and only public surface.
//
// Implements contracts.RiderDispatcher. Deliberately thin: a per-drop adapter
// over a logistics provider, or the kitchen's own delivery boy. No fleet, no
// rider app, no assignment algorithm — see domain/delivery.go for why that is an
// economics decision.
package delivery

import (
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	deliveryhttp "github.com/zevoxa/chowk-backend/internal/modules/delivery/adapter/http"
	deliverypg "github.com/zevoxa/chowk-backend/internal/modules/delivery/adapter/postgres"
	deliveryprovider "github.com/zevoxa/chowk-backend/internal/modules/delivery/adapter/provider"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/app"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

type Deps struct {
	Config *config.Config
	DB     *database.DB
	Events eventbus.Publisher
	Clock  clock.Clock
	Users  contracts.UserDirectory
	// Orders answers "is this order this person's", so the tracking view can be
	// scoped to the customer who placed it. Implemented by ordering.
	Orders contracts.OrderOwnership
}

type Module struct {
	deps Deps

	deliveries domain.DeliveryRepository
	provider   domain.Provider

	service *app.DispatchService
	handler *deliveryhttp.Handler
}

func New(deps Deps) *Module {
	deliveries := deliverypg.NewDeliveryRepository(deps.DB)

	// Manual is the default because it is what a single kitchen actually does:
	// its own delivery boy and a phone. Swapping in a 3PL adapter here is the
	// only change needed when per-drop outsourcing becomes cheaper than doing it
	// in-house.
	provider := deliveryprovider.NewManual("", 4000) // ~Rs40 assumed per drop

	service := app.NewDispatchService(deps.DB, deliveries, provider, deps.Users, deps.Orders, deps.Events, deps.Clock)

	return &Module{
		deps:       deps,
		deliveries: deliveries,
		provider:   provider,
		service:    service,
		handler:    deliveryhttp.NewHandler(service),
	}
}

func (m *Module) Name() string { return "delivery" }

// WebhookRoutes are public, authenticated by HMAC signature.
func (m *Module) WebhookRoutes() http.Handler { return m.handler.WebhookRoutes() }

// CustomerRoutes expose live tracking.
func (m *Module) CustomerRoutes() http.Handler { return m.handler.CustomerRoutes() }

// KitchenRoutes are the staff dispatch board.
func (m *Module) KitchenRoutes() http.Handler { return m.handler.KitchenRoutes() }

// RegisterSubscribers wires delivery's event handlers.
//
// Dispatch is triggered synchronously by ordering via RiderDispatcher when an
// order reaches `ready`, not by an event, because the kitchen needs to know
// immediately whether a rider was booked.
func (m *Module) RegisterSubscribers(_ eventbus.Subscriber) {}

// RiderDispatcher is the contract ordering depends on.
func (m *Module) RiderDispatcher() contracts.RiderDispatcher { return m.service }

func (m *Module) Service() *app.DispatchService { return m.service }
