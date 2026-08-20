// Package domain holds the delivery module's entities and ports.
//
// DELIBERATELY SMALL. There is no rider fleet, no rider app, no assignment
// algorithm, no shift management.
//
// That is an economics decision, not a shortcut. At ~10% commission on a ₹500
// order the contribution is roughly ₹32 IF delivery is passed through to the
// customer, and roughly MINUS ₹13 if it is absorbed. A rider fleet adds fixed
// cost, hiring, attrition and an entire second mobile app, all to compete on the
// one dimension the incumbents have already subsidised into the ground.
//
// So delivery is a thin adapter over a third-party logistics provider: per-drop
// cost, zero fixed cost, no fleet to manage. See docs/ALGORITHMS.md §2 for the
// assignment algorithms this would need if that ever changes.
package domain

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Status is the delivery lifecycle.
type Status string

const (
	// StatusRequested means a 3PL job has been created but no rider is assigned.
	StatusRequested Status = "requested"
	StatusAssigned  Status = "assigned"
	// StatusAtPickup means the rider has reached the kitchen.
	StatusAtPickup  Status = "at_pickup"
	StatusPickedUp  Status = "picked_up"
	StatusDelivered Status = "delivered"
	StatusCancelled Status = "cancelled"
	// StatusFailed covers a genuine delivery failure: nobody home, wrong address,
	// rider accident. Distinct from cancelled because the food is gone and the
	// money question is different.
	StatusFailed Status = "failed"
)

func (s Status) IsTerminal() bool {
	switch s {
	case StatusDelivered, StatusCancelled, StatusFailed:
		return true
	}
	return false
}

// Mode is how an order actually gets to the customer.
type Mode string

const (
	// ModeThirdParty is the default: a logistics provider handles it.
	ModeThirdParty Mode = "third_party"
	// ModeSelf is the kitchen's own delivery boy, arranged by phone. This is what
	// most single-kitchen operations actually do on day one, and pretending
	// otherwise would make the system unusable for them.
	ModeSelf Mode = "self"
	// ModePickup is the customer collecting. The healthiest option economically —
	// no delivery cost at all — and worth offering prominently.
	ModePickup Mode = "pickup"
)

// Delivery is one attempt to move an order from kitchen to customer.
type Delivery struct {
	ID        id.ID
	OrderID   id.ID
	KitchenID id.ID
	Mode      Mode
	Status    Status

	Provider string
	// ProviderRef is the 3PL's job id, for tracking and reconciliation.
	ProviderRef string
	// TrackingURL is the provider's own live-tracking page. Reusing theirs rather
	// than building a map view is most of why this module is small.
	TrackingURL string

	RiderName  string
	RiderPhone string

	// Cost is what the provider charges us. The customer-facing delivery FEE is
	// set by ordering; the difference between them is margin or subsidy, and
	// keeping both visible is how that stays deliberate rather than accidental.
	Cost money.Amount

	PickupETA     *time.Time
	DropETA       *time.Time
	AssignedAt    *time.Time
	PickedUpAt    *time.Time
	DeliveredAt   *time.Time
	FailureReason string

	CreatedAt time.Time
	UpdatedAt time.Time
}

var transitions = map[Status][]Status{
	StatusRequested: {StatusAssigned, StatusCancelled, StatusFailed},
	StatusAssigned:  {StatusAtPickup, StatusPickedUp, StatusCancelled, StatusFailed},
	StatusAtPickup:  {StatusPickedUp, StatusCancelled, StatusFailed},
	// No cancel once the food is with the rider: it is gone. A failed delivery is
	// resolved as StatusFailed plus a refund decision.
	StatusPickedUp:  {StatusDelivered, StatusFailed},
	StatusDelivered: {},
	StatusCancelled: {},
	StatusFailed:    {},
}

func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// TransitionTo advances the delivery, stamping the matching timestamp.
//
// Returns ErrAlreadyInStatus for a no-op so 3PL webhook processing can treat
// redelivery as success. Providers retry, and erroring would make them retry
// forever.
func (d *Delivery) TransitionTo(next Status, now time.Time) error {
	if d.Status == next {
		return ErrAlreadyInStatus
	}
	if !d.Status.CanTransitionTo(next) {
		return apperr.Conflict("INVALID_DELIVERY_TRANSITION",
			"A delivery that is "+string(d.Status)+" cannot become "+string(next)+".")
	}

	d.Status = next
	d.UpdatedAt = now

	switch next {
	case StatusAssigned:
		d.AssignedAt = &now
	case StatusPickedUp:
		d.PickedUpAt = &now
	case StatusDelivered:
		d.DeliveredAt = &now
	}
	return nil
}

var ErrAlreadyInStatus = apperr.Conflict("DELIVERY_ALREADY_IN_STATUS",
	"This delivery is already in that state.")

// ---------------------------------------------------------------------------
// Provider port
// ---------------------------------------------------------------------------

// Provider is the port every 3PL implements.
//
// The interface is small on purpose: create a job, cancel a job, verify a status
// webhook. Anything more — rider selection, routing, batching — is the
// provider's problem, and paying them to solve it is cheaper than solving it
// ourselves at this scale.
//
// Shadowfax, Porter, Pidge, Borzo and similar all fit this shape, so switching
// or running two in parallel for coverage is a new adapter rather than a rewrite.
type Provider interface {
	Name() string

	// CreateJob books a pickup. Returns immediately with a job reference; rider
	// assignment arrives later by webhook.
	CreateJob(ctx context.Context, req JobRequest) (JobResponse, error)

	// Cancel withdraws a job. Providers typically charge if a rider is already en
	// route, which is why ordering only cancels before `ready`.
	Cancel(ctx context.Context, providerRef, reason string) error

	// VerifyWebhook authenticates a status callback over the RAW body, for the
	// same reason as payment: the HMAC covers the exact bytes sent.
	VerifyWebhook(rawBody []byte, signature string) (WebhookPayload, error)
}

type JobRequest struct {
	OrderID     id.ID
	OrderNumber string

	PickupName    string
	PickupPhone   string
	PickupAddress string
	PickupLat     float64
	PickupLng     float64

	DropName    string
	DropPhone   string
	DropAddress string
	DropLat     float64
	DropLng     float64

	// OrderValue lets the provider insure the consignment and decide whether COD
	// collection is allowed.
	OrderValue money.Amount
	IsCOD      bool
	// ReadyAt tells the provider when to arrive, so a rider is not left waiting
	// at the kitchen — waiting time is usually billable.
	ReadyAt time.Time
}

type JobResponse struct {
	ProviderRef string
	TrackingURL string
	Cost        money.Amount
	PickupETA   *time.Time
	DropETA     *time.Time
	RawPayload  []byte
}

// WebhookPayload is a provider status update normalised into our vocabulary.
type WebhookPayload struct {
	EventID       string
	ProviderRef   string
	Status        Status
	RiderName     string
	RiderPhone    string
	DropETA       *time.Time
	FailureReason string
	RawPayload    []byte
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

type DeliveryRepository interface {
	Create(ctx context.Context, d *Delivery) error
	Update(ctx context.Context, d *Delivery) error
	FindByID(ctx context.Context, deliveryID id.ID) (*Delivery, error)
	FindByOrderID(ctx context.Context, orderID id.ID) (*Delivery, error)
	FindByProviderRef(ctx context.Context, provider, ref string) (*Delivery, error)
	// FindByOrderIDForUpdate row-locks, so two concurrent status webhooks
	// serialise rather than both applying.
	FindByOrderIDForUpdate(ctx context.Context, orderID id.ID) (*Delivery, error)
	ListActiveForKitchen(ctx context.Context, kitchenID id.ID) ([]*Delivery, error)
}
