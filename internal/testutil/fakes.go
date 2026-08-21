package testutil

import (
	"context"
	"sync"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Fakes for the cross-module contracts.
//
// These are hand-written rather than generated, and controllable rather than
// merely stubbed: a checkout test needs to force "this item is unavailable" or
// "the gateway declined" without standing up catalog and payment. Each fake also
// RECORDS its calls, because half of what a saga test asserts is "was the
// compensation actually invoked".

// FakePriceQuoter returns configured prices.
type FakePriceQuoter struct {
	Items map[id.ID]contracts.PricedItem
	// Err, if set, is returned instead — for testing the failure path.
	Err error
	// Calls counts invocations, so a test can assert prices were resolved exactly
	// once rather than per line.
	Calls int
}

func NewFakePriceQuoter() *FakePriceQuoter {
	return &FakePriceQuoter{Items: map[id.ID]contracts.PricedItem{}}
}

// AddItem registers an available item at the given rupee price.
func (f *FakePriceQuoter) AddItem(itemID, kitchenID id.ID, name string, rupees float64, taxBps int64) {
	f.Items[itemID] = contracts.PricedItem{
		MenuItemID:   itemID,
		Name:         name,
		UnitPrice:    money.FromRupees(rupees),
		TaxBps:       taxBps,
		IsAvailable:  true,
		KitchenID:    kitchenID,
		PackagingFee: money.Zero(money.INR),
	}
}

// MarkUnavailable simulates an item selling out between browsing and checkout.
func (f *FakePriceQuoter) MarkUnavailable(itemID id.ID) {
	if item, ok := f.Items[itemID]; ok {
		item.IsAvailable = false
		f.Items[itemID] = item
	}
}

func (f *FakePriceQuoter) QuotePrices(_ context.Context, req contracts.PriceQuoteRequest) ([]contracts.PricedItem, error) {
	f.Calls++
	if f.Err != nil {
		return nil, f.Err
	}

	out := make([]contracts.PricedItem, 0, len(req.MenuItemIDs))
	for _, wanted := range req.MenuItemIDs {
		item, ok := f.Items[wanted]
		if !ok {
			return nil, apperr.NotFound("MENU_ITEM_NOT_FOUND", "Not on the menu.")
		}
		out = append(out, item)
	}
	return out, nil
}

// FakeKitchenAvailability controls whether the kitchen accepts orders.
type FakeKitchenAvailability struct {
	Accepting   bool
	Reason      string
	Serves      bool
	KitchenName string
}

func NewFakeKitchenAvailability() *FakeKitchenAvailability {
	return &FakeKitchenAvailability{Accepting: true, Serves: true, KitchenName: "Test Kitchen"}
}

func (f *FakeKitchenAvailability) Name(context.Context, id.ID) (string, error) {
	return f.KitchenName, nil
}

func (f *FakeKitchenAvailability) IsAcceptingOrders(context.Context, id.ID) (bool, string, error) {
	return f.Accepting, f.Reason, nil
}

func (f *FakeKitchenAvailability) ServesLocation(context.Context, id.ID, float64, float64) (bool, error) {
	return f.Serves, nil
}

// FakeStockReserver records reserve/release/consume so a saga test can assert
// that compensation actually ran.
type FakeStockReserver struct {
	mu sync.Mutex

	// ReserveErr forces a shortage.
	ReserveErr error

	Reserved []id.ID
	Released []id.ID
	Consumed []id.ID
}

func NewFakeStockReserver() *FakeStockReserver { return &FakeStockReserver{} }

func (f *FakeStockReserver) Reserve(_ context.Context, orderID, _ id.ID, _ []contracts.ReservationLine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ReserveErr != nil {
		return f.ReserveErr
	}
	f.Reserved = append(f.Reserved, orderID)
	return nil
}

func (f *FakeStockReserver) Release(_ context.Context, orderID id.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Released = append(f.Released, orderID)
	return nil
}

func (f *FakeStockReserver) Consume(_ context.Context, orderID id.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Consumed = append(f.Consumed, orderID)
	return nil
}

func (f *FakeStockReserver) WasReleased(orderID id.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, released := range f.Released {
		if released == orderID {
			return true
		}
	}
	return false
}

// FakeDiscountCalculator applies a fixed discount when a code matches.
type FakeDiscountCalculator struct {
	// Code is the coupon that works; anything else is rejected.
	Code           string
	DiscountRupees float64
	CouponID       id.ID

	Redeemed []id.ID
	Released []id.ID
}

func NewFakeDiscountCalculator() *FakeDiscountCalculator {
	return &FakeDiscountCalculator{CouponID: id.New()}
}

func (f *FakeDiscountCalculator) Calculate(_ context.Context, req contracts.DiscountRequest) (contracts.DiscountResult, error) {
	if f.Code == "" || req.CouponCode != f.Code {
		return contracts.DiscountResult{
			Applied:        false,
			DiscountAmount: money.Zero(money.INR),
			RejectReason:   "That code is not valid.",
		}, nil
	}
	return contracts.DiscountResult{
		Applied:        true,
		CouponID:       f.CouponID,
		Code:           f.Code,
		DiscountAmount: money.FromRupees(f.DiscountRupees),
	}, nil
}

func (f *FakeDiscountCalculator) Redeem(_ context.Context, _, _, orderID id.ID) error {
	f.Redeemed = append(f.Redeemed, orderID)
	return nil
}

func (f *FakeDiscountCalculator) ReleaseRedemption(_ context.Context, orderID id.ID) error {
	f.Released = append(f.Released, orderID)
	return nil
}

// FakePaymentInitiator simulates a gateway.
type FakePaymentInitiator struct {
	// IntentErr forces a decline.
	IntentErr error
	// Refunds records amounts, so a cancellation test can assert the customer was
	// actually refunded and for how much.
	Refunds map[id.ID]money.Amount
	Intents []id.ID
}

func NewFakePaymentInitiator() *FakePaymentInitiator {
	return &FakePaymentInitiator{Refunds: map[id.ID]money.Amount{}}
}

func (f *FakePaymentInitiator) CreateIntent(_ context.Context, req contracts.PaymentIntentRequest) (contracts.PaymentIntent, error) {
	if f.IntentErr != nil {
		return contracts.PaymentIntent{}, f.IntentErr
	}
	f.Intents = append(f.Intents, req.OrderID)
	return contracts.PaymentIntent{
		PaymentID:      id.New(),
		ProviderRef:    "fake_" + id.HumanCode(8),
		ClientSecret:   "secret",
		Status:         "created",
		RequiresAction: true,
	}, nil
}

func (f *FakePaymentInitiator) Refund(_ context.Context, orderID id.ID, amount money.Amount, _ string) error {
	f.Refunds[orderID] = amount
	return nil
}

// FakeUserDirectory returns a configured user and address.
type FakeUserDirectory struct {
	User    contracts.UserSummary
	Address contracts.Address
	// AddressErr simulates an address that does not belong to the caller.
	AddressErr error
}

func NewFakeUserDirectory(userID id.ID) *FakeUserDirectory {
	addressID := id.New()
	return &FakeUserDirectory{
		User: contracts.UserSummary{
			UserID: userID, FullName: "Test Customer",
			Phone: "+919876543210", IsActive: true,
		},
		Address: contracts.Address{
			ID: addressID, UserID: userID, Label: "Home",
			Line1: "1 Test Road", City: "Lucknow", Pincode: "226001",
			// Real coordinates: (0,0) would pass a naive radius check while being
			// in the Gulf of Guinea.
			Latitude: 26.8500, Longitude: 80.9470,
		},
	}
}

func (f *FakeUserDirectory) GetUser(context.Context, id.ID) (contracts.UserSummary, error) {
	return f.User, nil
}

func (f *FakeUserDirectory) GetAddress(_ context.Context, _, _ id.ID) (contracts.Address, error) {
	if f.AddressErr != nil {
		return contracts.Address{}, f.AddressErr
	}
	return f.Address, nil
}

// FakeRiderDispatcher records dispatch requests.
type FakeRiderDispatcher struct {
	// Err simulates a 3PL outage, which must NOT block the kitchen.
	Err       error
	Requested []id.ID
}

func (f *FakeRiderDispatcher) RequestRider(_ context.Context, orderID, _ id.ID, _, _ float64) error {
	if f.Err != nil {
		return f.Err
	}
	f.Requested = append(f.Requested, orderID)
	return nil
}

// RecordingPublisher captures published events instead of writing an outbox row.
//
// Lets a test assert "order.placed was published with this total" without
// draining the relay.
type RecordingPublisher struct {
	mu     sync.Mutex
	Events []eventbus.Event
}

func NewRecordingPublisher() *RecordingPublisher { return &RecordingPublisher{} }

func (p *RecordingPublisher) Publish(_ context.Context, events ...eventbus.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Events = append(p.Events, events...)
	return nil
}

// Names returns the published event names in order.
func (p *RecordingPublisher) Names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.Events))
	for i, e := range p.Events {
		out[i] = string(e.Name)
	}
	return out
}

// Has reports whether an event with this name was published.
func (p *RecordingPublisher) Has(name eventbus.Name) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

// Compile-time proof every fake satisfies its contract, so an interface change
// cannot leave a stale fake behind.
var (
	_ contracts.PriceQuoter         = (*FakePriceQuoter)(nil)
	_ contracts.KitchenAvailability = (*FakeKitchenAvailability)(nil)
	_ contracts.StockReserver       = (*FakeStockReserver)(nil)
	_ contracts.DiscountCalculator  = (*FakeDiscountCalculator)(nil)
	_ contracts.PaymentInitiator    = (*FakePaymentInitiator)(nil)
	_ contracts.UserDirectory       = (*FakeUserDirectory)(nil)
	_ contracts.RiderDispatcher     = (*FakeRiderDispatcher)(nil)
	_ eventbus.Publisher            = (*RecordingPublisher)(nil)
)
