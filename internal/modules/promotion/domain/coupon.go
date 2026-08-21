// Package domain holds the promotion module's entities and ports.
//
// Promotion implements contracts.DiscountCalculator. The interesting problem is
// not the arithmetic — it is enforcing usage caps correctly under concurrency,
// because a coupon is by design something many people try to use at the same
// moment.
package domain

import (
	"context"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// DiscountType is how the discount is computed.
type DiscountType string

const (
	// DiscountPercentage takes DiscountValue as basis points and REQUIRES a cap.
	DiscountPercentage DiscountType = "percentage"
	// DiscountFlat takes DiscountValue as paise.
	DiscountFlat DiscountType = "flat"
	// DiscountFreeDelivery zeroes the delivery fee.
	DiscountFreeDelivery DiscountType = "free_delivery"
)

// FundedBy records who absorbs the discount.
//
// Load-bearing once partners are onboarded: a platform-funded coupon must NOT be
// deducted from a merchant's settlement. Getting this wrong means underpaying
// merchants for promotions they never agreed to, which is the fastest way to lose
// them.
type FundedBy string

const (
	FundedByPlatform FundedBy = "platform"
	FundedByMerchant FundedBy = "merchant"
	FundedByShared   FundedBy = "shared"
)

// Coupon is a discount offer.
type Coupon struct {
	ID          id.ID
	Code        string
	Description string

	DiscountType  DiscountType
	DiscountValue int64
	// MaxDiscount caps a percentage. Required for percentage coupons: "20% off"
	// with no cap is unbounded exposure on a large order.
	MaxDiscount *money.Amount

	MinOrder       money.Amount
	KitchenIDs     []id.ID
	FirstOrderOnly bool

	ValidFrom  time.Time
	ValidUntil *time.Time

	// MaxRedemptions nil means unlimited. Zero means exhausted, which is a real
	// and different state.
	MaxRedemptions        *int32
	MaxRedemptionsPerUser int32
	RedemptionCount       int32

	FundedBy FundedBy
	IsActive bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Eligibility is everything about a coupon that can be checked without counting
// redemptions.
type Eligibility struct {
	Now          time.Time
	KitchenID    id.ID
	Subtotal     money.Amount
	IsFirstOrder bool
}

// CheckEligibility validates the non-counting rules.
//
// Separated from the cap check because these are cheap and can run outside a
// transaction, while the caps need SERIALIZABLE. Failing fast here means most
// invalid coupons never open a transaction at all.
//
// Every rejection returns a reason the CUSTOMER can act on. "Coupon not valid" is
// useless — they cannot tell whether to add more items, wait, or give up.
func (c *Coupon) CheckEligibility(e Eligibility) error {
	if !c.IsActive {
		return apperr.Invalid("COUPON_INACTIVE", "That code is no longer available.")
	}
	if e.Now.Before(c.ValidFrom) {
		return apperr.Invalid("COUPON_NOT_STARTED", "That offer has not started yet.")
	}
	if c.ValidUntil != nil && e.Now.After(*c.ValidUntil) {
		return apperr.Invalid("COUPON_EXPIRED", "That offer has expired.")
	}

	// Exhausted is checked here too, using the cached counter, so the common case
	// short-circuits before the expensive path. The authoritative check happens
	// under SERIALIZABLE during redemption.
	if c.MaxRedemptions != nil && c.RedemptionCount >= *c.MaxRedemptions {
		return apperr.Invalid("COUPON_EXHAUSTED", "That offer has been fully claimed.")
	}

	if c.MinOrder.IsPositive() && e.Subtotal.LessThan(c.MinOrder) {
		shortfall, err := c.MinOrder.Sub(e.Subtotal)
		if err != nil {
			return apperr.Invalid("MIN_ORDER_NOT_MET", "Your order is below the minimum for that offer.")
		}
		// Tell them exactly how much more to add. This is the difference between a
		// rejection and an upsell.
		return apperr.Invalid("MIN_ORDER_NOT_MET",
			"Add ₹"+shortfall.String()+" more to use that offer.")
	}

	if c.FirstOrderOnly && !e.IsFirstOrder {
		return apperr.Invalid("FIRST_ORDER_ONLY", "That offer is only for your first order.")
	}

	if len(c.KitchenIDs) > 0 && !c.appliesToKitchen(e.KitchenID) {
		return apperr.Invalid("COUPON_WRONG_KITCHEN", "That offer is not valid at this kitchen.")
	}
	return nil
}

func (c *Coupon) appliesToKitchen(kitchenID id.ID) bool {
	for _, allowed := range c.KitchenIDs {
		if allowed == kitchenID {
			return true
		}
	}
	return false
}

// CalculateDiscount computes the amount off.
//
// Never returns more than the subtotal: a discount exceeding the order would
// produce a negative total, i.e. paying the customer to eat. The order's own
// RecalculateTotals also clamps this, and the database has a CHECK constraint —
// three layers, because this is money and a bug here hands out cash.
func (c *Coupon) CalculateDiscount(subtotal, deliveryFee money.Amount) money.Amount {
	var discount money.Amount

	switch c.DiscountType {
	case DiscountPercentage:
		discount = subtotal.ApplyBasisPoints(c.DiscountValue)
		if c.MaxDiscount != nil && discount.GreaterThan(*c.MaxDiscount) {
			discount = *c.MaxDiscount
		}

	case DiscountFlat:
		discount = money.FromMinor(c.DiscountValue, subtotal.Currency())

	case DiscountFreeDelivery:
		discount = deliveryFee

	default:
		return money.Zero(subtotal.Currency())
	}

	// Clamp to the subtotal. A ₹200-off coupon on a ₹150 order gives ₹150 off,
	// not ₹200 and a refund.
	if discount.GreaterThan(subtotal) {
		return subtotal
	}
	if discount.IsNegative() {
		return money.Zero(subtotal.Currency())
	}
	return discount
}

// Validate checks a coupon is coherent before persistence.
func (c *Coupon) Validate() error {
	err := apperr.Invalid("INVALID_COUPON", "Please check the coupon fields.")
	bad := false

	if strings.TrimSpace(c.Code) == "" {
		err.WithField("code", "required")
		bad = true
	}
	if len(c.Code) > 32 {
		err.WithField("code", "too_long")
		bad = true
	}

	switch c.DiscountType {
	case DiscountPercentage:
		if c.DiscountValue <= 0 || c.DiscountValue > 10000 {
			err.WithField("discount_value", "must_be_1_to_10000_bps")
			bad = true
		}
		// Enforced in the domain AND by a database CHECK: an uncapped percentage
		// coupon is the single easiest way to lose a lot of money quickly.
		if c.MaxDiscount == nil {
			err.WithField("max_discount_minor", "required_for_percentage")
			bad = true
		}
	case DiscountFlat:
		if c.DiscountValue <= 0 {
			err.WithField("discount_value", "must_be_positive")
			bad = true
		}
	case DiscountFreeDelivery:
	default:
		err.WithField("discount_type", "invalid")
		bad = true
	}

	if c.MaxRedemptionsPerUser < 1 {
		err.WithField("max_redemptions_per_user", "must_be_at_least_1")
		bad = true
	}
	if c.ValidUntil != nil && !c.ValidUntil.After(c.ValidFrom) {
		err.WithField("valid_until", "must_be_after_valid_from")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

// NormalizeCode uppercases and trims a customer-entered code.
//
// Customers type "welcome50", " WELCOME50 " and "Welcome50". All three must work,
// and the stored form is canonical so the lookup is an exact indexed match.
func NormalizeCode(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

// Redemption is one use of a coupon.
type Redemption struct {
	ID         id.ID
	CouponID   id.ID
	UserID     id.ID
	OrderID    id.ID
	Discount   money.Amount
	ReleasedAt *time.Time
	CreatedAt  time.Time
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

type CouponRepository interface {
	Create(ctx context.Context, c *Coupon) error
	Update(ctx context.Context, c *Coupon) error
	FindByCode(ctx context.Context, code string) (*Coupon, error)
	FindByID(ctx context.Context, couponID id.ID) (*Coupon, error)
	// ListActive backs the customer-facing offers screen.
	ListActive(ctx context.Context, now time.Time, kitchenID id.ID) ([]*Coupon, error)
	ListAll(ctx context.Context, limit int) ([]*Coupon, error)
}

type RedemptionRepository interface {
	// Create records a use and increments the coupon's counter in the same
	// statement batch. Must run inside the caller's transaction.
	Create(ctx context.Context, r *Redemption) error
	// CountLiveForUser counts unreleased redemptions, for the per-user cap.
	CountLiveForUser(ctx context.Context, couponID, userID id.ID) (int32, error)
	// CountLive counts unreleased redemptions in total, for the global cap. Read
	// under SERIALIZABLE, where it is authoritative.
	CountLive(ctx context.Context, couponID id.ID) (int32, error)
	// ReleaseForOrder returns a use after a cancellation. Marks rather than
	// deletes, so repeated order-then-cancel stays visible to fraud review.
	ReleaseForOrder(ctx context.Context, orderID id.ID) error
	FindByOrder(ctx context.Context, orderID id.ID) (*Redemption, error)
}
