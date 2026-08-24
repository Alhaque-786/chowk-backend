// Package gateway holds payment provider adapters.
package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/resilience"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

const razorpayBaseURL = "https://api.razorpay.com/v1"

// Razorpay implements domain.Gateway.
//
// Every outbound call goes through a resilience.Guard: circuit breaker, bulkhead,
// per-attempt timeout. A payment gateway is the classic dependency that starts
// taking 30 seconds instead of 300ms, and without the guard that becomes pool
// exhaustion and a full outage of our own service.
type Razorpay struct {
	keyID         string
	keySecret     string
	webhookSecret string
	client        *http.Client
	guard         *resilience.Guard
}

// NewRazorpay builds the live gateway.
//
// `guards` may be nil — a unit test constructing this directly has no registry and
// should not have to build one. Registering is what makes the circuit breaker's
// state visible on /status; without it the breaker still works and simply cannot be
// observed, which is the situation this parameter was added to end.
func NewRazorpay(keyID, keySecret, webhookSecret string, guards *resilience.Registry) *Razorpay {
	return &Razorpay{
		keyID:         keyID,
		keySecret:     keySecret,
		webhookSecret: webhookSecret,
		client: &http.Client{
			// A hard ceiling below the Guard's per-attempt timeout, so a hung
			// connection cannot outlive its budget.
			Timeout: 15 * time.Second,
		},
		guard: guards.Register(resilience.NewGuard(resilience.GuardConfig{
			Name:          "razorpay",
			Timeout:       10 * time.Second,
			MaxConcurrent: 20,
			// DisableRetry: creating a payment is NOT idempotent from our side
			// unless the provider honours the idempotency key, and a retried
			// charge is a double charge. Retries are enabled per-operation below
			// only where they are provably safe.
			DisableRetry: true,
		})),
	}
}

func (r *Razorpay) Name() string { return "razorpay" }

// CreateIntent opens a Razorpay order.
//
// Razorpay's model is: create an "order" server-side, hand its id to the
// checkout SDK, the customer pays, then a webhook tells us what happened. We
// never learn the outcome from the client.
func (r *Razorpay) CreateIntent(ctx context.Context, req domain.IntentRequest) (domain.IntentResponse, error) {
	body := map[string]any{
		// Razorpay takes the smallest currency unit, which is exactly what we
		// store. No conversion, no rounding, no float anywhere.
		"amount":   req.Amount.Minor(),
		"currency": string(req.Amount.Currency()),
		"receipt":  req.OrderNumber,
		"notes": map[string]string{
			"order_id": req.OrderID.String(),
			"user_id":  req.UserID.String(),
		},
	}

	var raw []byte
	err := r.guard.Call(ctx, "create_order", func(ctx context.Context) error {
		var callErr error
		raw, callErr = r.post(ctx, "/orders", body, req.IdempotencyKey)
		return callErr
	})
	if err != nil {
		return domain.IntentResponse{}, err
	}

	var parsed struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return domain.IntentResponse{}, apperr.Internal(
			fmt.Errorf("razorpay: decode create order response: %w", err))
	}

	return domain.IntentResponse{
		ProviderRef:     parsed.ID,
		ProviderOrderID: parsed.ID,
		// For Razorpay the "client secret" is just the order id plus the public
		// key; there is no separate secret. The frontend SDK needs both.
		ClientSecret:   parsed.ID,
		Status:         domain.StatusCreated,
		RequiresAction: true,
		RawPayload:     raw,
	}, nil
}

// Capture takes an authorized payment.
//
// Retry IS safe here, unlike CreateIntent: capturing an already-captured payment
// returns the same result rather than taking money twice.
func (r *Razorpay) Capture(ctx context.Context, providerRef string, amount money.Amount) error {
	body := map[string]any{
		"amount":   amount.Minor(),
		"currency": string(amount.Currency()),
	}
	return r.guard.Call(ctx, "capture", func(ctx context.Context) error {
		_, err := r.post(ctx, "/payments/"+providerRef+"/capture", body, "")
		return err
	})
}

func (r *Razorpay) Refund(ctx context.Context, req domain.RefundRequest) (domain.RefundResponse, error) {
	body := map[string]any{
		"amount": req.Amount.Minor(),
		"notes":  map[string]string{"reason": req.Reason},
	}

	var raw []byte
	err := r.guard.Call(ctx, "refund", func(ctx context.Context) error {
		var callErr error
		// The idempotency key is what makes a retried refund safe. Without it a
		// network blip plus a retry pays the customer twice.
		raw, callErr = r.post(ctx, "/payments/"+req.ProviderRef+"/refund", body, req.IdempotencyKey)
		return callErr
	})
	if err != nil {
		return domain.RefundResponse{}, err
	}

	var parsed struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return domain.RefundResponse{}, apperr.Internal(
			fmt.Errorf("razorpay: decode refund response: %w", err))
	}

	status := domain.RefundProcessing
	if parsed.Status == "processed" {
		status = domain.RefundCompleted
	}
	return domain.RefundResponse{ProviderRef: parsed.ID, Status: status, RawPayload: raw}, nil
}

// VerifyWebhook authenticates an inbound event.
//
// THIS FUNCTION IS THE SECURITY BOUNDARY OF THE WHOLE PAYMENT MODULE.
//
// Without it, anyone who can reach the webhook URL can POST
// {"event":"payment.captured","order_id":"..."} and mark any order paid. The URL
// is public by necessity — the gateway has to reach it — so the signature is the
// only thing standing between us and free food.
//
// Two details that are easy to get wrong and both fatal:
//
//  1. The HMAC is computed over the RAW BYTES as received. Parsing and
//     re-serialising changes key order and whitespace, so the digest will never
//     match. That is why this takes []byte, not a struct.
//  2. The comparison MUST be constant-time. A byte-by-byte compare leaks how
//     many leading bytes were correct, which is enough to forge a signature
//     given enough attempts.
func (r *Razorpay) VerifyWebhook(rawBody []byte, signature string) (domain.WebhookPayload, error) {
	if r.webhookSecret == "" {
		// Fail closed. Treating "no secret configured" as "skip verification"
		// would silently disable the check in exactly the environment where
		// someone forgot to set it.
		return domain.WebhookPayload{}, apperr.Internal(
			fmt.Errorf("razorpay: webhook secret is not configured"))
	}
	if signature == "" {
		return domain.WebhookPayload{}, apperr.Unauthorized("MISSING_SIGNATURE",
			"Webhook signature is missing.")
	}

	mac := hmac.New(sha256.New, []byte(r.webhookSecret))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))

	// hmac.Equal, never ==. Constant-time by contract.
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return domain.WebhookPayload{}, apperr.Unauthorized("INVALID_SIGNATURE",
			"Webhook signature does not match.")
	}

	// Only now is it safe to look at the contents.
	var envelope struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID               string `json:"id"`
					OrderID          string `json:"order_id"`
					Status           string `json:"status"`
					Amount           int64  `json:"amount"`
					Currency         string `json:"currency"`
					ErrorCode        string `json:"error_code"`
					ErrorDescription string `json:"error_description"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
		// Razorpay does not send a stable event id in every payload, so the
		// payment id plus the event name is used as the dedupe key. It is stable
		// per state transition, which is what matters.
		CreatedAt int64 `json:"created_at"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return domain.WebhookPayload{}, apperr.Invalid("MALFORMED_WEBHOOK",
			"Webhook body is not valid JSON.").WithCause(err)
	}

	entity := envelope.Payload.Payment.Entity

	return domain.WebhookPayload{
		EventID:       entity.ID + ":" + envelope.Event,
		EventType:     envelope.Event,
		ProviderRef:   entity.OrderID,
		Status:        mapRazorpayStatus(envelope.Event, entity.Status),
		Amount:        money.FromMinor(entity.Amount, money.Currency(entity.Currency)),
		FailureCode:   entity.ErrorCode,
		FailureReason: entity.ErrorDescription,
		RawPayload:    rawBody,
	}, nil
}

// mapRazorpayStatus translates provider vocabulary into ours.
//
// Doing this here — and only here — is what lets the application layer switch on
// domain.Status without knowing Razorpay exists. An unrecognised event returns
// empty, meaning "record it, do not act on it", which is the safe default for a
// provider adding new event types.
func mapRazorpayStatus(event, entityStatus string) domain.Status {
	switch event {
	case "payment.captured":
		return domain.StatusCaptured
	case "payment.authorized":
		return domain.StatusAuthorized
	case "payment.failed":
		return domain.StatusFailed
	case "refund.processed":
		return domain.StatusRefunded
	}

	switch entityStatus {
	case "captured":
		return domain.StatusCaptured
	case "authorized":
		return domain.StatusAuthorized
	case "failed":
		return domain.StatusFailed
	}
	return ""
}

// post performs an authenticated request against the Razorpay API.
func (r *Razorpay) post(ctx context.Context, path string, body any, idempotencyKey string) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("razorpay: marshal request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, razorpayBaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("razorpay: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(r.keyID, r.keySecret)
	if idempotencyKey != "" {
		req.Header.Set("X-Razorpay-Idempotency-Key", idempotencyKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		// Returned raw so the Guard can classify it as a timeout or a transport
		// failure and count it toward the breaker.
		return nil, fmt.Errorf("razorpay: %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := readAllLimited(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("razorpay: read response: %w", err)
	}

	// 4xx is our mistake and will never succeed on retry, so it must NOT count
	// toward the circuit breaker or be retried. 5xx is theirs and both apply.
	switch {
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("razorpay: %s: provider error %d: %s", path, resp.StatusCode, truncate(raw, 200))
	case resp.StatusCode >= 400:
		return nil, apperr.Invalid("PAYMENT_REJECTED",
			"The payment provider rejected this request.").
			WithCause(fmt.Errorf("razorpay %d: %s", resp.StatusCode, truncate(raw, 200)))
	}
	return raw, nil
}

var _ domain.Gateway = (*Razorpay)(nil)
