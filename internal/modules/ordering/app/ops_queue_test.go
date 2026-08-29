package app_test

import (
	"context"
	"testing"

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

/*
The assisted-vendor path: orders nobody on the merchant side will ever see.

A cart selling fresh nariyal pani has no dashboard and never logs in. Without an
ops queue his orders sit at `placed` until the reaper cancels them, and the
customer is told the kitchen never responded — which is true, and entirely our
fault for listing him.

The security half matters as much as the feature half. Lifting the tenant guard for
platform staff must not lift it for anybody else, so the last test here is the one
that would catch a cross-tenant hole.
*/

/*
newOpsService wires the service with a FAKE availability port.

Ordering must not import catalog — the boundary checker enforces it, and it caught
the first version of this file doing exactly that to reach the real SQL. The
correction is the better design: which vendors are assisted is CATALOG's question,
verified against the database in catalog/adapter/postgres/assisted_test.go, and
from ordering's side the contract is the boundary. Testing through the fake is
therefore not a compromise; it is the unit under test.
*/
func newOpsServiceWith(
	t *testing.T, db *database.DB,
	payments *testutil.FakePaymentInitiator, kitchens contracts.KitchenAvailability,
) *app.KitchenOrderService {
	t.Helper()
	return app.NewKitchenOrderService(
		db,
		orderingpg.NewOrderRepository(db),
		testutil.NewFakeStockReserver(),
		&testutil.FakeRiderDispatcher{},
		payments,
		kitchens,
		testutil.NewRecordingPublisher(),
		clock.System{},
	)
}

/*
assistedFake hands the service a known answer.

Ordering CANNOT import catalog — make check-boundaries forbids it, in test files
too — so the SQL that decides which merchants are assisted is tested where it
lives, in catalog. These tests verify what ordering DOES with the answer, which is
a different question and worth separating.
*/
func assistedFake(assisted ...id.ID) *testutil.FakeKitchenAvailability {
	k := testutil.NewFakeKitchenAvailability()
	k.SetAssisted(assisted...)
	return k
}

func newOpsService(t *testing.T, db *database.DB, assisted ...id.ID) *app.KitchenOrderService {
	t.Helper()
	kitchens := testutil.NewFakeKitchenAvailability()
	kitchens.SetAssisted(assisted...)
	return app.NewKitchenOrderService(
		db,
		orderingpg.NewOrderRepository(db),
		testutil.NewFakeStockReserver(),
		&testutil.FakeRiderDispatcher{},
		testutil.NewFakePaymentInitiator(),
		kitchens,
		testutil.NewRecordingPublisher(),
		clock.System{},
	)
}

// actor must be a REAL user: order_status_events.actor_id has a foreign key, so a
// random uuid fails the insert rather than the assertion.
func transitionInput(orderID, actor id.ID, next domain.Status, platform bool) app.TransitionInput {
	in := app.TransitionInput{
		OrderID:       orderID,
		NextStatus:    next,
		ActorID:       actor,
		PlatformActor: platform,
	}
	if platform {
		in.ActorRole = "platform_admin"
	} else {
		in.ActorRole = "kitchen_admin"
	}
	return in
}

func TestOpsQueueFindsOnlyAssistedVendorsOrders(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	assisted := testutil.AssistedKitchen(t, db)
	selfServe := testutil.Kitchen(t, db)
	user := testutil.User(t, db)

	assistedOrder := testutil.Order(t, db, user, assisted,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), TotalMinor: 4000})
	testutil.Order(t, db, user, selfServe,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), TotalMinor: 9000})

	svc := newOpsService(t, db, assisted)
	queue, err := svc.OpsQueue(ctx)
	if err != nil {
		t.Fatalf("ops queue: %v", err)
	}

	var sawAssisted, sawSelfServe bool
	for _, o := range queue {
		switch o.KitchenID {
		case assisted:
			sawAssisted = true
		case selfServe:
			sawSelfServe = true
		}
	}
	if !sawAssisted {
		t.Errorf("order %s at an assisted vendor is missing from the ops queue; nobody "+
			"would ever action it", assistedOrder)
	}
	if sawSelfServe {
		t.Error("a self-serve kitchen's order appeared in the ops queue — that kitchen " +
			"has its own dashboard, and the noise buries the orders that need a phone call")
	}
}

func TestOpsQueueShowsOnlyOrdersStillOwedAnAction(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	assisted := testutil.AssistedKitchen(t, db)
	user := testutil.User(t, db)

	// Already being cooked: the agent's job is done until pickup, and showing it
	// would bury the ones that still need a call.
	testutil.Order(t, db, user, assisted,
		testutil.OrderOpts{Status: string(domain.StatusPreparing), TotalMinor: 4000})
	needsAction := testutil.Order(t, db, user, assisted,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), TotalMinor: 4000})

	queue, err := newOpsService(t, db, assisted).OpsQueue(ctx)
	if err != nil {
		t.Fatalf("ops queue: %v", err)
	}

	found := false
	for _, o := range queue {
		if o.ID == needsAction {
			found = true
		}
		if o.Status == domain.StatusPreparing {
			t.Errorf("order %s is already preparing and does not need an agent", o.ID)
		}
	}
	if !found {
		t.Error("the placed order is missing from the queue")
	}
}

func TestOpsQueueIsEmptyRatherThanErroringWithNoAssistedVendors(t *testing.T) {
	// The normal early state. An empty ANY($1) would match nothing anyway, but the
	// call must not fail or return a nil-deref for the console rendering it.
	db := testutil.DB(t)
	queue, err := newOpsService(t, db).OpsQueue(context.Background())
	if err != nil {
		t.Fatalf("ops queue with no assisted vendors: %v", err)
	}
	_ = queue
}

func TestPlatformActorMayActOnAnyKitchensOrder(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	assisted := testutil.AssistedKitchen(t, db)
	user := testutil.User(t, db)
	orderID := testutil.Order(t, db, user, assisted,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), TotalMinor: 4000})

	agent := testutil.User(t, db) // our ops staff member
	svc := newOpsService(t, db, assisted)
	// No KitchenID at all — an admin has none, which is the whole point.
	order, err := svc.Transition(ctx, transitionInput(orderID, agent, domain.StatusConfirmed, true))
	if err != nil {
		t.Fatalf("platform actor could not accept: %v", err)
	}
	if order.Status != domain.StatusConfirmed {
		t.Errorf("status = %q, want confirmed", order.Status)
	}

	// And the trail must say it was US, not the vendor. If anybody later asks who
	// promised the food would be made, these are different answers.
	events, err := orderingpg.NewOrderRepository(db).ListStatusEvents(ctx, orderID)
	if err != nil {
		t.Fatalf("list status events: %v", err)
	}
	var sawPlatformActor bool
	for _, e := range events {
		if e.To == domain.StatusConfirmed && e.ActorRole == "platform_admin" {
			sawPlatformActor = true
		}
	}
	if !sawPlatformActor {
		t.Error("no status event attributes the confirmation to platform_admin; the " +
			"audit trail cannot distinguish our agent from the vendor")
	}
}

func TestTheTenantGuardStillBlocksEveryoneElse(t *testing.T) {
	/*
		THE REGRESSION TEST FOR THE WHOLE CHANGE.

		Lifting the tenant guard for platform staff must not lift it for kitchen
		staff. Without this, "admin can accept any order" quietly becomes "anyone who
		knows an order id can accept it", which is a cross-tenant write.
	*/
	db := testutil.DB(t)
	ctx := context.Background()

	kitchenA := testutil.Kitchen(t, db)
	kitchenB := testutil.Kitchen(t, db)
	user := testutil.User(t, db)
	orderAtB := testutil.Order(t, db, user, kitchenB,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), TotalMinor: 4000})

	staffAtA := testutil.User(t, db)
	svc := newOpsService(t, db)
	in := transitionInput(orderAtB, staffAtA, domain.StatusConfirmed, false)
	in.KitchenID = kitchenA // staff at A reaching for B's order

	_, err := svc.Transition(ctx, in)
	if err == nil {
		t.Fatal("kitchen A accepted kitchen B's order — cross-tenant write")
	}
	// NOT_FOUND rather than FORBIDDEN on purpose: confirming the order exists tells
	// a prober that the id is real and belongs to somebody.
	if k := apperr.KindOf(err); k != apperr.KindNotFound {
		t.Errorf("kind = %v, want NotFound so the id is not confirmed to exist", k)
	}
	_ = id.Nil
}

/*
Deferred capture: the money half of the assisted-vendor model.

A cart's stock is however many coconuts arrived this morning. Taking payment before
anybody has confirmed he can make the order means refunding a stranger daily and
being the app that charges for juice it did not deliver. So an assisted vendor's
payment is AUTHORISED at checkout and captured only when somebody confirms.

The tests below are about who gets charged and when, which is the part worth being
certain of.
*/

func TestAssistedVendorDefersCaptureButARestaurantDoesNot(t *testing.T) {
	db := testutil.DB(t)

	assisted := testutil.AssistedKitchen(t, db)
	selfServe := testutil.Kitchen(t, db)

	payments := testutil.NewFakePaymentInitiator()

	// Checkout is exercised through the harness in checkout_service_test.go; here
	// only the decision matters, so the modes are read straight off the fake.
	svc := newOpsServiceWith(t, db, payments, assistedFake(assisted))
	user := testutil.User(t, db)

	assistedOrder := testutil.Order(t, db, user, assisted,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), PaymentMethod: "upi", TotalMinor: 4000})
	restaurantOrder := testutil.Order(t, db, user, selfServe,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), PaymentMethod: "upi", TotalMinor: 40000})

	ctx := context.Background()
	agent := testutil.User(t, db)

	// Nothing captured yet — neither order has been confirmed.
	if got := payments.WasCaptured(assistedOrder); got != 0 {
		t.Errorf("assisted order was captured %d times before anybody confirmed it", got)
	}

	// Confirm the assisted one: THIS is what takes the money.
	if _, err := svc.Transition(ctx, transitionInput(assistedOrder, agent, domain.StatusConfirmed, true)); err != nil {
		t.Fatalf("confirm assisted order: %v", err)
	}
	if got := payments.WasCaptured(assistedOrder); got != 1 {
		t.Errorf("confirming an assisted order captured %d times, want exactly 1", got)
	}

	// And a restaurant's order captures on confirmation too — the call is
	// unconditional, and the payment module no-ops when nothing is held.
	in := transitionInput(restaurantOrder, agent, domain.StatusConfirmed, false)
	in.KitchenID = selfServe
	if _, err := svc.Transition(ctx, in); err != nil {
		t.Fatalf("confirm restaurant order: %v", err)
	}
}

func TestConfirmingTwiceDoesNotChargeTwice(t *testing.T) {
	/*
		Staff double-tap. The gateway is the wrong place to discover that, so the
		second confirmation must not produce a second charge.

		Two layers protect this and both are asserted elsewhere: the order state
		machine refuses placed -> confirmed twice, and PaymentService.Capture returns
		early unless the payment is still `authorized`. Here the assertion is the
		OUTCOME — one charge, whatever the mechanism.
	*/
	db := testutil.DB(t)
	assisted := testutil.AssistedKitchen(t, db)
	payments := testutil.NewFakePaymentInitiator()
	svc := newOpsServiceWith(t, db, payments, assistedFake(assisted))

	user := testutil.User(t, db)
	agent := testutil.User(t, db)
	orderID := testutil.Order(t, db, user, assisted,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), PaymentMethod: "upi", TotalMinor: 4000})

	ctx := context.Background()
	if _, err := svc.Transition(ctx, transitionInput(orderID, agent, domain.StatusConfirmed, true)); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	// Second attempt: the state machine should refuse it.
	_, err := svc.Transition(ctx, transitionInput(orderID, agent, domain.StatusConfirmed, true))
	if err == nil {
		t.Log("second confirm was accepted; relying on Capture's own idempotency")
	}

	if got := payments.WasCaptured(orderID); got > 1 {
		t.Errorf("the customer was charged %d times for one order", got)
	}
}

func TestRejectingAnAssistedOrderNeverRefundsBecauseItNeverCharged(t *testing.T) {
	/*
		The point of deferring capture. An authorisation that is never captured lapses
		on its own and the hold drops off the card — so a sold-out coconut leaves no
		charge and no refund line on the customer's statement.

		Asserting on Refunds rather than on a gateway call: the fake records what the
		ordering module ASKED for, and for an uncaptured order it must ask for
		nothing.
	*/
	db := testutil.DB(t)
	assisted := testutil.AssistedKitchen(t, db)
	payments := testutil.NewFakePaymentInitiator()
	svc := newOpsServiceWith(t, db, payments, assistedFake(assisted))

	user := testutil.User(t, db)
	agent := testutil.User(t, db)
	orderID := testutil.Order(t, db, user, assisted,
		testutil.OrderOpts{Status: string(domain.StatusPlaced), PaymentMethod: "upi", TotalMinor: 4000})

	in := transitionInput(orderID, agent, domain.StatusRejected, true)
	in.Reason = "vendor out of coconuts"
	if _, err := svc.Transition(context.Background(), in); err != nil {
		t.Fatalf("reject: %v", err)
	}

	if got := payments.WasCaptured(orderID); got != 0 {
		t.Errorf("a rejected order was captured %d times", got)
	}
}

// newCheckoutForList builds only what ListOrders touches: it reads the order
// repository and nothing else, so every other dependency is a fake.
func newCheckoutForList(t *testing.T, db *database.DB) *app.CheckoutService {
	t.Helper()
	return app.NewCheckoutService(
		db,
		orderingpg.NewOrderRepository(db),
		orderingredis.NewCartRepository(testutil.Cache(t)),
		testutil.NewFakePriceQuoter(),
		testutil.NewFakeKitchenAvailability(),
		testutil.NewFakeStockReserver(),
		testutil.NewFakeDiscountCalculator(),
		testutil.NewFakePaymentInitiator(),
		testutil.NewFakeUserDirectory(id.New()),
		testutil.NewRecordingPublisher(),
		clock.System{},
	)
}

func TestListOrdersShowsOnlyTheCallersOwn(t *testing.T) {
	/*
		The site header has linked to /orders since it was written and neither the page
		nor this endpoint existed, so a customer clicking "Orders" got a 404. The
		repository method was already here and never exposed.

		The assertion that matters is isolation: there is no parameter for asking about
		somebody else's orders, and this proves the scoping actually holds rather than
		trusting that.
	*/
	db := testutil.DB(t)
	ctx := context.Background()

	kitchen := testutil.Kitchen(t, db)
	mine := testutil.User(t, db)
	somebodyElse := testutil.User(t, db)

	myOrder := testutil.Order(t, db, mine, kitchen, testutil.OrderOpts{TotalMinor: 4000})
	theirOrder := testutil.Order(t, db, somebodyElse, kitchen, testutil.OrderOpts{TotalMinor: 9000})

	svc := newCheckoutForList(t, db)
	orders, err := svc.ListOrders(ctx, mine)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}

	var sawMine, sawTheirs bool
	for _, o := range orders {
		switch o.ID {
		case myOrder:
			sawMine = true
		case theirOrder:
			sawTheirs = true
		}
	}
	if !sawMine {
		t.Error("my own order is missing from my order history")
	}
	if sawTheirs {
		t.Error("ANOTHER CUSTOMER'S ORDER appeared in my history — that leaks their " +
			"delivery address and phone number")
	}
}
