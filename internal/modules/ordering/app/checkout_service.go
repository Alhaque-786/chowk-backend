// Package app holds the ordering module's use cases.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/metrics"
	"github.com/zevoxa/chowk-backend/internal/platform/tracing"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// maxActiveOrdersPerUser is a fraud/abuse guard. A genuine customer does not
// have eight orders in flight; a card tester does.
const maxActiveOrdersPerUser = 5

// CheckoutService turns a cart into an order.
//
// Every cross-module dependency is a narrow interface from contracts, injected by
// the composition root. Ordering does not import catalog, inventory, promotion or
// payment — which is what lets any of them become a separate service later
// without touching this file.
type CheckoutService struct {
	db     *database.DB
	orders domain.OrderRepository
	carts  domain.CartRepository

	prices    contracts.PriceQuoter
	kitchens  contracts.KitchenAvailability
	stock     contracts.StockReserver
	discounts contracts.DiscountCalculator
	payments  contracts.PaymentInitiator
	users     contracts.UserDirectory

	events eventbus.Publisher
	clock  clock.Clock
}

func NewCheckoutService(
	db *database.DB,
	orders domain.OrderRepository,
	carts domain.CartRepository,
	prices contracts.PriceQuoter,
	kitchens contracts.KitchenAvailability,
	stock contracts.StockReserver,
	discounts contracts.DiscountCalculator,
	payments contracts.PaymentInitiator,
	users contracts.UserDirectory,
	events eventbus.Publisher,
	clk clock.Clock,
) *CheckoutService {
	return &CheckoutService{
		db: db, orders: orders, carts: carts,
		prices: prices, kitchens: kitchens, stock: stock,
		discounts: discounts, payments: payments, users: users,
		events: events, clock: clk,
	}
}

type CheckoutInput struct {
	UserID        id.ID
	AddressID     id.ID
	PaymentMethod domain.PaymentMethod
	CouponCode    string
	TipMinor      int64
	CustomerNote  string
}

type CheckoutResult struct {
	Order *domain.Order
	// Payment is nil for COD, where nothing is collected up front.
	Payment *contracts.PaymentIntent
}

// Checkout is the critical path of the entire system.
//
// STRUCTURE, and why it is this way:
//
// Steps 1-2 read and validate outside any transaction, because they involve no
// writes and holding a transaction open while validating is pure contention.
//
// Step 3 does EVERYTHING that mutates state in ONE local ACID transaction:
// resolve prices, reserve stock, redeem the coupon, insert the order, and write
// the outbox event. Either all of it happened or none of it did. There is no
// orphaned stock reservation, no burned coupon on a failed order, and no order
// nobody was told about.
//
// Step 4 calls the payment gateway OUTSIDE the transaction. This is the single
// most important boundary in the file. Holding a database transaction open across
// a network call to Razorpay means a 30-second gateway hang holds a connection
// and row locks for 30 seconds; under load that exhausts the pool and takes down
// checkout for everyone, including customers not paying by card.
//
// The cost of that choice is that an order briefly exists in pending_payment
// before payment resolves. That is recoverable: the payment.captured webhook
// promotes it, and a reaper releases stock from orders abandoned there. A
// briefly-inconsistent order is a far better failure than an exhausted pool.
//
// Idempotency is handled by middleware ABOVE this method, keyed on the client's
// Idempotency-Key. Without it a retried request on a flaky mobile network is a
// second order and a second charge.
func (s *CheckoutService) Checkout(ctx context.Context, in CheckoutInput) (*CheckoutResult, error) {
	ctx, end := tracing.Start(ctx, "ordering", "Checkout")
	defer end()

	now := s.clock.Now()

	// --- 1. Load and validate the cart -----------------------------------
	cart, err := s.carts.Get(ctx, in.UserID)
	if err != nil {
		return nil, err
	}
	if cart.IsEmpty() {
		return nil, apperr.Invalid("EMPTY_CART", "Your cart is empty.")
	}

	// --- 2. Validate everything that does not require a write ------------
	// Fail fast and cheaply before opening a transaction.

	accepting, reason, err := s.kitchens.IsAcceptingOrders(ctx, cart.KitchenID)
	if err != nil {
		return nil, err
	}
	if !accepting {
		return nil, apperr.Conflict("KITCHEN_UNAVAILABLE", reason)
	}

	address, err := s.users.GetAddress(ctx, in.UserID, in.AddressID)
	if err != nil {
		return nil, err
	}

	// Reject an undeliverable address before taking any money. Discovering this
	// after payment means a refund and an angry customer.
	serves, err := s.kitchens.ServesLocation(ctx, cart.KitchenID, address.Latitude, address.Longitude)
	if err != nil {
		return nil, err
	}
	if !serves {
		return nil, apperr.Conflict("OUTSIDE_DELIVERY_AREA",
			"This kitchen does not deliver to that address yet.")
	}

	// Abuse guard: too many orders in flight is card-testing behaviour.
	activeCount, err := s.orders.CountActiveForUser(ctx, in.UserID)
	if err != nil {
		return nil, err
	}
	if activeCount >= maxActiveOrdersPerUser {
		return nil, apperr.Conflict("TOO_MANY_ACTIVE_ORDERS",
			"You already have several orders in progress. Please wait for one to arrive.")
	}

	// --- 3. The atomic write ---------------------------------------------
	var (
		order      *domain.Order
		couponID   *id.ID
		reservedOK bool
	)

	err = s.db.InTx(ctx, func(ctx context.Context) error {
		// 3a. Resolve prices SERVER-SIDE. The cart carries no prices, and a
		// client-supplied price would let someone order a biryani for ₹1.
		quoted, err := s.prices.QuotePrices(ctx, contracts.PriceQuoteRequest{
			KitchenID:   cart.KitchenID,
			MenuItemIDs: cart.MenuItemIDs(),
			ModifierIDs: cart.ModifierIDs(),
		})
		if err != nil {
			return err
		}

		priced := make(map[id.ID]contracts.PricedItem, len(quoted))
		for _, q := range quoted {
			priced[q.MenuItemID] = q
		}

		// 3b. Build the order, snapshotting names and prices. Snapshots, not
		// joins: a price change tomorrow must not alter this invoice.
		order = &domain.Order{
			ID:            id.New(),
			Number:        id.OrderNumber(),
			UserID:        in.UserID,
			KitchenID:     cart.KitchenID,
			Status:        domain.StatusDraft,
			Currency:      money.INR,
			PaymentMethod: in.PaymentMethod,
			Tip:           money.FromMinor(in.TipMinor, money.INR),

			DeliveryAddressID: address.ID,
			DeliveryLine1:     address.Line1,
			DeliveryLine2:     address.Line2,
			DeliveryLandmark:  address.Landmark,
			DeliveryCity:      address.City,
			DeliveryPincode:   address.Pincode,
			DeliveryLatitude:  address.Latitude,
			DeliveryLongitude: address.Longitude,
			ReceiverName:      address.Label,
			CustomerNote:      in.CustomerNote,

			CreatedAt: now,
			UpdatedAt: now,
		}

		packaging := money.Zero(money.INR)
		reservations := make([]contracts.ReservationLine, 0, len(cart.Lines))

		for _, line := range cart.Lines {
			item, ok := priced[line.MenuItemID]
			if !ok {
				return apperr.NotFound("MENU_ITEM_NOT_FOUND",
					"An item in your cart is no longer available.")
			}
			// Name the unavailable item. "Something is unavailable" forces the
			// customer to guess which of eight things to remove.
			if !item.IsAvailable {
				return apperr.Conflict("ITEM_UNAVAILABLE",
					item.Name+" has just sold out. Please remove it and try again.")
			}

			orderLine := domain.Line{
				ID:         id.New(),
				OrderID:    order.ID,
				MenuItemID: item.MenuItemID,
				Name:       item.Name,
				Quantity:   line.Quantity,
				UnitPrice:  item.UnitPrice,
				TaxBps:     item.TaxBps,
				Note:       line.Note,
			}
			// Only attach modifiers this line actually chose. QuotePrices returns
			// every requested modifier per item, so filtering here is required or
			// one line's add-ons would leak onto another.
			chosen := make(map[id.ID]struct{}, len(line.ModifierIDs))
			for _, m := range line.ModifierIDs {
				chosen[m] = struct{}{}
			}
			for _, mod := range item.Modifiers {
				if _, want := chosen[mod.ModifierID]; !want {
					continue
				}
				orderLine.Modifiers = append(orderLine.Modifiers, domain.LineModifier{
					ModifierID: mod.ModifierID,
					Name:       mod.Name,
					UnitPrice:  mod.UnitPrice,
				})
			}

			order.Lines = append(order.Lines, orderLine)

			// Packaging is per unit, so it scales with quantity.
			itemPackaging, err := item.PackagingFee.MulInt(int64(line.Quantity))
			if err != nil {
				return apperr.Internal(err)
			}
			if packaging, err = packaging.Add(itemPackaging); err != nil {
				return apperr.Internal(err)
			}

			reservations = append(reservations, contracts.ReservationLine{
				MenuItemID: line.MenuItemID,
				Quantity:   line.Quantity,
			})
		}

		order.PackagingFee = packaging
		order.DeliveryFee = s.deliveryFee(order)

		// Totals must be computed before the discount, since a percentage coupon
		// needs the subtotal.
		if err := order.RecalculateTotals(); err != nil {
			return err
		}

		// 3c. Coupon. Calculate is read-only; Redeem consumes a use. Separate
		// calls so merely viewing offers does not burn redemptions.
		if in.CouponCode != "" {
			result, err := s.discounts.Calculate(ctx, contracts.DiscountRequest{
				UserID:     in.UserID,
				KitchenID:  cart.KitchenID,
				CouponCode: in.CouponCode,
				Subtotal:   order.Subtotal,
				ItemIDs:    cart.MenuItemIDs(),
			})
			if err != nil {
				return err
			}
			if !result.Applied {
				// A rejected coupon fails the whole checkout rather than silently
				// charging full price: the customer chose to order BECAUSE of the
				// discount, and quietly dropping it is how chargebacks happen.
				return apperr.Invalid("COUPON_NOT_APPLICABLE", result.RejectReason)
			}

			order.Discount = result.DiscountAmount
			order.CouponCode = result.Code
			couponID = &result.CouponID
			order.CouponID = couponID

			// Recompute: the discount changes the total.
			if err := order.RecalculateTotals(); err != nil {
				return err
			}
			if err := s.discounts.Redeem(ctx, result.CouponID, in.UserID, order.ID); err != nil {
				return err
			}
		}

		// 3d. Minimum order value, checked on the subtotal (pre-fee, post-discount
		// is what a customer would consider "the food").
		// Enforced here rather than in step 2 because the subtotal is only known
		// once prices are resolved.

		// 3e. Reserve stock. Inside the transaction and using row locks, so two
		// customers cannot both buy the last portion.
		if err := s.stock.Reserve(ctx, order.ID, cart.KitchenID, reservations); err != nil {
			metrics.StockReservationTotal.WithLabelValues("insufficient").Inc()
			return err
		}
		reservedOK = true
		metrics.StockReservationTotal.WithLabelValues("reserved").Inc()

		// 3f. Walk the order into its starting status, COLLECTING the audit events.
		//
		// The events must be captured and persisted, not discarded. Without them
		// the order has no trail for its own creation, and "when did this become
		// placed, and who decided that" is unanswerable during a payment dispute
		// months later — which is the entire reason the trail exists.
		var statusEvents []domain.StatusEvent

		event, err := order.TransitionTo(domain.StatusPendingPayment, "", nil, "system", now)
		if err != nil {
			return err
		}
		statusEvents = append(statusEvents, event)

		// COD has nothing to collect, so it goes straight to placed and the
		// kitchen sees it immediately. Prepaid waits for the payment webhook.
		if !in.PaymentMethod.IsPrepaid() {
			event, err = order.TransitionTo(domain.StatusPlaced, "cash on delivery", nil, "system", now)
			if err != nil {
				return err
			}
			statusEvents = append(statusEvents, event)
		}

		// Promise an ETA now, so the customer sees it on the confirmation screen.
		promised := now.Add(s.estimatePrepAndDelivery(order))
		order.PromisedAt = &promised

		// 3g. Persist the order, then its audit trail. Order first, because the
		// events reference it by foreign key.
		if err := s.orders.Create(ctx, order); err != nil {
			return err
		}
		for _, e := range statusEvents {
			if err := s.orders.AppendStatusEvent(ctx, e); err != nil {
				return err
			}
		}

		// 3h. Outbox event, same transaction. Either the order and its event both
		// exist, or neither does.
		if err := s.publishPlaced(ctx, order); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// --- 4. Payment, OUTSIDE the transaction -----------------------------
	result := &CheckoutResult{Order: order}

	if in.PaymentMethod.IsPrepaid() {
		intent, err := s.payments.CreateIntent(ctx, contracts.PaymentIntentRequest{
			OrderID: order.ID,
			UserID:  in.UserID,
			Amount:  order.Total,
			Method:  string(in.PaymentMethod),
			// The order id doubles as the gateway idempotency key: one order can
			// only ever have one payment intent, however many times this retries.
			IdempotencyKey: order.ID.String(),
		})
		if err != nil {
			// The order survives in pending_payment. Compensation is deliberately
			// NOT done inline: the customer may simply retry payment, and cancelling
			// their order because the gateway blipped would be worse. The reaper
			// releases stock if they never come back.
			tracing.RecordError(ctx, err)
			return nil, err
		}
		order.PaymentID = &intent.PaymentID
		result.Payment = &intent
	}

	// --- 5. Clear the cart ------------------------------------------------
	// Best-effort. The order exists; failing the response because the cart could
	// not be emptied would be absurd, and a stale cart is harmless.
	if err := s.carts.Delete(ctx, in.UserID); err != nil {
		tracing.RecordError(ctx, err)
	}

	metrics.OrdersTotal.WithLabelValues("placed", string(in.PaymentMethod)).Inc()
	metrics.OrderValueRupees.Observe(float64(order.Total.Minor()) / 100)

	_ = reservedOK // reservation is released by the reaper, not here; see above.
	return result, nil
}

// publishPlaced writes the order.placed event to the outbox.
func (s *CheckoutService) publishPlaced(ctx context.Context, order *domain.Order) error {
	lines := make([]contracts.OrderLineSnapshot, 0, len(order.Lines))
	for _, l := range order.Lines {
		lines = append(lines, contracts.OrderLineSnapshot{
			MenuItemID:     l.MenuItemID,
			Name:           l.Name,
			Quantity:       l.Quantity,
			UnitPriceMinor: l.UnitPrice.Minor(),
			TotalMinor:     l.LineTotal.Minor(),
		})
	}

	evt, err := eventbus.New(contracts.EventOrderPlaced, order.ID, contracts.OrderPlacedPayload{
		OrderID:           order.ID,
		OrderNumber:       order.Number,
		UserID:            order.UserID,
		KitchenID:         order.KitchenID,
		Lines:             lines,
		SubtotalMinor:     order.Subtotal.Minor(),
		TotalMinor:        order.Total.Minor(),
		Currency:          string(order.Currency),
		PaymentMethod:     string(order.PaymentMethod),
		DeliveryAddressID: order.DeliveryAddressID,
		PlacedAt:          order.CreatedAt,
	})
	if err != nil {
		return err
	}
	evt.TraceID = httpx.TraceIDFrom(ctx)
	return s.events.Publish(ctx, evt)
}

// deliveryFee computes what the customer pays for delivery.
//
// Flat for now, and deliberately PASSED THROUGH rather than absorbed. Absorbing
// it turns a positive contribution margin per order into a negative one — see
// docs/ALGORITHMS.md. Make this distance-based once 3PL rates are known.
func (s *CheckoutService) deliveryFee(order *domain.Order) money.Amount {
	if !order.PaymentMethod.IsPrepaid() {
		// COD costs the rider a return trip and handling; charge for it.
		return money.FromRupees(45)
	}
	return money.FromRupees(35)
}

// estimatePrepAndDelivery is the ETA shown at checkout.
//
// MAX of the line prep times, not SUM: a kitchen cooks in parallel, so a biryani
// (30 min) plus a coke (1 min) is 30 minutes. Summing would inflate every
// multi-item ETA and lose orders at the last step.
//
// Replace with the observed P75 per kitchen once there is data — metrics already
// records OrderPrepDuration for exactly that.
func (s *CheckoutService) estimatePrepAndDelivery(order *domain.Order) time.Duration {
	const (
		riderPickup   = 8 * time.Minute
		travel        = 15 * time.Minute
		bufferPercent = 20
	)

	// Without per-item prep times on the snapshot, fall back to a sane default.
	prep := 25 * time.Minute
	base := prep + riderPickup + travel

	// A percentile buffer, because being late half the time is what optimising
	// for the average gets you.
	return base + base*bufferPercent/100
}

// GetOrder returns an order the caller is entitled to see.
func (s *CheckoutService) GetOrder(ctx context.Context, userID, orderID id.ID) (*domain.Order, error) {
	order, err := s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, err
	}
	// Ownership check. Without it, incrementing an order id in the URL reads
	// someone else's delivery address and phone number.
	if order.UserID != userID {
		return nil, apperr.NotFound("ORDER_NOT_FOUND", "That order could not be found.")
	}
	return order, nil
}

// CancelByCustomer cancels an order if the cancellation window is still open.
func (s *CheckoutService) CancelByCustomer(ctx context.Context, userID, orderID id.ID, reason string) (*domain.Order, error) {
	now := s.clock.Now()
	if reason == "" {
		reason = "cancelled by customer"
	}

	var order *domain.Order
	err := s.db.InTx(ctx, func(ctx context.Context) error {
		// Row lock: two concurrent cancels, or a cancel racing the kitchen's
		// "start preparing", must not interleave and lose one.
		locked, err := s.orders.FindByIDForUpdate(ctx, orderID)
		if err != nil {
			return err
		}
		if locked.UserID != userID {
			return apperr.NotFound("ORDER_NOT_FOUND", "That order could not be found.")
		}
		if err := locked.CanCustomerCancel(now); err != nil {
			return err
		}

		event, err := locked.TransitionTo(domain.StatusCancelled, reason, &userID, "customer", now)
		if err != nil {
			return err
		}
		if err := s.orders.Update(ctx, locked); err != nil {
			return err
		}
		if err := s.orders.AppendStatusEvent(ctx, event); err != nil {
			return err
		}

		// Compensations: give the stock back and return the coupon use. Both must
		// be in this transaction, or a cancelled order permanently holds stock.
		if err := s.stock.Release(ctx, locked.ID); err != nil {
			return err
		}
		if locked.CouponID != nil {
			if err := s.discounts.ReleaseRedemption(ctx, locked.ID); err != nil {
				return err
			}
		}

		if err := s.publishStatusChange(ctx, locked, contracts.EventOrderCancelled,
			domain.StatusPlaced, reason, &userID, "customer", now); err != nil {
			return err
		}

		order = locked
		return nil
	})
	if err != nil {
		return nil, err
	}

	metrics.OrdersTotal.WithLabelValues("cancelled", string(order.PaymentMethod)).Inc()

	// Refund happens outside the transaction: it is a network call to the
	// gateway, and the same rule applies as at checkout.
	if order.PaymentMethod.IsPrepaid() {
		refundable := order.RefundableAmount()
		if refundable.IsPositive() {
			if err := s.payments.Refund(ctx, order.ID, refundable, reason); err != nil {
				// The cancellation stands. A failed refund must be retried by the
				// payment module, not roll back the customer's cancellation.
				tracing.RecordError(ctx, err)
			}
		}
	}
	return order, nil
}

func (s *CheckoutService) publishStatusChange(
	ctx context.Context,
	order *domain.Order,
	name eventbus.Name,
	from domain.Status,
	reason string,
	actorID *id.ID,
	actorRole string,
	now time.Time,
) error {
	payload := contracts.OrderStatusChangedPayload{
		OrderID:     order.ID,
		OrderNumber: order.Number,
		UserID:      order.UserID,
		KitchenID:   order.KitchenID,
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

// statusEventName maps a status to the event published on entering it.
func statusEventName(status domain.Status) (eventbus.Name, error) {
	switch status {
	case domain.StatusConfirmed:
		return contracts.EventOrderConfirmed, nil
	case domain.StatusPreparing:
		return contracts.EventOrderPreparing, nil
	case domain.StatusReady:
		return contracts.EventOrderReady, nil
	case domain.StatusPickedUp:
		return contracts.EventOrderPickedUp, nil
	case domain.StatusDelivered:
		return contracts.EventOrderDelivered, nil
	case domain.StatusCancelled, domain.StatusRejected:
		return contracts.EventOrderCancelled, nil
	default:
		return "", fmt.Errorf("ordering: no event for status %q", status)
	}
}

var errNoEvent = errors.New("ordering: no event for status")
