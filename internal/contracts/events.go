// Package contracts is the published API between modules.
//
// This is the most important architectural boundary in the codebase. The rule:
//
//	A module may import `contracts` and `shared`. A module may NEVER import
//	another module's packages.
//
// So `ordering` cannot call into `inventory/app`. It depends on
// contracts.StockReserver, and the composition root injects the real
// implementation. That indirection is what makes extraction to a separate
// service a wiring change rather than a rewrite: swap the injected
// implementation for a gRPC client and `ordering` recompiles untouched.
//
// Everything in here should be treated as a versioned public API. Renaming an
// event or changing a payload field breaks consumers -- including, later,
// consumers you do not deploy.
package contracts

import (
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// Event names, grouped by publishing module. Format: "<aggregate>.<past tense>".
// Past tense matters -- an event is a fact that already happened, not a command
// asking someone to do something.
const (
	// identity
	EventUserRegistered eventbus.Name = "user.registered"
	EventUserVerified   eventbus.Name = "user.verified"
	EventUserSuspended  eventbus.Name = "user.suspended"
	EventAddressAdded   eventbus.Name = "user.address_added"

	// ordering -- the spine of the system
	EventOrderPlaced    eventbus.Name = "order.placed"
	EventOrderConfirmed eventbus.Name = "order.confirmed"
	EventOrderPreparing eventbus.Name = "order.preparing"
	EventOrderReady     eventbus.Name = "order.ready"
	EventOrderPickedUp  eventbus.Name = "order.picked_up"
	EventOrderDelivered eventbus.Name = "order.delivered"
	EventOrderCancelled eventbus.Name = "order.cancelled"
	EventOrderRefunded  eventbus.Name = "order.refunded"

	// payment
	EventPaymentAuthorized eventbus.Name = "payment.authorized"
	EventPaymentCaptured   eventbus.Name = "payment.captured"
	EventPaymentFailed     eventbus.Name = "payment.failed"
	EventRefundInitiated   eventbus.Name = "refund.initiated"
	EventRefundCompleted   eventbus.Name = "refund.completed"

	// inventory
	EventStockReserved eventbus.Name = "stock.reserved"
	EventStockReleased eventbus.Name = "stock.released"
	EventStockConsumed eventbus.Name = "stock.consumed"
	EventStockLow      eventbus.Name = "stock.low"
	EventItemSoldOut   eventbus.Name = "menu_item.sold_out"

	// catalog
	EventMenuItemCreated      eventbus.Name = "menu_item.created"
	EventMenuItemPriceChanged eventbus.Name = "menu_item.price_changed"
	EventKitchenOpened        eventbus.Name = "kitchen.opened"
	EventKitchenClosed        eventbus.Name = "kitchen.closed"

	// delivery
	EventRiderAssigned        eventbus.Name = "rider.assigned"
	EventRiderLocationUpdated eventbus.Name = "rider.location_updated"

	// review
	EventReviewSubmitted eventbus.Name = "review.submitted"
)

// ---------------------------------------------------------------------------
// Event payloads
//
// Payloads are self-contained snapshots, not just IDs. A consumer must not have
// to call back to the publisher to understand an event -- that reintroduces the
// coupling the event bus exists to remove, and it breaks outright once the two
// modules are separate services with independent availability.
// ---------------------------------------------------------------------------

type UserRegisteredPayload struct {
	UserID       id.ID     `json:"user_id"`
	Phone        string    `json:"phone"`
	Email        string    `json:"email,omitempty"`
	FullName     string    `json:"full_name"`
	ReferredBy   string    `json:"referred_by,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
}

type OrderLineSnapshot struct {
	MenuItemID     id.ID  `json:"menu_item_id"`
	Name           string `json:"name"`
	Quantity       int32  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	TotalMinor     int64  `json:"total_minor"`
}

type OrderPlacedPayload struct {
	OrderID           id.ID               `json:"order_id"`
	OrderNumber       string              `json:"order_number"`
	UserID            id.ID               `json:"user_id"`
	KitchenID         id.ID               `json:"kitchen_id"`
	Lines             []OrderLineSnapshot `json:"lines"`
	SubtotalMinor     int64               `json:"subtotal_minor"`
	TotalMinor        int64               `json:"total_minor"`
	Currency          string              `json:"currency"`
	PaymentMethod     string              `json:"payment_method"`
	DeliveryAddressID id.ID               `json:"delivery_address_id"`
	PlacedAt          time.Time           `json:"placed_at"`
}

// OrderStatusChangedPayload backs every order.* transition event. One shape for
// all of them means a consumer that only cares about "the order moved" writes a
// single handler.
type OrderStatusChangedPayload struct {
	OrderID     id.ID     `json:"order_id"`
	OrderNumber string    `json:"order_number"`
	UserID      id.ID     `json:"user_id"`
	KitchenID   id.ID     `json:"kitchen_id"`
	FromStatus  string    `json:"from_status"`
	ToStatus    string    `json:"to_status"`
	Reason      string    `json:"reason,omitempty"`
	ChangedBy   id.ID     `json:"changed_by,omitempty"`
	ChangedAt   time.Time `json:"changed_at"`
}

type PaymentPayload struct {
	PaymentID     id.ID     `json:"payment_id"`
	OrderID       id.ID     `json:"order_id"`
	UserID        id.ID     `json:"user_id"`
	AmountMinor   int64     `json:"amount_minor"`
	Currency      string    `json:"currency"`
	Provider      string    `json:"provider"`
	ProviderRef   string    `json:"provider_ref"`
	Method        string    `json:"method"`
	FailureReason string    `json:"failure_reason,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type StockLowPayload struct {
	KitchenID    id.ID   `json:"kitchen_id"`
	IngredientID id.ID   `json:"ingredient_id"`
	Name         string  `json:"name"`
	OnHand       float64 `json:"on_hand"`
	ReorderLevel float64 `json:"reorder_level"`
	Unit         string  `json:"unit"`
}

type ItemSoldOutPayload struct {
	KitchenID  id.ID  `json:"kitchen_id"`
	MenuItemID id.ID  `json:"menu_item_id"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
}

type RiderAssignedPayload struct {
	OrderID    id.ID     `json:"order_id"`
	RiderID    id.ID     `json:"rider_id"`
	RiderName  string    `json:"rider_name"`
	RiderPhone string    `json:"rider_phone"`
	AssignedAt time.Time `json:"assigned_at"`
	ETAMinutes int32     `json:"eta_minutes"`
}
