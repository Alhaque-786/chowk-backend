package httpx

import (
	"context"
	"net/http"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type (
	traceIDKey   struct{}
	principalKey struct{}
)

// Role is the authorization role carried in the access token.
//
// Roles live in the transport layer rather than in the identity module because
// every module's middleware needs them, and importing identity from ordering
// would break module independence.
type Role string

const (
	RoleCustomer      Role = "customer"       // orders food
	RoleKitchenStaff  Role = "kitchen_staff"  // marks items prepared
	RoleKitchenAdmin  Role = "kitchen_admin"  // manages menu, inventory, hours
	RoleRider         Role = "rider"          // delivers orders
	RoleSupport       Role = "support"        // reads orders, issues refunds
	RolePlatformAdmin Role = "platform_admin" // full access
)

// Principal is the authenticated caller, decoded from the access token. It never
// hits the database -- everything needed for authorization is in the token.
type Principal struct {
	UserID    id.ID
	Roles     []Role
	KitchenID id.ID // set for staff/admin, scopes them to one kitchen
	SessionID id.ID // lets a single session be revoked without a password reset
}

// HasRole reports whether the principal holds a role. A platform_admin
// implicitly satisfies every check, so admin tooling does not need every role
// enumerated on its token.
func (p Principal) HasRole(roles ...Role) bool {
	for _, held := range p.Roles {
		if held == RolePlatformAdmin {
			return true
		}
		for _, want := range roles {
			if held == want {
				return true
			}
		}
	}
	return false
}

func (p Principal) IsZero() bool { return p.UserID == id.Nil }

// WithPrincipal attaches the authenticated caller to the request context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the caller if authenticated. Handlers on protected
// routes can use MustPrincipal instead.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && !p.IsZero()
}

// MustPrincipal returns the caller or an unauthorized error.
//
// It returns an error rather than panicking so that a route accidentally
// registered outside the auth middleware fails as a clean 401 instead of a 500.
func MustPrincipal(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return Principal{}, apperr.Unauthorized("NOT_AUTHENTICATED", "You must be signed in to do this.")
	}
	return p, nil
}

// WithTraceID attaches the request's correlation id.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceIDFrom returns the correlation id, or "" outside a request.
func TraceIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(traceIDKey{}).(string)
	return v
}

// URLParamID parses a UUID path parameter, returning a 400-shaped error rather
// than letting a malformed id reach the database.
func URLParamID(r *http.Request, param string, extract func(*http.Request, string) string) (id.ID, error) {
	raw := extract(r, param)
	if raw == "" {
		return id.Nil, apperr.Invalid("MISSING_PATH_PARAM", "Missing "+param+" in the URL.")
	}
	parsed, err := id.Parse(raw)
	if err != nil {
		return id.Nil, apperr.Invalid("INVALID_ID", param+" is not a valid identifier.").WithCause(err)
	}
	return parsed, nil
}
