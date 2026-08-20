// Package provider holds third-party logistics adapters.
package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Manual is the default provider: the kitchen arranges delivery itself.
//
// This is not a placeholder — it is what a single cloud kitchen actually does on
// day one. Two delivery boys, a phone, and a WhatsApp group. Modelling that as a
// first-class provider means the system is usable immediately, and switching to a
// 3PL later is a config change rather than a migration.
//
// It creates a delivery record so the order lifecycle and tracking still work,
// and staff drive the status transitions by hand from the kitchen app.
type Manual struct {
	// webhookSecret lets a local script or the staff app sign status updates
	// through the same verified path a real provider would use, so the signature
	// code is exercised rather than bypassed.
	webhookSecret string
	// perDeliveryCost is what the kitchen reckons its own delivery costs. Recorded
	// so margin per order is knowable rather than guessed.
	perDeliveryCost money.Amount
}

func NewManual(webhookSecret string, perDeliveryCostMinor int64) *Manual {
	if webhookSecret == "" {
		webhookSecret = "local-manual-delivery-secret"
	}
	return &Manual{
		webhookSecret:   webhookSecret,
		perDeliveryCost: money.FromMinor(perDeliveryCostMinor, money.INR),
	}
}

func (m *Manual) Name() string { return "manual" }

// CreateJob records the intent to deliver. No network call: there is no provider
// to call.
//
// Returns no ETA, because an honest "we do not know yet" is better than a
// fabricated number the customer will hold us to.
func (m *Manual) CreateJob(_ context.Context, req domain.JobRequest) (domain.JobResponse, error) {
	return domain.JobResponse{
		ProviderRef: "manual_" + id.HumanCode(10),
		Cost:        m.perDeliveryCost,
		RawPayload:  []byte(`{"provider":"manual","note":"kitchen arranges its own rider"}`),
	}, nil
}

// Cancel is a no-op: there is no external job to withdraw.
func (m *Manual) Cancel(context.Context, string, string) error { return nil }

// VerifyWebhook uses the same HMAC-SHA256 scheme as a real provider.
//
// Deliberately not skipped for the manual provider. If manual updates went
// through an unverified path, the endpoint would be a way for anyone to mark any
// order delivered — and the signature code would be untested until the day a real
// 3PL is plugged in.
func (m *Manual) VerifyWebhook(rawBody []byte, signature string) (domain.WebhookPayload, error) {
	if signature == "" {
		return domain.WebhookPayload{}, apperr.Unauthorized("MISSING_SIGNATURE",
			"Delivery webhook signature is missing.")
	}

	mac := hmac.New(sha256.New, []byte(m.webhookSecret))
	mac.Write(rawBody)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(signature)) {
		return domain.WebhookPayload{}, apperr.Unauthorized("INVALID_SIGNATURE",
			"Delivery webhook signature does not match.")
	}

	var envelope struct {
		EventID       string `json:"event_id"`
		ProviderRef   string `json:"provider_ref"`
		Status        string `json:"status"`
		RiderName     string `json:"rider_name"`
		RiderPhone    string `json:"rider_phone"`
		DropETA       string `json:"drop_eta"`
		FailureReason string `json:"failure_reason"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return domain.WebhookPayload{}, apperr.Invalid("MALFORMED_WEBHOOK",
			"Delivery webhook body is not valid JSON.").WithCause(err)
	}

	payload := domain.WebhookPayload{
		EventID:       envelope.EventID,
		ProviderRef:   envelope.ProviderRef,
		Status:        mapStatus(envelope.Status),
		RiderName:     envelope.RiderName,
		RiderPhone:    envelope.RiderPhone,
		FailureReason: envelope.FailureReason,
		RawPayload:    rawBody,
	}
	if payload.EventID == "" {
		payload.EventID = envelope.ProviderRef + ":" + envelope.Status
	}
	if envelope.DropETA != "" {
		if parsed, err := time.Parse(time.RFC3339, envelope.DropETA); err == nil {
			payload.DropETA = &parsed
		}
	}
	return payload, nil
}

// mapStatus translates provider vocabulary into ours.
//
// Unknown statuses return empty, meaning "record it, do not act on it" — the safe
// default when a provider adds a status we have not seen.
func mapStatus(s string) domain.Status {
	switch s {
	case "assigned", "rider_assigned":
		return domain.StatusAssigned
	case "at_pickup", "reached_pickup":
		return domain.StatusAtPickup
	case "picked_up", "in_transit":
		return domain.StatusPickedUp
	case "delivered", "completed":
		return domain.StatusDelivered
	case "cancelled":
		return domain.StatusCancelled
	case "failed", "returned":
		return domain.StatusFailed
	}
	return ""
}

// WebhookSecret is exposed so local tooling can sign a test status update.
func (m *Manual) WebhookSecret() string { return m.webhookSecret }

var _ domain.Provider = (*Manual)(nil)
