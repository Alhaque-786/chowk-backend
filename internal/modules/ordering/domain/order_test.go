package domain

import (
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// TestStateMachineRejectsIllegalTransitions is the test that protects the most
// important invariant in the system: an order can never reach a state it has no
// legitimate path to.
//
// The specific bugs this prevents are the ones that produce an order which is
// both delivered and cancelled, or a cancelled order that gets delivered anyway --
// each of which means either free food or a charged customer with no meal.
func TestStateMachineRejectsIllegalTransitions(t *testing.T) {
	illegal := []struct {
		from Status
		to   Status
	}{
		// Cannot skip the kitchen entirely.
		{StatusPlaced, StatusDelivered},
		{StatusPlaced, StatusReady},
		{StatusPlaced, StatusPickedUp},
		// Cannot go backwards.
		{StatusDelivered, StatusPreparing},
		{StatusReady, StatusConfirmed},
		{StatusPreparing, StatusPlaced},
		// Terminal states are terminal.
		{StatusRefunded, StatusPlaced},
		{StatusCancelled, StatusConfirmed},
		{StatusDelivered, StatusCancelled},
		// Once a rider has the food, cancellation is not a valid path -- a failed
		// delivery is resolved as a refund on the delivered order.
		{StatusPickedUp, StatusCancelled},
		// Payment must clear before the kitchen sees the order, or a failed UPI
		// payment results in food cooked for free.
		{StatusPendingPayment, StatusConfirmed},
		{StatusPendingPayment, StatusPreparing},
	}

	for _, tc := range illegal {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			order := &Order{ID: id.New(), Status: tc.from}

			_, err := order.TransitionTo(tc.to, "test", nil, "", time.Now())
			if err == nil {
				t.Fatalf("transition %s -> %s was allowed; it must be rejected", tc.from, tc.to)
			}
			if apperr.KindOf(err) != apperr.KindConflict {
				t.Errorf("error kind = %v, want conflict", apperr.KindOf(err))
			}
			// The order must be unchanged after a rejected transition.
			if order.Status != tc.from {
				t.Errorf("status mutated to %s despite rejection", order.Status)
			}
		})
	}
}

func TestStateMachineAllowsHappyPath(t *testing.T) {
	// The full prepaid lifecycle.
	path := []Status{
		StatusPendingPayment, StatusPlaced, StatusConfirmed,
		StatusPreparing, StatusReady, StatusPickedUp, StatusDelivered,
	}

	order := &Order{ID: id.New(), Status: StatusDraft}
	now := time.Now()

	for _, next := range path {
		event, err := order.TransitionTo(next, "", nil, "system", now)
		if err != nil {
			t.Fatalf("transition to %s failed: %v", next, err)
		}
		if event.To != next {
			t.Errorf("event.To = %s, want %s", event.To, next)
		}
		if order.Status != next {
			t.Errorf("order.Status = %s, want %s", order.Status, next)
		}
	}

	// Timestamps must be stamped as the order moves; a delivered order with no
	// delivered_at is unreportable.
	if order.ConfirmedAt == nil {
		t.Error("ConfirmedAt was not stamped")
	}
	if order.PreparedAt == nil {
		t.Error("PreparedAt was not stamped")
	}
	if order.DeliveredAt == nil {
		t.Error("DeliveredAt was not stamped")
	}
}

// TestSameStateTransitionIsRejected covers the double-tap case: kitchen staff
// hitting "mark ready" twice, or an at-least-once event being redelivered.
func TestSameStateTransitionIsRejected(t *testing.T) {
	order := &Order{ID: id.New(), Status: StatusPreparing}

	_, err := order.TransitionTo(StatusPreparing, "", nil, "", time.Now())
	if err == nil {
		t.Fatal("transitioning to the current state should report a conflict")
	}
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != "ORDER_ALREADY_IN_STATUS" {
		t.Errorf("got %v, want ORDER_ALREADY_IN_STATUS", err)
	}
}

func TestCancelReasonIsRequired(t *testing.T) {
	order := &Order{ID: id.New(), Status: StatusPlaced}
	actor := id.New()

	event, err := order.TransitionTo(StatusCancelled, "kitchen closed", &actor, "kitchen_admin", time.Now())
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}

	// Cancellation must be attributable and explained, or a payment dispute
	// months later has no answer.
	if order.CancelReason != "kitchen closed" {
		t.Errorf("CancelReason = %q, want %q", order.CancelReason, "kitchen closed")
	}
	if order.CancelledBy == nil || *order.CancelledBy != actor {
		t.Error("CancelledBy was not recorded")
	}
	if order.CancelledAt == nil {
		t.Error("CancelledAt was not stamped")
	}
	if event.ActorRole != "kitchen_admin" {
		t.Errorf("event.ActorRole = %q, want kitchen_admin", event.ActorRole)
	}
}

func TestCanCustomerCancel(t *testing.T) {
	// The business rule: free cancellation until the kitchen starts cooking,
	// because after that the ingredients are spent.
	tests := []struct {
		status    Status
		canCancel bool
	}{
		{StatusPendingPayment, true},
		{StatusPlaced, true},
		{StatusConfirmed, true},
		{StatusPreparing, false},
		{StatusReady, false},
		{StatusPickedUp, false},
		{StatusDelivered, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			order := &Order{Status: tt.status}
			err := order.CanCustomerCancel(time.Now())

			if tt.canCancel && err != nil {
				t.Errorf("customer should be able to cancel a %s order: %v", tt.status, err)
			}
			if !tt.canCancel && err == nil {
				t.Errorf("customer must NOT be able to cancel a %s order", tt.status)
			}
		})
	}
}

// TestRecalculateTotals verifies the arithmetic that the database's
// orders_total_consistency_check also enforces. Two independent checks on the
// same invariant, because a mismatch between the cart preview and the charged
// amount is invisible in testing and enraging in production.
func TestRecalculateTotals(t *testing.T) {
	order := &Order{
		Currency: money.INR,
		Lines: []Line{
			{
				Name:      "Chicken Biryani",
				Quantity:  2,
				UnitPrice: money.FromMinor(24900, money.INR), // 249.00
				TaxBps:    500,                               // 5% GST
			},
			{
				Name:      "Coke",
				Quantity:  1,
				UnitPrice: money.FromMinor(6000, money.INR), // 60.00
				TaxBps:    1800,                             // 18% on packaged drinks
			},
		},
		PackagingFee: money.FromMinor(2000, money.INR), // 20.00
		DeliveryFee:  money.FromMinor(3000, money.INR), // 30.00
		Discount:     money.FromMinor(5000, money.INR), // 50.00 off
		Tip:          money.FromMinor(1000, money.INR), // 10.00
	}

	if err := order.RecalculateTotals(); err != nil {
		t.Fatalf("RecalculateTotals: %v", err)
	}

	// Subtotal: 249*2 + 60 = 558.00
	if got, want := order.Subtotal.Minor(), int64(55800); got != want {
		t.Errorf("Subtotal = %d, want %d", got, want)
	}

	// Per-line tax, NOT a blended rate on the subtotal. 498.00 @ 5% = 24.90;
	// 60.00 @ 18% = 10.80. Total 35.70.
	if got, want := order.TaxAmount.Minor(), int64(3570); got != want {
		t.Errorf("TaxAmount = %d, want %d (per-line rates, not blended)", got, want)
	}

	// Total: 558 + 20 + 30 + 35.70 + 10 - 50 = 603.70
	if got, want := order.Total.Minor(), int64(60370); got != want {
		t.Errorf("Total = %d, want %d", got, want)
	}

	// The database will enforce this too; assert it here so a failure points at
	// the arithmetic rather than at a constraint violation.
	computed := order.Subtotal.Minor() + order.PackagingFee.Minor() +
		order.DeliveryFee.Minor() + order.TaxAmount.Minor() +
		order.Tip.Minor() - order.Discount.Minor()
	if order.Total.Minor() != computed {
		t.Errorf("Total %d does not equal its components %d", order.Total.Minor(), computed)
	}
}

// TestDiscountCannotExceedSubtotal guards against a generous coupon producing a
// negative total -- i.e. the platform paying the customer to eat.
func TestDiscountCannotExceedSubtotal(t *testing.T) {
	order := &Order{
		Currency: money.INR,
		Lines: []Line{
			{Quantity: 1, UnitPrice: money.FromMinor(10000, money.INR), TaxBps: 0},
		},
		Discount: money.FromMinor(50000, money.INR), // 500 off a 100 order
	}

	if err := order.RecalculateTotals(); err != nil {
		t.Fatalf("RecalculateTotals: %v", err)
	}

	if order.Discount.Minor() > order.Subtotal.Minor() {
		t.Errorf("Discount %d exceeds subtotal %d; it must be capped",
			order.Discount.Minor(), order.Subtotal.Minor())
	}
	if order.Total.IsNegative() {
		t.Errorf("Total is negative (%d); an order must never pay the customer", order.Total.Minor())
	}
}

func TestModifiersAreIncludedInLineTotal(t *testing.T) {
	order := &Order{
		Currency: money.INR,
		Lines: []Line{
			{
				Quantity:  2,
				UnitPrice: money.FromMinor(20000, money.INR), // 200.00
				Modifiers: []LineModifier{
					{Name: "Extra cheese", UnitPrice: money.FromMinor(3000, money.INR)}, // 30.00
					{Name: "Large", UnitPrice: money.FromMinor(5000, money.INR)},        // 50.00
				},
			},
		},
	}

	if err := order.RecalculateTotals(); err != nil {
		t.Fatalf("RecalculateTotals: %v", err)
	}

	// (200 + 30 + 50) x 2 = 560.00. Dropping modifiers here would undercharge on
	// every customised order.
	if got, want := order.Lines[0].LineTotal.Minor(), int64(56000); got != want {
		t.Errorf("LineTotal with modifiers = %d, want %d", got, want)
	}
}

func TestRefundableAmount(t *testing.T) {
	newOrder := func(status Status, method PaymentMethod) *Order {
		return &Order{
			Currency:      money.INR,
			Status:        status,
			PaymentMethod: method,
			Total:         money.FromMinor(50000, money.INR),
			DeliveryFee:   money.FromMinor(3000, money.INR),
		}
	}

	// COD: nothing was collected, so nothing is refundable.
	if got := newOrder(StatusCancelled, PaymentCOD).RefundableAmount(); !got.IsZero() {
		t.Errorf("COD refund = %d, want 0", got.Minor())
	}

	// Prepaid, cancelled before pickup: full refund.
	if got := newOrder(StatusCancelled, PaymentUPI).RefundableAmount(); got.Minor() != 50000 {
		t.Errorf("pre-pickup refund = %d, want 50000 (full)", got.Minor())
	}

	// Prepaid, already picked up: the rider did the work, so the delivery fee is
	// retained.
	if got := newOrder(StatusPickedUp, PaymentUPI).RefundableAmount(); got.Minor() != 47000 {
		t.Errorf("post-pickup refund = %d, want 47000 (total minus delivery fee)", got.Minor())
	}
}

func TestTerminalStates(t *testing.T) {
	if !StatusRefunded.IsTerminal() {
		t.Error("refunded must be terminal")
	}
	if StatusDelivered.IsTerminal() {
		t.Error("delivered must NOT be terminal -- it can still be refunded")
	}
	if StatusPlaced.IsTerminal() {
		t.Error("placed must not be terminal")
	}
}

func TestIsActive(t *testing.T) {
	active := []Status{StatusPlaced, StatusConfirmed, StatusPreparing, StatusReady, StatusPickedUp}
	for _, s := range active {
		if !s.IsActive() {
			t.Errorf("%s should count as active (stock stays reserved)", s)
		}
	}

	inactive := []Status{StatusDraft, StatusPendingPayment, StatusDelivered, StatusCancelled, StatusRefunded}
	for _, s := range inactive {
		if s.IsActive() {
			t.Errorf("%s should NOT count as active", s)
		}
	}
}
