// Package postgres implements the catalog module's repository ports.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

type KitchenRepository struct {
	db *database.DB
}

func NewKitchenRepository(db *database.DB) *KitchenRepository {
	return &KitchenRepository{db: db}
}

const kitchenColumns = `
	id, merchant_id, name, slug, description, cuisines, cover_image_url, phone, email,
	address_line1, city, pincode, latitude, longitude, delivery_radius_m,
	is_active, is_accepting_orders, pause_reason,
	commission_bps, packaging_fee_minor, min_order_minor, cost_for_two_minor,
	avg_prep_minutes, rating_average, rating_count, fssai_license, fssai_expiry, gstin,
	is_first_party, version, created_at, updated_at`

func (r *KitchenRepository) Create(ctx context.Context, k *domain.Kitchen) error {
	const q = `
		INSERT INTO kitchens (
			id, merchant_id, name, slug, description, cuisines, cover_image_url, phone, email,
			address_line1, city, pincode, latitude, longitude, delivery_radius_m,
			is_active, is_accepting_orders, pause_reason,
			commission_bps, packaging_fee_minor, min_order_minor, cost_for_two_minor,
			avg_prep_minutes,
			fssai_license, gstin, is_first_party, version, created_at, updated_at,
			fssai_expiry
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
		          $19,$20,$21,$22,$23,$24,$25,$26,1,$27,$28,$29)`

	// Postgres distinguishes NULL from an empty array, and the column is NOT
	// NULL. A caller with no cuisines set should get an empty array, not a raw
	// constraint violation they have to decode.
	cuisines := k.Cuisines
	if cuisines == nil {
		cuisines = []string{}
	}

	_, err := r.db.Q(ctx).Exec(ctx, q,
		k.ID, k.MerchantID, k.Name, k.Slug, k.Description, cuisines, k.CoverImageURL, k.Phone, k.Email,
		k.AddressLine1, k.City, k.Pincode, k.Latitude, k.Longitude, k.DeliveryRadiusM,
		k.IsActive, k.IsAcceptingOrders, k.PauseReason,
		k.CommissionBps, k.PackagingFee.Minor(), k.MinOrder.Minor(), k.CostForTwo.Minor(),
		k.AvgPrepMinutes,
		k.FSSAILicense, k.GSTIN, k.IsFirstParty, k.CreatedAt, k.UpdatedAt, k.FSSAIExpiry,
	)
	if err != nil {
		if database.IsUniqueViolation(err, "kitchens_slug_key") {
			return apperr.Conflict("KITCHEN_SLUG_TAKEN", "That kitchen URL is already in use.")
		}
		return apperr.Internal(fmt.Errorf("catalog: create kitchen: %w", err))
	}
	return nil
}

// Update applies optimistic locking.
//
// The `AND version = $N` predicate is what turns a lost update into a visible
// conflict. Two admins editing the same kitchen from two tablets would otherwise
// silently overwrite each other, and the loser would never know.
func (r *KitchenRepository) Update(ctx context.Context, k *domain.Kitchen) error {
	const q = `
		UPDATE kitchens SET
			name = $2, slug = $3, description = $4, cuisines = $5, cover_image_url = $6,
			phone = $7, email = $8,
			address_line1 = $9, city = $10, pincode = $11, latitude = $12, longitude = $13,
			delivery_radius_m = $14, is_active = $15, is_accepting_orders = $16,
			pause_reason = $17, commission_bps = $18, packaging_fee_minor = $19,
			min_order_minor = $20, cost_for_two_minor = $21, avg_prep_minutes = $22,
			fssai_license = $23, gstin = $24, fssai_expiry = $27,
			version = version + 1, updated_at = $25
		WHERE id = $1 AND version = $26`

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		k.ID, k.Name, k.Slug, k.Description, k.Cuisines, k.CoverImageURL, k.Phone, k.Email,
		k.AddressLine1, k.City, k.Pincode, k.Latitude, k.Longitude,
		k.DeliveryRadiusM, k.IsActive, k.IsAcceptingOrders,
		k.PauseReason, k.CommissionBps, k.PackagingFee.Minor(),
		k.MinOrder.Minor(), k.CostForTwo.Minor(), k.AvgPrepMinutes,
		k.FSSAILicense, k.GSTIN,
		k.UpdatedAt, k.Version,
		// $27, appended rather than inserted at its natural position: renumbering
		// 27 placeholders by hand is exactly how an off-by-one gets written, and a
		// silently shifted argument would write a licence number into a date column.
		k.FSSAIExpiry,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("catalog: update kitchen %s: %w", k.ID, err))
	}
	if tag.RowsAffected() == 0 {
		// Either the row is gone or someone else wrote first. Both mean the
		// caller's copy is stale and it must reload rather than retry blindly.
		return apperr.Conflict("KITCHEN_MODIFIED",
			"This kitchen was changed by someone else. Please reload and try again.")
	}
	k.Version++
	return nil
}

func (r *KitchenRepository) FindByID(ctx context.Context, kitchenID id.ID) (*domain.Kitchen, error) {
	q := `SELECT ` + kitchenColumns + ` FROM kitchens WHERE id = $1`
	return r.scanOne(ctx, q, kitchenID)
}

func (r *KitchenRepository) FindBySlug(ctx context.Context, slug string) (*domain.Kitchen, error) {
	q := `SELECT ` + kitchenColumns + ` FROM kitchens WHERE slug = $1`
	return r.scanOne(ctx, q, slug)
}

// FindNearby does the indexed bounding-box prefilter.
//
// It deliberately does NOT compute exact distance in SQL. Doing the trig in
// Postgres would prevent the index from being used at all, forcing a full scan.
// The box narrows on the index; the app filters precisely.
func (r *KitchenRepository) FindNearby(ctx context.Context, lat, lng float64, radiusM int32, limit int) ([]*domain.Kitchen, error) {
	minLat, maxLat, minLng, maxLng := domain.BoundingBox(lat, lng, radiusM)

	q := `SELECT ` + kitchenColumns + ` FROM kitchens
		WHERE is_active
		  AND latitude BETWEEN $1 AND $2
		  AND longitude BETWEEN $3 AND $4
		LIMIT $5`

	rows, err := r.db.Q(ctx).Query(ctx, q, minLat, maxLat, minLng, maxLng, limit)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: find nearby kitchens: %w", err))
	}
	defer rows.Close()

	var out []*domain.Kitchen
	for rows.Next() {
		k, err := scanKitchen(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan kitchen: %w", err))
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (r *KitchenRepository) ListForMerchant(ctx context.Context, merchantID id.ID) ([]*domain.Kitchen, error) {
	q := `SELECT ` + kitchenColumns + ` FROM kitchens WHERE merchant_id = $1 ORDER BY name`

	rows, err := r.db.Q(ctx).Query(ctx, q, merchantID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: list kitchens: %w", err))
	}
	defer rows.Close()

	var out []*domain.Kitchen
	for rows.Next() {
		k, err := scanKitchen(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan kitchen: %w", err))
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetAcceptingOrders bypasses optimistic locking on purpose.
//
// This is the "we are swamped, stop the orders" switch. It has to win
// immediately and unconditionally — making staff resolve a version conflict
// while the kitchen is drowning would be exactly the wrong behaviour.
func (r *KitchenRepository) SetAcceptingOrders(ctx context.Context, kitchenID id.ID, accepting bool, reason string) error {
	const q = `
		UPDATE kitchens
		SET is_accepting_orders = $2, pause_reason = $3, updated_at = now()
		WHERE id = $1`

	tag, err := r.db.Q(ctx).Exec(ctx, q, kitchenID, accepting, reason)
	if err != nil {
		return apperr.Internal(fmt.Errorf("catalog: set accepting orders: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("KITCHEN_NOT_FOUND", "That kitchen could not be found.")
	}
	return nil
}

// ReplaceHours swaps a kitchen's whole schedule atomically.
//
// Delete-then-insert rather than a diff: the schedule is small, and computing a
// minimal patch would risk leaving an orphaned shift behind. Must run inside a
// transaction so the kitchen is never left with no hours at all.
func (r *KitchenRepository) ReplaceHours(ctx context.Context, kitchenID id.ID, hours []domain.OpeningHours) error {
	return r.db.InTx(ctx, func(ctx context.Context) error {
		if _, err := r.db.Q(ctx).Exec(ctx, `DELETE FROM kitchen_hours WHERE kitchen_id = $1`, kitchenID); err != nil {
			return apperr.Internal(fmt.Errorf("catalog: clear hours: %w", err))
		}

		const ins = `
			INSERT INTO kitchen_hours (id, kitchen_id, day_of_week, opens_at, closes_at)
			VALUES ($1, $2, $3, $4, $5)`

		for _, h := range hours {
			if h.ClosesAt <= h.OpensAt {
				return apperr.Invalid("INVALID_HOURS",
					"A shift must close after it opens. Split shifts crossing midnight into two entries.")
			}
			hID := h.ID
			if id.IsNil(hID) {
				hID = id.New()
			}
			if _, err := r.db.Q(ctx).Exec(ctx, ins, hID, kitchenID, h.DayOfWeek,
				durationToTime(h.OpensAt), durationToTime(h.ClosesAt)); err != nil {
				return apperr.Internal(fmt.Errorf("catalog: insert hours: %w", err))
			}
		}
		return nil
	})
}

// LoadHours batches by kitchen so a nearby-search does not run one query per
// result.
func (r *KitchenRepository) LoadHours(ctx context.Context, kitchenIDs []id.ID) (map[id.ID][]domain.OpeningHours, error) {
	if len(kitchenIDs) == 0 {
		return map[id.ID][]domain.OpeningHours{}, nil
	}

	// ANY($1) over an array rather than a generated IN list: one placeholder
	// whatever the count, so Postgres caches the plan instead of recompiling per
	// distinct length.
	const q = `
		SELECT id, kitchen_id, day_of_week, opens_at, closes_at
		FROM kitchen_hours
		WHERE kitchen_id = ANY($1)
		ORDER BY day_of_week, opens_at`

	rows, err := r.db.Q(ctx).Query(ctx, q, kitchenIDs)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: load hours: %w", err))
	}
	defer rows.Close()

	out := make(map[id.ID][]domain.OpeningHours, len(kitchenIDs))
	for rows.Next() {
		var (
			h                 domain.OpeningHours
			opensAt, closesAt time.Time
		)
		if err := rows.Scan(&h.ID, &h.KitchenID, &h.DayOfWeek, &opensAt, &closesAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan hours: %w", err))
		}
		h.OpensAt = timeToDuration(opensAt)
		h.ClosesAt = timeToDuration(closesAt)
		out[h.KitchenID] = append(out[h.KitchenID], h)
	}
	return out, rows.Err()
}

func (r *KitchenRepository) scanOne(ctx context.Context, q string, args ...any) (*domain.Kitchen, error) {
	k, err := scanKitchen(r.db.Q(ctx).QueryRow(ctx, q, args...))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("KITCHEN_NOT_FOUND", "That kitchen could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("catalog: find kitchen: %w", err))
	}
	return k, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanKitchen(row scanner) (*domain.Kitchen, error) {
	var (
		k                                  domain.Kitchen
		packagingFee, minOrder, costForTwo int64
		ratingAvg                          float64
	)
	if err := row.Scan(
		&k.ID, &k.MerchantID, &k.Name, &k.Slug, &k.Description, &k.Cuisines, &k.CoverImageURL, &k.Phone, &k.Email,
		&k.AddressLine1, &k.City, &k.Pincode, &k.Latitude, &k.Longitude, &k.DeliveryRadiusM,
		&k.IsActive, &k.IsAcceptingOrders, &k.PauseReason,
		&k.CommissionBps, &packagingFee, &minOrder, &costForTwo, &k.AvgPrepMinutes,
		&ratingAvg, &k.RatingCount, &k.FSSAILicense, &k.FSSAIExpiry, &k.GSTIN,
		&k.IsFirstParty, &k.Version, &k.CreatedAt, &k.UpdatedAt,
	); err != nil {
		return nil, err
	}
	k.PackagingFee = money.FromMinor(packagingFee, money.INR)
	k.MinOrder = money.FromMinor(minOrder, money.INR)
	k.CostForTwo = money.FromMinor(costForTwo, money.INR)
	k.RatingAverage = ratingAvg
	return &k, nil
}

// Postgres TIME maps to a time.Time on a zero date; these convert to and from
// a duration since midnight, which is what the domain actually means.
func durationToTime(d time.Duration) time.Time {
	return time.Date(2000, 1, 1, int(d.Hours()), int(d.Minutes())%60, 0, 0, time.UTC)
}

func timeToDuration(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

var _ domain.KitchenRepository = (*KitchenRepository)(nil)

/*
AssistedIDs lists active kitchens whose merchant operates in assisted mode.

Joins to merchants because operating_mode is a property of the VENDOR, not the
outlet: a man's ability to use a dashboard does not vary by which cart he is
standing behind. Restricted to active kitchens, since a deactivated outlet's
orders are not something ops should be asked to action.
*/
func (r *KitchenRepository) AssistedIDs(ctx context.Context) ([]id.ID, error) {
	const q = `
		SELECT k.id
		FROM kitchens k
		JOIN merchants m ON m.id = k.merchant_id
		WHERE m.operating_mode = 'assisted' AND k.is_active
		ORDER BY k.id`

	rows, err := r.db.Q(ctx).Query(ctx, q)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("catalog: list assisted kitchens: %w", err))
	}
	defer rows.Close()

	var out []id.ID
	for rows.Next() {
		var kitchenID id.ID
		if err := rows.Scan(&kitchenID); err != nil {
			return nil, apperr.Internal(fmt.Errorf("catalog: scan assisted kitchen: %w", err))
		}
		out = append(out, kitchenID)
	}
	return out, rows.Err()
}
