// Package sourcing's assembly file and only public surface.
package sourcing

import (
	"net/http"

	sourcinghttp "github.com/zevoxa/chowk-backend/internal/modules/sourcing/adapter/http"
	sourcingpg "github.com/zevoxa/chowk-backend/internal/modules/sourcing/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/app"
	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

type Deps struct {
	DB    *database.DB
	Clock clock.Clock
}

type Module struct {
	deps Deps

	leads   domain.LeadRepository
	service *app.SourcingService
	handler *sourcinghttp.Handler
}

func New(deps Deps) *Module {
	leads := sourcingpg.NewLeadRepository(deps.DB)
	service := app.NewSourcingService(leads, deps.Clock)

	return &Module{
		deps:    deps,
		leads:   leads,
		service: service,
		handler: sourcinghttp.NewHandler(service),
	}
}

func (m *Module) Name() string { return "sourcing" }

// PublicRoutes is the submission form. See the handler for why it is public.
func (m *Module) PublicRoutes() http.Handler { return m.handler.PublicRoutes() }

// AdminRoutes is the queue our own team works.
func (m *Module) AdminRoutes() http.Handler { return m.handler.AdminRoutes() }

func (m *Module) Service() *app.SourcingService { return m.service }
