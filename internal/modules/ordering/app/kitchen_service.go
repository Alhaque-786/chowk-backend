package app

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/paging"
)

// KitchenOrderService drives the staff-facing order lifecycle: accept, start
// cooking, mark ready, hand over, deliver, reject.
type KitchenOrderService struct {
	db     *database.DB
	orders domain.OrderRepository

	stock    contracts.StockReserver
	riders   contracts.RiderDispatcher
	payments contracts.PaymentInitiator
	// kitchens resolves the kitchen NAME for event payloads. Without it every
	// status notification says "the kitchen" instead of "Khansama", which reads
	// like a placeholder someone forgot to fill -- because it is one.
	kitchens contracts.KitchenAvailability

	events eventbus.Publisher
	clock  clock.Clock
}

func NewKitchenOrderService(
	db *database.DB,
	orders domain.OrderRepository,
	stock contracts.StockReserver,
	riders contracts.RiderDispatcher,
	payments contracts.PaymentInitiator,
	kitchens contracts.KitchenAvailability,
	events eventbus.Publisher,
	clk clock.Clock,
) *KitchenOrderService {
	return &KitchenOrderService{
		db: db, orders: orders, stock: stock, riders: riders,
		payments: payments, kitchens: kitchens, events: events, clock: clk,
	}
}

// Queue returns the kitchen's live orders, oldest first.
//
// Only active statuses: a kitchen tablet showing yesterday's delivered orders is
// noise, and the partial index makes this query cheap however large the table
// grows.
func (s *KitchenOrderService) Queue(ctx context.Context, kitchenID id.ID) ([]*domain.Order, error) {
	page, err := paging.NewRequest("", 100, paging.Ascending)
	if err != nil {
		return nil, err
	}
	return s.orders.ListForKitchen(ctx, kitchenID, domain.Filter{
		Statuses: []domain.Status{
			domain.StatusPlaced, domain.StatusConfirmed,
			domain.StatusPreparing, domain.StatusReady, domain.StatusPickedUp,
		},
	}, page)
}

type TransitionInput struct {
	OrderID    id.ID
	KitchenID  id.ID
	NextStatus domain.Status
	Reason     string
	ActorID    id.ID
	ActorRole  string
}

// Transition moves an order to its next status.
//
// One method for every staff-driven transition rather than accept()/prepare()/
// ready() variants, because the ONLY thing that differs between them is the
// target status and its side effects. The state machine in the domain decides
// legality; this method handles locking, persistence, side effects and the event.
func (s *KitchenOrderService) Transition(ctx context.Context, in TransitionInput) (*domain.Order, error) {
	now := s.clock.Now()

	var (
		order *domain.Order
		from  domain.Status
	)

	err := s.db.InTx(ctx, func(ctx context.Context) error {
		// Row lock for the whole transaction. Two staff tapping the same button on
		// two tablets, or a customer cancelling at the same instant, must serialise
		// here rather than both succeeding and one silently losing.
		locked, err := s.orders.FindByIDForUpdate(ctx, in.OrderID)
		if err != nil {
			return err
		}
		// Tenant guard: staff at one kitchen must not be able to touch another
		// kitchen's order by passing its id.
		if locked.KitchenID != in.KitchenID {
			return apperr.NotFound("ORDER_NOT_FOUND", "That order could not be found.")
		}

		from = locked.Status

		event, err := locked.TransitionTo(in.NextStatus, in.Reason, &in.ActorID, in.ActorRole, now)
		if err != nil {
			return err
		}
		if err := s.orders.Update(ctx, locked); err != nil {
			return err
		}
		if err := s.orders.AppendStatusEvent(ctx, event); err != nil {
			return err
		}

		// Side effects that must be atomic with the status change.
		switch in.NextStatus {
		case domain.StatusPreparing:
			// Cooking has started: the ingredients are now actually spent, so the
			// reservation becomes consumption. Doing this at `placed` would be too
			// early (the order may still be cancelled) and at `ready` too late
			// (stock would read as available while it is in a pan).
			if err := s.stock.Consume(ctx, locked.ID); err != nil {
				return err
			}

		case domain.StatusCancelled, domain.StatusRejected:
			// Give the stock back. Without this a rejected order holds ingredients
			// forever.
			if err := s.stock.Release(ctx, locked.ID); err != nil {
				return err
			}
		}

		eventName, err := statusEventName(in.NextStatus)
		if err != nil {
			return err
		}
		if err := s.publishStatus(ctx, locked, eventName, from, in.Reason, &in.ActorID, in.ActorRole, now); err != nil {
			return err
		}

		order = locked
		return nil
	})
	if err != nil {
		return nil, err
	}

	// --- side effects that must NOT be in the transaction ---

	// Requesting a rider is a network call to a 3PL. Same rule as the payment
	// gateway: never hold a transaction open across it.
	if in.NextStatus == domain.StatusReady {
		if err := s.riders.RequestRider(ctx, order.ID, order.KitchenID,
			order.DeliveryLatitude, order.DeliveryLongitude); err != nil {
			// A failed dispatch must not undo "the food is ready". Staff can call a
			// rider manually; the alternative is refusing to let them mark food
			// ready because a 3PL API was down.
			metrics.ErrorsTotal.WithLabelValues("unavailable", "RIDER_DISPATCH_FAILED").Inc()
		}
	}

	// A rejected prepaid order owes the customer a refund.
	if in.NextStatus == domain.StatusRejected && order.PaymentMethod.IsPrepaid() {
		if refundable := order.RefundableAmount(); refundable.IsPositive() {
			_ = s.payments.Refund(ctx, order.ID, refundable, in.Reason)
		}
	}

	switch in.NextStatus {
	case domain.StatusDelivered:
		metrics.OrdersTotal.WithLabelValues("delivered", string(order.PaymentMethod)).Inc()
		// Feed real prep time back so ETAs can eventually be derived from
		// observation instead of a configured guess.
		if order.ConfirmedAt != nil && order.PreparedAt != nil {
			metrics.OrderPrepDuration.WithLabelValues(order.KitchenID.String()).
				Observe(order.PreparedAt.Sub(*order.ConfirmedAt).Seconds())
		}
	case domain.StatusRejected:
		metrics.OrdersTotal.WithLabelValues("rejected", string(order.PaymentMethod)).Inc()
	}

	return order, nil
}

func (s *KitchenOrderService) publishStatus(
	ctx context.Context,
	order *domain.Order,
	name eventbus.Name,
	from domain.Status,
	reason string,
	actorID *id.ID,
	actorRole string,
	now time.Time,
) error {
	// Snapshot the name onto the event so consumers need not call back for it.
	// Best effort: a lookup failure must not stop the kitchen marking food ready.
	kitchenName, nameErr := s.kitchens.Name(ctx, order.KitchenID)
	if nameErr != nil || kitchenName == "" {
		kitchenName = "The kitchen"
	}

	payload := contracts.OrderStatusChangedPayload{
		OrderID:     order.ID,
		OrderNumber: order.Number,
		UserID:      order.UserID,
		KitchenID:   order.KitchenID,
		KitchenName: kitchenName,
		FromStatus:  string(from),
		ToStatus:    string(order.Status),
		Reason:      reason,
		ChangedAt:   now,
	}
	if actorID != nil {
		payload.ChangedBy = *actorID
	}

	evt, err := eventbus.New(name, order.ID, payload)
	if err != nil {
		return err
	}
	evt.TraceID = httpx.TraceIDFrom(ctx)
	return s.events.Publish(ctx, evt)
}

// GetForKitchen loads one order, scoped to the caller's kitchen.
func (s *KitchenOrderService) GetForKitchen(ctx context.Context, kitchenID, orderID id.ID) (*domain.Order, error) {
	order, err := s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if order.KitchenID != kitchenID {
		return nil, apperr.NotFound("ORDER_NOT_FOUND", "That order could not be found.")
	}
	return order, nil
}

// ReapAbandonedOrders releases stock from orders stuck in pending_payment.
//
// Runs in the worker. Without it, a customer who opens checkout and walks away
// holds the last portion of biryani indefinitely — and because the order was
// never paid, nobody is watching it.
func (s *KitchenOrderService) ReapAbandonedOrders(ctx context.Context) (int, error) {
	// 30 minutes is generous: it must comfortably exceed how long a UPI or 3-D
	// Secure flow can legitimately take, or the reaper would cancel orders that
	// are about to succeed.
	cutoff := s.clock.Now().Add(-30 * time.Minute)

	stale, err := s.orders.FindStale(ctx, domain.StatusPendingPayment, cutoff, 100)
	if err != nil {
		return 0, err
	}

	reaped := 0
	for _, order := range stale {
		// One transaction PER order, not one for the batch: a single problem order
		// must not block the rest, and holding locks on 100 orders at once would
		// contend with live traffic.
		err := s.db.InTx(ctx, func(ctx context.Context) error {
			locked, err := s.orders.FindByIDForUpdate(ctx, order.ID)
			if err != nil {
				return err
			}
			// Re-check under the lock: payment may have landed between the scan and
			// now, and cancelling a just-paid order would be a serious bug.
			if locked.Status != domain.StatusPendingPayment {
				return nil
			}

			event, err := locked.TransitionTo(domain.StatusCancelled,
				"payment not completed", nil, "system", s.clock.Now())
			if err != nil {
				return err
			}
			if err := s.orders.Update(ctx, locked); err != nil {
				return err
			}
			if err := s.orders.AppendStatusEvent(ctx, event); err != nil {
				return err
			}
			if err := s.stock.Release(ctx, locked.ID); err != nil {
				return err
			}
			return s.publishStatus(ctx, locked, contracts.EventOrderCancelled,
				domain.StatusPendingPayment, "payment not completed", nil, "system", s.clock.Now())
		})
		if err != nil {
			continue
		}
		reaped++
	}
	return reaped, nil
}
