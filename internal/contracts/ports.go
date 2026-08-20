package contracts

import (
	"context"

	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/id"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/money"
)

// The interfaces below are the synchronous half of the inter-module API. Events
// cover "tell someone something happened"; these cover "I need an answer before
// I can proceed" -- checkout genuinely cannot continue without knowing whether
// stock exists.
//
// Each is defined from the CONSUMER's point of view and kept as narrow as the
// consumer's actual need. `ordering` does not get a full CatalogService with
// forty methods; it gets PriceQuoter with one. Narrow interfaces are what keep
// a module's real dependency surface visible and its tests easy to fake.

// ---------------------------------------------------------------------------
// Consumed by: ordering. Implemented by: catalog.
// ---------------------------------------------------------------------------

// PricedItem is a validated, priced line resolved from the catalog at checkout.
//
// Prices are resolved server-side and never trusted from the client. A client
// that could send its own price could order a biryani for ₹1.
type PricedItem struct {
	MenuItemID   id.ID
	Name         string
	UnitPrice    money.Amount
	TaxBps       int64 // GST in basis points; 5% = 500
	IsAvailable  bool
	KitchenID    id.ID
	PackagingFee money.Amount
	Modifiers    []PricedModifier
}

type PricedModifier struct {
	ModifierID id.ID
	Name       string
	UnitPrice  money.Amount
}

type PriceQuoteRequest struct {
	KitchenID   id.ID
	MenuItemIDs []id.ID
	ModifierIDs []id.ID
}

// PriceQuoter resolves authoritative prices and availability for a cart.
type PriceQuoter interface {
	QuotePrices(ctx context.Context, req PriceQuoteRequest) ([]PricedItem, error)
}

// KitchenAvailability answers "can this kitchen accept an order right now" --
// open hours, manual pause, and whether it delivers to the given point.
type KitchenAvailability interface {
	IsAcceptingOrders(ctx context.Context, kitchenID id.ID) (bool, string, error)
	ServesLocation(ctx context.Context, kitchenID id.ID, lat, lng float64) (bool, error)
}

// ---------------------------------------------------------------------------
// Consumed by: ordering. Implemented by: inventory.
// ---------------------------------------------------------------------------

type ReservationLine struct {
	MenuItemID id.ID
	Quantity   int32
}

// StockReserver holds stock during checkout.
//
// Reserve/Release/Consume is a two-phase commit against inventory. Without it,
// two customers can buy the last portion of biryani simultaneously and one gets
// a cancellation. Reserve is called inside the order transaction; Consume when
// the kitchen starts cooking; Release on cancellation or reservation expiry.
type StockReserver interface {
	// Reserve fails with a conflict if any line cannot be satisfied. It must be
	// called inside the caller's transaction so a failed order leaves no
	// orphaned reservation.
	Reserve(ctx context.Context, orderID, kitchenID id.ID, lines []ReservationLine) error
	Release(ctx context.Context, orderID id.ID) error
	Consume(ctx context.Context, orderID id.ID) error
}

// ---------------------------------------------------------------------------
// Consumed by: ordering. Implemented by: promotion.
// ---------------------------------------------------------------------------

type DiscountRequest struct {
	UserID       id.ID
	KitchenID    id.ID
	CouponCode   string
	Subtotal     money.Amount
	ItemIDs      []id.ID
	IsFirstOrder bool
}

type DiscountResult struct {
	Applied        bool
	CouponID       id.ID
	Code           string
	Description    string
	DiscountAmount money.Amount
	RejectReason   string // why it did not apply, shown to the customer
}

// DiscountCalculator validates a coupon and computes the discount. Redeem is
// separate and transactional, because "is this valid" and "consume one of its
// 100 uses" must not be the same call -- a user browsing offers should not burn
// redemptions.
type DiscountCalculator interface {
	Calculate(ctx context.Context, req DiscountRequest) (DiscountResult, error)
	Redeem(ctx context.Context, couponID, userID, orderID id.ID) error
	ReleaseRedemption(ctx context.Context, orderID id.ID) error
}

// ---------------------------------------------------------------------------
// Consumed by: ordering. Implemented by: payment.
// ---------------------------------------------------------------------------

type PaymentIntentRequest struct {
	OrderID        id.ID
	UserID         id.ID
	Amount         money.Amount
	Method         string // "upi" | "card" | "netbanking" | "wallet" | "cod"
	IdempotencyKey string
}

type PaymentIntent struct {
	PaymentID   id.ID
	ProviderRef string
	// ClientSecret is handed to the frontend SDK to complete the payment. It is
	// never logged.
	ClientSecret   string
	Status         string
	RequiresAction bool
}

// PaymentInitiator creates a payment intent for an order.
type PaymentInitiator interface {
	CreateIntent(ctx context.Context, req PaymentIntentRequest) (PaymentIntent, error)
	Refund(ctx context.Context, orderID id.ID, amount money.Amount, reason string) error
}

// ---------------------------------------------------------------------------
// Consumed by: ordering, catalog. Implemented by: identity.
// ---------------------------------------------------------------------------

// Address is the delivery destination snapshot ordering needs. Lat/lng are
// required because delivery-radius checks and rider routing both need the point,
// not the text.
type Address struct {
	ID        id.ID
	UserID    id.ID
	Label     string
	Line1     string
	Line2     string
	Landmark  string
	City      string
	State     string
	Pincode   string
	Latitude  float64
	Longitude float64
}

type UserSummary struct {
	UserID   id.ID
	FullName string
	Phone    string
	Email    string
	IsActive bool
}

// UserDirectory is a read-only lookup into identity.
type UserDirectory interface {
	GetUser(ctx context.Context, userID id.ID) (UserSummary, error)
	GetAddress(ctx context.Context, userID, addressID id.ID) (Address, error)
}

// ---------------------------------------------------------------------------
// Consumed by: delivery, ordering. Implemented by: delivery.
// ---------------------------------------------------------------------------

// RiderDispatcher requests a rider for a ready order. Implementations may assign
// synchronously or queue for a matching pass.
type RiderDispatcher interface {
	RequestRider(ctx context.Context, orderID, kitchenID id.ID, dropLat, dropLng float64) error
}

// ---------------------------------------------------------------------------
// Consumed by: every module. Implemented by: notification.
// ---------------------------------------------------------------------------

type NotificationChannel string

const (
	ChannelPush     NotificationChannel = "push"
	ChannelSMS      NotificationChannel = "sms"
	ChannelEmail    NotificationChannel = "email"
	ChannelWhatsApp NotificationChannel = "whatsapp"
)

type NotificationRequest struct {
	UserID   id.ID
	Channels []NotificationChannel
	// TemplateID selects a stored template. Message bodies are not passed as raw
	// strings so copy can be changed and localised without a deploy.
	TemplateID string
	Params     map[string]string
	// DedupeKey makes sending idempotent, since events are delivered at least
	// once and a customer must not get the same SMS twice.
	DedupeKey string
}

type Notifier interface {
	Send(ctx context.Context, req NotificationRequest) error
}
