// Package app holds the catalog module's use cases.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// menuCacheTTL is short on purpose. The menu is invalidated explicitly on every
// write, so the TTL is only a backstop against a missed invalidation — not the
// primary freshness mechanism.
const menuCacheTTL = 5 * time.Minute

// CatalogService serves the menu and answers pricing/availability questions.
type CatalogService struct {
	db       *database.DB
	kitchens domain.KitchenRepository
	menus    domain.MenuRepository
	loader   *cache.Loader
	cache    cache.Cache
	events   eventbus.Publisher
	clock    clock.Clock
}

func NewCatalogService(
	db *database.DB,
	kitchens domain.KitchenRepository,
	menus domain.MenuRepository,
	loader *cache.Loader,
	c cache.Cache,
	events eventbus.Publisher,
	clk clock.Clock,
) *CatalogService {
	return &CatalogService{
		db: db, kitchens: kitchens, menus: menus,
		loader: loader, cache: c, events: events, clock: clk,
	}
}

// ---------------------------------------------------------------------------
// Customer-facing reads
// ---------------------------------------------------------------------------

// NearbyKitchen is a kitchen plus how far away it is.
type NearbyKitchen struct {
	Kitchen   *domain.Kitchen
	DistanceM int32
	IsOpen    bool
	ClosedWhy domain.ClosedReason
}

// FindNearby returns kitchens that deliver to a point, nearest first.
//
// Two-stage by design: an indexed bounding-box scan narrows the candidates, then
// exact Haversine filtering removes the box corners. Doing only the box would
// include kitchens up to 41% beyond the radius.
func (s *CatalogService) FindNearby(ctx context.Context, lat, lng float64, radiusM int32, limit int) ([]NearbyKitchen, error) {
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return nil, apperr.Invalid("INVALID_LOCATION", "That location is not valid.")
	}
	if radiusM <= 0 || radiusM > 50000 {
		radiusM = 10000
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	// Over-fetch, because the exact filter will discard some candidates and we
	// still want a full page afterwards.
	candidates, err := s.kitchens.FindNearby(ctx, lat, lng, radiusM, limit*3)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Hours are needed to report open/closed, and loading them per kitchen would
	// be N+1.
	ids := make([]id.ID, 0, len(candidates))
	for _, k := range candidates {
		ids = append(ids, k.ID)
	}
	hours, err := s.kitchens.LoadHours(ctx, ids)
	if err != nil {
		return nil, err
	}

	now := s.clock.Now()
	out := make([]NearbyKitchen, 0, len(candidates))

	for _, k := range candidates {
		// The kitchen's own radius decides, not the search radius: a kitchen that
		// only delivers 2 km must not appear for a customer 8 km away even if
		// they searched a 10 km radius.
		distance := k.DistanceTo(lat, lng)
		if distance > float64(k.DeliveryRadiusM) {
			continue
		}

		k.Hours = hours[k.ID]
		open, why := k.CanAcceptOrder(now)

		out = append(out, NearbyKitchen{
			Kitchen:   k,
			DistanceM: int32(distance),
			IsOpen:    open,
			ClosedWhy: why,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// GetMenu returns a kitchen's full customer-facing menu, cached.
//
// This is the hottest read in the system — every customer hits it before
// ordering — and it changes rarely, which makes it the single best caching
// candidate. Served through the stampede-protected loader so a cache expiry
// during the lunch rush cannot send 2,000 concurrent queries at Postgres.
func (s *CatalogService) GetMenu(ctx context.Context, kitchenID id.ID) (*domain.Menu, error) {
	return cache.GetOrLoad(ctx, s.loader, cache.MenuKey(kitchenID.String()), menuCacheTTL,
		func(ctx context.Context) (*domain.Menu, error) {
			return s.loadMenu(ctx, kitchenID)
		})
}

// loadMenu reads a menu from Postgres in four batched queries, never N+1.
func (s *CatalogService) loadMenu(ctx context.Context, kitchenID id.ID) (*domain.Menu, error) {
	kitchen, err := s.kitchens.FindByID(ctx, kitchenID)
	if err != nil {
		return nil, err
	}
	if !kitchen.IsActive {
		return nil, apperr.NotFound("KITCHEN_NOT_FOUND", "That kitchen could not be found.")
	}

	hours, err := s.kitchens.LoadHours(ctx, []id.ID{kitchenID})
	if err != nil {
		return nil, err
	}
	kitchen.Hours = hours[kitchenID]

	categories, err := s.menus.ListCategories(ctx, kitchenID, true)
	if err != nil {
		return nil, err
	}

	// Orderable only: a sold-out or hidden dish must not appear on the customer
	// menu at all. The kitchen dashboard uses ListItems with orderableOnly=false.
	items, err := s.menus.ListItems(ctx, kitchenID, true)
	if err != nil {
		return nil, err
	}

	if len(items) > 0 {
		itemIDs := make([]id.ID, len(items))
		for i := range items {
			itemIDs[i] = items[i].ID
		}
		groups, err := s.menus.LoadModifierGroups(ctx, itemIDs)
		if err != nil {
			return nil, err
		}
		for i := range items {
			items[i].ModifierGroups = groups[items[i].ID]
		}
	}

	byCategory := make(map[id.ID][]domain.MenuItem, len(categories)+1)
	for _, item := range items {
		key := id.Nil
		if item.CategoryID != nil {
			key = *item.CategoryID
		}
		byCategory[key] = append(byCategory[key], item)
	}

	return &domain.Menu{
		Kitchen:         kitchen,
		Categories:      categories,
		ItemsByCategory: byCategory,
	}, nil
}

// InvalidateMenu drops the cached menu. Called after every write.
//
// Explicit invalidation rather than relying on the TTL, because a price change
// must be visible immediately — charging the old price for five minutes is a
// real problem, not a cosmetic one.
func (s *CatalogService) InvalidateMenu(ctx context.Context, kitchenID id.ID) error {
	return s.loader.Invalidate(ctx,
		cache.MenuKey(kitchenID.String()),
		cache.KitchenKey(kitchenID.String()),
	)
}

func (s *CatalogService) GetKitchen(ctx context.Context, kitchenID id.ID) (*domain.Kitchen, error) {
	k, err := s.kitchens.FindByID(ctx, kitchenID)
	if err != nil {
		return nil, err
	}
	hours, err := s.kitchens.LoadHours(ctx, []id.ID{kitchenID})
	if err != nil {
		return nil, err
	}
	k.Hours = hours[kitchenID]
	return k, nil
}

func (s *CatalogService) GetKitchenBySlug(ctx context.Context, slug string) (*domain.Kitchen, error) {
	k, err := s.kitchens.FindBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	hours, err := s.kitchens.LoadHours(ctx, []id.ID{k.ID})
	if err != nil {
		return nil, err
	}
	k.Hours = hours[k.ID]
	return k, nil
}

func (s *CatalogService) SearchItems(ctx context.Context, kitchenID id.ID, query string, limit int) ([]domain.MenuItem, error) {
	if len(query) < 2 {
		return nil, apperr.Invalid("QUERY_TOO_SHORT", "Please type at least two characters.")
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	return s.menus.SearchItems(ctx, kitchenID, query, limit)
}

// ---------------------------------------------------------------------------
// contracts.PriceQuoter — the checkout dependency
// ---------------------------------------------------------------------------

// QuotePrices resolves authoritative prices for a cart.
//
// This is the reason catalog owns pricing. The client sends item IDs and
// quantities; it never sends prices. A client that could send its own price
// could order a biryani for ₹1, and no amount of validation elsewhere would
// catch it.
//
// Returns one PricedItem per requested id, including unavailable ones flagged
// IsAvailable=false — checkout needs to tell the customer *which* item went
// out of stock, not just that something did.
func (s *CatalogService) QuotePrices(ctx context.Context, req contracts.PriceQuoteRequest) ([]contracts.PricedItem, error) {
	if len(req.MenuItemIDs) == 0 {
		return nil, apperr.Invalid("EMPTY_CART", "Your cart is empty.")
	}
	if len(req.MenuItemIDs) > 100 {
		return nil, apperr.Invalid("CART_TOO_LARGE", "A single order can contain at most 100 distinct items.")
	}

	items, err := s.menus.FindItemsByIDs(ctx, req.MenuItemIDs)
	if err != nil {
		return nil, err
	}

	found := make(map[id.ID]domain.MenuItem, len(items))
	for _, item := range items {
		found[item.ID] = item
	}

	// Resolve requested modifiers in one batch.
	modifiers := make(map[id.ID]domain.Modifier)
	if len(req.ModifierIDs) > 0 {
		mods, err := s.menus.FindModifiersByIDs(ctx, req.ModifierIDs)
		if err != nil {
			return nil, err
		}
		for _, m := range mods {
			modifiers[m.ID] = m
		}
	}

	out := make([]contracts.PricedItem, 0, len(req.MenuItemIDs))
	for _, wantID := range req.MenuItemIDs {
		item, ok := found[wantID]
		if !ok {
			// A referenced item that does not exist is a hard error, not an
			// availability problem: the client is sending something invented, and
			// silently dropping it would produce an order the customer did not place.
			return nil, apperr.NotFound("MENU_ITEM_NOT_FOUND",
				"One of the items in your cart is no longer on the menu.")
		}
		// Cross-kitchen guard. One order is always from one kitchen; mixing them
		// would break the whole delivery and settlement model.
		if item.KitchenID != req.KitchenID {
			return nil, apperr.Invalid("MIXED_KITCHEN_CART",
				"All items in an order must be from the same kitchen.")
		}

		priced := contracts.PricedItem{
			MenuItemID:   item.ID,
			Name:         item.Name,
			UnitPrice:    item.Price,
			TaxBps:       int64(item.TaxBps),
			IsAvailable:  item.IsOrderable(),
			KitchenID:    item.KitchenID,
			PackagingFee: item.PackagingFee,
		}

		for _, modID := range req.ModifierIDs {
			mod, ok := modifiers[modID]
			if !ok {
				continue
			}
			// An unavailable modifier is skipped rather than failing the whole
			// order: "extra cheese ran out" should not block the pizza.
			if !mod.IsAvailable {
				continue
			}
			priced.Modifiers = append(priced.Modifiers, contracts.PricedModifier{
				ModifierID: mod.ID,
				Name:       mod.Name,
				UnitPrice:  mod.Price,
			})
		}

		out = append(out, priced)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// contracts.KitchenAvailability
// ---------------------------------------------------------------------------

// IsAcceptingOrders reports whether the kitchen can take an order right now,
// with a customer-presentable reason when it cannot.
func (s *CatalogService) IsAcceptingOrders(ctx context.Context, kitchenID id.ID) (bool, string, error) {
	kitchen, err := s.GetKitchen(ctx, kitchenID)
	if err != nil {
		return false, "", err
	}

	ok, why := kitchen.CanAcceptOrder(s.clock.Now())
	if ok {
		return true, "", nil
	}
	// The staff-entered pause reason is more specific than our generic copy, so
	// prefer it when present.
	if why == domain.ReasonPaused && kitchen.PauseReason != "" {
		return false, kitchen.PauseReason, nil
	}
	return false, why.CustomerMessage(kitchen.Name), nil
}

// AssistedKitchenIDs implements contracts.KitchenAvailability.
func (s *CatalogService) AssistedKitchenIDs(ctx context.Context) ([]id.ID, error) {
	return s.kitchens.AssistedIDs(ctx)
}

// IsAssisted implements contracts.KitchenAvailability.
func (s *CatalogService) IsAssisted(ctx context.Context, kitchenID id.ID) (bool, error) {
	return s.kitchens.IsAssisted(ctx, kitchenID)
}

// Name returns a kitchen's display name, for snapshotting onto events.
func (s *CatalogService) Name(ctx context.Context, kitchenID id.ID) (string, error) {
	kitchen, err := s.kitchens.FindByID(ctx, kitchenID)
	if err != nil {
		return "", err
	}
	return kitchen.Name, nil
}

func (s *CatalogService) ServesLocation(ctx context.Context, kitchenID id.ID, lat, lng float64) (bool, error) {
	kitchen, err := s.kitchens.FindByID(ctx, kitchenID)
	if err != nil {
		return false, err
	}
	return kitchen.ServesPoint(lat, lng), nil
}

// ---------------------------------------------------------------------------
// Kitchen-facing writes
// ---------------------------------------------------------------------------

// SetAcceptingOrders is the staff kill switch, used when the kitchen is swamped.
func (s *CatalogService) SetAcceptingOrders(ctx context.Context, kitchenID id.ID, accepting bool, reason string) error {
	if !accepting && reason == "" {
		reason = "The kitchen is temporarily not taking orders."
	}
	if err := s.kitchens.SetAcceptingOrders(ctx, kitchenID, accepting, reason); err != nil {
		return err
	}

	evtName := contracts.EventKitchenClosed
	if accepting {
		evtName = contracts.EventKitchenOpened
	}
	evt, err := eventbus.New(evtName, kitchenID, map[string]any{
		"kitchen_id": kitchenID,
		"accepting":  accepting,
		"reason":     reason,
	})
	if err != nil {
		return err
	}
	if err := s.events.Publish(ctx, evt); err != nil {
		return err
	}
	return s.InvalidateMenu(ctx, kitchenID)
}

// UpsertItemInput is the admin payload for creating or editing a dish.
type UpsertItemInput struct {
	ID                *id.ID
	KitchenID         id.ID
	CategoryID        *id.ID
	Name              string
	Description       string
	ImageURL          string
	PriceMinor        int64
	ComparePriceMinor *int64
	TaxBps            int32
	PackagingFeeMinor int64
	FoodType          string
	SpiceLevel        int16
	Allergens         []string
	Serves            int16
	PrepMinutes       int32
	DisplayOrder      int32
	IsAvailable       bool
	IsRecommended     bool
	// Version is required on update, for optimistic locking.
	Version int32
}

// SaveItem creates or updates a menu item and invalidates the cached menu.
func (s *CatalogService) SaveItem(ctx context.Context, in UpsertItemInput) (*domain.MenuItem, error) {
	now := s.clock.Now()

	item := &domain.MenuItem{
		KitchenID:     in.KitchenID,
		CategoryID:    in.CategoryID,
		Name:          in.Name,
		Description:   in.Description,
		ImageURL:      in.ImageURL,
		Price:         money.FromMinor(in.PriceMinor, money.INR),
		TaxBps:        in.TaxBps,
		PackagingFee:  money.FromMinor(in.PackagingFeeMinor, money.INR),
		FoodType:      domain.FoodType(in.FoodType),
		SpiceLevel:    in.SpiceLevel,
		Allergens:     in.Allergens,
		Serves:        in.Serves,
		PrepMinutes:   in.PrepMinutes,
		DisplayOrder:  in.DisplayOrder,
		IsAvailable:   in.IsAvailable,
		IsRecommended: in.IsRecommended,
		UpdatedAt:     now,
	}
	if in.Allergens == nil {
		item.Allergens = []string{}
	}
	if in.ComparePriceMinor != nil {
		cp := money.FromMinor(*in.ComparePriceMinor, money.INR)
		item.ComparePrice = &cp
	}
	if item.Serves <= 0 {
		item.Serves = 1
	}

	if err := item.Validate(); err != nil {
		return nil, err
	}

	isNew := in.ID == nil
	if isNew {
		item.ID = id.New()
		item.CreatedAt = now
		item.Version = 1
		if err := s.menus.CreateItem(ctx, item); err != nil {
			return nil, err
		}
	} else {
		item.ID = *in.ID
		item.Version = in.Version
		if err := s.menus.UpdateItem(ctx, item); err != nil {
			return nil, err
		}
	}

	if isNew {
		if evt, err := eventbus.New(contracts.EventMenuItemCreated, item.ID, map[string]any{
			"kitchen_id":   item.KitchenID,
			"menu_item_id": item.ID,
			"name":         item.Name,
			"price_minor":  item.Price.Minor(),
		}); err == nil {
			_ = s.events.Publish(ctx, evt)
		}
	}

	if err := s.InvalidateMenu(ctx, in.KitchenID); err != nil {
		return nil, fmt.Errorf("catalog: invalidate menu: %w", err)
	}
	return item, nil
}

// ListItemsForKitchen returns every item including hidden and sold-out ones,
// for the staff dashboard.
func (s *CatalogService) ListItemsForKitchen(ctx context.Context, kitchenID id.ID) ([]domain.MenuItem, error) {
	return s.menus.ListItems(ctx, kitchenID, false)
}

// Compile-time proof the module satisfies the contracts other modules depend on.
var (
	_ contracts.PriceQuoter         = (*CatalogService)(nil)
	_ contracts.KitchenAvailability = (*CatalogService)(nil)
)
