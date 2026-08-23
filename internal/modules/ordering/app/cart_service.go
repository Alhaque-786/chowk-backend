package app

import (
	"context"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// CartService manages the cart and produces a priced preview.
type CartService struct {
	carts    domain.CartRepository
	prices   contracts.PriceQuoter
	kitchens contracts.KitchenAvailability
	clock    clock.Clock
}

func NewCartService(
	carts domain.CartRepository,
	prices contracts.PriceQuoter,
	kitchens contracts.KitchenAvailability,
	clk clock.Clock,
) *CartService {
	return &CartService{carts: carts, prices: prices, kitchens: kitchens, clock: clk}
}

type AddToCartInput struct {
	UserID      id.ID
	KitchenID   id.ID
	MenuItemID  id.ID
	Quantity    int32
	ModifierIDs []id.ID
	Note        string
}

// AddToCart adds an item, validating it exists and is orderable.
//
// Validation happens here rather than only at checkout so the customer learns
// immediately that something is sold out, instead of at the last step after
// entering an address.
func (s *CartService) AddToCart(ctx context.Context, in AddToCartInput) (*domain.Cart, error) {
	cart, err := s.carts.Get(ctx, in.UserID)
	if err != nil {
		return nil, err
	}

	// One order is always from one kitchen. Switching kitchens replaces the cart
	// rather than silently mixing, which would break delivery and settlement.
	if !id.IsNil(cart.KitchenID) && cart.KitchenID != in.KitchenID && !cart.IsEmpty() {
		return nil, apperr.Conflict("DIFFERENT_KITCHEN",
			"Your cart has items from another kitchen. Clear it to order from this one.")
	}
	cart.KitchenID = in.KitchenID

	// Resolve the item to confirm it exists, belongs to this kitchen, and is
	// available. QuotePrices does all three.
	quoted, err := s.prices.QuotePrices(ctx, contracts.PriceQuoteRequest{
		KitchenID:   in.KitchenID,
		MenuItemIDs: []id.ID{in.MenuItemID},
		ModifierIDs: in.ModifierIDs,
	})
	if err != nil {
		return nil, err
	}
	if len(quoted) == 0 || !quoted[0].IsAvailable {
		return nil, apperr.Conflict("ITEM_UNAVAILABLE", "That item is not available right now.")
	}

	if err := cart.AddLine(in.MenuItemID, in.Quantity, in.ModifierIDs, in.Note, s.clock.Now()); err != nil {
		return nil, err
	}
	if err := s.carts.Save(ctx, cart); err != nil {
		return nil, err
	}
	return cart, nil
}

func (s *CartService) SetQuantity(ctx context.Context, userID, lineID id.ID, quantity int32) (*domain.Cart, error) {
	cart, err := s.carts.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := cart.SetLineQuantity(lineID, quantity, s.clock.Now()); err != nil {
		return nil, err
	}
	if err := s.carts.Save(ctx, cart); err != nil {
		return nil, err
	}
	return cart, nil
}

func (s *CartService) Clear(ctx context.Context, userID id.ID) error {
	return s.carts.Delete(ctx, userID)
}

func (s *CartService) Get(ctx context.Context, userID id.ID) (*domain.Cart, error) {
	return s.carts.Get(ctx, userID)
}

// CartPreview is the priced cart shown before checkout.
//
// Computed from live catalog prices every time, never from anything stored on the
// cart. That is what makes the preview and the charged amount agree, and it is
// why a stale cart cannot lock in an old price.
type CartPreview struct {
	Cart      *domain.Cart
	Lines     []PreviewLine
	Subtotal  money.Amount
	Packaging money.Amount
	Delivery  money.Amount
	Tax       money.Amount
	Total     money.Amount
	// Unavailable names items the customer must remove before checking out.
	Unavailable []string
	/*
	  KitchenName exists so the UI can NAME the kitchen a cart belongs to.

	  The case that needs it: adding a dish from a second kitchen is refused,
	  because a cart belongs to one kitchen. "Your cart has items from another
	  kitchen" makes the customer go and look; "Replace your Khansama order with
	  Tunday Kababi?" is a decision they can make on the spot.

	  Empty for an empty cart, and empty rather than an error if the lookup fails —
	  a missing name degrades the wording, and failing the whole cart read over it
	  would be a far worse trade.
	*/
	KitchenName  string
	KitchenOpen  bool
	ClosedReason string
	MeetsMinimum bool
	MinimumOrder money.Amount
}

type PreviewLine struct {
	LineID      id.ID
	MenuItemID  id.ID
	Name        string
	Quantity    int32
	UnitPrice   money.Amount
	LineTotal   money.Amount
	Modifiers   []string
	IsAvailable bool
}

// Preview prices the cart for the checkout screen.
//
// It reuses the SAME domain.Order.RecalculateTotals the real order uses. Building
// the preview with separate arithmetic is how a cart total ends up disagreeing
// with the charged amount — a bug that is invisible in testing and enraging in
// production.
func (s *CartService) Preview(ctx context.Context, userID id.ID) (*CartPreview, error) {
	cart, err := s.carts.Get(ctx, userID)
	if err != nil {
		return nil, err
	}

	preview := &CartPreview{
		Cart:         cart,
		Subtotal:     money.Zero(money.INR),
		Packaging:    money.Zero(money.INR),
		Delivery:     money.Zero(money.INR),
		Tax:          money.Zero(money.INR),
		Total:        money.Zero(money.INR),
		MinimumOrder: money.Zero(money.INR),
		MeetsMinimum: true,
	}
	if cart.IsEmpty() {
		return preview, nil
	}

	open, reason, err := s.kitchens.IsAcceptingOrders(ctx, cart.KitchenID)
	if err != nil {
		return nil, err
	}
	preview.KitchenOpen = open
	preview.ClosedReason = reason

	// Best-effort: the name only improves the wording of a message, so a failure
	// here must not fail the cart. Same treatment the order confirmation gives it.
	if name, nameErr := s.kitchens.Name(ctx, cart.KitchenID); nameErr == nil {
		preview.KitchenName = name
	}

	quoted, err := s.prices.QuotePrices(ctx, contracts.PriceQuoteRequest{
		KitchenID:   cart.KitchenID,
		MenuItemIDs: cart.MenuItemIDs(),
		ModifierIDs: cart.ModifierIDs(),
	})
	if err != nil {
		return nil, err
	}
	priced := make(map[id.ID]contracts.PricedItem, len(quoted))
	for _, q := range quoted {
		priced[q.MenuItemID] = q
	}

	// Build a throwaway Order purely to reuse its totals arithmetic.
	scratch := &domain.Order{Currency: money.INR}
	packaging := money.Zero(money.INR)

	for _, line := range cart.Lines {
		item, ok := priced[line.MenuItemID]
		if !ok {
			preview.Unavailable = append(preview.Unavailable, "an item that is no longer on the menu")
			continue
		}
		if !item.IsAvailable {
			preview.Unavailable = append(preview.Unavailable, item.Name)
		}

		orderLine := domain.Line{
			MenuItemID: item.MenuItemID,
			Name:       item.Name,
			Quantity:   line.Quantity,
			UnitPrice:  item.UnitPrice,
			TaxBps:     item.TaxBps,
		}
		chosen := make(map[id.ID]struct{}, len(line.ModifierIDs))
		for _, m := range line.ModifierIDs {
			chosen[m] = struct{}{}
		}
		var modNames []string
		for _, mod := range item.Modifiers {
			if _, want := chosen[mod.ModifierID]; !want {
				continue
			}
			orderLine.Modifiers = append(orderLine.Modifiers, domain.LineModifier{
				ModifierID: mod.ModifierID, Name: mod.Name, UnitPrice: mod.UnitPrice,
			})
			modNames = append(modNames, mod.Name)
		}
		scratch.Lines = append(scratch.Lines, orderLine)

		itemPackaging, err := item.PackagingFee.MulInt(int64(line.Quantity))
		if err != nil {
			return nil, apperr.Internal(err)
		}
		if packaging, err = packaging.Add(itemPackaging); err != nil {
			return nil, apperr.Internal(err)
		}

		preview.Lines = append(preview.Lines, PreviewLine{
			LineID:      line.LineID,
			MenuItemID:  item.MenuItemID,
			Name:        item.Name,
			Quantity:    line.Quantity,
			UnitPrice:   item.UnitPrice,
			Modifiers:   modNames,
			IsAvailable: item.IsAvailable,
		})
	}

	scratch.PackagingFee = packaging
	scratch.DeliveryFee = money.FromRupees(35)
	if err := scratch.RecalculateTotals(); err != nil {
		return nil, err
	}

	// Copy the computed line totals back, so the UI shows the same per-line
	// figure the invoice will.
	for i := range preview.Lines {
		if i < len(scratch.Lines) {
			preview.Lines[i].LineTotal = scratch.Lines[i].LineTotal
		}
	}

	preview.Subtotal = scratch.Subtotal
	preview.Packaging = scratch.PackagingFee
	preview.Delivery = scratch.DeliveryFee
	preview.Tax = scratch.TaxAmount
	preview.Total = scratch.Total
	return preview, nil
}
