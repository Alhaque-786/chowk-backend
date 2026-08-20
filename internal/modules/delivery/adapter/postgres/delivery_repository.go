// Package postgres implements the delivery module's repository port.
package postgres

import (
	"context"
	"fmt"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

type DeliveryRepository struct {
	db *database.DB
}

func NewDeliveryRepository(db *database.DB) *DeliveryRepository {
	return &DeliveryRepository{db: db}
}

const deliveryColumns = `
	id, order_id, kitchen_id, mode, status, provider, provider_ref, tracking_url,
	rider_name, rider_phone, cost_minor, pickup_eta, drop_eta,
	assigned_at, picked_up_at, delivered_at, failure_reason, created_at, updated_at`

func (r *DeliveryRepository) Create(ctx context.Context, d *domain.Delivery) error {
	const q = `
		INSERT INTO deliveries (id, order_id, kitchen_id, mode, status, provider,
		                        provider_ref, tracking_url, cost_minor, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		d.ID, d.OrderID, d.KitchenID, string(d.Mode), string(d.Status), d.Provider,
		nullString(d.ProviderRef), d.TrackingURL, d.Cost.Minor(), d.CreatedAt, d.UpdatedAt,
	); err != nil {
		// The unique index on order_id is the real guard against paying twice for
		// one drop; the service checks first, but only this arbitrates a race.
		if database.IsUniqueViolation(err, "deliveries_order_key") {
			return apperr.Conflict("DELIVERY_ALREADY_EXISTS",
				"A delivery has already been booked for this order.")
		}
		return apperr.Internal(fmt.Errorf("delivery: create: %w", err))
	}
	return nil
}

func (r *DeliveryRepository) Update(ctx context.Context, d *domain.Delivery) error {
	const q = `
		UPDATE deliveries SET
			mode = $2, status = $3, provider_ref = $4, tracking_url = $5,
			rider_name = $6, rider_phone = $7, cost_minor = $8,
			pickup_eta = $9, drop_eta = $10, assigned_at = $11, picked_up_at = $12,
			delivered_at = $13, failure_reason = $14, updated_at = $15
		WHERE id = $1`

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		d.ID, string(d.Mode), string(d.Status), nullString(d.ProviderRef), d.TrackingURL,
		d.RiderName, d.RiderPhone, d.Cost.Minor(), d.PickupETA, d.DropETA,
		d.AssignedAt, d.PickedUpAt, d.DeliveredAt, d.FailureReason, d.UpdatedAt,
	)
	if err != nil {
		if database.IsCheckViolation(err) {
			return apperr.Invalid("INVALID_DELIVERY",
				"A failed delivery must record a reason.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("delivery: update %s: %w", d.ID, err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("DELIVERY_NOT_FOUND", "That delivery could not be found.")
	}
	return nil
}

func (r *DeliveryRepository) FindByID(ctx context.Context, deliveryID id.ID) (*domain.Delivery, error) {
	q := `SELECT ` + deliveryColumns + ` FROM deliveries WHERE id = $1`
	return r.findOne(ctx, q, deliveryID)
}

func (r *DeliveryRepository) FindByOrderID(ctx context.Context, orderID id.ID) (*domain.Delivery, error) {
	q := `SELECT ` + deliveryColumns + ` FROM deliveries WHERE order_id = $1`
	return r.findOne(ctx, q, orderID)
}

// FindByOrderIDForUpdate row-locks so two concurrent status webhooks serialise.
func (r *DeliveryRepository) FindByOrderIDForUpdate(ctx context.Context, orderID id.ID) (*domain.Delivery, error) {
	q := `SELECT ` + deliveryColumns + ` FROM deliveries WHERE order_id = $1 FOR UPDATE`
	return r.findOne(ctx, q, orderID)
}

func (r *DeliveryRepository) FindByProviderRef(ctx context.Context, provider, ref string) (*domain.Delivery, error) {
	q := `SELECT ` + deliveryColumns + ` FROM deliveries WHERE provider = $1 AND provider_ref = $2`
	return r.findOne(ctx, q, provider, ref)
}

// ListActiveForKitchen backs the dispatch board. Matches the partial index.
func (r *DeliveryRepository) ListActiveForKitchen(ctx context.Context, kitchenID id.ID) ([]*domain.Delivery, error) {
	q := `SELECT ` + deliveryColumns + ` FROM deliveries
		WHERE kitchen_id = $1
		  AND status IN ('requested','assigned','at_pickup','picked_up')
		ORDER BY created_at`

	rows, err := r.db.Q(ctx).Query(ctx, q, kitchenID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("delivery: list active: %w", err))
	}
	defer rows.Close()

	var out []*domain.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("delivery: scan: %w", err))
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *DeliveryRepository) findOne(ctx context.Context, q string, args ...any) (*domain.Delivery, error) {
	d, err := scanDelivery(r.db.Q(ctx).QueryRow(ctx, q, args...))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("DELIVERY_NOT_FOUND", "That delivery could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("delivery: find: %w", err))
	}
	return d, nil
}

func scanDelivery(row scanner) (*domain.Delivery, error) {
	var (
		d           domain.Delivery
		mode        string
		status      string
		providerRef *string
		cost        int64
	)
	if err := row.Scan(
		&d.ID, &d.OrderID, &d.KitchenID, &mode, &status, &d.Provider, &providerRef,
		&d.TrackingURL, &d.RiderName, &d.RiderPhone, &cost, &d.PickupETA, &d.DropETA,
		&d.AssignedAt, &d.PickedUpAt, &d.DeliveredAt, &d.FailureReason,
		&d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	d.Mode = domain.Mode(mode)
	d.Status = domain.Status(status)
	d.Cost = money.FromMinor(cost, money.INR)
	if providerRef != nil {
		d.ProviderRef = *providerRef
	}
	return &d, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var _ domain.DeliveryRepository = (*DeliveryRepository)(nil)
