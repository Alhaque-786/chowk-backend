// Package payment is the module's assembly file and only public surface.
//
// Payment owns the money. It implements contracts.PaymentInitiator for ordering
// and publishes payment.* events that ordering consumes to promote or cancel an
// order. It never calls ordering directly.
package payment

import (
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	paymentgateway "github.com/zevoxa/chowk-backend/internal/modules/payment/adapter/gateway"
	paymenthttp "github.com/zevoxa/chowk-backend/internal/modules/payment/adapter/http"
	paymentpg "github.com/zevoxa/chowk-backend/internal/modules/payment/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/payment/app"
	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/resilience"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

type Deps struct {
	Config *config.Config
	DB     *database.DB
	Events eventbus.Publisher
	Clock  clock.Clock

	// Users resolves customer contact details for the gateway.
	Users contracts.UserDirectory

	// Guards collects the module's circuit breakers so /status can report them.
	// Optional: nil means the breakers work but are unobservable.
	Guards *resilience.Registry
}

type Module struct {
	deps Deps

	payments domain.PaymentRepository
	webhooks domain.WebhookRepository
	gateway  domain.Gateway

	service *app.PaymentService
	handler *paymenthttp.Handler
}

func New(deps Deps) (*Module, error) {
	payments := paymentpg.NewPaymentRepository(deps.DB)
	webhooks := paymentpg.NewWebhookRepository(deps.DB)

	gateway, signatureHeader, err := buildGateway(deps.Config, deps.Guards)
	if err != nil {
		return nil, err
	}

	service := app.NewPaymentService(
		deps.DB, payments, webhooks, gateway, deps.Users, deps.Events, deps.Clock,
	)

	return &Module{
		deps:     deps,
		payments: payments,
		webhooks: webhooks,
		gateway:  gateway,
		service:  service,
		handler:  paymenthttp.NewHandler(service, signatureHeader),
	}, nil
}

func (m *Module) Name() string { return "payment" }

// WebhookRoutes are public and authenticated by HMAC signature, not by a token.
func (m *Module) WebhookRoutes() http.Handler { return m.handler.WebhookRoutes() }

// CustomerRoutes let a customer check their payment status.
func (m *Module) CustomerRoutes() http.Handler { return m.handler.CustomerRoutes() }

// RegisterSubscribers wires payment's own event handlers.
//
// Payment publishes far more than it consumes: it is the source of truth about
// money, so other modules react to it rather than the reverse.
func (m *Module) RegisterSubscribers(_ eventbus.Subscriber) {}

// PaymentInitiator is the contract ordering depends on. Replacing
// contracts.NullPaymentInitiator with this in the composition root is what
// switches online payment on.
func (m *Module) PaymentInitiator() contracts.PaymentInitiator { return m.service }

// Service is exposed so the worker can run reconciliation.
func (m *Module) Service() *app.PaymentService { return m.service }

// GatewayName reports which provider is live, for the startup log and the admin
// health view. Worth surfacing: "why are payments failing" is often "the mock is
// running in staging".
func (m *Module) GatewayName() string { return m.gateway.Name() }

// MockGateway returns the mock when one is in use, so local tooling can sign a
// test webhook. Returns nil in production.
func (m *Module) MockGateway() *paymentgateway.Mock {
	if mock, ok := m.gateway.(*paymentgateway.Mock); ok {
		return mock
	}
	return nil
}
