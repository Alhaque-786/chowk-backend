package domain

import (
	"context"

	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Gateway is the port every payment provider implements.
//
// This interface is the anti-corruption layer. Razorpay's JSON shapes, its field
// names, its status vocabulary and its quirks stop HERE and never reach the
// application layer. Two payoffs:
//
//   - A second provider (for failover, or because one negotiates better rates)
//     is a new adapter, not a rewrite.
//   - Local development and tests run against a mock with no network and no
//     credentials, which is the difference between a fast test suite and one
//     nobody runs.
type Gateway interface {
	// Name identifies the provider in stored rows and metrics.
	Name() string

	// CreateIntent asks the gateway to open a payment.
	//
	// IdempotencyKey is passed through to the provider, so a retried call returns
	// the SAME intent rather than opening a second one. Without it, a network
	// timeout on our side plus a retry equals two payment attempts for one order.
	CreateIntent(ctx context.Context, req IntentRequest) (IntentResponse, error)

	// Capture takes funds that were only authorized. Providers that settle
	// immediately may implement this as a no-op.
	Capture(ctx context.Context, providerRef string, amount money.Amount) error

	// Refund returns money. Must be idempotent on IdempotencyKey: a retried
	// refund must not pay the customer twice.
	Refund(ctx context.Context, req RefundRequest) (RefundResponse, error)

	// VerifyWebhook checks the signature over the RAW request body and returns
	// the normalised event.
	//
	// Takes raw bytes, not a parsed struct, because the signature is computed
	// over the exact bytes sent. Re-serialising parsed JSON changes key order and
	// whitespace, so the HMAC would never match — a mistake that looks like a
	// misconfigured secret and wastes hours.
	VerifyWebhook(rawBody []byte, signature string) (WebhookPayload, error)
}

type IntentRequest struct {
	OrderID        id.ID
	OrderNumber    string
	UserID         id.ID
	Amount         money.Amount
	Method         string
	CustomerPhone  string
	CustomerEmail  string
	IdempotencyKey string
}

type IntentResponse struct {
	ProviderRef     string
	ProviderOrderID string
	// ClientSecret is handed to the frontend SDK to complete payment. Never
	// logged.
	ClientSecret   string
	Status         Status
	RequiresAction bool
	RawPayload     []byte
}

type RefundRequest struct {
	ProviderRef    string
	Amount         money.Amount
	Reason         string
	IdempotencyKey string
}

type RefundResponse struct {
	ProviderRef string
	Status      RefundStatus
	RawPayload  []byte
}

// WebhookPayload is a provider event normalised into our vocabulary.
//
// Adapters translate provider-specific event names ("payment.captured",
// "charge.succeeded") into our Status, so the application layer never switches on
// a provider string.
type WebhookPayload struct {
	// EventID is the provider's unique id for this delivery. The uniqueness
	// constraint on it is what makes redelivery safe.
	EventID   string
	EventType string
	// ProviderRef identifies which payment this concerns.
	ProviderRef string
	// Status is the payment state this event implies. Empty when the event is
	// informational and should be recorded but not acted on.
	Status        Status
	Amount        money.Amount
	FailureCode   string
	FailureReason string
	RawPayload    []byte
}
