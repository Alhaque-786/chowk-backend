// Package tenant enforces multi-tenant data isolation.
//
// # WHY THIS EXISTS NOW, WHILE THERE IS STILL ONE KITCHEN
//
// Today every kitchen belongs to us, so a query that forgets `WHERE kitchen_id =
// ?` returns slightly wrong data. Once third-party restaurants are onboarded, that
// same omission shows Domino's their competitor's order volume, margins and
// customer list. It is no longer a bug; it is a breach that ends the platform's
// credibility and probably the contract.
//
// Tenant isolation is the single hardest thing to retrofit in a SaaS backend.
// Every query, every cache key, every report, every export has to be revisited,
// and the failure mode is silent -- nothing errors, the data is just wrong in a
// direction nobody notices until a merchant does. Adding the guard while there is
// one tenant and forty queries is nearly free. Adding it at four hundred queries
// is a quarter of engineering time and you will still miss one.
//
// THE MODEL
//
//	Merchant  -- a restaurant business (the party with a contract and a bank account)
//	  └── Outlet -- one physical kitchen/location. `kitchens` rows are outlets.
//	        └── Menu, inventory, orders, staff
//
// A merchant may run several outlets ("Biryani House, Bandra" and "Biryani House,
// Andheri") sharing a brand and settlement account but with independent stock and
// hours. Modelling outlets as separate merchants would break payouts; modelling
// them as one would break inventory.
//
// # THE RULE
//
// Every query touching merchant-owned data must be scoped through a Scope
// obtained from this package. Repositories should take a Scope, not a raw
// kitchen ID, so "I forgot to filter" becomes a compile error rather than a leak.
package tenant

import (
	"context"

	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/httpx"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/apperr"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/id"
)

// Scope is proof that a caller is authorised for specific tenant data.
//
// It is deliberately unexported-field-only and constructible ONLY through the
// functions below. A repository cannot fabricate one, so a query cannot be run
// unscoped by accident -- the type system carries the authorisation decision.
type Scope struct {
	// merchantID is the owning business. Zero for a platform-wide scope.
	merchantID id.ID
	// outletIDs restricts to specific outlets. Empty with a merchantID set means
	// "all outlets of this merchant".
	outletIDs []id.ID
	// platformWide is true only for platform staff. It is a separate boolean
	// rather than "merchantID == nil" so that an accidentally-zero merchant ID
	// can never silently widen into full access.
	platformWide bool
}

// ForMerchant scopes to one merchant and all its outlets. Used by a merchant
// owner viewing their whole business.
func ForMerchant(merchantID id.ID) (Scope, error) {
	if id.IsNil(merchantID) {
		return Scope{}, apperr.Forbidden("NO_TENANT_SCOPE",
			"Your account is not linked to a restaurant.")
	}
	return Scope{merchantID: merchantID}, nil
}

// ForOutlet scopes to a single outlet. Used by kitchen staff, who should see
// only the location they work at.
func ForOutlet(merchantID, outletID id.ID) (Scope, error) {
	if id.IsNil(outletID) {
		return Scope{}, apperr.Forbidden("NO_TENANT_SCOPE",
			"Your account is not linked to a kitchen.")
	}
	return Scope{merchantID: merchantID, outletIDs: []id.ID{outletID}}, nil
}

// Platform returns an unrestricted scope.
//
// Only ever construct this from a verified platform_admin principal. Every call
// site should be greppable and reviewable, which is why it is a named
// constructor rather than a zero value.
func Platform() Scope {
	return Scope{platformWide: true}
}

// FromPrincipal derives the scope from the authenticated caller.
//
// This is the single translation point from "who is this" to "what may they
// see". Centralising it means the authorisation rule exists once, not once per
// handler.
func FromPrincipal(p httpx.Principal, merchantID id.ID) (Scope, error) {
	switch {
	case p.HasRole(httpx.RolePlatformAdmin):
		return Platform(), nil

	case p.HasRole(httpx.RoleSupport):
		// Support needs cross-merchant read access to resolve customer issues.
		// This is a real privilege-escalation surface: every support read should
		// be written to audit_log so misuse is detectable after the fact.
		return Platform(), nil

	case p.HasRole(httpx.RoleKitchenAdmin):
		// A merchant admin sees all of their own outlets.
		return ForMerchant(merchantID)

	case p.HasRole(httpx.RoleKitchenStaff):
		// Staff are pinned to the one outlet on their token.
		return ForOutlet(merchantID, p.KitchenID)

	default:
		// Customers have no tenant scope at all. They access their own orders
		// through a user-scoped path, never a tenant-scoped one.
		return Scope{}, apperr.Forbidden("NOT_A_MERCHANT_USER",
			"This area is for restaurant partners.")
	}
}

// IsPlatformWide reports whether the scope bypasses tenant filtering.
func (s Scope) IsPlatformWide() bool { return s.platformWide }

func (s Scope) MerchantID() id.ID { return s.merchantID }

// OutletIDs returns the outlet restriction, empty if the scope covers all of the
// merchant's outlets.
func (s Scope) OutletIDs() []id.ID { return s.outletIDs }

// AllowsOutlet reports whether the scope permits access to a specific outlet.
//
// Use this on any request carrying an outlet id from the client. Without it, a
// merchant can pass another merchant's outlet id and read their data -- the
// classic IDOR (insecure direct object reference) vulnerability, and by far the
// most common real-world multi-tenant breach.
func (s Scope) AllowsOutlet(outletID id.ID) bool {
	if s.platformWide {
		return true
	}
	if len(s.outletIDs) == 0 {
		// Merchant-wide scope: the caller may reach any of their outlets, but the
		// query must still filter by merchant_id. Verifying that this outlet
		// actually belongs to the merchant is the repository's job, since only it
		// can see the ownership row.
		return !id.IsNil(s.merchantID)
	}
	for _, allowed := range s.outletIDs {
		if allowed == outletID {
			return true
		}
	}
	return false
}

// RequireOutlet returns a forbidden error if the outlet is out of scope.
//
// The message deliberately says "not found" rather than "not yours": confirming
// that an id exists but belongs to someone else leaks the existence of other
// merchants' outlets and lets an attacker enumerate the platform.
func (s Scope) RequireOutlet(outletID id.ID) error {
	if s.AllowsOutlet(outletID) {
		return nil
	}
	return apperr.NotFound("OUTLET_NOT_FOUND", "That kitchen could not be found.")
}

// SQL returns a WHERE fragment and args that constrain a query to this scope,
// with placeholders starting at argOffset+1.
//
// Repositories call this instead of hand-writing tenant predicates, so the
// filtering logic exists in exactly one place and cannot be subtly different in
// the one query someone wrote in a hurry.
//
//	scopeSQL, scopeArgs := scope.SQL("o.kitchen_id", "k.merchant_id", len(args))
func (s Scope) SQL(outletColumn, merchantColumn string, argOffset int) (string, []any) {
	if s.platformWide {
		// TRUE rather than an empty string, so callers can always concatenate
		// without special-casing and cannot accidentally produce "WHERE AND ...".
		return "TRUE", nil
	}

	if len(s.outletIDs) > 0 {
		// ANY($n) over an array rather than a generated IN (...) list: one
		// placeholder regardless of count, so the query plan is cached instead of
		// recompiled for every distinct number of outlets.
		return outletColumn + " = ANY($" + itoa(argOffset+1) + ")", []any{s.outletIDs}
	}

	if !id.IsNil(s.merchantID) {
		return merchantColumn + " = $" + itoa(argOffset+1), []any{s.merchantID}
	}

	// Fail CLOSED. An unscoped, non-platform Scope must return nothing rather
	// than everything. A bug here should show a merchant an empty dashboard --
	// annoying, obvious, immediately reported -- not the whole platform's data.
	return "FALSE", nil
}

// itoa avoids importing strconv for a single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

type scopeKey struct{}

// Into attaches a scope to the context, so middleware can resolve it once and
// handlers deeper down do not each re-derive it.
func Into(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// From retrieves the scope. Returns an error rather than a zero Scope when
// absent, so a route mistakenly mounted outside the tenant middleware fails
// closed with a 403 instead of silently running unscoped.
func From(ctx context.Context) (Scope, error) {
	s, ok := ctx.Value(scopeKey{}).(Scope)
	if !ok {
		return Scope{}, apperr.Forbidden("NO_TENANT_SCOPE",
			"This request is missing its restaurant context.")
	}
	return s, nil
}
