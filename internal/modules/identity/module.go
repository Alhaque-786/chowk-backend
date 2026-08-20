// Package identity is the module's assembly file and its only public surface.
//
// Every module follows this exact shape:
//
//  1. New(deps) builds the module's internals -- repositories, services,
//     handlers -- and returns a struct.
//  2. PublicRoutes() / PrivateRoutes() / AdminRoutes() expose http.Handlers.
//  3. RegisterSubscribers() wires the module's event handlers.
//  4. Named accessors expose the contracts.* interfaces the module implements
//     for other modules to consume.
//
// The composition root in cmd/api only ever touches these methods. It cannot
// reach a repository or a domain type, which is what enforces the boundary at
// compile time rather than by convention.
package identity

import (
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/config"
	identityhttp "github.com/zevoxa/chowk-backend/internal/modules/identity/adapter/http"
	identitypg "github.com/zevoxa/chowk-backend/internal/modules/identity/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/identity/app"
	"github.com/zevoxa/chowk-backend/internal/modules/identity/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/password"
	"github.com/zevoxa/chowk-backend/internal/platform/token"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

// Deps is everything the module needs from outside. Making it an explicit struct
// rather than a long parameter list means adding a dependency is a visible,
// reviewable change at every call site.
type Deps struct {
	Config *config.Config
	DB     *database.DB
	Cache  cache.Cache
	Events eventbus.Publisher
	Tokens *token.Manager
	Clock  clock.Clock
}

// Module is the assembled identity module.
type Module struct {
	deps Deps

	users    domain.UserRepository
	sessions domain.SessionRepository

	authService *app.AuthService
	authHandler *identityhttp.AuthHandler
}

// New assembles the module. It performs no I/O -- construction must not fail, so
// startup ordering stays simple and a module cannot half-initialise.
func New(deps Deps) *Module {
	users := identitypg.NewUserRepository(deps.DB)
	sessions := identitypg.NewSessionRepository(deps.DB)

	hasher := password.NewHasher(deps.Config.Auth.BcryptCost)

	authService := app.NewAuthService(
		deps.DB, users, sessions, deps.Tokens, hasher,
		deps.Cache, deps.Events, deps.Clock,
		deps.Config.Auth.OTPTTL, deps.Config.Auth.OTPMaxAttempts,
	)

	return &Module{
		deps:        deps,
		users:       users,
		sessions:    sessions,
		authService: authService,
		// Expose OTPs in responses outside production only, so local dev needs
		// no SMS provider.
		authHandler: identityhttp.NewAuthHandler(authService, !deps.Config.IsProduction()),
	}
}

func (m *Module) Name() string { return "identity" }

// AuthRoutes returns the /auth subtree: register, login, OTP, refresh (public)
// plus logout and logout-all (authenticated).
//
// The composition root passes its authenticate middleware in, so this module
// stays independent of how tokens are verified.
func (m *Module) AuthRoutes(authenticate func(http.Handler) http.Handler) http.Handler {
	return m.authHandler.Routes(authenticate)
}

// RegisterSubscribers wires this module's event handlers.
//
// Identity currently publishes more than it consumes. The hook exists from day
// one so that adding a consumer later is a one-line change here rather than a
// new plumbing path through the composition root.
func (m *Module) RegisterSubscribers(_ eventbus.Subscriber) {}

// AuthService is exposed so the composition root can build the auth middleware,
// which needs to verify tokens.
func (m *Module) AuthService() *app.AuthService { return m.authService }
