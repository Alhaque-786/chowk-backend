package contracts

import (
	"context"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Null adapters for contracts whose owning module is not built yet.
//
// This is the Null Object pattern, and using it here is a deliberate choice over
// the alternatives:
//
//   - Nil interfaces would panic at the first call, turning "module not built"
//     into a 500 at checkout.
//   - `if s.stock != nil` guards would scatter the incompleteness across every
//     call site and never get cleaned up.
//   - Blocking ordering until inventory, promotion and payment all exist would
//     mean no working order flow for weeks.
//
// Each adapter is honest about what it does: the ones whose absence is SAFE
// degrade silently (no stock tracking yet == no reservations to make), and the
// ones whose absence is NOT safe fail loudly rather than pretending to work. A
// null payment adapter that silently "succeeded" would mark orders paid without
// taking money, which is the worst possible failure in this system.
//
// Replace each one in the composition root as its module lands. Deleting an entry
// here is the definition of "that module is done".

// ---------------------------------------------------------------------------
// NullStockReserver — inventory not built yet.
//
// Safe to no-op: with no ingredient data there is nothing to reserve, and every
// item's availability is still governed by catalog's is_available flag. The cost
// is that overselling is possible under concurrency, which is acceptable while a
// single kitchen is manually watching its own stock, and is exactly what the
// inventory module exists to fix.
// ---------------------------------------------------------------------------

type NullStockReserver struct{}

func (NullStockReserver) Reserve(context.Context, id.ID, id.ID, []ReservationLine) error {
	return nil
}
func (NullStockReserver) Release(context.Context, id.ID) error { return nil }
func (NullStockReserver) Consume(context.Context, id.ID) error { return nil }

// ---------------------------------------------------------------------------
// NullDiscountCalculator — promotion not built yet.
//
// Rejects every coupon rather than silently ignoring the code. Ignoring it would
// charge full price on an order the customer only placed because of the
// discount — the classic route to a chargeback and a lost customer.
// ---------------------------------------------------------------------------

type NullDiscountCalculator struct{}

func (NullDiscountCalculator) Calculate(_ context.Context, req DiscountRequest) (DiscountResult, error) {
	return DiscountResult{
		Applied:        false,
		DiscountAmount: money.Zero(money.INR),
		RejectReason:   "Promo codes are not available yet.",
	}, nil
}

func (NullDiscountCalculator) Redeem(context.Context, id.ID, id.ID, id.ID) error { return nil }
func (NullDiscountCalculator) ReleaseRedemption(context.Context, id.ID) error    { return nil }

// ---------------------------------------------------------------------------
// NullPaymentInitiator — payment not built yet.
//
// FAILS LOUDLY. This is the one null adapter that must not degrade quietly: a
// silent success would create orders that look paid while no money moved, and
// the kitchen would cook them. Prepaid checkout is therefore unavailable until
// the payment module exists; COD works today because it never calls this.
// ---------------------------------------------------------------------------

type NullPaymentInitiator struct{}

func (NullPaymentInitiator) CreateIntent(context.Context, PaymentIntentRequest) (PaymentIntent, error) {
	return PaymentIntent{}, apperr.Unavailable("ONLINE_PAYMENT_UNAVAILABLE",
		"Online payment is not available yet. Please choose Cash on Delivery.")
}

func (NullPaymentInitiator) Refund(context.Context, id.ID, money.Amount, string) error {
	// Returning an error rather than nil: a refund that silently does nothing is
	// money owed to a customer that nobody knows about.
	return apperr.Unavailable("REFUND_UNAVAILABLE",
		"Refunds cannot be processed automatically yet.")
}

// ---------------------------------------------------------------------------
// NullRiderDispatcher — delivery not built yet.
//
// Safe to no-op: with no rider fleet or 3PL integration, orders are dispatched
// by whatever the kitchen does today (a phone call, its own delivery boy). The
// order still reaches `ready`; only automatic rider assignment is missing.
// ---------------------------------------------------------------------------

type NullRiderDispatcher struct{}

func (NullRiderDispatcher) RequestRider(context.Context, id.ID, id.ID, float64, float64) error {
	return nil
}

// ---------------------------------------------------------------------------
// NullNotifier — notification not built yet.
//
// Safe to no-op. The customer sees order status in the app; they simply do not
// also get an SMS. Degraded, not broken.
// ---------------------------------------------------------------------------

type NullNotifier struct{}

func (NullNotifier) Send(context.Context, NotificationRequest) error { return nil }

// Compile-time proof each null adapter still satisfies its contract, so a change
// to an interface cannot leave a stale adapter behind.
var (
	_ StockReserver      = NullStockReserver{}
	_ DiscountCalculator = NullDiscountCalculator{}
	_ PaymentInitiator   = NullPaymentInitiator{}
	_ RiderDispatcher    = NullRiderDispatcher{}
	_ Notifier           = NullNotifier{}
)
