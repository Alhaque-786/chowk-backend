package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

const testSecret = "test-webhook-secret-do-not-use"

// sign produces a valid signature for a body, the way a gateway would.
func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookRejectsMissingSignature covers the most basic attack: just POST the
// payload.
//
// The webhook URL is public by necessity — the gateway has to reach it — so the
// signature is the only thing between an attacker and marking any order paid.
func TestWebhookRejectsMissingSignature(t *testing.T) {
	gw := NewMock(testSecret)
	body := []byte(`{"event":"payment.captured","provider_ref":"x","amount_minor":100}`)

	_, err := gw.VerifyWebhook(body, "")
	if err == nil {
		t.Fatal("an unsigned webhook was accepted")
	}
	if got := apperr.KindOf(err); got != apperr.KindUnauthorized {
		t.Errorf("error kind = %v, want unauthorized", got)
	}
}

func TestWebhookRejectsForgedSignature(t *testing.T) {
	gw := NewMock(testSecret)
	body := []byte(`{"event":"payment.captured","provider_ref":"x","amount_minor":100}`)

	forged := []string{
		"deadbeef",
		strings.Repeat("a", 64),
		sign(t, "the-wrong-secret", body),
	}

	for _, signature := range forged {
		if _, err := gw.VerifyWebhook(body, signature); err == nil {
			t.Errorf("forged signature %q... was accepted", signature[:min(8, len(signature))])
		}
	}
}

// TestWebhookRejectsTamperedBody is the important one.
//
// An attacker who intercepts a genuine webhook has a VALID signature. If they can
// change the body and reuse it, they can rewrite the amount or point the capture
// at a different order. The HMAC covering the body is what stops that.
func TestWebhookRejectsTamperedBody(t *testing.T) {
	gw := NewMock(testSecret)

	original := []byte(`{"event":"payment.captured","provider_ref":"pay_1","amount_minor":50000}`)
	signature := sign(t, testSecret, original)

	// Sanity: the untampered pair must verify, or the test proves nothing.
	if _, err := gw.VerifyWebhook(original, signature); err != nil {
		t.Fatalf("the original body failed to verify: %v", err)
	}

	tampered := [][]byte{
		[]byte(`{"event":"payment.captured","provider_ref":"pay_1","amount_minor":1}`),         // cheaper
		[]byte(`{"event":"payment.captured","provider_ref":"pay_OTHER","amount_minor":50000}`), // other order
		[]byte(`{"event":"payment.captured","provider_ref":"pay_1","amount_minor":50000} `),    // trailing space
	}

	for _, body := range tampered {
		if _, err := gw.VerifyWebhook(body, signature); err == nil {
			t.Errorf("tampered body was accepted with the original signature: %s", body)
		}
	}
}

// TestWebhookIsByteExact guards against a subtle mistake: verifying a
// re-serialised body.
//
// Semantically identical JSON with different key order or whitespace has a
// DIFFERENT digest. If anyone "helpfully" parses the body and re-encodes it before
// verifying, every webhook fails — and it looks exactly like a misconfigured
// secret, which is hours of debugging the wrong thing.
func TestWebhookIsByteExact(t *testing.T) {
	gw := NewMock(testSecret)

	compact := []byte(`{"event":"payment.captured","provider_ref":"pay_1","amount_minor":100}`)
	// Same JSON semantically, different bytes.
	reordered := []byte(`{"provider_ref":"pay_1","event":"payment.captured","amount_minor":100}`)

	signature := sign(t, testSecret, compact)

	if _, err := gw.VerifyWebhook(compact, signature); err != nil {
		t.Fatalf("exact bytes failed to verify: %v", err)
	}
	if _, err := gw.VerifyWebhook(reordered, signature); err == nil {
		t.Error("semantically-equal but byte-different JSON verified; the HMAC must be over raw bytes")
	}
}

func TestWebhookAcceptsValidSignature(t *testing.T) {
	gw := NewMock(testSecret)
	body := []byte(`{"event":"payment.captured","provider_ref":"pay_abc","amount_minor":44845}`)

	payload, err := gw.VerifyWebhook(body, sign(t, testSecret, body))
	if err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}

	if payload.ProviderRef != "pay_abc" {
		t.Errorf("ProviderRef = %q, want pay_abc", payload.ProviderRef)
	}
	if payload.Status != domain.StatusCaptured {
		t.Errorf("Status = %q, want captured", payload.Status)
	}
	if payload.Amount.Minor() != 44845 {
		t.Errorf("Amount = %d, want 44845", payload.Amount.Minor())
	}
	// EventID must be populated: it is the dedupe key that makes redelivery safe.
	if payload.EventID == "" {
		t.Error("EventID is empty; duplicate webhooks could not be detected")
	}
}

// TestWebhookMapsStatuses checks the provider-vocabulary translation.
//
// An UNKNOWN event must map to an empty status, meaning "record it, do not act on
// it". Guessing would be worse: a provider adding a new event type must not
// accidentally capture or fail a payment.
func TestWebhookMapsStatuses(t *testing.T) {
	gw := NewMock(testSecret)

	tests := []struct {
		event string
		want  domain.Status
	}{
		{"payment.captured", domain.StatusCaptured},
		{"payment.authorized", domain.StatusAuthorized},
		{"payment.failed", domain.StatusFailed},
		{"payment.some_future_event", ""},
		{"", ""},
	}

	for _, tt := range tests {
		body := []byte(`{"event":"` + tt.event + `","provider_ref":"p","amount_minor":1}`)
		payload, err := gw.VerifyWebhook(body, sign(t, testSecret, body))
		if err != nil {
			t.Fatalf("event %q: %v", tt.event, err)
		}
		if payload.Status != tt.want {
			t.Errorf("event %q mapped to %q, want %q", tt.event, payload.Status, tt.want)
		}
	}
}

// TestRazorpayVerificationMatchesMock asserts both adapters use the same scheme.
//
// This is why the mock is trustworthy: if the real adapter's verification ever
// diverges, the mock stops proving anything about production and this test fails.
func TestRazorpayVerificationMatchesMock(t *testing.T) {
	razorpay := NewRazorpay("key", "secret", testSecret, nil)

	// A Razorpay-shaped body, since its adapter parses a nested envelope.
	body := []byte(`{"event":"payment.captured","payload":{"payment":{"entity":` +
		`{"id":"pay_1","order_id":"order_1","status":"captured","amount":50000,"currency":"INR"}}}}`)

	if _, err := razorpay.VerifyWebhook(body, sign(t, testSecret, body)); err != nil {
		t.Fatalf("valid Razorpay webhook rejected: %v", err)
	}
	if _, err := razorpay.VerifyWebhook(body, "forged"); err == nil {
		t.Error("Razorpay accepted a forged signature")
	}
}

// TestRazorpayFailsClosedWithoutSecret is the configuration-mistake case.
//
// Treating "no secret configured" as "skip verification" would silently disable
// the check in exactly the environment where somebody forgot to set it — which is
// the environment where it matters most.
func TestRazorpayFailsClosedWithoutSecret(t *testing.T) {
	razorpay := NewRazorpay("key", "secret", "", nil)

	body := []byte(`{"event":"payment.captured"}`)
	if _, err := razorpay.VerifyWebhook(body, "anything"); err == nil {
		t.Fatal("verification passed with no webhook secret configured; it must fail closed")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
