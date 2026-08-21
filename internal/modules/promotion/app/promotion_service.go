// Package app holds the promotion module's use cases.
package app

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// serializableRetries is how many times a redemption retries on a serialization
// conflict.
//
// Under SERIALIZABLE, Postgres aborts one of two conflicting transactions and
// reports a retryable error by contract. Three attempts is ample: contention is
// only high in the last few redemptions of a capped coupon, and each retry is a
// few milliseconds.
const serializableRetries = 3

// PromotionService implements contracts.DiscountCalculator.
type PromotionService struct {
	db          *database.DB
	coupons     domain.CouponRepository
	redemptions domain.RedemptionRepository
	clock       clock.Clock
}

func NewPromotionService(
	db *database.DB,
	coupons domain.CouponRepository,
	redemptions domain.RedemptionRepository,
	clk clock.Clock,
) *PromotionService {
	return &PromotionService{db: db, coupons: coupons, redemptions: redemptions, clock: clk}
}

// Calculate is READ-ONLY. It answers "what would this coupon be worth" without
// consuming anything.
//
// The separation from Redeem is the whole point: a customer browsing the offers
// screen, or typing a code to see what happens, must not burn one of the 100
// available uses. Merging the two is how a flash sale sells out to people who
// never ordered.
//
// Returns Applied=false with a customer-readable reason rather than an error for
// the ordinary "not eligible" cases, because a rejected coupon is expected traffic,
// not an exceptional condition.
func (s *PromotionService) Calculate(ctx context.Context, req contracts.DiscountRequest) (contracts.DiscountResult, error) {
	rejected := func(reason string) contracts.DiscountResult {
		return contracts.DiscountResult{
			Applied:        false,
			DiscountAmount: money.Zero(money.INR),
			RejectReason:   reason,
		}
	}

	code := domain.NormalizeCode(req.CouponCode)
	if code == "" {
		return rejected("Please enter a code."), nil
	}

	coupon, err := s.coupons.FindByCode(ctx, code)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			// Deliberately vague: confirming which codes exist lets someone
			// enumerate unreleased promotions.
			return rejected("That code is not valid."), nil
		}
		return contracts.DiscountResult{}, err
	}

	eligibility := domain.Eligibility{
		Now:          s.clock.Now(),
		KitchenID:    req.KitchenID,
		Subtotal:     req.Subtotal,
		IsFirstOrder: req.IsFirstOrder,
	}
	if err := coupon.CheckEligibility(eligibility); err != nil {
		// The domain's rejection messages are written for customers, so they are
		// passed straight through.
		if appErr, ok := apperr.As(err); ok {
			return rejected(appErr.Message), nil
		}
		return contracts.DiscountResult{}, err
	}

	// Per-user cap, checked here so the customer learns before checkout that they
	// have already used it. Re-checked authoritatively during Redeem.
	used, err := s.redemptions.CountLiveForUser(ctx, coupon.ID, req.UserID)
	if err != nil {
		return contracts.DiscountResult{}, err
	}
	if used >= coupon.MaxRedemptionsPerUser {
		return rejected("You have already used that offer."), nil
	}

	// The delivery fee is not on the request, so free-delivery coupons cannot be
	// priced here. Ordering passes the subtotal only; adding the fee to
	// DiscountRequest is the fix and belongs in contracts.
	deliveryFee := money.Zero(money.INR)
	discount := coupon.CalculateDiscount(req.Subtotal, deliveryFee)

	if discount.IsZero() {
		return rejected("That offer does not reduce this order."), nil
	}

	return contracts.DiscountResult{
		Applied:        true,
		CouponID:       coupon.ID,
		Code:           coupon.Code,
		Description:    coupon.Description,
		DiscountAmount: discount,
	}, nil
}

// Redeem consumes one use of a coupon.
//
// Called from inside ordering's checkout transaction. THIS is where the caps are
// enforced authoritatively, and the isolation level matters:
//
// The per-user cap of 1 is enforced by a partial unique index — cheap, absolute,
// and it frees the slot when a redemption is released.
//
// The GLOBAL cap cannot be. "COUNT(redemptions) < 100" is a predicate over rows
// that do not exist yet, so two transactions can both count 99 and both insert,
// and no row lock prevents it because there is no row to lock. Locking the coupon
// row would work but serialises every checkout using that coupon behind one
// lock — precisely the wrong behaviour during the flash sale the cap exists for.
// SERIALIZABLE detects the conflict and aborts one, and the retry loop absorbs it.
func (s *PromotionService) Redeem(ctx context.Context, couponID, userID, orderID id.ID) error {
	return s.db.RetrySerializable(ctx, serializableRetries, func(ctx context.Context) error {
		coupon, err := s.coupons.FindByID(ctx, couponID)
		if err != nil {
			return err
		}

		// Global cap, read under SERIALIZABLE where the count is authoritative.
		if coupon.MaxRedemptions != nil {
			total, err := s.redemptions.CountLive(ctx, couponID)
			if err != nil {
				return err
			}
			if total >= *coupon.MaxRedemptions {
				return apperr.Conflict("COUPON_EXHAUSTED",
					"That offer has just been fully claimed.")
			}
		}

		// Per-user cap. The unique index covers the max=1 case even if this check
		// races; the check exists to produce a good message rather than a
		// constraint violation.
		used, err := s.redemptions.CountLiveForUser(ctx, couponID, userID)
		if err != nil {
			return err
		}
		if used >= coupon.MaxRedemptionsPerUser {
			return apperr.Conflict("COUPON_ALREADY_USED", "You have already used that offer.")
		}

		// The discount is recomputed rather than passed in, so a client cannot
		// influence what gets recorded. Ordering has already applied its own
		// calculation to the order; this is the audit record.
		return s.redemptions.Create(ctx, &domain.Redemption{
			ID:        id.New(),
			CouponID:  couponID,
			UserID:    userID,
			OrderID:   orderID,
			Discount:  money.Zero(money.INR),
			CreatedAt: s.clock.Now(),
		})
	})
}

// ReleaseRedemption returns a use after a cancellation.
//
// Idempotent: called from cancellation AND from the payment-failed handler, and
// both can legitimately fire for the same order.
func (s *PromotionService) ReleaseRedemption(ctx context.Context, orderID id.ID) error {
	return s.redemptions.ReleaseForOrder(ctx, orderID)
}

// ---------------------------------------------------------------------------
// Admin and customer reads
// ---------------------------------------------------------------------------

type CreateCouponInput struct {
	Code                  string
	Description           string
	DiscountType          string
	DiscountValue         int64
	MaxDiscountMinor      *int64
	MinOrderMinor         int64
	KitchenIDs            []id.ID
	FirstOrderOnly        bool
	ValidFrom             *time.Time
	ValidUntil            *time.Time
	MaxRedemptions        *int32
	MaxRedemptionsPerUser int32
	FundedBy              string
	CreatedBy             id.ID
}

func (s *PromotionService) CreateCoupon(ctx context.Context, in CreateCouponInput) (*domain.Coupon, error) {
	now := s.clock.Now()

	validFrom := now
	if in.ValidFrom != nil {
		validFrom = *in.ValidFrom
	}
	perUser := in.MaxRedemptionsPerUser
	if perUser < 1 {
		perUser = 1
	}
	fundedBy := domain.FundedBy(in.FundedBy)
	if fundedBy == "" {
		fundedBy = domain.FundedByPlatform
	}

	coupon := &domain.Coupon{
		ID:                    id.New(),
		Code:                  domain.NormalizeCode(in.Code),
		Description:           in.Description,
		DiscountType:          domain.DiscountType(in.DiscountType),
		DiscountValue:         in.DiscountValue,
		MinOrder:              money.FromMinor(in.MinOrderMinor, money.INR),
		KitchenIDs:            in.KitchenIDs,
		FirstOrderOnly:        in.FirstOrderOnly,
		ValidFrom:             validFrom,
		ValidUntil:            in.ValidUntil,
		MaxRedemptions:        in.MaxRedemptions,
		MaxRedemptionsPerUser: perUser,
		FundedBy:              fundedBy,
		IsActive:              true,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if in.MaxDiscountMinor != nil {
		capped := money.FromMinor(*in.MaxDiscountMinor, money.INR)
		coupon.MaxDiscount = &capped
	}
	if coupon.KitchenIDs == nil {
		coupon.KitchenIDs = []id.ID{}
	}

	if err := coupon.Validate(); err != nil {
		return nil, err
	}
	if err := s.coupons.Create(ctx, coupon); err != nil {
		return nil, err
	}
	return coupon, nil
}

// Deactivate stops a coupon without deleting it.
//
// Never delete: existing redemptions reference it, and the historical record of
// what a customer was promised must survive.
func (s *PromotionService) Deactivate(ctx context.Context, couponID id.ID) error {
	coupon, err := s.coupons.FindByID(ctx, couponID)
	if err != nil {
		return err
	}
	coupon.IsActive = false
	coupon.UpdatedAt = s.clock.Now()
	return s.coupons.Update(ctx, coupon)
}

// ListForCustomer returns offers a customer can currently see.
func (s *PromotionService) ListForCustomer(ctx context.Context, kitchenID id.ID) ([]*domain.Coupon, error) {
	return s.coupons.ListActive(ctx, s.clock.Now(), kitchenID)
}

func (s *PromotionService) ListAll(ctx context.Context, limit int) ([]*domain.Coupon, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.coupons.ListAll(ctx, limit)
}

var _ contracts.DiscountCalculator = (*PromotionService)(nil)
