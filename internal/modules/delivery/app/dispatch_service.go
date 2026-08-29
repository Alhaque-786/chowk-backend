// Package app holds the delivery module's use cases.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// DispatchService books deliveries and tracks their status.
type DispatchService struct {
	db         *database.DB
	deliveries domain.DeliveryRepository
	provider   domain.Provider
	users      contracts.UserDirectory
	orders     contracts.OrderOwnership
	events     eventbus.Publisher
	clock      clock.Clock
}

func NewDispatchService(
	db *database.DB,
	deliveries domain.DeliveryRepository,
	provider domain.Provider,
	users contracts.UserDirectory,
	orders contracts.OrderOwnership,
	events eventbus.Publisher,
	clk clock.Clock,
) *DispatchService {
	return &DispatchService{
		db: db, deliveries: deliveries, provider: provider,
		users: users, orders: orders, events: events, clock: clk,
	}
}

// RequestRider implements contracts.RiderDispatcher.
//
// Called by ordering when an order reaches `ready`, OUTSIDE its transaction —
// this makes a network call to a 3PL, and holding a transaction across it would
// let a slow provider exhaust the connection pool.
//
// Idempotent: a redelivered order.ready must not book a second rider, which would
// mean paying twice for one delivery.
func (s *DispatchService) RequestRider(ctx context.Context, orderID, kitchenID id.ID, dropLat, dropLng float64) error {
	existing, err := s.deliveries.FindByOrderID(ctx, orderID)
	if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
		return err
	}
	if existing != nil {
		// Already booked. Success, not an error: at-least-once delivery means this
		// will legitimately be called again.
		return nil
	}

	now := s.clock.Now()

	// Persist BEFORE calling the provider, so a crash mid-call still leaves a
	// record to reconcile against. The reverse order loses track of a rider we
	// have already paid for.
	delivery := &domain.Delivery{
		ID:        id.New(),
		OrderID:   orderID,
		KitchenID: kitchenID,
		Mode:      domain.ModeThirdParty,
		Status:    domain.StatusRequested,
		Provider:  s.provider.Name(),
		Cost:      money.Zero(money.INR),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.deliveries.Create(ctx, delivery); err != nil {
		return err
	}

	job, err := s.provider.CreateJob(ctx, domain.JobRequest{
		OrderID: orderID,
		DropLat: dropLat,
		DropLng: dropLng,
		ReadyAt: now,
	})
	if err != nil {
		// The delivery record stays in `requested` so it appears on the kitchen's
		// dashboard as needing a manual rider. Deliberately does NOT return an
		// error to the caller: ordering must still be able to mark food ready when
		// a 3PL API is down, or a provider outage becomes a kitchen outage.
		logger.From(ctx).Error("booking a rider failed; falling back to manual dispatch",
			"order_id", orderID, "provider", s.provider.Name(), "error", err)
		return nil
	}

	delivery.ProviderRef = job.ProviderRef
	delivery.TrackingURL = job.TrackingURL
	delivery.Cost = job.Cost
	delivery.PickupETA = job.PickupETA
	delivery.DropETA = job.DropETA
	delivery.UpdatedAt = s.clock.Now()

	return s.deliveries.Update(ctx, delivery)
}

// ProcessWebhook applies a provider status update.
//
// Same shape as payment: verify the signature over the raw body first, then apply
// under a row lock, then publish. Redelivery is a no-op success so the provider
// stops retrying.
func (s *DispatchService) ProcessWebhook(ctx context.Context, rawBody []byte, signature string) error {
	payload, err := s.provider.VerifyWebhook(rawBody, signature)
	if err != nil {
		return err
	}
	if payload.Status == "" {
		// Informational event. Nothing to apply, and not an error.
		return nil
	}

	delivery, err := s.deliveries.FindByProviderRef(ctx, s.provider.Name(), payload.ProviderRef)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			logger.From(ctx).Warn("delivery webhook for an unknown job",
				"provider_ref", payload.ProviderRef)
			return nil
		}
		return err
	}

	now := s.clock.Now()

	return s.db.InTx(ctx, func(ctx context.Context) error {
		locked, err := s.deliveries.FindByOrderIDForUpdate(ctx, delivery.OrderID)
		if err != nil {
			return err
		}

		if err := locked.TransitionTo(payload.Status, now); err != nil {
			if errors.Is(err, domain.ErrAlreadyInStatus) {
				return nil
			}
			return err
		}

		// Rider details arrive with the assignment, and the customer needs them to
		// call about a missing delivery.
		if payload.RiderName != "" {
			locked.RiderName = payload.RiderName
		}
		if payload.RiderPhone != "" {
			locked.RiderPhone = payload.RiderPhone
		}
		if payload.DropETA != nil {
			locked.DropETA = payload.DropETA
		}
		locked.FailureReason = payload.FailureReason

		if err := s.deliveries.Update(ctx, locked); err != nil {
			return err
		}
		return s.publishStatus(ctx, locked)
	})
}

// publishStatus announces a delivery event.
//
// Ordering can subscribe to advance the order (picked_up, delivered) and
// notification to tell the customer. Delivery does not call either.
func (s *DispatchService) publishStatus(ctx context.Context, d *domain.Delivery) error {
	if d.Status != domain.StatusAssigned {
		// Only the assignment carries a distinct payload today. The order's own
		// picked_up/delivered transitions are driven by the kitchen app, so
		// republishing them here would duplicate events.
		return nil
	}

	eta := int32(0)
	if d.DropETA != nil {
		eta = int32(time.Until(*d.DropETA).Minutes())
	}

	evt, err := eventbus.New(contracts.EventRiderAssigned, d.OrderID, contracts.RiderAssignedPayload{
		OrderID:    d.OrderID,
		RiderName:  d.RiderName,
		RiderPhone: d.RiderPhone,
		AssignedAt: s.clock.Now(),
		ETAMinutes: eta,
	})
	if err != nil {
		return err
	}
	evt.TraceID = httpx.TraceIDFrom(ctx)
	return s.events.Publish(ctx, evt)
}

/*
GetForOrder returns the delivery for a customer's tracking view.

SCOPED TO THE VIEWER, which it was not. This endpoint previously checked only
that the caller held a valid token, so any signed-in user could read any
order's delivery by id and receive the rider's name and personal phone number.
Order ids are UUIDv7 and therefore time-ordered, so they are partially
guessable rather than opaque — consecutive orders share a long prefix.

platformActor is a BOOLEAN rather than a nil-able viewer id, matching the ops
queue. Support genuinely needs to see any delivery to answer "where is my
food", and a zero value that happened to mean "staff" is exactly the mistake
this shape prevents: forgetting to set it denies access rather than granting
it.

Not-yours and does-not-exist return the SAME not-found error. Distinguishing
them would turn this into an oracle for which order ids are real.
*/
func (s *DispatchService) GetForOrder(
	ctx context.Context, orderID, viewerID id.ID, platformActor bool,
) (*domain.Delivery, error) {
	if !platformActor {
		owned, err := s.orders.IsOrderOwnedBy(ctx, orderID, viewerID)
		if err != nil {
			return nil, err
		}
		if !owned {
			return nil, apperr.NotFound("DELIVERY_NOT_FOUND", "That delivery could not be found.")
		}
	}
	return s.deliveries.FindByOrderID(ctx, orderID)
}

// ListActiveForKitchen backs the staff dispatch board: what is out for delivery,
// and what still needs a rider.
func (s *DispatchService) ListActiveForKitchen(ctx context.Context, kitchenID id.ID) ([]*domain.Delivery, error) {
	return s.deliveries.ListActiveForKitchen(ctx, kitchenID)
}

// MarkStatusManually lets staff drive a delivery by hand.
//
// Necessary, not a convenience: with the manual provider this is the ONLY way a
// delivery progresses, and even with a 3PL a provider outage must not leave staff
// unable to record that food went out.
func (s *DispatchService) MarkStatusManually(ctx context.Context, orderID id.ID, next domain.Status, riderName, riderPhone string) (*domain.Delivery, error) {
	now := s.clock.Now()

	var result *domain.Delivery
	err := s.db.InTx(ctx, func(ctx context.Context) error {
		locked, err := s.deliveries.FindByOrderIDForUpdate(ctx, orderID)
		if err != nil {
			return err
		}
		if err := locked.TransitionTo(next, now); err != nil {
			if errors.Is(err, domain.ErrAlreadyInStatus) {
				result = locked
				return nil
			}
			return err
		}
		if riderName != "" {
			locked.RiderName = riderName
		}
		if riderPhone != "" {
			locked.RiderPhone = riderPhone
		}
		locked.Mode = domain.ModeSelf

		if err := s.deliveries.Update(ctx, locked); err != nil {
			return err
		}
		result = locked
		return s.publishStatus(ctx, locked)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

var _ contracts.RiderDispatcher = (*DispatchService)(nil)
