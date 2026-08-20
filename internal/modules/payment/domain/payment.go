// Package domain holds the payment module's entities and ports.
//
// Payment is the module where a bug costs money directly, so three rules run
// through all of it:
//
//  1. The GATEWAY is the only authority on whether money moved. Never the client,
//     never our own optimism.
//  2. Every webhook is signature-verified before a single field is trusted.
//  3. Everything is idempotent. Gateways retry aggressively, and a duplicate
//     capture is a double charge.
package domain

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Status is the payment lifecycle.
type Status string

const (
	// StatusCreated means an intent exists but the customer has not acted.
	StatusCreated Status = "created"
	// StatusPending means the customer is mid-flow (UPI collect sent, 3-D Secure
	// challenge open). Can sit here for minutes.
	StatusPending Status = "pending"
	// StatusAuthorized means funds are held but not taken. Some methods settle
	// straight to captured and skip this.
	StatusAuthorized Status = "authorized"
	// StatusCaptured means the money is ours. This is the only status that may
	// promote an order out of pending_payment.
	StatusCaptured          Status = "captured"
	StatusFailed            Status = "failed"
	StatusRefunded          Status = "refunded"
	StatusPartiallyRefunded Status = "partially_refunded"
)

// IsTerminal reports whether the gateway will send no further updates.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusCaptured, StatusFailed, StatusRefunded:
		return true
	}
	return false
}

// IsSettled reports whether we actually hold the money.
func (s Status) IsSettled() bool {
	return s == StatusCaptured || s == StatusPartiallyRefunded
}

// transitions is the payment state machine.
//
// Deliberately permissive in one direction and strict in the other: a gateway
// can legitimately jump created -> captured (UPI intent settles instantly), but
// nothing may leave a terminal state except into a refund. Allowing captured ->
// failed would let a late, out-of-order webhook un-capture money we hold.
var transitions = map[Status][]Status{
	StatusCreated:           {StatusPending, StatusAuthorized, StatusCaptured, StatusFailed},
	StatusPending:           {StatusAuthorized, StatusCaptured, StatusFailed},
	StatusAuthorized:        {StatusCaptured, StatusFailed},
	StatusCaptured:          {StatusRefunded, StatusPartiallyRefunded},
	StatusPartiallyRefunded: {StatusRefunded, StatusPartiallyRefunded},
	StatusFailed:            {},
	StatusRefunded:          {},
}

func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Payment is one attempt to collect for an order.
type Payment struct {
	ID      id.ID
	OrderID id.ID
	UserID  id.ID
	Amount  money.Amount
	Status  Status
	Method  string

	Provider string
	// ProviderRef is the gateway's own id. Unique, so a webhook arriving twice
	// updates the existing row instead of inserting a second payment.
	ProviderRef     string
	ProviderOrderID string
	// ProviderPayload is the raw gateway response, kept verbatim for
	// reconciliation and disputes. Never parsed for business logic -- the
	// provider can change its shape without warning.
	ProviderPayload []byte

	FailureCode   string
	FailureReason string

	IdempotencyKey string

	AuthorizedAt *time.Time
	CapturedAt   *time.Time
	FailedAt     *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TransitionTo advances the payment, stamping the matching timestamp.
//
// Returns a sentinel-checkable conflict for a same-status transition so webhook
// processing can treat redelivery as success rather than failure. That
// distinction matters: returning an error would make the gateway retry forever.
func (p *Payment) TransitionTo(next Status, now time.Time) error {
	if p.Status == next {
		return ErrAlreadyInStatus
	}
	if !p.Status.CanTransitionTo(next) {
		return apperr.Conflict("INVALID_PAYMENT_TRANSITION",
			"A payment that is "+string(p.Status)+" cannot become "+string(next)+".")
	}

	p.Status = next
	p.UpdatedAt = now

	switch next {
	case StatusAuthorized:
		p.AuthorizedAt = &now
	case StatusCaptured:
		p.CapturedAt = &now
	case StatusFailed:
		p.FailedAt = &now
	}
	return nil
}

// ErrAlreadyInStatus signals a no-op transition, which for an at-least-once
// webhook is the expected case, not a failure.
var ErrAlreadyInStatus = apperr.Conflict("PAYMENT_ALREADY_IN_STATUS",
	"This payment is already in that state.")

// Refund is one refund against a payment.
//
// A separate entity rather than a column, because one payment can be refunded in
// parts (two of five items were wrong) and each part has its own gateway
// lifecycle.
type Refund struct {
	ID          id.ID
	PaymentID   id.ID
	OrderID     id.ID
	Amount      money.Amount
	Reason      string
	Status      RefundStatus
	ProviderRef string
	InitiatedBy *id.ID
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type RefundStatus string

const (
	RefundInitiated  RefundStatus = "initiated"
	RefundProcessing RefundStatus = "processing"
	RefundCompleted  RefundStatus = "completed"
	RefundFailed     RefundStatus = "failed"
)

// WebhookEvent is an inbound gateway callback, persisted before processing.
//
// Stored first for two reasons: the unique (provider, provider_event_id) index
// makes duplicate delivery a no-op rather than a double capture, and if our
// processing has a bug the raw event can be replayed once it is fixed.
type WebhookEvent struct {
	ID              id.ID
	Provider        string
	ProviderEventID string
	EventType       string
	Payload         []byte
	SignatureValid  bool
	ProcessedAt     *time.Time
	ProcessError    string
	ReceivedAt      time.Time
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

type PaymentRepository interface {
	Create(ctx context.Context, p *Payment) error
	Update(ctx context.Context, p *Payment) error
	FindByID(ctx context.Context, paymentID id.ID) (*Payment, error)
	// FindByProviderRef resolves a webhook to our payment row.
	FindByProviderRef(ctx context.Context, provider, ref string) (*Payment, error)
	FindByOrderID(ctx context.Context, orderID id.ID) (*Payment, error)
	// FindByIdempotencyKey makes intent creation safe to retry.
	FindByIdempotencyKey(ctx context.Context, key string) (*Payment, error)
	// FindByIDForUpdate row-locks a payment, so two concurrent webhooks for the
	// same payment serialise instead of both applying.
	FindByIDForUpdate(ctx context.Context, paymentID id.ID) (*Payment, error)

	CreateRefund(ctx context.Context, r *Refund) error
	UpdateRefund(ctx context.Context, r *Refund) error
	ListRefunds(ctx context.Context, paymentID id.ID) ([]*Refund, error)
	// TotalRefunded backs the "cannot refund more than was captured" check.
	TotalRefunded(ctx context.Context, paymentID id.ID) (money.Amount, error)
}

type WebhookRepository interface {
	// Store persists an event, reporting false if this provider_event_id was
	// already seen. That boolean IS the idempotency guarantee.
	Store(ctx context.Context, e *WebhookEvent) (isNew bool, err error)
	MarkProcessed(ctx context.Context, eventID id.ID, processErr string) error
}
