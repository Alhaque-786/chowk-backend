package contracts

import (
	"context"

	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
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
	/*
		AssistedKitchenIDs lists outlets whose orders NOBODY ON THE MERCHANT SIDE
		WILL EVER SEE.

		An assisted vendor — the archetype is a cart selling fresh nariyal pani —
		has no dashboard, no app and no intention of acquiring either. His orders
		would sit at `placed` until they timed out, so our ops queue has to find
		them, and ordering cannot join `merchants` without breaking the module
		boundary.

		Returns ids rather than taking a filter because the set is SMALL and changes
		rarely (a merchant's operating mode is an onboarding decision, not a runtime
		one), which makes it cheap to cache and cheap to reason about. A filter
		pushed into catalog would put ordering's query semantics inside another
		module.
	*/
	AssistedKitchenIDs(ctx context.Context) ([]id.ID, error)
	// IsAssisted reports whether ONE outlet is operated on the vendor's behalf.
	// Checkout needs the single-kitchen form to choose a capture mode; scanning
	// the full list per checkout would grow with every vendor onboarded.
	IsAssisted(ctx context.Context, kitchenID id.ID) (bool, error)
	ServesLocation(ctx context.Context, kitchenID id.ID, lat, lng float64) (bool, error)
	// Name is needed so ordering can snapshot it onto the events it publishes.
	// Cheap because catalog caches the kitchen, and it keeps consumers from
	// having to call back for context they need to render a message.
	Name(ctx context.Context, kitchenID id.ID) (string, error)
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
// Consumed by: delivery. Implemented by: ordering.
// ---------------------------------------------------------------------------

// OrderOwnership answers whether an order belongs to a customer.
//
// It exists for one rule: a customer may see the delivery for their own order and
// nobody else's. Delivery owns the tracking view but cannot see orders, and
// ordering owns orders but not the tracking view — so the answer crosses a seam.
type OrderOwnership interface {
	/*
		IsOrderOwnedBy reports whether an order belongs to a customer.

		Exists because the delivery module could not answer it and was not asking.
		Its tracking endpoint checked only that the caller was signed in, so any
		authenticated user could read any order's delivery by id and get back the
		rider's NAME AND PERSONAL PHONE NUMBER. Order ids are UUIDv7 and therefore
		time-ordered, which makes them partially guessable rather than opaque.

		One narrow question rather than an order repository, for the same reason as
		CustomerHistory below: a port shaped like "give me the order" invites
		delivery to grow order logic, and a port shaped like this cannot.
	*/
	IsOrderOwnedBy(ctx context.Context, orderID, userID id.ID) (bool, error)
}

// ---------------------------------------------------------------------------
// Consumed by: promotion. Implemented by: ordering.
// ---------------------------------------------------------------------------

// CustomerHistory answers questions about a customer's past orders.
//
// It exists for one rule: a first-order-only coupon. Promotion owns that rule but
// cannot see orders, and ordering owns orders but not the rule — so the answer has
// to cross a seam, and this is the seam.
//
// Deliberately ONE narrow question rather than exposing an order repository. A
// port shaped like "give me the orders" invites promotion to grow order logic; a
// port shaped like "has this person ordered" cannot.
type CustomerHistory interface {
	// HasEverOrdered reports whether the customer has any order at all, in any
	// state. Cancelled and rejected orders COUNT: the offer is for somebody who
	// has never ordered before, and placing an order then cancelling it is not
	// the same as never having ordered.
	HasEverOrdered(ctx context.Context, userID id.ID) (bool, error)
}

// ---------------------------------------------------------------------------
// Consumed by: ordering. Implemented by: promotion.
// ---------------------------------------------------------------------------

type DiscountRequest struct {
	UserID     id.ID
	KitchenID  id.ID
	CouponCode string
	Subtotal   money.Amount
	ItemIDs    []id.ID
	/*
	  IsFirstOrder is resolved by promotion itself now, NOT supplied by the caller.

	  It was an input, and no caller ever set it — so it defaulted to false and
	  every first-order-only coupon was rejected for everybody, permanently. A
	  field that must be populated for correctness, in a struct where forgetting it
	  is silent and looks like a normal rejection, is the wrong shape.

	  Kept only so a caller that genuinely knows can still assert it; promotion
	  overrides false by asking CustomerHistory.
	*/
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

	// CaptureMode decides WHEN the money actually moves. See CaptureMode.
	CaptureMode CaptureMode
}

/*
CaptureMode separates authorising a payment from taking it.

CaptureImmediate is right for a restaurant: it has a dashboard, it accepts within
seconds, and the order is as good as confirmed by the time the customer's phone
buzzes.

CaptureOnConfirmation exists for assisted vendors, and it is the difference between
a workable business and an angry one. A cart selling fresh nariyal pani has no
inventory system — his stock is however many coconuts arrived this morning — so a
meaningful share of orders cannot be fulfilled and nobody knows until somebody
phones him. Taking the money first means refunding a stranger daily and being the
app that charges for juice it did not deliver.

Authorising instead places a HOLD. If the vendor can make it, we capture and the
customer sees one clean charge. If he cannot, we simply never capture: card
authorisations lapse on their own after a few days and the hold drops off, so the
release path needs no gateway call and no refund appears on the statement at all.
*/
type CaptureMode string

const (
	// CaptureImmediate is the zero value on purpose: a request that forgets to set
	// this behaves the way every existing caller already does.
	CaptureImmediate      CaptureMode = ""
	CaptureOnConfirmation CaptureMode = "on_confirmation"
)

type PaymentIntent struct {
	PaymentID   id.ID
	ProviderRef string
	// ClientSecret is handed to the frontend SDK to complete the payment. It is
	// never logged.
	ClientSecret string
	// Provider names the gateway that opened this intent, so the client knows
	// which SDK to hand ClientSecret to.
	//
	// Sent rather than assumed because the provider is expected to multiply --
	// Cashfree and PhonePe are plausible additions, and a marketplace may end up
	// choosing per merchant or failing over to a second gateway. A client that
	// hard-codes one provider silently breaks the day a second appears, and the
	// breakage is at the payment step of somebody's order.
	Provider       string
	Status         string
	RequiresAction bool
}

// PaymentInitiator creates a payment intent for an order.
type PaymentInitiator interface {
	CreateIntent(ctx context.Context, req PaymentIntentRequest) (PaymentIntent, error)
	Refund(ctx context.Context, orderID id.ID, amount money.Amount, reason string) error

	/*
		Capture takes funds that were only authorised, and is a no-op when there is
		nothing held — an order paid immediately, or one paid in cash.

		Called when a vendor commits to making the food. Idempotent by necessity:
		staff double-tap accept, and the gateway is not the place to find out.
	*/
	Capture(ctx context.Context, orderID id.ID) error
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
