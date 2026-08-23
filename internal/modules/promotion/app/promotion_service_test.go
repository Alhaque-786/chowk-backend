package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	promotionpg "github.com/zevoxa/chowk-backend/internal/modules/promotion/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion/app"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

type fixture struct {
	db        *database.DB
	service   *app.PromotionService
	coupons   domain.CouponRepository
	kitchenID id.ID

	couponID id.ID
	userIDs  []id.ID
	orderIDs []id.ID
}

// setup creates a real coupon plus real users and orders, and COMMITS them.
//
// Committed rather than run inside a rolled-back transaction because these tests
// are about genuine concurrency: two SERIALIZABLE transactions must be able to
// conflict with each other, which is impossible inside a single one.
// stubHistory reports whatever it is told about a customer's order history.
//
// Its zero value says "never ordered", which is the state most of these tests
// want, so the majority need not think about it at all.
type stubHistory struct{ ordered bool }

func (h stubHistory) HasEverOrdered(context.Context, id.ID) (bool, error) {
	return h.ordered, nil
}

func setup(t *testing.T, in app.CreateCouponInput, participants int) *fixture {
	t.Helper()

	db := testutil.DB(t)
	ctx := context.Background()

	f := &fixture{
		db:        db,
		kitchenID: id.MustParse("00000000-0000-0000-0000-0000000000a1"), // seeded Khansama
	}
	f.coupons = promotionpg.NewCouponRepository(db)
	redemptions := promotionpg.NewRedemptionRepository(db)
	// A stub history, so these tests do not need real orders. `false` means "has
	// never ordered", which is the first-order case; TestFirstOrderOnlyCoupon
	// exercises both sides explicitly.
	f.service = app.NewPromotionService(db, f.coupons, redemptions, clock.System{}, stubHistory{})

	if in.Code == "" {
		in.Code = "TEST" + id.HumanCode(8)
	}
	coupon, err := f.service.CreateCoupon(ctx, in)
	if err != nil {
		t.Skipf("cannot create coupon (is the DB migrated?): %v", err)
	}
	f.couponID = coupon.ID

	// Clean up after ourselves.
	//
	// These tests run against the SHARED development database, not a throwaway
	// one, and without this every run left its coupons behind as active rows.
	// GET /offers is public, so they surfaced in the customer UI: the home page
	// was advertising eleven offers called "Test flat discount". A test that
	// pollutes the product it is testing is a bug in the test.
	//
	// Redemptions go first: they hold an FK to the coupon, so deleting in the
	// other order fails. Errors are logged rather than fatal -- a cleanup failure
	// must not turn a passing test red, but it must not be silent either.
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := f.db.Q(cleanupCtx).Exec(cleanupCtx,
			`DELETE FROM coupon_redemptions WHERE coupon_id = $1`, coupon.ID); err != nil {
			t.Logf("cleanup: coupon_redemptions for %s: %v", coupon.ID, err)
		}
		if _, err := f.db.Q(cleanupCtx).Exec(cleanupCtx,
			`DELETE FROM coupons WHERE id = $1`, coupon.ID); err != nil {
			t.Logf("cleanup: coupon %s: %v", coupon.ID, err)
		}
	})

	// A distinct user and order per participant, so the per-user cap does not
	// mask the global cap under test.
	for i := 0; i < participants; i++ {
		userID := id.New()
		// Fatalf, not Skipf: a reachable database rejecting this insert is a bug in
		// the test, not an unavailable environment. Skipping here silently hid
		// eleven tests behind a malformed phone number.
		if _, err := db.Q(ctx).Exec(ctx, `
			INSERT INTO users (id, phone, full_name, status, roles, referral_code, created_at, updated_at)
			VALUES ($1, $2, 'Coupon Racer', 'active', ARRAY['customer'], $3, now(), now())`,
			userID, uniquePhone(), id.HumanCode(6)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		f.userIDs = append(f.userIDs, userID)

		orderID := id.New()
		if _, err := db.Q(ctx).Exec(ctx, `
			INSERT INTO orders (id, number, user_id, kitchen_id, status, payment_method,
			                    subtotal_minor, total_minor, currency, version, created_at, updated_at)
			VALUES ($1,$2,$3,$4,'pending_payment','cod',0,0,'INR',1,now(),now())`,
			orderID, "CPN-"+id.HumanCode(8), userID, f.kitchenID); err != nil {
			t.Fatalf("create order: %v", err)
		}
		f.orderIDs = append(f.orderIDs, orderID)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = db.Q(c).Exec(c, `DELETE FROM coupon_redemptions WHERE coupon_id = $1`, f.couponID)
		for _, orderID := range f.orderIDs {
			_, _ = db.Q(c).Exec(c, `DELETE FROM orders WHERE id = $1`, orderID)
		}
		for _, userID := range f.userIDs {
			_, _ = db.Q(c).Exec(c, `DELETE FROM users WHERE id = $1`, userID)
		}
		_, _ = db.Q(c).Exec(c, `DELETE FROM coupons WHERE id = $1`, f.couponID)
	})

	return f
}

// uniquePhone builds a valid E.164 Indian mobile.
//
// The schema enforces `^\+91[6-9][0-9]{9}$`, so this must produce the +91 prefix,
// a leading 6-9, and exactly nine more digits. crypto-random digits keep it from
// colliding with seed data or with a concurrent test run.
func uniquePhone() string {
	digits := make([]byte, 0, 9)
	for len(digits) < 9 {
		for _, r := range id.HumanCode(16) {
			if r >= '2' && r <= '9' && len(digits) < 9 {
				digits = append(digits, byte(r))
			}
		}
	}
	return "+917" + string(digits)
}

func (f *fixture) redemptionCount(t *testing.T) int32 {
	t.Helper()
	coupon, err := f.coupons.FindByID(context.Background(), f.couponID)
	if err != nil {
		t.Fatalf("reload coupon: %v", err)
	}
	return coupon.RedemptionCount
}

func (f *fixture) liveRedemptions(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.Q(context.Background()).QueryRow(context.Background(),
		`SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1 AND released_at IS NULL`,
		f.couponID).Scan(&n); err != nil {
		t.Fatalf("count redemptions: %v", err)
	}
	return n
}

func flatCoupon(maxTotal *int32, perUser int32) app.CreateCouponInput {
	return app.CreateCouponInput{
		Description:           "Test flat discount",
		DiscountType:          "flat",
		DiscountValue:         5000, // Rs50 off
		MinOrderMinor:         0,
		MaxRedemptions:        maxTotal,
		MaxRedemptionsPerUser: perUser,
		FundedBy:              "platform",
	}
}

// ---------------------------------------------------------------------------
// The two claims that were untested
// ---------------------------------------------------------------------------

// TestPreviewDoesNotConsumeARedemption is the reason Calculate and Redeem are
// separate methods.
//
// A customer browsing the offers screen, or typing a code to see what it does,
// must not burn one of the limited uses. Merging preview and redemption is how a
// flash sale "sells out" to people who never ordered.
func TestPreviewDoesNotConsumeARedemption(t *testing.T) {
	cap3 := int32(3)
	f := setup(t, flatCoupon(&cap3, 1), 1)
	ctx := context.Background()

	coupon, err := f.coupons.FindByID(ctx, f.couponID)
	if err != nil {
		t.Fatalf("load coupon: %v", err)
	}

	// Ten previews, by different users.
	for i := 0; i < 10; i++ {
		result, err := f.service.Calculate(ctx, contracts.DiscountRequest{
			UserID:     f.userIDs[0],
			KitchenID:  f.kitchenID,
			CouponCode: coupon.Code,
			Subtotal:   money.FromRupees(500),
		})
		if err != nil {
			t.Fatalf("preview %d: %v", i, err)
		}
		if !result.Applied {
			t.Fatalf("preview %d was rejected: %s", i, result.RejectReason)
		}
		if got := result.DiscountAmount.Minor(); got != 5000 {
			t.Errorf("preview %d discount = %d, want 5000", i, got)
		}
	}

	if got := f.redemptionCount(t); got != 0 {
		t.Errorf("redemption_count = %d after 10 previews, want 0 — preview must not consume a use", got)
	}
	if got := f.liveRedemptions(t); got != 0 {
		t.Errorf("%d redemption rows exist after 10 previews, want 0", got)
	}
}

// TestConcurrentRedemptionsRespectGlobalCap is the SERIALIZABLE test.
//
// Six goroutines redeem simultaneously against a coupon capped at three. Exactly
// three must succeed.
//
// This is the invariant no row lock can protect: "COUNT(redemptions) < 3" is a
// predicate over rows that do not exist yet, so two transactions can both count
// 2 and both insert. Only SERIALIZABLE isolation detects that, which is why
// Redeem runs under db.RetrySerializable.
func TestConcurrentRedemptionsRespectGlobalCap(t *testing.T) {
	const cap, racers = 3, 6

	capN := int32(cap)
	// Per-user cap raised above 1 so the partial unique index cannot be what
	// enforces the limit — the global cap has to do the work.
	f := setup(t, flatCoupon(&capN, 10), racers)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		exhausted int
		tooBusy   int
		other     []error
	)

	// Start barrier: make the goroutines contend as tightly as possible rather
	// than drifting apart as they are spawned.
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(userID, orderID id.ID) {
			defer wg.Done()
			<-start

			err := f.service.Redeem(context.Background(), f.couponID, userID, orderID)

			mu.Lock()
			defer mu.Unlock()
			// Asserted on the CODE, not just the kind.
			//
			// Both outcomes below are KindConflict, and they mean opposite things:
			// COUPON_EXHAUSTED is the cap doing its job, TOO_BUSY is the caller
			// running out of serialization retries without the cap ever being
			// evaluated. Counting them together -- which this test used to do --
			// means a run where every loser merely gave up on contention passes
			// identically to one where the cap actually held. The point of the
			// test is the difference.
			switch code := apperr.CodeOf(err); {
			case err == nil:
				succeeded++
			case code == "COUPON_EXHAUSTED":
				exhausted++
			case code == "TOO_BUSY":
				tooBusy++
			default:
				other = append(other, err)
			}
		}(f.userIDs[i], f.orderIDs[i])
	}

	close(start)
	wg.Wait()

	for _, err := range other {
		t.Errorf("unexpected error (not a cap conflict): %v", err)
	}
	// Zero, not "a few are fine". With jittered backoff over three attempts,
	// six racers on one row should always resolve to a definite answer. A
	// TOO_BUSY here means the backoff is not spreading them, which is exactly the
	// regression the jitter was added to prevent -- and it would otherwise hide
	// as a passing test, because TOO_BUSY is a conflict like COUPON_EXHAUSTED.
	if tooBusy != 0 {
		t.Errorf("%d callers exhausted their serialization retries; backoff is not spreading contention", tooBusy)
	}
	if succeeded != cap {
		t.Errorf("%d redemptions succeeded, want exactly %d — the global cap was breached or under-filled",
			succeeded, cap)
	}
	if exhausted != racers-cap {
		t.Errorf("%d callers were told the coupon was exhausted, want %d", exhausted, racers-cap)
	}

	// The ledger and the cached counter must agree. A drift here means the
	// counter increment escaped the redemption's transaction.
	if got := f.liveRedemptions(t); got != cap {
		t.Errorf("%d live redemption rows, want %d", got, cap)
	}
	if got := f.redemptionCount(t); int(got) != cap {
		t.Errorf("cached redemption_count = %d, want %d — counter drifted from the ledger", got, cap)
	}
}

// TestConcurrentRedemptionsBySameUserRespectPerUserCap covers the other cap,
// which is enforced by a partial unique index rather than by isolation level.
func TestConcurrentRedemptionsBySameUserRespectPerUserCap(t *testing.T) {
	const racers = 4

	// No global cap at all, so only the per-user limit of 1 can stop anything.
	f := setup(t, flatCoupon(nil, 1), racers)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	start := make(chan struct{})

	// ONE user, four different orders.
	user := f.userIDs[0]
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(orderID id.ID) {
			defer wg.Done()
			<-start
			if err := f.service.Redeem(context.Background(), f.couponID, user, orderID); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(f.orderIDs[i])
	}
	close(start)
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d redemptions succeeded for one user with a per-user cap of 1, want 1", succeeded)
	}
	if got := f.liveRedemptions(t); got != 1 {
		t.Errorf("%d live redemptions, want 1", got)
	}
}

// TestRedeemIsIdempotentPerOrder covers the retried-checkout path.
//
// The unique index on order_id means a second Redeem for the same order is a
// no-op success, not a second use. Without that, a client retrying on a flaky
// network would consume two of a limited coupon for one order.
func TestRedeemIsIdempotentPerOrder(t *testing.T) {
	cap5 := int32(5)
	f := setup(t, flatCoupon(&cap5, 5), 1)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := f.service.Redeem(ctx, f.couponID, f.userIDs[0], f.orderIDs[0]); err != nil {
			t.Fatalf("redeem attempt %d: %v", i, err)
		}
	}

	if got := f.liveRedemptions(t); got != 1 {
		t.Errorf("%d redemption rows after 3 redeems of the SAME order, want 1", got)
	}
	if got := f.redemptionCount(t); got != 1 {
		t.Errorf("redemption_count = %d after 3 redeems of the same order, want 1", got)
	}
}

// TestReleaseReturnsTheUse covers cancellation.
func TestReleaseReturnsTheUse(t *testing.T) {
	cap1 := int32(1)
	f := setup(t, flatCoupon(&cap1, 1), 2)
	ctx := context.Background()

	if err := f.service.Redeem(ctx, f.couponID, f.userIDs[0], f.orderIDs[0]); err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	// Cap of 1 is now reached: a second user must be refused.
	if err := f.service.Redeem(ctx, f.couponID, f.userIDs[1], f.orderIDs[1]); err == nil {
		t.Fatal("second redeem succeeded against a cap of 1")
	}

	if err := f.service.ReleaseRedemption(ctx, f.orderIDs[0]); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := f.redemptionCount(t); got != 0 {
		t.Errorf("redemption_count = %d after release, want 0", got)
	}

	// The freed slot must be usable.
	if err := f.service.Redeem(ctx, f.couponID, f.userIDs[1], f.orderIDs[1]); err != nil {
		t.Errorf("redeem after release failed; the slot was not returned: %v", err)
	}

	// Idempotent: release runs from cancellation AND from the payment-failed
	// handler, and both can legitimately fire for one order.
	if err := f.service.ReleaseRedemption(ctx, f.orderIDs[0]); err != nil {
		t.Errorf("second release returned an error; it must be idempotent: %v", err)
	}
}

// TestExhaustedCouponIsRejectedAtPreview: once full, the offer stops being
// offered rather than failing at checkout after the customer has committed.
func TestExhaustedCouponIsRejectedAtPreview(t *testing.T) {
	cap1 := int32(1)
	f := setup(t, flatCoupon(&cap1, 5), 2)
	ctx := context.Background()

	coupon, err := f.coupons.FindByID(ctx, f.couponID)
	if err != nil {
		t.Fatalf("load coupon: %v", err)
	}
	if err := f.service.Redeem(ctx, f.couponID, f.userIDs[0], f.orderIDs[0]); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	result, err := f.service.Calculate(ctx, contracts.DiscountRequest{
		UserID:     f.userIDs[1],
		KitchenID:  f.kitchenID,
		CouponCode: coupon.Code,
		Subtotal:   money.FromRupees(500),
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if result.Applied {
		t.Error("an exhausted coupon still previewed as valid")
	}
	if result.RejectReason == "" {
		t.Error("no reason given for the rejection; the customer needs to know why")
	}
}

// ---------------------------------------------------------------------------
// Discount arithmetic and eligibility
// ---------------------------------------------------------------------------

// TestPercentageDiscountIsCapped is the money test.
//
// An uncapped percentage coupon is unbounded exposure on a large order, which is
// why the cap is mandatory in three places (DTO, domain, DB CHECK).
func TestPercentageDiscountIsCapped(t *testing.T) {
	capAmount := int64(10000) // Rs100 maximum
	f := setup(t, app.CreateCouponInput{
		Description:           "50% off up to Rs100",
		DiscountType:          "percentage",
		DiscountValue:         5000, // 50%
		MaxDiscountMinor:      &capAmount,
		MaxRedemptionsPerUser: 1,
		FundedBy:              "platform",
	}, 1)
	ctx := context.Background()

	coupon, err := f.coupons.FindByID(ctx, f.couponID)
	if err != nil {
		t.Fatalf("load coupon: %v", err)
	}

	tests := []struct {
		subtotalRupees float64
		wantMinor      int64
		why            string
	}{
		{100, 5000, "50% of 100 = 50, under the cap"},
		{200, 10000, "50% of 200 = 100, exactly the cap"},
		{500, 10000, "50% of 500 = 250, CAPPED at 100"},
		{5000, 10000, "50% of 5000 = 2500, still capped at 100"},
	}

	for _, tt := range tests {
		result, err := f.service.Calculate(ctx, contracts.DiscountRequest{
			UserID:     f.userIDs[0],
			KitchenID:  f.kitchenID,
			CouponCode: coupon.Code,
			Subtotal:   money.FromRupees(tt.subtotalRupees),
		})
		if err != nil {
			t.Fatalf("subtotal %.0f: %v", tt.subtotalRupees, err)
		}
		if got := result.DiscountAmount.Minor(); got != tt.wantMinor {
			t.Errorf("subtotal Rs%.0f: discount = %d, want %d (%s)",
				tt.subtotalRupees, got, tt.wantMinor, tt.why)
		}
	}
}

// TestFlatDiscountNeverExceedsSubtotal: a Rs50-off coupon on a Rs30 order gives
// Rs30 off, not Rs50 and a refund.
func TestFlatDiscountNeverExceedsSubtotal(t *testing.T) {
	f := setup(t, flatCoupon(nil, 1), 1)
	ctx := context.Background()

	coupon, err := f.coupons.FindByID(ctx, f.couponID)
	if err != nil {
		t.Fatalf("load coupon: %v", err)
	}

	result, err := f.service.Calculate(ctx, contracts.DiscountRequest{
		UserID:     f.userIDs[0],
		KitchenID:  f.kitchenID,
		CouponCode: coupon.Code,
		Subtotal:   money.FromRupees(30), // less than the Rs50 discount
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got := result.DiscountAmount.Minor(); got != 3000 {
		t.Errorf("discount = %d on a Rs30 order with a Rs50 coupon, want 3000 (clamped to subtotal)", got)
	}
}

// TestMinOrderRejectionTellsCustomerTheShortfall — the difference between a
// rejection and an upsell.
func TestMinOrderRejectionTellsCustomerTheShortfall(t *testing.T) {
	in := flatCoupon(nil, 1)
	in.MinOrderMinor = 30000 // Rs300 minimum
	f := setup(t, in, 1)
	ctx := context.Background()

	coupon, err := f.coupons.FindByID(ctx, f.couponID)
	if err != nil {
		t.Fatalf("load coupon: %v", err)
	}

	result, err := f.service.Calculate(ctx, contracts.DiscountRequest{
		UserID:     f.userIDs[0],
		KitchenID:  f.kitchenID,
		CouponCode: coupon.Code,
		Subtotal:   money.FromRupees(250), // Rs50 short
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if result.Applied {
		t.Fatal("a below-minimum order was accepted")
	}
	// The message must name the shortfall, not just say no.
	if !containsStr(result.RejectReason, "50") {
		t.Errorf("reason %q does not tell the customer how much more to add", result.RejectReason)
	}
}

func TestCodeIsCaseAndWhitespaceInsensitive(t *testing.T) {
	f := setup(t, flatCoupon(nil, 1), 1)
	ctx := context.Background()

	coupon, err := f.coupons.FindByID(ctx, f.couponID)
	if err != nil {
		t.Fatalf("load coupon: %v", err)
	}

	for _, variant := range []string{
		coupon.Code,
		lower(coupon.Code),
		"  " + coupon.Code + "  ",
		"  " + lower(coupon.Code) + "\t",
	} {
		result, err := f.service.Calculate(ctx, contracts.DiscountRequest{
			UserID:     f.userIDs[0],
			KitchenID:  f.kitchenID,
			CouponCode: variant,
			Subtotal:   money.FromRupees(500),
		})
		if err != nil {
			t.Fatalf("variant %q: %v", variant, err)
		}
		if !result.Applied {
			t.Errorf("variant %q was rejected: %s", variant, result.RejectReason)
		}
	}
}

func TestUnknownCodeIsRejectedVaguely(t *testing.T) {
	f := setup(t, flatCoupon(nil, 1), 1)

	result, err := f.service.Calculate(context.Background(), contracts.DiscountRequest{
		UserID:     f.userIDs[0],
		KitchenID:  f.kitchenID,
		CouponCode: "DEFINITELY-NOT-A-REAL-CODE",
		Subtotal:   money.FromRupees(500),
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if result.Applied {
		t.Fatal("a nonexistent code was accepted")
	}
	// Deliberately vague: confirming which codes exist lets someone enumerate
	// unreleased promotions.
	if containsStr(result.RejectReason, "not found") || containsStr(result.RejectReason, "exist") {
		t.Errorf("reason %q reveals whether the code exists", result.RejectReason)
	}
}

// TestExpiredCouponIsRejected verifies the validity window.
func TestExpiredCouponIsRejected(t *testing.T) {
	past := time.Now().UTC().Add(-1 * time.Hour)
	in := flatCoupon(nil, 1)
	in.ValidFrom = ptr(time.Now().UTC().Add(-2 * time.Hour))
	in.ValidUntil = &past
	f := setup(t, in, 1)

	coupon, err := f.coupons.FindByID(context.Background(), f.couponID)
	if err != nil {
		t.Fatalf("load coupon: %v", err)
	}

	result, err := f.service.Calculate(context.Background(), contracts.DiscountRequest{
		UserID:     f.userIDs[0],
		KitchenID:  f.kitchenID,
		CouponCode: coupon.Code,
		Subtotal:   money.FromRupees(500),
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if result.Applied {
		t.Error("an expired coupon was accepted")
	}
}

// TestPercentageCouponRequiresACap: refused at the service layer, so the DB
// CHECK is a backstop rather than the only guard.
func TestPercentageCouponRequiresACap(t *testing.T) {
	db := testutil.DB(t)
	service := app.NewPromotionService(db,
		promotionpg.NewCouponRepository(db),
		promotionpg.NewRedemptionRepository(db),
		clock.System{},
		stubHistory{})

	_, err := service.CreateCoupon(context.Background(), app.CreateCouponInput{
		Code:                  "UNCAPPED" + id.HumanCode(6),
		DiscountType:          "percentage",
		DiscountValue:         2000, // 20% with NO cap
		MaxRedemptionsPerUser: 1,
	})
	if err == nil {
		t.Fatal("an uncapped percentage coupon was created; that is unbounded exposure on a large order")
	}
	if apperr.KindOf(err) != apperr.KindInvalid {
		t.Errorf("error kind = %v, want invalid", apperr.KindOf(err))
	}
}

func ptr[T any](v T) *T { return &v }

func lower(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return string(out)
}

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestFirstOrderOnlyCouponIsResolvedNotAssumed is a regression test for a coupon
// that could never be redeemed by anybody.
//
// `DiscountRequest.IsFirstOrder` was an INPUT that no caller ever set. So it was
// always false, and every first_order_only coupon was rejected for every customer
// — permanently, and invisibly, because the rejection is indistinguishable from a
// legitimate one. The seeded WELCOME50 offer was dead on arrival.
//
// Promotion now resolves it by asking CustomerHistory. This asserts BOTH
// directions, because a fix that only checks the happy path would pass just as
// well if the answer were hardcoded to true — which would give the discount to
// everybody and be a worse bug than the one it replaced.
func TestFirstOrderOnlyCouponIsResolvedNotAssumed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hasOrdered  bool
		wantApplied bool
	}{
		{"new customer gets the offer", false, true},
		{"returning customer does not", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, app.CreateCouponInput{
				Description:           "First order only",
				DiscountType:          "flat",
				DiscountValue:         5000,
				FirstOrderOnly:        true,
				MaxRedemptionsPerUser: 1,
			}, 1)

			coupon, err := f.coupons.FindByID(context.Background(), f.couponID)
			if err != nil {
				t.Fatalf("load coupon: %v", err)
			}

			// Rebuilt with the history this case needs. Same repositories, so the
			// coupon created by setup is the one under test.
			service := app.NewPromotionService(
				f.db,
				f.coupons,
				promotionpg.NewRedemptionRepository(f.db),
				clock.System{},
				stubHistory{ordered: tc.hasOrdered},
			)

			result, calcErr := service.Calculate(context.Background(), contracts.DiscountRequest{
				UserID:    f.userIDs[0],
				KitchenID: f.kitchenID,
				// Deliberately NOT setting IsFirstOrder: the point is that the
				// service resolves it rather than trusting the caller.
				CouponCode: coupon.Code,
				Subtotal:   money.FromRupees(500),
			})
			if calcErr != nil {
				t.Fatalf("Calculate: %v", calcErr)
			}
			if result.Applied != tc.wantApplied {
				t.Errorf("applied = %v, want %v (reason: %q)",
					result.Applied, tc.wantApplied, result.RejectReason)
			}
		})
	}
}
