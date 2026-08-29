package app

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

/*
Rider position: writing it, and who may read it.

The read rules are the interesting half, and they are stricter than the
delivery record's. A customer may see WHERE THEIR RIDER IS only while he is
actually carrying their food. Once the order is delivered the position stops
being about their order and becomes a live feed of a named individual's
movements for the rest of his shift — which is not something a food order buys
you, and which the customer has no reason to want.
*/
type LocationService struct {
	deliveries domain.DeliveryRepository
	live       domain.LivePositionStore
	track      domain.TrackRepository
	orders     contracts.OrderOwnership
	clock      clock.Clock
}

func NewLocationService(
	deliveries domain.DeliveryRepository,
	live domain.LivePositionStore,
	track domain.TrackRepository,
	orders contracts.OrderOwnership,
	clk clock.Clock,
) *LocationService {
	return &LocationService{
		deliveries: deliveries, live: live, track: track, orders: orders, clock: clk,
	}
}

// ReportInput is a position as a rider's handset sends it.
type ReportInput struct {
	DeliveryID id.ID
	// RiderID is the authenticated caller, never a value from the request body.
	RiderID    id.ID
	Latitude   float64
	Longitude  float64
	AccuracyM  float64
	RecordedAt time.Time
}

/*
Report records where a rider is.

Called every few seconds per active rider, so the hot path is deliberately one
Redis write. The Postgres write happens only when the point survives
decimation, and a failure there is logged rather than returned: the rider's
app must not retry, and must not think it is offline, because a history row
could not be written. The live position — the thing anybody is actually
waiting for — already succeeded.

A rider may only report against a delivery ASSIGNED TO HIM, and only while it
is in flight. Both checks are on the delivery row rather than on anything the
handset sent.
*/
func (s *LocationService) Report(ctx context.Context, in ReportInput) error {
	delivery, err := s.deliveries.FindByID(ctx, in.DeliveryID)
	if err != nil {
		return err
	}

	// Not-yours and does-not-exist are the same answer, as everywhere else: a
	// rider probing ids should learn nothing about which deliveries are real.
	if delivery.RiderID == nil || *delivery.RiderID != in.RiderID {
		return apperr.NotFound("DELIVERY_NOT_FOUND", "That delivery could not be found.")
	}
	if delivery.Status.IsTerminal() {
		// The app can keep reporting for a few seconds after a rider marks a drop.
		// That is not an error and should not be shown as one, but it must not be
		// stored either — the delivery is over.
		return apperr.Conflict("DELIVERY_FINISHED", "That delivery is already finished.")
	}

	now := s.clock.Now()
	position, err := domain.NewPosition(
		in.DeliveryID, in.RiderID, in.Latitude, in.Longitude, in.AccuracyM, in.RecordedAt, now,
	)
	if err != nil {
		return err
	}

	if err := s.live.Put(ctx, position, domain.LivePositionTTL); err != nil {
		return err
	}

	// Everything below is best-effort history. See the doc comment.
	latest, err := s.track.Latest(ctx, in.DeliveryID)
	if err != nil {
		logger.From(ctx).Warn("could not read track tail", "delivery_id", in.DeliveryID, "error", err)
		return nil
	}
	if !position.ShouldPersist(latest) {
		return nil
	}
	if err := s.track.Append(ctx, position); err != nil {
		logger.From(ctx).Warn("could not append track point", "delivery_id", in.DeliveryID, "error", err)
	}
	return nil
}

// LiveLocation is what the customer's map draws.
type LiveLocation struct {
	Latitude  float64
	Longitude float64
	// RecordedAt lets the client say "as of a minute ago" rather than implying
	// the dot is live to the second.
	RecordedAt time.Time
}

/*
ForOrder returns the rider's current position, for the customer watching.

Returns nil, nil in every ordinary "nothing to show" case — no delivery yet, no
rider assigned, no position reported, the position gone stale, or the order
already delivered. The caller renders no dot. None of those is an error and
none should look like one.

THE ACCESS RULES ARE NARROWER THAN THE DELIVERY RECORD'S, deliberately:

	the order must be the caller's, as everywhere else;
	the delivery must be IN FLIGHT.

The second is the one worth arguing about. After delivery, a position is no
longer information about the customer's order — it is a live feed of a named
person's movements, and a completed food order is not consent to that.
*/
func (s *LocationService) ForOrder(
	ctx context.Context, orderID, viewerID id.ID, platformActor bool,
) (*LiveLocation, error) {
	if !platformActor {
		owned, err := s.orders.IsOrderOwnedBy(ctx, orderID, viewerID)
		if err != nil {
			return nil, err
		}
		if !owned {
			return nil, apperr.NotFound("DELIVERY_NOT_FOUND", "That delivery could not be found.")
		}
	}

	delivery, err := s.deliveries.FindByOrderID(ctx, orderID)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return nil, nil // no rider yet; the kitchen is still cooking
		}
		return nil, err
	}
	if delivery.Status.IsTerminal() {
		return nil, nil
	}

	position := s.live.Get(ctx, delivery.ID)
	if position == nil {
		return nil, nil
	}
	// A stale dot is worse than no dot: the customer watches a rider who is not
	// moving and concludes he is parked round the corner, when his phone died.
	if position.IsStale(s.clock.Now()) {
		return nil, nil
	}

	return &LiveLocation{
		Latitude:   position.Latitude,
		Longitude:  position.Longitude,
		RecordedAt: position.RecordedAt,
	}, nil
}

// ActiveForRider is what a rider's app asks for on every foreground: what am I
// carrying right now.
func (s *LocationService) ActiveForRider(ctx context.Context, riderID id.ID) ([]*domain.Delivery, error) {
	return s.deliveries.ListActiveForRider(ctx, riderID)
}
