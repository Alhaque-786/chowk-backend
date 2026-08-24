package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Mock is a gateway for local development and tests.
//
// It exists so the whole payment path — intent, webhook, capture, order
// promotion — can be exercised with no Razorpay account, no network, and no
// secrets in a developer's environment. Without it, "does checkout work" is
// untestable until someone has credentials, and integration tests need the
// internet.
//
// Crucially it uses the SAME HMAC verification as the real adapter, so the
// signature path is genuinely tested rather than stubbed out. A mock that skipped
// verification would let a signature bug reach production untested.
type Mock struct {
	webhookSecret string
	// failNext makes a test force a declined payment without special-casing the
	// service under test.
	failNext bool
}

func NewMock(webhookSecret string) *Mock {
	if webhookSecret == "" {
		// A fixed default so local webhooks can be signed by a script without
		// configuring anything. Never used in production: config requires a real
		// secret there.
		webhookSecret = "local-mock-webhook-secret" //nolint:gosec // dev default; config.Load requires a real secret in production
	}
	return &Mock{webhookSecret: webhookSecret}
}

func (m *Mock) Name() string { return "mock" }

// FailNext makes the next CreateIntent report a declined payment.
func (m *Mock) FailNext() { m.failNext = true }

// WebhookSecret is exposed so the local test helper can sign a payload.
func (m *Mock) WebhookSecret() string { return m.webhookSecret }

func (m *Mock) CreateIntent(_ context.Context, req domain.IntentRequest) (domain.IntentResponse, error) {
	if m.failNext {
		m.failNext = false
		return domain.IntentResponse{}, apperr.Invalid("PAYMENT_DECLINED",
			"The payment was declined. Please try another method.")
	}

	// A deterministic-looking provider ref, so logs and webhook payloads are
	// readable while still being unique.
	ref := "mock_pay_" + strings.ReplaceAll(id.New().String(), "-", "")[:16]

	return domain.IntentResponse{
		ProviderRef:     ref,
		ProviderOrderID: ref,
		ClientSecret:    ref + "_secret",
		Status:          domain.StatusCreated,
		RequiresAction:  true,
		RawPayload:      []byte(fmt.Sprintf(`{"mock":true,"ref":%q,"amount":%d}`, ref, req.Amount.Minor())),
	}, nil
}

func (m *Mock) Capture(context.Context, string, money.Amount) error { return nil }

func (m *Mock) Refund(_ context.Context, req domain.RefundRequest) (domain.RefundResponse, error) {
	return domain.RefundResponse{
		ProviderRef: "mock_rfnd_" + strings.ReplaceAll(id.New().String(), "-", "")[:16],
		// Completed immediately: a mock has no settlement delay to simulate, and
		// pretending otherwise would just make local testing slower.
		Status:     domain.RefundCompleted,
		RawPayload: []byte(fmt.Sprintf(`{"mock":true,"amount":%d}`, req.Amount.Minor())),
	}, nil
}

// VerifyWebhook uses the identical HMAC-SHA256 scheme as Razorpay, so the
// verification path is exercised for real in development.
func (m *Mock) VerifyWebhook(rawBody []byte, signature string) (domain.WebhookPayload, error) {
	if signature == "" {
		return domain.WebhookPayload{}, apperr.Unauthorized("MISSING_SIGNATURE",
			"Webhook signature is missing.")
	}

	mac := hmac.New(sha256.New, []byte(m.webhookSecret))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return domain.WebhookPayload{}, apperr.Unauthorized("INVALID_SIGNATURE",
			"Webhook signature does not match.")
	}

	var envelope struct {
		Event         string `json:"event"`
		EventID       string `json:"event_id"`
		ProviderRef   string `json:"provider_ref"`
		AmountMinor   int64  `json:"amount_minor"`
		FailureCode   string `json:"failure_code"`
		FailureReason string `json:"failure_reason"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return domain.WebhookPayload{}, apperr.Invalid("MALFORMED_WEBHOOK",
			"Webhook body is not valid JSON.").WithCause(err)
	}

	eventID := envelope.EventID
	if eventID == "" {
		eventID = envelope.ProviderRef + ":" + envelope.Event
	}

	var status domain.Status
	switch envelope.Event {
	case "payment.captured":
		status = domain.StatusCaptured
	case "payment.authorized":
		status = domain.StatusAuthorized
	case "payment.failed":
		status = domain.StatusFailed
	}

	return domain.WebhookPayload{
		EventID:       eventID,
		EventType:     envelope.Event,
		ProviderRef:   envelope.ProviderRef,
		Status:        status,
		Amount:        money.FromMinor(envelope.AmountMinor, money.INR),
		FailureCode:   envelope.FailureCode,
		FailureReason: envelope.FailureReason,
		RawPayload:    rawBody,
	}, nil
}

// readAllLimited bounds a response body so a misbehaving provider cannot exhaust
// memory.
func readAllLimited(r io.Reader) ([]byte, error) {
	const maxResponse = 1 << 20 // 1 MiB
	return io.ReadAll(io.LimitReader(r, maxResponse))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

var _ domain.Gateway = (*Mock)(nil)
