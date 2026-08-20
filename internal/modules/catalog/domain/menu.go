package domain

import (
	"context"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// FoodType is a mandatory label in India, not a preference. It drives the
// green/red mark the UI is legally required to show.
type FoodType string

const (
	FoodVeg    FoodType = "veg"
	FoodNonVeg FoodType = "non_veg"
	FoodEgg    FoodType = "egg"
	FoodVegan  FoodType = "vegan"
)

func (f FoodType) Valid() bool {
	switch f {
	case FoodVeg, FoodNonVeg, FoodEgg, FoodVegan:
		return true
	}
	return false
}

// Category groups items on the menu.
type Category struct {
	ID          id.ID
	KitchenID   id.ID
	Name        string
	Description string
	// DisplayOrder is explicit because the kitchen decides that Starters come
	// before Biryani. Alphabetical would be wrong.
	DisplayOrder int32
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MenuItem is a sellable dish.
//
// This is the pricing authority for the whole system. Ordering never trusts a
// price from the client; it asks catalog.
type MenuItem struct {
	ID          id.ID
	KitchenID   id.ID
	CategoryID  *id.ID
	Name        string
	Description string
	ImageURL    string

	Price money.Amount
	// ComparePrice is the struck-through "was" price. Nil means no offer.
	ComparePrice *money.Amount
	// TaxBps is per-item because GST genuinely differs by item: 5% on prepared
	// food, 18% on packaged beverages. One blended rate on the order subtotal
	// would be wrong and would fail an audit.
	TaxBps       int32
	PackagingFee money.Amount

	FoodType   FoodType
	SpiceLevel int16
	Allergens  []string
	Calories   *int32
	Serves     int16

	// Two independent flags: IsAvailable is the manual toggle, IsSoldOut is set
	// automatically by inventory when ingredients run out. Separate so the
	// kitchen can see which one hid the dish.
	IsAvailable   bool
	IsSoldOut     bool
	PrepMinutes   int32
	DisplayOrder  int32
	IsRecommended bool

	RatingAverage float64
	RatingCount   int32
	OrderCount    int32

	Version   int32
	CreatedAt time.Time
	UpdatedAt time.Time

	// ModifierGroups is loaded on demand.
	ModifierGroups []ModifierGroup
}

// IsOrderable reports whether a customer can add this to a cart.
func (m *MenuItem) IsOrderable() bool {
	return m.IsAvailable && !m.IsSoldOut
}

// EffectivePrice is what the customer pays for one unit before modifiers.
func (m *MenuItem) EffectivePrice() money.Amount { return m.Price }

// DiscountBps is the implied discount versus the compare price, for the UI
// badge. Zero when there is no offer.
func (m *MenuItem) DiscountBps() int64 {
	if m.ComparePrice == nil || m.ComparePrice.Minor() <= 0 {
		return 0
	}
	saved := m.ComparePrice.Minor() - m.Price.Minor()
	if saved <= 0 {
		return 0
	}
	return saved * 10000 / m.ComparePrice.Minor()
}

func (m *MenuItem) Validate() error {
	err := apperr.Invalid("INVALID_MENU_ITEM", "Please check the menu item fields.")
	bad := false

	if strings.TrimSpace(m.Name) == "" {
		err.WithField("name", "required")
		bad = true
	}
	if !m.Price.IsPositive() {
		err.WithField("price", "must_be_positive")
		bad = true
	}
	// A compare price at or below the real price renders as a price increase
	// dressed up as a discount.
	if m.ComparePrice != nil && !m.ComparePrice.GreaterThan(m.Price) {
		err.WithField("compare_price", "must_exceed_price")
		bad = true
	}
	if m.TaxBps < 0 || m.TaxBps > 10000 {
		err.WithField("tax_bps", "must_be_0_to_10000")
		bad = true
	}
	if !m.FoodType.Valid() {
		err.WithField("food_type", "invalid")
		bad = true
	}
	if m.SpiceLevel < 0 || m.SpiceLevel > 3 {
		err.WithField("spice_level", "must_be_0_to_3")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

// ModifierGroup is a set of choices attached to an item: "Choose a size",
// "Add-ons".
//
// MinSelect/MaxSelect express every real rule: a required single choice is
// (1,1), optional add-ons are (0,n).
type ModifierGroup struct {
	ID           id.ID
	KitchenID    id.ID
	Name         string
	MinSelect    int16
	MaxSelect    int16
	DisplayOrder int32
	Modifiers    []Modifier
}

// IsRequired reports whether the customer must pick at least one.
func (g *ModifierGroup) IsRequired() bool { return g.MinSelect > 0 }

// ValidateSelection checks a customer's chosen modifiers against the group's
// rules.
//
// Enforced server-side because a client that could bypass this would let someone
// order a pizza with no size selected — which has no price.
func (g *ModifierGroup) ValidateSelection(selected []id.ID) error {
	// Only count ids that actually belong to this group.
	count := int16(0)
	valid := make(map[id.ID]bool, len(g.Modifiers))
	for _, m := range g.Modifiers {
		valid[m.ID] = true
	}
	for _, s := range selected {
		if valid[s] {
			count++
		}
	}

	if count < g.MinSelect {
		return apperr.Invalid("MODIFIER_SELECTION_REQUIRED",
			"Please choose at least "+plural(g.MinSelect)+" from \""+g.Name+"\".")
	}
	if g.MaxSelect > 0 && count > g.MaxSelect {
		return apperr.Invalid("TOO_MANY_MODIFIERS",
			"You can choose at most "+plural(g.MaxSelect)+" from \""+g.Name+"\".")
	}
	return nil
}

type Modifier struct {
	ID      id.ID
	GroupID id.ID
	Name    string
	// Price may be zero (a free choice like "no onions") but never negative.
	Price        money.Amount
	IsAvailable  bool
	DisplayOrder int32
}

func plural(n int16) string {
	if n == 1 {
		return "1 option"
	}
	return itoa16(n) + " options"
}

func itoa16(n int16) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Menu is the whole customer-facing menu for one kitchen: what a single API call
// returns and what gets cached as one Redis value.
type Menu struct {
	Kitchen    *Kitchen
	Categories []Category
	// Items keyed by category id. The "" key holds uncategorised items.
	ItemsByCategory map[id.ID][]MenuItem
}

// ---------------------------------------------------------------------------
// Repository ports
// ---------------------------------------------------------------------------

type MenuRepository interface {
	CreateCategory(ctx context.Context, c *Category) error
	UpdateCategory(ctx context.Context, c *Category) error
	ListCategories(ctx context.Context, kitchenID id.ID, activeOnly bool) ([]Category, error)

	CreateItem(ctx context.Context, m *MenuItem) error
	UpdateItem(ctx context.Context, m *MenuItem) error
	FindItemByID(ctx context.Context, itemID id.ID) (*MenuItem, error)
	// FindItemsByIDs is the batch read checkout uses. Batched deliberately:
	// one query for a ten-item cart, not ten.
	FindItemsByIDs(ctx context.Context, itemIDs []id.ID) ([]MenuItem, error)
	ListItems(ctx context.Context, kitchenID id.ID, orderableOnly bool) ([]MenuItem, error)
	SearchItems(ctx context.Context, kitchenID id.ID, query string, limit int) ([]MenuItem, error)

	// SetSoldOut is called by the inventory module's event handler, never by a
	// human. The manual switch is IsAvailable.
	SetSoldOut(ctx context.Context, itemID id.ID, soldOut bool) error

	CreateModifierGroup(ctx context.Context, g *ModifierGroup) error
	CreateModifier(ctx context.Context, m *Modifier) error
	AttachModifierGroup(ctx context.Context, itemID, groupID id.ID) error
	// LoadModifierGroups batches by item so rendering a menu is one extra query,
	// not one per dish.
	LoadModifierGroups(ctx context.Context, itemIDs []id.ID) (map[id.ID][]ModifierGroup, error)
	FindModifiersByIDs(ctx context.Context, modifierIDs []id.ID) ([]Modifier, error)
}
