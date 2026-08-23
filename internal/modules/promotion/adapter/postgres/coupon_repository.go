// Package postgres implements the promotion module's repository ports.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/promotion/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

type CouponRepository struct{ db *database.DB }

func NewCouponRepository(db *database.DB) *CouponRepository { return &CouponRepository{db: db} }

const couponColumns = `
	id, code, description, discount_type, discount_value, max_discount_minor,
	min_order_minor, kitchen_ids, first_order_only, valid_from, valid_until,
	max_redemptions, max_redemptions_per_user, redemption_count,
	funded_by, is_active, created_at, updated_at`

func (r *CouponRepository) Create(ctx context.Context, c *domain.Coupon) error {
	const q = `
		INSERT INTO coupons (
			id, code, description, discount_type, discount_value, max_discount_minor,
			min_order_minor, kitchen_ids, first_order_only, valid_from, valid_until,
			max_redemptions, max_redemptions_per_user, funded_by, is_active,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`

	kitchenIDs := c.KitchenIDs
	if kitchenIDs == nil {
		kitchenIDs = []id.ID{}
	}

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		c.ID, c.Code, c.Description, string(c.DiscountType), c.DiscountValue,
		maxDiscountMinor(c), c.MinOrder.Minor(), kitchenIDs, c.FirstOrderOnly,
		c.ValidFrom, c.ValidUntil, c.MaxRedemptions, c.MaxRedemptionsPerUser,
		string(c.FundedBy), c.IsActive, c.CreatedAt, c.UpdatedAt,
	); err != nil {
		if database.IsUniqueViolation(err, "coupons_code_key") {
			return apperr.Conflict("COUPON_CODE_EXISTS", "A coupon with that code already exists.")
		}
		if database.IsCheckViolation(err) {
			// The DB enforces the same rules as the domain -- notably that a
			// percentage coupon MUST have a cap. Reaching here means the domain
			// check was bypassed.
			return apperr.Invalid("INVALID_COUPON",
				"Those coupon values are not valid. A percentage discount needs a maximum.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("promotion: create coupon: %w", err))
	}
	return nil
}

func (r *CouponRepository) Update(ctx context.Context, c *domain.Coupon) error {
	const q = `
		UPDATE coupons SET
			description = $2, min_order_minor = $3, kitchen_ids = $4,
			valid_until = $5, max_redemptions = $6, is_active = $7, updated_at = $8
		WHERE id = $1`

	// Deliberately does NOT allow changing discount_type or discount_value.
	// Editing what a live coupon is worth would mean two customers using the same
	// code got different deals with no record of why.
	tag, err := r.db.Q(ctx).Exec(ctx, q,
		c.ID, c.Description, c.MinOrder.Minor(), c.KitchenIDs,
		c.ValidUntil, c.MaxRedemptions, c.IsActive, c.UpdatedAt,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("promotion: update coupon: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("COUPON_NOT_FOUND", "That coupon could not be found.")
	}
	return nil
}

// FindByCode matches case-insensitively via the functional unique index on
// upper(code), so the lookup stays an index scan.
func (r *CouponRepository) FindByCode(ctx context.Context, code string) (*domain.Coupon, error) {
	q := `SELECT ` + couponColumns + ` FROM coupons WHERE upper(code) = upper($1)`
	return r.findOne(ctx, q, code)
}

func (r *CouponRepository) FindByID(ctx context.Context, couponID id.ID) (*domain.Coupon, error) {
	q := `SELECT ` + couponColumns + ` FROM coupons WHERE id = $1`
	return r.findOne(ctx, q, couponID)
}

func (r *CouponRepository) findOne(ctx context.Context, q string, args ...any) (*domain.Coupon, error) {
	c, err := scanCoupon(r.db.Q(ctx).QueryRow(ctx, q, args...))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("COUPON_NOT_FOUND", "That code is not valid.")
		}
		return nil, apperr.Internal(fmt.Errorf("promotion: find coupon: %w", err))
	}
	return c, nil
}

// ListActive returns offers a customer can currently use.
//
// The kitchen filter uses "empty array OR contains", because an empty
// kitchen_ids means "valid everywhere" rather than "valid nowhere".
// ListActive returns offers a customer can currently use.
//
// kitchenID is OPTIONAL, and the two cases mean different things:
//
//   - A kitchen given: offers usable at THAT kitchen. Platform-wide offers plus
//     ones scoped to it, which is what a kitchen's own page wants.
//   - id.Nil: every active offer, whatever its scope. This is what a LISTING page
//     wants -- it renders many kitchens and attributes each offer using the
//     kitchen_ids the API now returns.
//
// The nil case used to fall through to the same predicate with a zero UUID, so
// `$2 = ANY(kitchen_ids)` matched nothing and a kitchen-scoped offer was
// invisible on every listing. Passing a real NULL instead of the zero UUID is
// what separates "no filter requested" from "filter by a kitchen that cannot
// exist" -- id.Nil is a valid UUID value, so it could never express the
// difference on its own.
func (r *CouponRepository) ListActive(ctx context.Context, now time.Time, kitchenID id.ID) ([]*domain.Coupon, error) {
	var filter any
	if kitchenID != id.Nil {
		filter = kitchenID
	}

	q := `SELECT ` + couponColumns + ` FROM coupons
		WHERE is_active
		  AND valid_from <= $1
		  AND (valid_until IS NULL OR valid_until > $1)
		  AND (max_redemptions IS NULL OR redemption_count < max_redemptions)
		  AND (
		        $2::uuid IS NULL
		     OR cardinality(kitchen_ids) = 0
		     OR $2 = ANY(kitchen_ids)
		  )
		ORDER BY min_order_minor, code`
	return r.query(ctx, q, now, filter)
}

func (r *CouponRepository) ListAll(ctx context.Context, limit int) ([]*domain.Coupon, error) {
	q := `SELECT ` + couponColumns + ` FROM coupons ORDER BY created_at DESC LIMIT $1`
	return r.query(ctx, q, limit)
}

func (r *CouponRepository) query(ctx context.Context, q string, args ...any) ([]*domain.Coupon, error) {
	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("promotion: query coupons: %w", err))
	}
	defer rows.Close()

	var out []*domain.Coupon
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("promotion: scan coupon: %w", err))
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCoupon(row scanner) (*domain.Coupon, error) {
	var (
		c            domain.Coupon
		discountType string
		fundedBy     string
		maxDiscount  *int64
		minOrder     int64
	)
	if err := row.Scan(
		&c.ID, &c.Code, &c.Description, &discountType, &c.DiscountValue, &maxDiscount,
		&minOrder, &c.KitchenIDs, &c.FirstOrderOnly, &c.ValidFrom, &c.ValidUntil,
		&c.MaxRedemptions, &c.MaxRedemptionsPerUser, &c.RedemptionCount,
		&fundedBy, &c.IsActive, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	c.DiscountType = domain.DiscountType(discountType)
	c.FundedBy = domain.FundedBy(fundedBy)
	c.MinOrder = money.FromMinor(minOrder, money.INR)
	if maxDiscount != nil {
		capped := money.FromMinor(*maxDiscount, money.INR)
		c.MaxDiscount = &capped
	}
	return &c, nil
}

func maxDiscountMinor(c *domain.Coupon) *int64 {
	if c.MaxDiscount == nil {
		return nil
	}
	v := c.MaxDiscount.Minor()
	return &v
}

// ---------------------------------------------------------------------------
// Redemptions
// ---------------------------------------------------------------------------

type RedemptionRepository struct{ db *database.DB }

func NewRedemptionRepository(db *database.DB) *RedemptionRepository {
	return &RedemptionRepository{db: db}
}

// Create records a use and bumps the cached counter in the SAME transaction.
//
// The counter is denormalised so eligibility checks are one row read rather than
// a COUNT on every checkout. Incrementing it here, transactionally, is what keeps
// it from drifting from the redemptions table.
func (r *RedemptionRepository) Create(ctx context.Context, red *domain.Redemption) error {
	// ON CONFLICT DO NOTHING, not catch-the-violation.
	//
	// This distinction is not stylistic. In Postgres, ANY error raised inside a
	// transaction poisons it: every subsequent statement fails and COMMIT reports
	// "commit unexpectedly resulted in rollback". Letting the unique violation fire
	// and then swallowing it therefore does NOT produce idempotent behaviour -- it
	// produces an internal error on exactly the retried checkout the constraint was
	// meant to make safe. ON CONFLICT raises nothing, so the transaction stays
	// usable, which matters doubly here because Redeem runs under SERIALIZABLE.
	const insert = `
		INSERT INTO coupon_redemptions (id, coupon_id, user_id, order_id, discount_minor, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (order_id) DO NOTHING
		RETURNING id`

	var inserted id.ID
	err := r.db.Q(ctx).QueryRow(ctx, insert,
		red.ID, red.CouponID, red.UserID, red.OrderID, red.Discount.Minor(), red.CreatedAt,
	).Scan(&inserted)

	switch {
	case database.IsNotFound(err):
		// The conflict fired: this order already has a redemption. Success from the
		// caller's point of view -- the coupon is applied -- and critically the
		// counter must NOT be bumped again.
		return nil
	case err != nil:
		return apperr.Internal(fmt.Errorf("promotion: create redemption: %w", err))
	}

	// Only reached when a row was genuinely inserted, so the cached counter cannot
	// drift above the ledger on a retry.
	const bump = `UPDATE coupons SET redemption_count = redemption_count + 1, updated_at = now() WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, bump, red.CouponID); err != nil {
		return apperr.Internal(fmt.Errorf("promotion: increment redemption count: %w", err))
	}
	return nil
}

// CountLiveForUser counts unreleased redemptions for the per-user cap.
func (r *RedemptionRepository) CountLiveForUser(ctx context.Context, couponID, userID id.ID) (int32, error) {
	const q = `
		SELECT count(*) FROM coupon_redemptions
		WHERE coupon_id = $1 AND user_id = $2 AND released_at IS NULL`

	var n int32
	if err := r.db.Q(ctx).QueryRow(ctx, q, couponID, userID).Scan(&n); err != nil {
		return 0, apperr.Internal(fmt.Errorf("promotion: count user redemptions: %w", err))
	}
	return n, nil
}

// CountLive counts unreleased redemptions in total, for the global cap.
//
// Read under SERIALIZABLE by the caller, which is what makes it authoritative:
// this is a count over rows that may not exist yet, and no row lock can protect
// that predicate.
func (r *RedemptionRepository) CountLive(ctx context.Context, couponID id.ID) (int32, error) {
	const q = `SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1 AND released_at IS NULL`

	var n int32
	if err := r.db.Q(ctx).QueryRow(ctx, q, couponID).Scan(&n); err != nil {
		return 0, apperr.Internal(fmt.Errorf("promotion: count redemptions: %w", err))
	}
	return n, nil
}

// ReleaseForOrder returns a use after a cancellation.
//
// Marks rather than deletes, so repeated order-then-cancel remains visible to
// fraud review. Decrements the cached counter to match. Idempotent: the WHERE
// clause makes a second release a no-op, so the counter cannot go negative.
func (r *RedemptionRepository) ReleaseForOrder(ctx context.Context, orderID id.ID) error {
	const release = `
		UPDATE coupon_redemptions SET released_at = now()
		WHERE order_id = $1 AND released_at IS NULL
		RETURNING coupon_id`

	var couponID id.ID
	err := r.db.Q(ctx).QueryRow(ctx, release, orderID).Scan(&couponID)
	if err != nil {
		if database.IsNotFound(err) {
			// No live redemption for this order: never used a coupon, or already
			// released. Both are success.
			return nil
		}
		return apperr.Internal(fmt.Errorf("promotion: release redemption: %w", err))
	}

	// GREATEST guards the counter: even if it somehow drifted, releasing cannot
	// drive it below zero and make an exhausted coupon look available.
	const decrement = `
		UPDATE coupons
		SET redemption_count = GREATEST(0, redemption_count - 1), updated_at = now()
		WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, decrement, couponID); err != nil {
		return apperr.Internal(fmt.Errorf("promotion: decrement redemption count: %w", err))
	}
	return nil
}

func (r *RedemptionRepository) FindByOrder(ctx context.Context, orderID id.ID) (*domain.Redemption, error) {
	const q = `
		SELECT id, coupon_id, user_id, order_id, discount_minor, released_at, created_at
		FROM coupon_redemptions WHERE order_id = $1`

	var (
		red      domain.Redemption
		discount int64
	)
	if err := r.db.Q(ctx).QueryRow(ctx, q, orderID).Scan(&red.ID, &red.CouponID,
		&red.UserID, &red.OrderID, &discount, &red.ReleasedAt, &red.CreatedAt); err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("REDEMPTION_NOT_FOUND", "No coupon was used on that order.")
		}
		return nil, apperr.Internal(fmt.Errorf("promotion: find redemption: %w", err))
	}
	red.Discount = money.FromMinor(discount, money.INR)
	return &red, nil
}

type scanner interface {
	Scan(dest ...any) error
}

var (
	_ domain.CouponRepository     = (*CouponRepository)(nil)
	_ domain.RedemptionRepository = (*RedemptionRepository)(nil)
)
