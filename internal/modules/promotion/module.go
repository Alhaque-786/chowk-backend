// Package promotion is the module's assembly file and only public surface.
//
// Implements contracts.DiscountCalculator, which is the last null adapter in the
// system. Replacing it in the composition root is what turns coupons on.
package promotion

import (
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	promotionhttp "github.com/zevoxa/chowk-backend/internal/modules/promotion/adapter/http"
	promotionpg "github.com/zevoxa/chowk-backend/internal/modules/promotion/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion/app"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion/domain"
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

	coupons     domain.CouponRepository
	redemptions domain.RedemptionRepository

	service *app.PromotionService
	handler *promotionhttp.Handler
}

func New(deps Deps) *Module {
	coupons := promotionpg.NewCouponRepository(deps.DB)
	redemptions := promotionpg.NewRedemptionRepository(deps.DB)

	service := app.NewPromotionService(deps.DB, coupons, redemptions, deps.Clock)

	return &Module{
		deps:        deps,
		coupons:     coupons,
		redemptions: redemptions,
		service:     service,
		handler:     promotionhttp.NewHandler(service),
	}
}

func (m *Module) Name() string { return "promotion" }

// CustomerRoutes: browse offers, check a code before checkout.
func (m *Module) CustomerRoutes() http.Handler { return m.handler.CustomerRoutes() }

// AdminRoutes: create and deactivate coupons.
func (m *Module) AdminRoutes() http.Handler { return m.handler.AdminRoutes() }

// RegisterSubscribers wires promotion's event handlers.
//
// Release is driven SYNCHRONOUSLY by ordering through DiscountCalculator, not by
// an event, because it must be in the same transaction as the cancellation. If it
// were eventual, a cancelled order would briefly still hold its coupon use and a
// customer retrying immediately would be told they had already used it.
func (m *Module) RegisterSubscribers(_ eventbus.Subscriber) {}

// DiscountCalculator is the contract ordering depends on.
func (m *Module) DiscountCalculator() contracts.DiscountCalculator { return m.service }

func (m *Module) Service() *app.PromotionService { return m.service }
