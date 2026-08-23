// Package domain holds the ordering module's entities and the order lifecycle.
//
// Ordering is the spine of the system: it is the only module allowed to create
// an order, and every other module reacts to the events it publishes.
package domain

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
	"github.com/zevoxa/chowk-backend/internal/shared/paging"
)

// Status is the order lifecycle state.
type Status string

const (
	// StatusDraft is a cart. It becomes an order only on successful checkout.
	StatusDraft Status = "draft"
	// StatusPendingPayment means placed but awaiting gateway confirmation. The
	// kitchen must not see it yet -- otherwise a failed UPI payment results in
	// food that was cooked for free.
	StatusPendingPayment Status = "pending_payment"
	// StatusPlaced means paid (or COD) and awaiting kitchen acceptance.
	StatusPlaced Status = "placed"
	// StatusConfirmed means the kitchen accepted it.
	StatusConfirmed Status = "confirmed"
	StatusPreparing Status = "preparing"
	// StatusReady means packed and awaiting a rider.
	StatusReady     Status = "ready"
	StatusPickedUp  Status = "picked_up"
	StatusDelivered Status = "delivered"
	StatusCancelled Status = "cancelled"
	// StatusRejected is the kitchen refusing (out of stock, too busy). Distinct
	// from cancelled because the refund and the analytics differ.
	StatusRejected Status = "rejected"
	StatusRefunded Status = "refunded"
)

// transitions is the authoritative state machine.
//
// Encoding it as data rather than as scattered if-statements is the whole point:
// every legal move is visible in one screen, an illegal move is impossible by
// construction, and adding a state is a single edit instead of an audit of the
// codebase. Without this you eventually get an order that is both `delivered`
// and `cancelled`, and no way to reconstruct how.
var transitions = map[Status][]Status{
	StatusDraft:          {StatusPendingPayment, StatusCancelled},
	StatusPendingPayment: {StatusPlaced, StatusCancelled},
	// A kitchen may reject a paid order; that path must refund.
	StatusPlaced:    {StatusConfirmed, StatusRejected, StatusCancelled},
	StatusConfirmed: {StatusPreparing, StatusCancelled},
	// Once cooking starts, the customer can no longer cancel freely -- only
	// support can, and it is a write-off. See CanCustomerCancel.
	StatusPreparing: {StatusReady, StatusCancelled},
	StatusReady:     {StatusPickedUp, StatusCancelled},
	// No cancel from picked_up: the food is with the rider. A failed delivery is
	// resolved as a refund on the delivered order.
	StatusPickedUp:  {StatusDelivered},
	StatusDelivered: {StatusRefunded},
	// Terminal states.
	StatusCancelled: {StatusRefunded},
	StatusRejected:  {StatusRefunded},
	StatusRefunded:  {},
}

// IsTerminal reports whether no further transition is possible.
func (s Status) IsTerminal() bool { return len(transitions[s]) == 0 }

// CanTransitionTo reports whether the move is legal.
func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// IsActive reports whether the order is still in flight, for the "current
// orders" view and for deciding whether stock stays reserved.
func (s Status) IsActive() bool {
	switch s {
	case StatusPlaced, StatusConfirmed, StatusPreparing, StatusReady, StatusPickedUp:
		return true
	}
	return false
}

// PaymentMethod is how the customer pays.
type PaymentMethod string

const (
	PaymentUPI        PaymentMethod = "upi"
	PaymentCard       PaymentMethod = "card"
	PaymentNetBanking PaymentMethod = "netbanking"
	PaymentWallet     PaymentMethod = "wallet"
	// PaymentCOD skips the pending_payment state entirely.
	PaymentCOD PaymentMethod = "cod"
)

func (m PaymentMethod) IsPrepaid() bool { return m != PaymentCOD }

// Order is the aggregate root.
//
// Every price field is a snapshot taken at checkout, never a live join to the
// menu. If a kitchen raises the price of a dish tomorrow, yesterday's invoice
// must not change -- that is both a correctness and a legal requirement.
type Order struct {
	ID        id.ID
	Number    string // customer-facing, e.g. "CK-8F2K9Q"
	UserID    id.ID
	KitchenID id.ID
	Status    Status
	Lines     []Line

	// --- Money breakdown. Each component is stored, not derived, so a historical
	// invoice can be reproduced byte-for-byte even after tax rates change. ---
	Subtotal     money.Amount // sum of line totals, pre-tax
	PackagingFee money.Amount
	DeliveryFee  money.Amount
	Discount     money.Amount // positive number, subtracted
	TaxAmount    money.Amount
	Tip          money.Amount
	Total        money.Amount
	Currency     money.Currency

	PaymentMethod PaymentMethod
	PaymentID     *id.ID
	CouponID      *id.ID
	CouponCode    string

	// Delivery snapshot -- copied, not referenced. If the customer later edits
	// or deletes that saved address, the order still records where it went.
	DeliveryAddressID id.ID
	DeliveryLine1     string
	DeliveryLine2     string
	DeliveryLandmark  string
	DeliveryCity      string
	DeliveryPincode   string
	DeliveryLatitude  float64
	DeliveryLongitude float64
	ReceiverName      string
	ReceiverPhone     string

	RiderID *id.ID

	CustomerNote string
	// KitchenNote is internal (e.g. "regular customer, extra raita").
	KitchenNote string

	PromisedAt   *time.Time // ETA shown to the customer at checkout
	ConfirmedAt  *time.Time
	PreparedAt   *time.Time
	PickedUpAt   *time.Time
	DeliveredAt  *time.Time
	CancelledAt  *time.Time
	CancelReason string
	CancelledBy  *id.ID

	// Version backs optimistic locking. Two staff tablets, or a customer
	// cancelling while the kitchen accepts, would otherwise produce a lost update
	// where one action silently vanishes with no error. Every UPDATE carries
	// `AND version = $expected`, so the second writer gets a conflict and the UI
	// can reload instead of lying to the user.
	Version int32

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Line is one item in the order.
type Line struct {
	ID         id.ID
	OrderID    id.ID
	MenuItemID id.ID
	// Name is snapshotted so a renamed or deleted dish still prints correctly on
	// an old invoice.
	Name      string
	Quantity  int32
	UnitPrice money.Amount
	// LineTotal is (UnitPrice + modifiers) x Quantity, stored to avoid
	// recomputing with a since-changed rounding rule.
	LineTotal money.Amount
	TaxBps    int64
	TaxAmount money.Amount
	Modifiers []LineModifier
	Note      string
}

type LineModifier struct {
	ModifierID id.ID
	Name       string
	UnitPrice  money.Amount
}

// StatusEvent is one row of the order's audit trail.
//
// The trail is append-only and non-negotiable: for any payment dispute or
// customer complaint, "who moved this order to cancelled, when, and why" must be
// answerable months later.
type StatusEvent struct {
	ID        id.ID
	OrderID   id.ID
	From      Status
	To        Status
	Reason    string
	ActorID   *id.ID
	ActorRole string
	CreatedAt time.Time
}

// TransitionTo moves the order to the next state, validating the move and
// stamping the matching timestamp.
//
// It returns the audit event rather than writing it, because persistence belongs
// to the app layer -- the domain stays free of I/O.
func (o *Order) TransitionTo(next Status, reason string, actorID *id.ID, actorRole string, now time.Time) (StatusEvent, error) {
	if o.Status == next {
		// Idempotent no-op. Events are delivered at least once and the kitchen
		// UI can double-tap; neither should be an error.
		return StatusEvent{}, apperr.Conflict("ORDER_ALREADY_IN_STATUS",
			"This order is already "+string(next)+".")
	}
	if !o.Status.CanTransitionTo(next) {
		return StatusEvent{}, apperr.Conflict("INVALID_ORDER_TRANSITION",
			"An order that is "+string(o.Status)+" cannot become "+string(next)+".")
	}

	from := o.Status
	o.Status = next
	o.UpdatedAt = now

	switch next {
	case StatusConfirmed:
		o.ConfirmedAt = &now
	case StatusReady:
		o.PreparedAt = &now
	case StatusPickedUp:
		o.PickedUpAt = &now
	case StatusDelivered:
		o.DeliveredAt = &now
	case StatusCancelled, StatusRejected:
		o.CancelledAt = &now
		o.CancelReason = reason
		o.CancelledBy = actorID
	}

	return StatusEvent{
		ID:        id.New(),
		OrderID:   o.ID,
		From:      from,
		To:        next,
		Reason:    reason,
		ActorID:   actorID,
		ActorRole: actorRole,
		CreatedAt: now,
	}, nil
}

// CanCustomerCancel decides whether the customer may cancel unilaterally.
//
// The rule is a business decision, not a technical one: free cancellation until
// the kitchen starts cooking, because after that the ingredients are spent and
// someone has to absorb the cost.
func (o *Order) CanCustomerCancel(now time.Time) error {
	switch o.Status {
	case StatusPendingPayment, StatusPlaced, StatusConfirmed:
		return nil
	case StatusPreparing, StatusReady, StatusPickedUp:
		return apperr.Conflict("CANCEL_WINDOW_CLOSED",
			"Your food is already being prepared, so it can no longer be cancelled. Contact support if something is wrong.")
	case StatusDelivered:
		return apperr.Conflict("ORDER_DELIVERED", "This order has already been delivered.")
	default:
		return apperr.Conflict("ORDER_NOT_CANCELLABLE", "This order cannot be cancelled.")
	}
}

// RefundableAmount is what should be returned to the customer on cancellation.
//
// Tips are refunded with the order (the rider did not complete it), and the
// delivery fee is only kept once a rider has actually collected the food.
func (o *Order) RefundableAmount() money.Amount {
	if !o.PaymentMethod.IsPrepaid() {
		// Nothing was collected for COD.
		return money.Zero(o.Currency)
	}
	if o.Status == StatusPickedUp || o.Status == StatusDelivered {
		refund, err := o.Total.Sub(o.DeliveryFee)
		if err != nil {
			return o.Total
		}
		return refund
	}
	return o.Total
}

// RecalculateTotals recomputes every derived amount from the lines.
//
// Called during checkout, before the order is persisted. It is the only place
// totals are computed, so the arithmetic cannot diverge between the cart preview
// and the charged amount -- a class of bug that is invisible in testing and
// enraging in production.
func (o *Order) RecalculateTotals() error {
	subtotal := money.Zero(o.Currency)
	tax := money.Zero(o.Currency)

	for i := range o.Lines {
		line := &o.Lines[i]

		unit := line.UnitPrice
		for _, m := range line.Modifiers {
			var err error
			if unit, err = unit.Add(m.UnitPrice); err != nil {
				return apperr.Internal(err)
			}
		}

		lineTotal, err := unit.MulInt(int64(line.Quantity))
		if err != nil {
			return apperr.Internal(err)
		}
		line.LineTotal = lineTotal

		// Tax is computed per line, because GST differs by item (5% on most
		// prepared food, 18% on packaged beverages). One blended rate on the
		// subtotal would be wrong and would fail an audit.
		line.TaxAmount = lineTotal.ApplyBasisPoints(line.TaxBps)

		if subtotal, err = subtotal.Add(lineTotal); err != nil {
			return apperr.Internal(err)
		}
		if tax, err = tax.Add(line.TaxAmount); err != nil {
			return apperr.Internal(err)
		}
	}

	o.Subtotal = subtotal
	o.TaxAmount = tax

	// Discount must never exceed the subtotal, or a generous coupon would
	// produce a negative total and a refund to the customer.
	if o.Discount.GreaterThan(subtotal) {
		o.Discount = subtotal
	}

	total, err := money.Sum(subtotal, o.PackagingFee, o.DeliveryFee, tax, o.Tip)
	if err != nil {
		return apperr.Internal(err)
	}
	if total, err = total.Sub(o.Discount); err != nil {
		return apperr.Internal(err)
	}
	if total.IsNegative() {
		total = money.Zero(o.Currency)
	}
	o.Total = total
	return nil
}

// ---------------------------------------------------------------------------
// Repository ports
// ---------------------------------------------------------------------------

type OrderRepository interface {
	// Create persists the order and its lines. Must be called inside a
	// transaction alongside the stock reservation and the outbox event.
	Create(ctx context.Context, o *Order) error
	Update(ctx context.Context, o *Order) error
	FindByID(ctx context.Context, orderID id.ID) (*Order, error)
	FindByNumber(ctx context.Context, number string) (*Order, error)
	// FindByIDForUpdate takes a row lock, so two concurrent status changes
	// cannot interleave and lose one of them.
	FindByIDForUpdate(ctx context.Context, orderID id.ID) (*Order, error)
	ListForUser(ctx context.Context, userID id.ID, filter Filter, page paging.Request) ([]*Order, error)
	ListForKitchen(ctx context.Context, kitchenID id.ID, filter Filter, page paging.Request) ([]*Order, error)
	AppendStatusEvent(ctx context.Context, e StatusEvent) error
	ListStatusEvents(ctx context.Context, orderID id.ID) ([]StatusEvent, error)
	// CountActiveForUser backs the "you have too many open orders" fraud check.
	CountActiveForUser(ctx context.Context, userID id.ID) (int, error)

	// HasEverOrdered backs the first-order-only coupon rule.
	//
	// EXISTS separately from CountActiveForUser because the questions differ: that
	// one asks "are they mid-order right now", this one asks "have they ever
	// ordered". Cancelled and rejected orders count here — placing an order and
	// cancelling it is not the same as never having ordered, and treating it as
	// such would make the offer farmable by cancelling.
	HasEverOrdered(ctx context.Context, userID id.ID) (bool, error)
	// FindStale finds orders stuck in a status past a deadline, so the reaper
	// worker can release their stock and abandon them.
	FindStale(ctx context.Context, status Status, olderThan time.Time, limit int) ([]*Order, error)
}

type Filter struct {
	Statuses []Status
	From     *time.Time
	To       *time.Time
	Search   string
}

// IdempotencyRepository backs safe checkout retries.
//
// This is not optional. Mobile clients retry on flaky networks, and a retried
// POST /checkout without this creates a second order and a second charge. The
// client sends an Idempotency-Key; the first request stores its response and
// every retry replays it.
type IdempotencyRepository interface {
	// Claim atomically reserves a key, returning the stored response if this key
	// was already used.
	Claim(ctx context.Context, key string, userID id.ID, endpoint string, now time.Time) (existing []byte, claimed bool, err error)
	Complete(ctx context.Context, key string, response []byte) error
	Release(ctx context.Context, key string) error
}
