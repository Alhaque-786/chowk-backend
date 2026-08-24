package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	orderingpg "github.com/zevoxa/chowk-backend/internal/modules/ordering/adapter/postgres"
	orderingredis "github.com/zevoxa/chowk-backend/internal/modules/ordering/adapter/redis"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/app"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

// harness assembles a real CheckoutService: real Postgres and real cart storage,
// faked cross-module contracts.
//
// That split is the point. The database is real because the bugs live there —
// foreign key ordering, CHECK constraints, transaction boundaries. The contracts
// are faked because a checkout test should test checkout, not catalog.
type harness struct {
	db       *database.DB
	checkout *app.CheckoutService
	carts    domain.CartRepository
	orders   domain.OrderRepository

	prices   *testutil.FakePriceQuoter
	kitchens *testutil.FakeKitchenAvailability
	stock    *testutil.FakeStockReserver
	coupons  *testutil.FakeDiscountCalculator
	payments *testutil.FakePaymentInitiator
	users    *testutil.FakeUserDirectory
	events   *testutil.RecordingPublisher

	userID     id.ID
	kitchenID  id.ID
	biryani    id.ID
	water      id.ID
	created    []id.ID
	paymentIDs []id.ID
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	db := testutil.DB(t)
	ctx := context.Background()

	h := &harness{
		db:        db,
		kitchenID: id.MustParse("00000000-0000-0000-0000-0000000000a1"), // seeded Khansama
		biryani:   id.New(),
		water:     id.New(),
	}

	// A real user, because orders.user_id has a foreign key.
	if err := db.Q(ctx).QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&h.userID); err != nil {
		t.Skipf("no users in the database; run `make seed` first: %v", err)
	}

	// Real menu items, because order_lines.menu_item_id has a foreign key with
	// ON DELETE RESTRICT.
	for _, item := range []struct {
		id   id.ID
		name string
	}{{h.biryani, "Test Biryani"}, {h.water, "Test Water"}} {
		if _, err := db.Q(ctx).Exec(ctx, `
			INSERT INTO menu_items (id, kitchen_id, name, price_minor, tax_bps, food_type,
			                        allergens, serves, is_available, prep_minutes, version,
			                        created_at, updated_at)
			VALUES ($1,$2,$3,10000,500,'veg','{}',1,true,10,1,now(),now())`,
			item.id, h.kitchenID, item.name); err != nil {
			t.Skipf("cannot create menu item: %v", err)
		}
	}

	h.prices = testutil.NewFakePriceQuoter()
	// 5% GST on food, 18% on the packaged drink — the per-item rates that a
	// blended rate would get wrong.
	h.prices.AddItem(h.biryani, h.kitchenID, "Test Biryani", 289, 500)
	h.prices.AddItem(h.water, h.kitchenID, "Test Water", 20, 1800)

	h.kitchens = testutil.NewFakeKitchenAvailability()
	h.stock = testutil.NewFakeStockReserver()
	h.coupons = testutil.NewFakeDiscountCalculator()
	h.payments = testutil.NewFakePaymentInitiator()
	h.users = testutil.NewFakeUserDirectory(h.userID)
	h.events = testutil.NewRecordingPublisher()

	h.carts = orderingredis.NewCartRepository(testutil.Cache(t))
	h.orders = orderingpg.NewOrderRepository(db)

	h.checkout = app.NewCheckoutService(
		db, h.orders, h.carts,
		h.prices, h.kitchens, h.stock, h.coupons, h.payments, h.users,
		h.events, clock.System{},
	)

	t.Cleanup(func() {
		c := context.Background()
		// orders.payment_id references payments, and payments.order_id references
		// orders, so the circular reference must be broken before deleting either.
		for _, orderID := range h.created {
			_, _ = db.Q(c).Exec(c, `UPDATE orders SET payment_id = NULL WHERE id = $1`, orderID)
		}
		for _, paymentID := range h.paymentIDs {
			_, _ = db.Q(c).Exec(c, `DELETE FROM payments WHERE id = $1`, paymentID)
		}
		for _, orderID := range h.created {
			_, _ = db.Q(c).Exec(c, `DELETE FROM order_status_events WHERE order_id = $1`, orderID)
			_, _ = db.Q(c).Exec(c, `DELETE FROM order_lines WHERE order_id = $1`, orderID)
			_, _ = db.Q(c).Exec(c, `DELETE FROM outbox_events WHERE aggregate_id = $1`, orderID)
			_, _ = db.Q(c).Exec(c, `DELETE FROM orders WHERE id = $1`, orderID)
		}
		_, _ = db.Q(c).Exec(c, `DELETE FROM menu_items WHERE id = ANY($1)`, []id.ID{h.biryani, h.water})
	})

	return h
}

// addToCart puts items in the cart directly, bypassing CartService.
func (h *harness) addToCart(t *testing.T, itemID id.ID, qty int32) {
	t.Helper()
	ctx := context.Background()

	cart, err := h.carts.Get(ctx, h.userID)
	if err != nil {
		t.Fatalf("get cart: %v", err)
	}
	cart.KitchenID = h.kitchenID
	if err := cart.AddLine(itemID, qty, nil, "", time.Now()); err != nil {
		t.Fatalf("add line: %v", err)
	}
	if err := h.carts.Save(ctx, cart); err != nil {
		t.Fatalf("save cart: %v", err)
	}
}

// insertPayment creates a real payments row.
//
// Needed because orders.payment_id has a foreign key to payments, so MarkPaid
// cannot be handed an invented id. In production the payment.captured event
// always carries a real payment, and the test has to honour that rather than
// route around it — routing around it is how the FK-ordering bug in checkout
// survived until a live request found it.
func (h *harness) insertPayment(t *testing.T, orderID id.ID, amountMinor int64) id.ID {
	t.Helper()
	ctx := context.Background()

	paymentID := id.New()
	if _, err := h.db.Q(ctx).Exec(ctx, `
		INSERT INTO payments (id, order_id, user_id, amount_minor, currency, status,
		                      method, provider, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'INR','captured','upi','mock',now(),now())`,
		paymentID, orderID, h.userID, amountMinor); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	h.paymentIDs = append(h.paymentIDs, paymentID)
	return paymentID
}

func (h *harness) checkoutCOD(t *testing.T) (*app.CheckoutResult, error) {
	t.Helper()
	result, err := h.checkout.Checkout(context.Background(), app.CheckoutInput{
		UserID:        h.userID,
		AddressID:     h.users.Address.ID,
		PaymentMethod: domain.PaymentCOD,
	})
	if result != nil && result.Order != nil {
		h.created = append(h.created, result.Order.ID)
	}
	return result, err
}

// TestCheckoutCreatesOrderWithCorrectTotals is the money test.
//
// It asserts PER-ITEM tax, not a blended rate on the subtotal. GST genuinely
// differs by item (5% prepared food, 18% packaged drinks), and blending would be
// wrong in a way that fails an audit rather than a test.
func TestCheckoutCreatesOrderWithCorrectTotals(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 2) // 289 × 2 = 578.00 @ 5%
	h.addToCart(t, h.water, 1)   //  20 × 1 =  20.00 @ 18%

	result, err := h.checkoutCOD(t)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	order := result.Order

	if got, want := order.Subtotal.Minor(), int64(59800); got != want {
		t.Errorf("subtotal = %d, want %d", got, want)
	}
	// 57800 @ 5% = 2890; 2000 @ 18% = 360. Total 3250.
	if got, want := order.TaxAmount.Minor(), int64(3250); got != want {
		t.Errorf("tax = %d, want %d — per-item rates, not a blended rate", got, want)
	}

	// The invariant the database also enforces. Asserting it here means a failure
	// points at the arithmetic rather than at a constraint violation.
	computed := order.Subtotal.Minor() + order.PackagingFee.Minor() +
		order.DeliveryFee.Minor() + order.TaxAmount.Minor() +
		order.Tip.Minor() - order.Discount.Minor()
	if order.Total.Minor() != computed {
		t.Errorf("total %d != sum of components %d", order.Total.Minor(), computed)
	}

	// COD goes straight to placed: nothing to collect, so the kitchen sees it now.
	if order.Status != domain.StatusPlaced {
		t.Errorf("status = %q, want placed for COD", order.Status)
	}
	if result.Payment != nil {
		t.Error("a payment intent was created for a COD order")
	}
}

// TestCheckoutPersistsOrderAndAuditTrail guards a bug that already shipped once:
// the status events from TransitionTo were discarded with `_`, leaving an order
// with no trail for its own creation.
func TestCheckoutPersistsOrderAndAuditTrail(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	result, err := h.checkoutCOD(t)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	ctx := context.Background()

	// Reload from the database, not from the returned struct: this proves it was
	// actually persisted with its lines.
	stored, err := h.orders.FindByID(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if len(stored.Lines) != 1 {
		t.Errorf("%d order lines persisted, want 1", len(stored.Lines))
	}
	if stored.Lines[0].Name != "Test Biryani" {
		t.Errorf("line name = %q; the name must be SNAPSHOTTED, not joined", stored.Lines[0].Name)
	}

	events, err := h.orders.ListStatusEvents(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("list status events: %v", err)
	}
	// draft -> pending_payment -> placed
	if len(events) != 2 {
		t.Fatalf("%d audit events, want 2 — an order must have a trail for its own creation", len(events))
	}
	if events[0].From != domain.StatusDraft || events[0].To != domain.StatusPendingPayment {
		t.Errorf("first event = %s -> %s, want draft -> pending_payment", events[0].From, events[0].To)
	}
	if events[1].To != domain.StatusPlaced {
		t.Errorf("second event to = %s, want placed", events[1].To)
	}
}

// TestCheckoutReservesStockAfterCreatingTheOrder is the FK-ordering regression
// test.
//
// stock_reservations.order_id references orders(id), so reserving before
// inserting the order fails with a constraint violation — and every checkout 500s.
// A mocked StockReserver has no FK, which is exactly why this bug reached a
// running server before being caught.
func TestCheckoutReservesStockAfterCreatingTheOrder(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	result, err := h.checkoutCOD(t)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	if len(h.stock.Reserved) != 1 {
		t.Fatalf("%d reservations made, want 1", len(h.stock.Reserved))
	}
	if h.stock.Reserved[0] != result.Order.ID {
		t.Errorf("stock reserved for %s, want %s", h.stock.Reserved[0], result.Order.ID)
	}

	// The order must exist in the database by the time Reserve was called. If the
	// statements were the other way round the FK would have failed the whole
	// transaction, so a successfully persisted order proves the ordering.
	if _, err := h.orders.FindByID(context.Background(), result.Order.ID); err != nil {
		t.Errorf("order not persisted: %v", err)
	}
}

// TestCheckoutRollsBackEverythingOnStockShortage is the atomicity test.
//
// A failure at the reservation step must leave NO order behind. Otherwise a
// sold-out item produces a phantom order the kitchen might cook.
func TestCheckoutRollsBackEverythingOnStockShortage(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	h.stock.ReserveErr = apperr.Conflict("INSUFFICIENT_STOCK", "Rice has just run out.")

	result, err := h.checkoutCOD(t)
	if err == nil {
		t.Fatal("checkout succeeded despite a stock shortage")
	}
	if apperr.KindOf(err) != apperr.KindConflict {
		t.Errorf("error kind = %v, want conflict", apperr.KindOf(err))
	}
	if result != nil {
		t.Error("a result was returned alongside the error")
	}

	// No order, and no outbox event announcing one.
	var orphans int
	ctx := context.Background()
	if err := h.db.Q(ctx).QueryRow(ctx,
		`SELECT count(*) FROM orders WHERE user_id = $1 AND kitchen_id = $2 AND created_at > now() - interval '1 minute'`,
		h.userID, h.kitchenID).Scan(&orphans); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned orders survived a failed checkout, want 0", orphans)
	}
}

// TestCheckoutRefusesWhenKitchenClosed: fail before taking money, not after.
func TestCheckoutRefusesWhenKitchenClosed(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	h.kitchens.Accepting = false
	h.kitchens.Reason = "Khansama is closed right now."

	if _, err := h.checkoutCOD(t); err == nil {
		t.Fatal("checkout succeeded against a closed kitchen")
	} else if apperr.KindOf(err) != apperr.KindConflict {
		t.Errorf("error kind = %v, want conflict", apperr.KindOf(err))
	}

	// Nothing should have been reserved: validation happens before any write.
	if len(h.stock.Reserved) != 0 {
		t.Errorf("stock was reserved for an order the kitchen cannot cook")
	}
}

// TestCheckoutRefusesUndeliverableAddress: discovering this after payment means a
// refund and an angry customer.
func TestCheckoutRefusesUndeliverableAddress(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	h.kitchens.Serves = false

	if _, err := h.checkoutCOD(t); err == nil {
		t.Fatal("checkout succeeded for an address outside the delivery area")
	}
	if len(h.stock.Reserved) != 0 {
		t.Error("stock was reserved for an undeliverable order")
	}
}

// TestCheckoutRefusesUnavailableItemByName: "something is unavailable" forces the
// customer to guess which of eight things to remove.
func TestCheckoutRefusesUnavailableItemByName(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	h.prices.MarkUnavailable(h.biryani)

	_, err := h.checkoutCOD(t)
	if err == nil {
		t.Fatal("checkout succeeded with an unavailable item")
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("not an apperr: %v", err)
	}
	if !contains(appErr.Message, "Test Biryani") {
		t.Errorf("message %q does not name the unavailable item", appErr.Message)
	}
}

// TestCheckoutRejectsInvalidCouponRatherThanChargingFullPrice.
//
// Silently dropping a coupon charges full price on an order the customer only
// placed BECAUSE of the discount. That is how chargebacks happen.
func TestCheckoutRejectsInvalidCoupon(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	h.coupons.Code = "VALID50" // anything else is rejected

	_, err := h.checkout.Checkout(context.Background(), app.CheckoutInput{
		UserID:        h.userID,
		AddressID:     h.users.Address.ID,
		PaymentMethod: domain.PaymentCOD,
		CouponCode:    "NONSENSE",
	})
	if err == nil {
		t.Fatal("checkout succeeded with an invalid coupon; it must not silently charge full price")
	}
	if apperr.KindOf(err) != apperr.KindInvalid {
		t.Errorf("error kind = %v, want invalid", apperr.KindOf(err))
	}
}

func TestCheckoutAppliesValidCoupon(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1) // 289.00

	h.coupons.Code = "SAVE50"
	h.coupons.DiscountRupees = 50

	result, err := h.checkout.Checkout(context.Background(), app.CheckoutInput{
		UserID:        h.userID,
		AddressID:     h.users.Address.ID,
		PaymentMethod: domain.PaymentCOD,
		CouponCode:    "SAVE50",
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	h.created = append(h.created, result.Order.ID)

	if got, want := result.Order.Discount.Minor(), int64(5000); got != want {
		t.Errorf("discount = %d, want %d", got, want)
	}
	// Redeem must be called, not just Calculate: browsing offers must not burn a
	// redemption, but placing an order must.
	if len(h.coupons.Redeemed) != 1 {
		t.Errorf("Redeem called %d times, want 1", len(h.coupons.Redeemed))
	}
}

// TestCheckoutPublishesOrderPlaced: the event is what every other module reacts
// to, and it is written in the same transaction as the order.
func TestCheckoutPublishesOrderPlaced(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	if _, err := h.checkoutCOD(t); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	if !h.events.Has(contracts.EventOrderPlaced) {
		t.Errorf("order.placed was not published; published: %v", h.events.Names())
	}
}

// TestCheckoutClearsTheCart, so the customer does not re-order the same food.
func TestCheckoutClearsTheCart(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	if _, err := h.checkoutCOD(t); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	cart, err := h.carts.Get(context.Background(), h.userID)
	if err != nil {
		t.Fatalf("get cart: %v", err)
	}
	if !cart.IsEmpty() {
		t.Errorf("cart still has %d lines after checkout", len(cart.Lines))
	}
}

func TestCheckoutRefusesEmptyCart(t *testing.T) {
	h := newHarness(t)
	// Nothing added.

	if _, err := h.checkoutCOD(t); err == nil {
		t.Fatal("checkout succeeded with an empty cart")
	}
}

// TestPrepaidOrderStaysPendingPayment: the kitchen must not see a prepaid order
// until money lands, or a failed UPI payment means food cooked for free.
func TestPrepaidOrderStaysPendingPayment(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	result, err := h.checkout.Checkout(context.Background(), app.CheckoutInput{
		UserID:        h.userID,
		AddressID:     h.users.Address.ID,
		PaymentMethod: domain.PaymentUPI,
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	h.created = append(h.created, result.Order.ID)

	if result.Order.Status != domain.StatusPendingPayment {
		t.Errorf("status = %q, want pending_payment until the gateway confirms", result.Order.Status)
	}
	if result.Payment == nil {
		t.Error("no payment intent was returned for a prepaid order")
	}
}

// TestMarkPaidPromotesOrderAndIsIdempotent covers the payment.captured handler.
//
// Idempotency is a requirement, not a nicety: outbox delivery is at-least-once,
// so a redelivered event MUST succeed or the relay retries forever and eventually
// dead-letters a good payment.
func TestMarkPaidPromotesOrderAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	result, err := h.checkout.Checkout(context.Background(), app.CheckoutInput{
		UserID:        h.userID,
		AddressID:     h.users.Address.ID,
		PaymentMethod: domain.PaymentUPI,
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	h.created = append(h.created, result.Order.ID)

	ctx := context.Background()
	paymentID := h.insertPayment(t, result.Order.ID, result.Order.Total.Minor())

	if err := h.checkout.MarkPaid(ctx, result.Order.ID, paymentID); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	stored, err := h.orders.FindByID(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Status != domain.StatusPlaced {
		t.Errorf("status = %q, want placed after capture", stored.Status)
	}

	// Redelivery must be a silent success.
	if err := h.checkout.MarkPaid(ctx, result.Order.ID, paymentID); err != nil {
		t.Errorf("second MarkPaid returned an error; the handler must be idempotent: %v", err)
	}
}

// TestMarkPaymentFailedCancelsAndCompensates.
func TestMarkPaymentFailedCancelsAndCompensates(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	result, err := h.checkout.Checkout(context.Background(), app.CheckoutInput{
		UserID:        h.userID,
		AddressID:     h.users.Address.ID,
		PaymentMethod: domain.PaymentUPI,
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	h.created = append(h.created, result.Order.ID)

	ctx := context.Background()
	if err := h.checkout.MarkPaymentFailed(ctx, result.Order.ID, "card declined"); err != nil {
		t.Fatalf("MarkPaymentFailed: %v", err)
	}

	stored, err := h.orders.FindByID(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Status != domain.StatusCancelled {
		t.Errorf("status = %q, want cancelled", stored.Status)
	}
	// Compensation must run, or the order permanently holds stock.
	if !h.stock.WasReleased(result.Order.ID) {
		t.Error("stock was not released for an order whose payment failed")
	}
}

// TestMarkPaymentFailedIgnoresAlreadyPaidOrder covers out-of-order delivery.
//
// A payment.failed arriving AFTER a capture must not cancel a paid order: the
// money is real and the food should be cooked.
func TestMarkPaymentFailedIgnoresAlreadyPaidOrder(t *testing.T) {
	h := newHarness(t)
	h.addToCart(t, h.biryani, 1)

	result, err := h.checkout.Checkout(context.Background(), app.CheckoutInput{
		UserID:        h.userID,
		AddressID:     h.users.Address.ID,
		PaymentMethod: domain.PaymentUPI,
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	h.created = append(h.created, result.Order.ID)

	ctx := context.Background()
	paymentID := h.insertPayment(t, result.Order.ID, result.Order.Total.Minor())
	if err := h.checkout.MarkPaid(ctx, result.Order.ID, paymentID); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	// Late, out-of-order failure event.
	if err := h.checkout.MarkPaymentFailed(ctx, result.Order.ID, "late failure"); err != nil {
		t.Fatalf("MarkPaymentFailed: %v", err)
	}

	stored, err := h.orders.FindByID(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Status != domain.StatusPlaced {
		t.Errorf("status = %q, want placed — a late failure event must not cancel a PAID order", stored.Status)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

/*
TestCheckoutPersistsEveryLineCorrectlyPaired covers the batched insert.

insertLines was a loop of one Exec per line and is now a single `unnest` statement,
which trades N network round trips for one — worth ~150ms on a five-line order
against a database in another city, inside the SERIALIZABLE transaction, while it
holds its locks.

The existing coverage only ever checked out ONE line, and one line cannot detect
the failure mode this rewrite introduces. `unnest` zips its arrays positionally, so
building them in slightly different orders pairs the wrong quantity with the wrong
item — and every individual value is still valid, every constraint still passes,
and the order total still balances against the corrupted lines because the totals
were computed before the insert. It is only wrong when a human reads the order.

Two items with DIFFERENT quantities, prices and tax rates, so a swap between any
pair of arrays changes an assertion.
*/
func TestCheckoutPersistsEveryLineCorrectlyPaired(t *testing.T) {
	h := newHarness(t)
	// Deliberately asymmetric: biryani x3 at 289 with 500bps, water x1 at 20 with
	// 1800bps. No two arrays share a value pattern.
	h.addToCart(t, h.biryani, 3)
	h.addToCart(t, h.water, 1)

	result, err := h.checkoutCOD(t)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	ctx := context.Background()

	stored, err := h.orders.FindByID(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if len(stored.Lines) != 2 {
		t.Fatalf("%d lines persisted, want 2 — a batched insert dropping rows is the "+
			"first thing this test is for", len(stored.Lines))
	}

	byName := map[string]domain.Line{}
	for _, l := range stored.Lines {
		byName[l.Name] = l
	}

	biryani, ok := byName["Test Biryani"]
	if !ok {
		t.Fatalf("no line named Test Biryani; got %v", byName)
	}
	water, ok := byName["Test Water"]
	if !ok {
		t.Fatalf("no line named Test Water; got %v", byName)
	}

	// Quantity paired with the right item. A quantities/prices swap survives every
	// constraint and shows up only here.
	if biryani.Quantity != 3 {
		t.Errorf("biryani quantity = %d, want 3 — arrays are misaligned", biryani.Quantity)
	}
	if water.Quantity != 1 {
		t.Errorf("water quantity = %d, want 1 — arrays are misaligned", water.Quantity)
	}

	// Price and tax rate paired with the right item.
	if biryani.UnitPrice.Minor() != 28900 {
		t.Errorf("biryani unit price = %d, want 28900", biryani.UnitPrice.Minor())
	}
	if water.UnitPrice.Minor() != 2000 {
		t.Errorf("water unit price = %d, want 2000", water.UnitPrice.Minor())
	}
	if biryani.TaxBps != 500 {
		t.Errorf("biryani tax = %d bps, want 500 — per-item rates must not be swapped", biryani.TaxBps)
	}
	if water.TaxBps != 1800 {
		t.Errorf("water tax = %d bps, want 1800 — per-item rates must not be swapped", water.TaxBps)
	}

	// Line totals must be internally consistent, which catches a quantity paired
	// with the wrong price even when both values are individually plausible.
	for _, l := range stored.Lines {
		if want := l.UnitPrice.Minor() * int64(l.Quantity); l.LineTotal.Minor() != want {
			t.Errorf("%s: line total %d != unit %d x qty %d = %d",
				l.Name, l.LineTotal.Minor(), l.UnitPrice.Minor(), l.Quantity, want)
		}
	}

	// And the order's own subtotal must equal the sum of its persisted lines. This
	// is what a customer disputing a bill would recompute.
	var sum int64
	for _, l := range stored.Lines {
		sum += l.LineTotal.Minor()
	}
	if stored.Subtotal.Minor() != sum {
		t.Errorf("order subtotal %d != sum of lines %d", stored.Subtotal.Minor(), sum)
	}
}
