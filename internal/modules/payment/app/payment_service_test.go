package app_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	paymentgateway "github.com/zevoxa/chowk-backend/internal/modules/payment/adapter/gateway"
	paymentpg "github.com/zevoxa/chowk-backend/internal/modules/payment/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/payment/app"
	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

/*
The payment service had NO tests, and it is the module where a bug costs money.

ProcessWebhook is the sharpest edge in the system: the URL is public, the body is
attacker-controllable, and what it does on success is mark an order paid. The
signature check is the only thing standing between the internet and free food.

These tests run against real Postgres, because most of what is worth asserting is
enforced there — the unique (provider, event_id) index that makes redelivery a
no-op instead of a double capture, and the row lock that serialises two webhooks
for the same payment.
*/

const webhookSecret = "test-webhook-secret-long-enough-to-be-realistic"

type fixture struct {
	svc       *app.PaymentService
	payments  domain.PaymentRepository
	gateway   *paymentgateway.Mock
	events    *testutil.RecordingPublisher
	userID    id.ID
	kitchenID id.ID
	orderID   id.ID
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := testutil.DB(t)

	f := &fixture{
		gateway: paymentgateway.NewMock(webhookSecret),
		events:  testutil.NewRecordingPublisher(),
	}
	f.kitchenID = testutil.Kitchen(t, db)
	f.userID = testutil.User(t, db)
	f.orderID = testutil.Order(t, db, f.userID, f.kitchenID, testutil.OrderOpts{TotalMinor: 50000})

	f.payments = paymentpg.NewPaymentRepository(db)
	f.svc = app.NewPaymentService(
		db, f.payments, paymentpg.NewWebhookRepository(db),
		f.gateway, testutil.NewFakeUserDirectory(f.userID), f.events, clock.System{},
	)
	return f
}

// intent creates a payment for the fixture order and returns its provider ref.
func (f *fixture) intent(t *testing.T, minor int64) contracts.PaymentIntent {
	t.Helper()
	got, err := f.svc.CreateIntent(context.Background(), contracts.PaymentIntentRequest{
		OrderID: f.orderID, UserID: f.userID,
		Amount: money.FromMinor(minor, money.INR), Method: "upi",
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}
	return got
}

// signed builds a webhook body and its valid HMAC, the way the gateway would.
func signed(t *testing.T, event, providerRef string, amountMinor int64) ([]byte, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event":        event,
		"event_id":     "evt_" + id.HumanCode(12),
		"provider_ref": providerRef,
		"amount_minor": amountMinor,
	})
	if err != nil {
		t.Fatalf("marshal webhook: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return body, hex.EncodeToString(mac.Sum(nil))
}

// --------------------------------------------------------------------------
// Signature verification — the security boundary
// --------------------------------------------------------------------------

func TestForgedWebhookIsRejectedAndNotStored(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	intent := f.intent(t, 50000)

	body, _ := signed(t, "payment.captured", intent.ProviderRef, 50000)

	for name, sig := range map[string]string{
		"empty signature":   "",
		"garbage signature": "not-a-signature",
		"wrong-key HMAC":    wrongKeyHMAC(t, body),
		"truncated HMAC":    "abc123",
	} {
		t.Run(name, func(t *testing.T) {
			err := f.svc.ProcessWebhook(ctx, body, sig)
			if err == nil {
				t.Fatal("an unsigned or wrongly-signed webhook was ACCEPTED; the URL is " +
					"public, so this is free food for anyone who finds it")
			}
			if k := apperr.KindOf(err); k != apperr.KindUnauthorized {
				t.Errorf("rejection kind = %v, want Unauthorized", k)
			}
		})
	}

	// And the payment must be untouched.
	after, err := f.payments.FindByID(ctx, intent.PaymentID)
	if err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if after.Status == domain.StatusCaptured {
		t.Fatal("payment was captured by a forged webhook")
	}
}

func wrongKeyHMAC(t *testing.T, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte("a-different-secret-entirely"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// --------------------------------------------------------------------------
// Redelivery — the double-capture guard
// --------------------------------------------------------------------------

func TestDuplicateWebhookIsANoopAndReportsSuccess(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	intent := f.intent(t, 50000)
	body, sig := signed(t, "payment.captured", intent.ProviderRef, 50000)

	if err := f.svc.ProcessWebhook(ctx, body, sig); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	/*
		The SAME body again, which is what at-least-once delivery guarantees will
		happen. It must succeed: returning an error makes the gateway retry forever,
		and we genuinely have already done the work.

		TWO MECHANISMS ARE INVOLVED AND THEY GUARD DIFFERENT FAILURES. Established by
		planting each, because I described this wrongly twice before checking.

		1. Payment.TransitionTo returns ErrAlreadyInStatus for a same-status move, and
		   the caller treats that as success and publishes nothing. This is what
		   prevents a DOUBLE CAPTURE: bypassing the `!isNew` early return entirely
		   (planted as `if !isNew && isNew`) leaves this test passing.

		2. The unique (provider, provider_event_id) index is not a safety net at all —
		   it is load-bearing for Store, which uses
		   `ON CONFLICT (provider, provider_event_id) DO NOTHING`. Drop the index and
		   Postgres raises SQLSTATE 42P10, "no unique or exclusion constraint matching
		   the ON CONFLICT specification", so EVERY webhook fails with a 500. Verified
		   by actually dropping it: both tests in this file fail on the FIRST delivery,
		   not on a duplicate.

		So the index does not stop a double capture; the state machine does. The index
		stops the gateway from re-running the whole transaction on every retry, and
		removing it breaks webhook processing outright.
	*/
	if err := f.svc.ProcessWebhook(ctx, body, sig); err != nil {
		t.Fatalf("redelivery must be a successful no-op, got: %v", err)
	}

	// Exactly one capture event, not two. This is the assertion that would catch a
	// double credit.
	var captured int
	for _, n := range f.events.Names() {
		if n == string(contracts.EventPaymentCaptured) {
			captured++
		}
	}
	if captured != 1 {
		t.Errorf("published %d payment.captured events for one payment, want 1", captured)
	}
}

// --------------------------------------------------------------------------
// Amount tampering
// --------------------------------------------------------------------------

func TestCaptureForTheWrongAmountIsRefused(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	intent := f.intent(t, 50000)

	// The gateway claims a different amount than the order. Tampering, a currency
	// mix-up, or the wrong payment matched — all three mean do not capture.
	body, sig := signed(t, "payment.captured", intent.ProviderRef, 1)

	err := f.svc.ProcessWebhook(ctx, body, sig)
	if err == nil {
		t.Fatal("a capture for ₹0.01 against a ₹500 order was accepted")
	}
	if code := apperr.CodeOf(err); code != "PAYMENT_AMOUNT_MISMATCH" {
		t.Errorf("code = %q, want PAYMENT_AMOUNT_MISMATCH", code)
	}

	after, err := f.payments.FindByID(ctx, intent.PaymentID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status == domain.StatusCaptured {
		t.Error("payment reached captured despite the amount mismatch")
	}
	if f.events.Has(contracts.EventPaymentCaptured) {
		t.Error("a captured event was published for a mismatched amount, which would " +
			"promote the order")
	}
}

// --------------------------------------------------------------------------
// Ordinary paths
// --------------------------------------------------------------------------

func TestCapturedWebhookMovesThePaymentAndPublishes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	intent := f.intent(t, 50000)
	body, sig := signed(t, "payment.captured", intent.ProviderRef, 50000)

	if err := f.svc.ProcessWebhook(ctx, body, sig); err != nil {
		t.Fatalf("process: %v", err)
	}

	after, err := f.payments.FindByID(ctx, intent.PaymentID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != domain.StatusCaptured {
		t.Errorf("status = %q, want captured", after.Status)
	}
	// Ordering subscribes to this and promotes the order. Payment must not call
	// ordering directly, so the event IS the contract.
	if !f.events.Has(contracts.EventPaymentCaptured) {
		t.Errorf("no %s published; the order would never be promoted. published: %v",
			contracts.EventPaymentCaptured, f.events.Names())
	}
}

func TestFailedWebhookRecordsTheReason(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	intent := f.intent(t, 50000)

	body, err := json.Marshal(map[string]any{
		"event":          "payment.failed",
		"event_id":       "evt_" + id.HumanCode(12),
		"provider_ref":   intent.ProviderRef,
		"amount_minor":   50000,
		"failure_code":   "insufficient_funds",
		"failure_reason": "Card declined by issuer",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)

	if err := f.svc.ProcessWebhook(ctx, body, hex.EncodeToString(mac.Sum(nil))); err != nil {
		t.Fatalf("process: %v", err)
	}

	after, err := f.payments.FindByID(ctx, intent.PaymentID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != domain.StatusFailed {
		t.Errorf("status = %q, want failed", after.Status)
	}
	// Kept so support can answer "why did my payment fail" without the provider
	// dashboard.
	if after.FailureCode != "insufficient_funds" {
		t.Errorf("failure code = %q, want insufficient_funds", after.FailureCode)
	}
	if after.FailureReason == "" {
		t.Error("failure reason was dropped")
	}
}

func TestWebhookForUnknownPaymentIsIgnoredNotRetriedForever(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// A provider ref we have never issued — in practice a test payment fired from
	// the provider's dashboard. Retrying will not conjure the row, so erroring
	// would make the gateway retry until it gives up and alerts somebody.
	body, sig := signed(t, "payment.captured", "pay_nonexistent_"+id.HumanCode(8), 50000)

	if err := f.svc.ProcessWebhook(ctx, body, sig); err != nil {
		t.Errorf("a webhook for an unknown payment should be accepted and ignored, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// Intent creation
// --------------------------------------------------------------------------

func TestCreateIntentIsIdempotentPerKey(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	key := "checkout-" + id.HumanCode(10)

	req := contracts.PaymentIntentRequest{
		OrderID: f.orderID, UserID: f.userID,
		Amount: money.FromMinor(50000, money.INR), Method: "upi",
		IdempotencyKey: key,
	}

	first, err := f.svc.CreateIntent(ctx, req)
	if err != nil {
		t.Fatalf("first intent: %v", err)
	}
	// A retried checkout — the customer double-tapped, or the client retried a
	// timeout. It must return the SAME payment, not open a second one against the
	// same order.
	second, err := f.svc.CreateIntent(ctx, req)
	if err != nil {
		t.Fatalf("retried intent: %v", err)
	}

	if first.PaymentID != second.PaymentID {
		t.Errorf("retry created a second payment (%s then %s); the customer could be "+
			"charged twice for one order", first.PaymentID, second.PaymentID)
	}

	var count int
	db := testutil.DB(t)
	if err := db.Q(ctx).QueryRow(ctx,
		`SELECT count(*) FROM payments WHERE order_id = $1`, f.orderID).Scan(&count); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if count != 1 {
		t.Errorf("%d payment rows for one order, want 1", count)
	}
}

func TestCreateIntentRecordsTheAttemptEvenWhenTheGatewayFails(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Two-phase on purpose: our row is written BEFORE the gateway is called, so a
	// gateway failure still leaves something for reconciliation to find. Calling
	// the gateway first means a crash in between leaves money moving with nothing
	// on our side pointing at it.
	f.gateway.FailNext()

	_, err := f.svc.CreateIntent(ctx, contracts.PaymentIntentRequest{
		OrderID: f.orderID, UserID: f.userID,
		Amount: money.FromMinor(50000, money.INR), Method: "upi",
	})
	if err == nil {
		t.Fatal("a gateway failure should surface to the caller")
	}

	db := testutil.DB(t)
	var count int
	if err := db.Q(ctx).QueryRow(ctx,
		`SELECT count(*) FROM payments WHERE order_id = $1`, f.orderID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count == 0 {
		t.Error("no payment row was written before the gateway call, so a failed " +
			"attempt is invisible to reconciliation")
	}
}

func TestGetForOrderReturnsNotFoundRatherThanNil(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	_, err := f.svc.GetForOrder(ctx, id.New())
	if err == nil {
		t.Fatal("expected an error for an order with no payment")
	}
	if k := apperr.KindOf(err); k != apperr.KindNotFound {
		t.Errorf("kind = %v, want NotFound (a nil payment would panic a caller)", k)
	}
	_ = fmt.Sprint(f.orderID)
}

func TestDuplicateWebhookIsStoredOnlyOnce(t *testing.T) {
	/*
		Asserts the storage layer directly: three identical deliveries, one row.

		This is the dedupe that keeps a retrying gateway from re-running the whole
		transaction each time. It is NOT what prevents a double capture — see the note
		in the test above, which was corrected after planting the bug and finding the
		state machine doing that work.
	*/
	f := setup(t)
	ctx := context.Background()
	db := testutil.DB(t)
	intent := f.intent(t, 50000)
	body, sig := signed(t, "payment.captured", intent.ProviderRef, 50000)

	for i := 0; i < 3; i++ {
		if err := f.svc.ProcessWebhook(ctx, body, sig); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	var stored int
	if err := db.Q(ctx).QueryRow(ctx, `
		SELECT count(*) FROM webhook_events
		WHERE provider = $1 AND provider_event_id = (
			SELECT provider_event_id FROM webhook_events
			WHERE provider = $1 ORDER BY received_at DESC LIMIT 1)`,
		f.gateway.Name()).Scan(&stored); err != nil {
		t.Fatalf("count webhook events: %v", err)
	}
	if stored != 1 {
		t.Errorf("three identical deliveries produced %d webhook_events rows, want 1 — "+
			"the unique (provider, provider_event_id) index is not doing its job", stored)
	}
}
