// Package http is the catalog module's transport adapter.
package http

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/catalog/app"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type Handler struct {
	catalog *app.CatalogService
}

func NewHandler(catalog *app.CatalogService) *Handler {
	return &Handler{catalog: catalog}
}

// PublicRoutes are the customer-facing browse endpoints. No auth required — a
// customer must be able to see the menu before signing in, or the funnel is
// broken.
func (h *Handler) PublicRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/nearby", h.FindNearby)
	r.Get("/{kitchenID}", h.GetKitchen)
	r.Get("/{kitchenID}/menu", h.GetMenu)
	r.Get("/{kitchenID}/search", h.SearchItems)
	return r
}

// KitchenRoutes are for staff. Mounted behind Authenticate + RequireRole.
func (h *Handler) KitchenRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/{kitchenID}/items", h.ListItemsForStaff)
	r.Post("/{kitchenID}/items", h.CreateItem)
	r.Patch("/{kitchenID}/items/{itemID}", h.UpdateItem)
	r.Post("/{kitchenID}/accepting-orders", h.SetAcceptingOrders)
	return r
}

// ---------------------------------------------------------------------------
// Response DTOs
//
// Separate from domain entities so an internal field added later cannot leak.
// ---------------------------------------------------------------------------

type kitchenResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Description string   `json:"description,omitempty"`
	Cuisines    []string `json:"cuisines"`
	// omitempty, so a kitchen with no photograph sends no key at all rather than
	// an empty string the client has to test. Absent means "fall back".
	CoverImageURL  string  `json:"cover_image_url,omitempty"`
	City           string  `json:"city,omitempty"`
	Latitude       float64 `json:"latitude"`
	Longitude      float64 `json:"longitude"`
	IsOpen         bool    `json:"is_open"`
	ClosedReason   string  `json:"closed_reason,omitempty"`
	AvgPrepMinutes int32   `json:"avg_prep_minutes"`
	MinOrderMinor  int64   `json:"min_order_minor"`
	// omitempty: 0 means the merchant has not set it, and the card must show
	// nothing rather than "Rs0 for two".
	CostForTwoMinor   int64   `json:"cost_for_two_minor,omitempty"`
	PackagingFeeMinor int64   `json:"packaging_fee_minor"`
	RatingAverage     float64 `json:"rating_average"`
	RatingCount       int32   `json:"rating_count"`
	DistanceM         *int32  `json:"distance_m,omitempty"`
	// Commission and merchant terms are deliberately NOT exposed to customers.
}

func toKitchenResponse(k *domain.Kitchen, now time.Time, distanceM *int32) kitchenResponse {
	open, why := k.CanAcceptOrder(now)
	resp := kitchenResponse{
		ID:                k.ID.String(),
		Name:              k.Name,
		Slug:              k.Slug,
		Description:       k.Description,
		Cuisines:          k.Cuisines,
		CoverImageURL:     k.CoverImageURL,
		City:              k.City,
		Latitude:          k.Latitude,
		Longitude:         k.Longitude,
		IsOpen:            open,
		AvgPrepMinutes:    k.AvgPrepMinutes,
		MinOrderMinor:     k.MinOrder.Minor(),
		CostForTwoMinor:   k.CostForTwo.Minor(),
		PackagingFeeMinor: k.PackagingFee.Minor(),
		RatingAverage:     k.RatingAverage,
		RatingCount:       k.RatingCount,
		DistanceM:         distanceM,
	}
	if !open {
		resp.ClosedReason = why.CustomerMessage(k.Name)
	}
	if resp.Cuisines == nil {
		resp.Cuisines = []string{}
	}
	return resp
}

type modifierResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceMinor int64  `json:"price_minor"`
}

type modifierGroupResponse struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	MinSelect  int16              `json:"min_select"`
	MaxSelect  int16              `json:"max_select"`
	IsRequired bool               `json:"is_required"`
	Modifiers  []modifierResponse `json:"modifiers"`
}

type itemResponse struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	ImageURL          string   `json:"image_url,omitempty"`
	PriceMinor        int64    `json:"price_minor"`
	ComparePriceMinor *int64   `json:"compare_price_minor,omitempty"`
	DiscountBps       int64    `json:"discount_bps,omitempty"`
	TaxBps            int32    `json:"tax_bps"`
	FoodType          string   `json:"food_type"`
	SpiceLevel        int16    `json:"spice_level"`
	Allergens         []string `json:"allergens"`
	Calories          *int32   `json:"calories,omitempty"`
	Serves            int16    `json:"serves"`
	PrepMinutes       int32    `json:"prep_minutes"`
	IsRecommended     bool     `json:"is_recommended"`
	RatingAverage     float64  `json:"rating_average"`
	// Staff-only fields. Omitted from the customer menu because a customer never
	// sees a hidden or sold-out dish at all.
	IsAvailable *bool  `json:"is_available,omitempty"`
	IsSoldOut   *bool  `json:"is_sold_out,omitempty"`
	Version     *int32 `json:"version,omitempty"`

	ModifierGroups []modifierGroupResponse `json:"modifier_groups,omitempty"`
}

func toItemResponse(m *domain.MenuItem, forStaff bool) itemResponse {
	resp := itemResponse{
		ID:            m.ID.String(),
		Name:          m.Name,
		Description:   m.Description,
		ImageURL:      m.ImageURL,
		PriceMinor:    m.Price.Minor(),
		DiscountBps:   m.DiscountBps(),
		TaxBps:        m.TaxBps,
		FoodType:      string(m.FoodType),
		SpiceLevel:    m.SpiceLevel,
		Allergens:     m.Allergens,
		Calories:      m.Calories,
		Serves:        m.Serves,
		PrepMinutes:   m.PrepMinutes,
		IsRecommended: m.IsRecommended,
		RatingAverage: m.RatingAverage,
	}
	if resp.Allergens == nil {
		resp.Allergens = []string{}
	}
	if m.ComparePrice != nil {
		v := m.ComparePrice.Minor()
		resp.ComparePriceMinor = &v
	}
	// Staff need the version to send back on update (optimistic locking) and the
	// two availability flags to know which one hid the dish.
	if forStaff {
		resp.IsAvailable = &m.IsAvailable
		resp.IsSoldOut = &m.IsSoldOut
		resp.Version = &m.Version
	}

	for _, g := range m.ModifierGroups {
		gr := modifierGroupResponse{
			ID:         g.ID.String(),
			Name:       g.Name,
			MinSelect:  g.MinSelect,
			MaxSelect:  g.MaxSelect,
			IsRequired: g.IsRequired(),
			Modifiers:  make([]modifierResponse, 0, len(g.Modifiers)),
		}
		for _, mod := range g.Modifiers {
			if !mod.IsAvailable && !forStaff {
				continue
			}
			gr.Modifiers = append(gr.Modifiers, modifierResponse{
				ID: mod.ID.String(), Name: mod.Name, PriceMinor: mod.Price.Minor(),
			})
		}
		resp.ModifierGroups = append(resp.ModifierGroups, gr)
	}
	return resp
}

type categoryResponse struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Items       []itemResponse `json:"items"`
}

type menuResponse struct {
	Kitchen    kitchenResponse    `json:"kitchen"`
	Categories []categoryResponse `json:"categories"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) FindNearby(w http.ResponseWriter, r *http.Request) {
	lat, err := floatParam(r, "lat")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	lng, err := floatParam(r, "lng")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	radius := int32(httpx.QueryInt(r, "radius_m", 10000))
	limit := httpx.QueryInt(r, "limit", 20)

	results, err := h.catalog.FindNearby(r.Context(), lat, lng, radius, limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	now := time.Now()
	out := make([]kitchenResponse, 0, len(results))
	for _, res := range results {
		d := res.DistanceM
		out = append(out, toKitchenResponse(res.Kitchen, now, &d))
	}
	httpx.OK(w, map[string]any{"kitchens": out})
}

func (h *Handler) GetKitchen(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := h.kitchenIDFrom(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	kitchen, err := h.catalog.GetKitchen(r.Context(), kitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toKitchenResponse(kitchen, time.Now(), nil))
}

// GetMenu is the hottest endpoint in the system. Served from cache.
func (h *Handler) GetMenu(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := h.kitchenIDFrom(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	menu, err := h.catalog.GetMenu(r.Context(), kitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	now := time.Now()
	resp := menuResponse{
		Kitchen:    toKitchenResponse(menu.Kitchen, now, nil),
		Categories: make([]categoryResponse, 0, len(menu.Categories)+1),
	}

	for _, c := range menu.Categories {
		items := menu.ItemsByCategory[c.ID]
		// Skip empty categories: an empty section on a menu looks like a bug to a
		// customer.
		if len(items) == 0 {
			continue
		}
		cr := categoryResponse{
			ID: c.ID.String(), Name: c.Name, Description: c.Description,
			Items: make([]itemResponse, 0, len(items)),
		}
		for i := range items {
			cr.Items = append(cr.Items, toItemResponse(&items[i], false))
		}
		resp.Categories = append(resp.Categories, cr)
	}

	// Uncategorised items still need somewhere to appear, or they are invisible
	// and the kitchen cannot understand why nobody orders them.
	if uncategorised := menu.ItemsByCategory[id.Nil]; len(uncategorised) > 0 {
		cr := categoryResponse{ID: "", Name: "More", Items: make([]itemResponse, 0, len(uncategorised))}
		for i := range uncategorised {
			cr.Items = append(cr.Items, toItemResponse(&uncategorised[i], false))
		}
		resp.Categories = append(resp.Categories, cr)
	}

	httpx.OK(w, resp)
}

func (h *Handler) SearchItems(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := h.kitchenIDFrom(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	items, err := h.catalog.SearchItems(r.Context(), kitchenID, query, httpx.QueryInt(r, "limit", 20))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]itemResponse, 0, len(items))
	for i := range items {
		out = append(out, toItemResponse(&items[i], false))
	}
	httpx.OK(w, map[string]any{"items": out})
}

// ---------------------------------------------------------------------------
// Staff handlers
// ---------------------------------------------------------------------------

func (h *Handler) ListItemsForStaff(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := h.authorizedKitchen(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	items, err := h.catalog.ListItemsForKitchen(r.Context(), kitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]itemResponse, 0, len(items))
	for i := range items {
		out = append(out, toItemResponse(&items[i], true))
	}
	httpx.OK(w, map[string]any{"items": out})
}

type upsertItemRequest struct {
	CategoryID        *string  `json:"category_id"`
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	ImageURL          string   `json:"image_url"`
	PriceMinor        int64    `json:"price_minor"`
	ComparePriceMinor *int64   `json:"compare_price_minor"`
	TaxBps            int32    `json:"tax_bps"`
	PackagingFeeMinor int64    `json:"packaging_fee_minor"`
	FoodType          string   `json:"food_type"`
	SpiceLevel        int16    `json:"spice_level"`
	Allergens         []string `json:"allergens"`
	Serves            int16    `json:"serves"`
	PrepMinutes       int32    `json:"prep_minutes"`
	DisplayOrder      int32    `json:"display_order"`
	IsAvailable       *bool    `json:"is_available"`
	IsRecommended     bool     `json:"is_recommended"`
	// Version is required on update so a stale edit is rejected rather than
	// silently overwriting someone else's change.
	Version int32 `json:"version"`
}

func (r *upsertItemRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please check the highlighted fields.")
	bad := false

	if strings.TrimSpace(r.Name) == "" {
		err.WithField("name", "required")
		bad = true
	}
	if r.PriceMinor <= 0 {
		err.WithField("price_minor", "must_be_positive")
		bad = true
	}
	if r.FoodType == "" {
		// Legally required labelling in India; defaulting it would be wrong.
		err.WithField("food_type", "required")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

func (h *Handler) CreateItem(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := h.authorizedKitchen(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req upsertItemRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	in, err := req.toInput(kitchenID, nil)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	item, err := h.catalog.SaveItem(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, toItemResponse(item, true))
}

func (h *Handler) UpdateItem(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := h.authorizedKitchen(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	itemID, err := id.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That item id is not valid."))
		return
	}

	var req upsertItemRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.Version <= 0 {
		httpx.Error(w, r, apperr.Invalid("VERSION_REQUIRED",
			"A version is required to update an item. Reload it and try again."))
		return
	}

	in, err := req.toInput(kitchenID, &itemID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	item, err := h.catalog.SaveItem(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toItemResponse(item, true))
}

func (r *upsertItemRequest) toInput(kitchenID id.ID, itemID *id.ID) (app.UpsertItemInput, error) {
	in := app.UpsertItemInput{
		ID:                itemID,
		KitchenID:         kitchenID,
		Name:              strings.TrimSpace(r.Name),
		Description:       r.Description,
		ImageURL:          r.ImageURL,
		PriceMinor:        r.PriceMinor,
		ComparePriceMinor: r.ComparePriceMinor,
		TaxBps:            r.TaxBps,
		PackagingFeeMinor: r.PackagingFeeMinor,
		FoodType:          r.FoodType,
		SpiceLevel:        r.SpiceLevel,
		Allergens:         r.Allergens,
		Serves:            r.Serves,
		PrepMinutes:       r.PrepMinutes,
		DisplayOrder:      r.DisplayOrder,
		IsAvailable:       true,
		IsRecommended:     r.IsRecommended,
		Version:           r.Version,
	}
	if r.IsAvailable != nil {
		in.IsAvailable = *r.IsAvailable
	}
	if r.CategoryID != nil && *r.CategoryID != "" {
		parsed, err := id.Parse(*r.CategoryID)
		if err != nil {
			return in, apperr.Invalid("INVALID_ID", "That category id is not valid.")
		}
		in.CategoryID = &parsed
	}
	if in.TaxBps == 0 {
		// 5% GST is the correct default for prepared food; 0 would silently
		// under-collect on every order.
		in.TaxBps = 500
	}
	return in, nil
}

type acceptingOrdersRequest struct {
	Accepting bool   `json:"accepting"`
	Reason    string `json:"reason"`
}

// SetAcceptingOrders is the kill switch staff hit when the kitchen is swamped.
func (h *Handler) SetAcceptingOrders(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := h.authorizedKitchen(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req acceptingOrdersRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.catalog.SetAcceptingOrders(r.Context(), kitchenID, req.Accepting, req.Reason); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, map[string]any{"accepting_orders": req.Accepting, "reason": req.Reason})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) kitchenIDFrom(r *http.Request) (id.ID, error) {
	raw := chi.URLParam(r, "kitchenID")
	parsed, err := id.Parse(raw)
	if err != nil {
		return id.Nil, apperr.Invalid("INVALID_ID", "That kitchen id is not valid.")
	}
	return parsed, nil
}

// authorizedKitchen resolves the kitchen from the URL and verifies the caller is
// allowed to act on it.
//
// This is the IDOR guard. Without it, staff at kitchen A could pass kitchen B's
// id and edit their menu — and once third-party merchants are onboarded, that is
// a breach rather than a bug. Platform admins bypass the check by design.
func (h *Handler) authorizedKitchen(r *http.Request) (id.ID, error) {
	kitchenID, err := h.kitchenIDFrom(r)
	if err != nil {
		return id.Nil, err
	}

	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return id.Nil, err
	}
	if principal.HasRole(httpx.RolePlatformAdmin) {
		return kitchenID, nil
	}
	if principal.KitchenID != kitchenID {
		// "Not found", never "not yours": confirming the id exists but belongs to
		// someone else lets an attacker enumerate other merchants' outlets.
		return id.Nil, apperr.NotFound("KITCHEN_NOT_FOUND", "That kitchen could not be found.")
	}
	return kitchenID, nil
}

func floatParam(r *http.Request, key string) (float64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return 0, apperr.Invalid("MISSING_PARAM", "The "+key+" parameter is required.").
			WithField(key, "required")
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, apperr.Invalid("INVALID_PARAM", "The "+key+" parameter must be a number.").
			WithField(key, "not_a_number")
	}
	return v, nil
}
