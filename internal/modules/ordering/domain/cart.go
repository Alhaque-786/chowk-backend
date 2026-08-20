package domain

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// Cart is a customer's in-progress selection.
//
// Deliberately NOT a database table. A cart is high-write (every quantity tap),
// short-lived, and disposable — putting it in Postgres means thousands of UPDATEs
// on rows that mostly get abandoned, competing with the order writes that
// actually matter. It lives in Redis with a TTL and simply expires.
//
// It also holds NO prices. Only item ids and quantities. Prices are resolved at
// checkout from catalog, every time. A cart that cached prices would let a
// customer hold a stale price for hours, and would be trivially tamperable if a
// client could ever write to it.
type Cart struct {
	UserID    id.ID      `json:"user_id"`
	KitchenID id.ID      `json:"kitchen_id"`
	Lines     []CartLine `json:"lines"`
	// Note is the customer's request to the kitchen ("less spicy").
	Note      string    `json:"note,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CartLine is one selection: an item, a quantity, and chosen modifiers.
type CartLine struct {
	// LineID distinguishes two lines of the same dish with different modifiers --
	// one biryani with extra raita and one without are separate lines, not
	// quantity 2.
	LineID      id.ID   `json:"line_id"`
	MenuItemID  id.ID   `json:"menu_item_id"`
	Quantity    int32   `json:"quantity"`
	ModifierIDs []id.ID `json:"modifier_ids,omitempty"`
	Note        string  `json:"note,omitempty"`
}

const (
	// MaxCartLines bounds the cart so a malicious client cannot make checkout
	// resolve ten thousand items.
	MaxCartLines = 50
	// MaxLineQuantity is per line. Genuine bulk orders go through a different
	// flow; 50 of one dish through the consumer app is almost always a mistake
	// or an attack.
	MaxLineQuantity = 50
)

func NewCart(userID, kitchenID id.ID, now time.Time) *Cart {
	return &Cart{
		UserID:    userID,
		KitchenID: kitchenID,
		Lines:     []CartLine{},
		UpdatedAt: now,
	}
}

func (c *Cart) IsEmpty() bool { return len(c.Lines) == 0 }

// TotalQuantity is the badge count on the cart icon.
func (c *Cart) TotalQuantity() int32 {
	var total int32
	for _, l := range c.Lines {
		total += l.Quantity
	}
	return total
}

// AddLine adds a selection, merging into an existing line when the item AND its
// modifiers match exactly.
//
// Merging matters for the obvious reason (tapping "add" twice should show
// quantity 2, not two identical lines) and the subtle one: without it, the same
// dish appears repeatedly and the customer cannot tell why.
func (c *Cart) AddLine(menuItemID id.ID, quantity int32, modifierIDs []id.ID, note string, now time.Time) error {
	if quantity <= 0 {
		return apperr.Invalid("INVALID_QUANTITY", "Quantity must be at least 1.")
	}
	if quantity > MaxLineQuantity {
		return apperr.Invalid("QUANTITY_TOO_LARGE",
			"You can order at most 50 of one item. Contact us for bulk orders.")
	}

	if existing := c.findMatchingLine(menuItemID, modifierIDs, note); existing != nil {
		newQty := existing.Quantity + quantity
		if newQty > MaxLineQuantity {
			return apperr.Invalid("QUANTITY_TOO_LARGE",
				"You can order at most 50 of one item.")
		}
		existing.Quantity = newQty
		c.UpdatedAt = now
		return nil
	}

	if len(c.Lines) >= MaxCartLines {
		return apperr.Invalid("CART_FULL",
			"Your cart is full. Please place this order before adding more items.")
	}

	c.Lines = append(c.Lines, CartLine{
		LineID:      id.New(),
		MenuItemID:  menuItemID,
		Quantity:    quantity,
		ModifierIDs: modifierIDs,
		Note:        note,
	})
	c.UpdatedAt = now
	return nil
}

// SetLineQuantity updates a line, removing it when the quantity reaches zero.
//
// Zero means remove rather than erroring, because that is what a customer
// tapping "−" down to nothing means.
func (c *Cart) SetLineQuantity(lineID id.ID, quantity int32, now time.Time) error {
	if quantity > MaxLineQuantity {
		return apperr.Invalid("QUANTITY_TOO_LARGE", "You can order at most 50 of one item.")
	}

	for i := range c.Lines {
		if c.Lines[i].LineID != lineID {
			continue
		}
		if quantity <= 0 {
			c.Lines = append(c.Lines[:i], c.Lines[i+1:]...)
		} else {
			c.Lines[i].Quantity = quantity
		}
		c.UpdatedAt = now
		return nil
	}
	return apperr.NotFound("CART_LINE_NOT_FOUND", "That item is no longer in your cart.")
}

func (c *Cart) RemoveLine(lineID id.ID, now time.Time) error {
	return c.SetLineQuantity(lineID, 0, now)
}

func (c *Cart) Clear(now time.Time) {
	c.Lines = []CartLine{}
	c.Note = ""
	c.UpdatedAt = now
}

// MenuItemIDs returns the distinct item ids for a price quote.
func (c *Cart) MenuItemIDs() []id.ID {
	seen := make(map[id.ID]struct{}, len(c.Lines))
	out := make([]id.ID, 0, len(c.Lines))
	for _, l := range c.Lines {
		if _, dup := seen[l.MenuItemID]; dup {
			continue
		}
		seen[l.MenuItemID] = struct{}{}
		out = append(out, l.MenuItemID)
	}
	return out
}

// ModifierIDs returns every distinct modifier id across all lines.
func (c *Cart) ModifierIDs() []id.ID {
	seen := make(map[id.ID]struct{})
	var out []id.ID
	for _, l := range c.Lines {
		for _, m := range l.ModifierIDs {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

// findMatchingLine locates a line with the same item, same modifiers and same
// note. Modifier order is ignored, since the client may send them in any order.
func (c *Cart) findMatchingLine(menuItemID id.ID, modifierIDs []id.ID, note string) *CartLine {
	for i := range c.Lines {
		l := &c.Lines[i]
		if l.MenuItemID != menuItemID || l.Note != note {
			continue
		}
		if sameIDSet(l.ModifierIDs, modifierIDs) {
			return l
		}
	}
	return nil
}

func sameIDSet(a, b []id.ID) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[id.ID]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		counts[v]--
		if counts[v] < 0 {
			return false
		}
	}
	return true
}

// CartRepository persists carts. Backed by Redis, not Postgres.
type CartRepository interface {
	Get(ctx context.Context, userID id.ID) (*Cart, error)
	Save(ctx context.Context, cart *Cart) error
	Delete(ctx context.Context, userID id.ID) error
}
