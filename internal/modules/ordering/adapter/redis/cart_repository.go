// Package redis stores carts.
//
// Redis rather than Postgres because a cart is high-write (every quantity tap),
// short-lived, and mostly abandoned. Putting it in Postgres means thousands of
// UPDATEs on rows that will never become orders, competing for the same
// connection pool as the order writes that actually matter. A cart also wants to
// simply expire, which a TTL gives for free and a table requires a cleanup job to
// emulate.
package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// cartTTL is how long an abandoned cart survives.
//
// Long enough that a customer can browse, get interrupted, and come back after
// dinner; short enough that a stale cart does not resurface days later with
// prices that have since changed. Prices are re-resolved at checkout regardless,
// so the risk is confusion rather than mischarging.
const cartTTL = 24 * time.Hour

type CartRepository struct {
	cache cache.Cache
}

func NewCartRepository(c cache.Cache) *CartRepository {
	return &CartRepository{cache: c}
}

// Get returns the user's cart, or an empty one.
//
// A missing cart is NOT an error: a first-time visitor has no cart, and forcing
// every caller to distinguish "no cart" from "empty cart" would put that
// branching in every handler for no benefit.
func (r *CartRepository) Get(ctx context.Context, userID id.ID) (*domain.Cart, error) {
	cart, err := cache.GetJSON[*domain.Cart](ctx, r.cache, cache.CartKey(userID.String()))
	if err != nil {
		if errors.Is(err, cache.ErrMiss) {
			return domain.NewCart(userID, id.Nil, time.Now().UTC()), nil
		}
		return nil, apperr.Internal(fmt.Errorf("ordering: get cart: %w", err))
	}
	// A corrupt entry decodes to nil rather than erroring; treat it as empty so a
	// bad write cannot wedge a customer out of ordering permanently.
	if cart == nil {
		return domain.NewCart(userID, id.Nil, time.Now().UTC()), nil
	}
	return cart, nil
}

// Save writes the cart, refreshing its TTL.
//
// Refreshing on every write is deliberate: an actively-edited cart should not
// expire mid-session just because it was first created 24 hours ago.
func (r *CartRepository) Save(ctx context.Context, cart *domain.Cart) error {
	// An empty cart is deleted rather than stored. Keeping empty carts around
	// wastes memory and makes "does this user have a cart" ambiguous.
	if cart.IsEmpty() {
		return r.Delete(ctx, cart.UserID)
	}
	if err := cache.SetJSON(ctx, r.cache, cache.CartKey(cart.UserID.String()), cart, cartTTL); err != nil {
		return apperr.Internal(fmt.Errorf("ordering: save cart: %w", err))
	}
	return nil
}

func (r *CartRepository) Delete(ctx context.Context, userID id.ID) error {
	if err := r.cache.Delete(ctx, cache.CartKey(userID.String())); err != nil {
		return apperr.Internal(fmt.Errorf("ordering: delete cart: %w", err))
	}
	return nil
}

var _ domain.CartRepository = (*CartRepository)(nil)
