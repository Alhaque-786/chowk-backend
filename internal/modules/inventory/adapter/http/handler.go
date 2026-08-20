// Package http is the inventory module's transport adapter.
package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/inventory/app"
	"github.com/zevoxa/chowk-backend/internal/modules/inventory/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type Handler struct {
	inventory *app.InventoryService
}

func NewHandler(inventory *app.InventoryService) *Handler {
	return &Handler{inventory: inventory}
}

// KitchenRoutes are the staff stock screens.
func (h *Handler) KitchenRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/ingredients", h.ListIngredients)
	r.Get("/low-stock", h.ListLowStock)
	r.Post("/ingredients/{ingredientID}/receive", h.ReceiveStock)
	r.Post("/ingredients/{ingredientID}/adjust", h.AdjustStock)
	r.Get("/ingredients/{ingredientID}/movements", h.ListMovements)
	return r
}

type ingredientResponse struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	SKU      string  `json:"sku,omitempty"`
	Unit     string  `json:"unit"`
	OnHand   float64 `json:"on_hand"`
	Reserved float64 `json:"reserved"`
	// Available is what can still be promised. Surfaced explicitly so staff do
	// not have to subtract, and so the number they see is the one checkout uses.
	Available     float64 `json:"available"`
	ReorderLevel  float64 `json:"reorder_level"`
	NeedsReorder  bool    `json:"needs_reorder"`
	UnitCostMinor int64   `json:"unit_cost_minor"`
	SupplierName  string  `json:"supplier_name,omitempty"`
}

func toIngredientResponse(i *domain.Ingredient) ingredientResponse {
	return ingredientResponse{
		ID: i.ID.String(), Name: i.Name, SKU: i.SKU, Unit: string(i.Unit),
		OnHand: i.OnHand, Reserved: i.Reserved, Available: i.Available(),
		ReorderLevel: i.ReorderLevel, NeedsReorder: i.NeedsReorder(),
		UnitCostMinor: i.UnitCost.Minor(), SupplierName: i.SupplierName,
	}
}

func (h *Handler) ListIngredients(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := kitchenScope(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.inventory.ListForKitchen(r.Context(), kitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]ingredientResponse, 0, len(items))
	for _, i := range items {
		out = append(out, toIngredientResponse(i))
	}
	httpx.OK(w, map[string]any{"ingredients": out})
}

// ListLowStock is the "what do I need to buy" screen.
func (h *Handler) ListLowStock(w http.ResponseWriter, r *http.Request) {
	kitchenID, err := kitchenScope(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, err := h.inventory.ListLowStock(r.Context(), kitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]ingredientResponse, 0, len(items))
	for _, i := range items {
		out = append(out, toIngredientResponse(i))
	}
	httpx.OK(w, map[string]any{"ingredients": out})
}

type receiveRequest struct {
	Quantity      float64 `json:"quantity"`
	UnitCostMinor int64   `json:"unit_cost_minor"`
	Note          string  `json:"note"`
}

func (r *receiveRequest) Validate() error {
	if r.Quantity <= 0 {
		return apperr.Invalid("INVALID_QUANTITY", "Received quantity must be positive.").
			WithField("quantity", "must_be_positive")
	}
	return nil
}

func (h *Handler) ReceiveStock(w http.ResponseWriter, r *http.Request) {
	if _, err := kitchenScope(r); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ingredientID, err := pathID(r, "ingredientID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p, _ := httpx.PrincipalFrom(r.Context())

	var req receiveRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.inventory.ReceiveStock(r.Context(), app.ReceiveStockInput{
		IngredientID:  ingredientID,
		Quantity:      req.Quantity,
		UnitCostMinor: req.UnitCostMinor,
		Note:          req.Note,
		ActorID:       p.UserID,
	}); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, map[string]any{"received": req.Quantity})
}

type adjustRequest struct {
	// NewOnHand is the COUNTED quantity, not a delta. Staff count what is on the
	// shelf; making them compute a difference is how counts get entered wrong.
	NewOnHand float64 `json:"new_on_hand"`
	Reason    string  `json:"reason"`
	IsWastage bool    `json:"is_wastage"`
}

func (r *adjustRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please check the fields.")
	bad := false
	if r.NewOnHand < 0 {
		err.WithField("new_on_hand", "must_not_be_negative")
		bad = true
	}
	if r.Reason == "" {
		// Adjustments are where shrinkage hides, so an unexplained one is refused.
		err.WithField("reason", "required")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

func (h *Handler) AdjustStock(w http.ResponseWriter, r *http.Request) {
	if _, err := kitchenScope(r); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ingredientID, err := pathID(r, "ingredientID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p, _ := httpx.PrincipalFrom(r.Context())

	var req adjustRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.inventory.AdjustStock(r.Context(), app.AdjustStockInput{
		IngredientID: ingredientID,
		NewOnHand:    req.NewOnHand,
		Reason:       req.Reason,
		IsWastage:    req.IsWastage,
		ActorID:      p.UserID,
	}); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, map[string]any{"on_hand": req.NewOnHand})
}

type movementResponse struct {
	Type          string  `json:"type"`
	Quantity      float64 `json:"quantity"`
	BalanceAfter  float64 `json:"balance_after"`
	ReferenceType string  `json:"reference_type,omitempty"`
	Note          string  `json:"note,omitempty"`
	CreatedAt     string  `json:"created_at"`
}

// ListMovements is the ledger view: how did we get to this balance.
func (h *Handler) ListMovements(w http.ResponseWriter, r *http.Request) {
	if _, err := kitchenScope(r); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ingredientID, err := pathID(r, "ingredientID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	movements, err := h.inventory.ListMovements(r.Context(), ingredientID, httpx.QueryInt(r, "limit", 50))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]movementResponse, 0, len(movements))
	for _, m := range movements {
		out = append(out, movementResponse{
			Type: string(m.Type), Quantity: m.Quantity, BalanceAfter: m.BalanceAfter,
			ReferenceType: m.ReferenceType, Note: m.Note,
			CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	httpx.OK(w, map[string]any{"movements": out})
}

// kitchenScope resolves the caller's kitchen.
//
// Staff are pinned to the kitchen on their token; there is no kitchen id in the
// URL to tamper with, which removes the IDOR surface entirely rather than
// guarding against it.
func kitchenScope(r *http.Request) (id.ID, error) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return id.Nil, err
	}
	if id.IsNil(p.KitchenID) {
		return id.Nil, apperr.Forbidden("NO_KITCHEN_SCOPE",
			"Your account is not linked to a kitchen.")
	}
	return p.KitchenID, nil
}

func pathID(r *http.Request, param string) (id.ID, error) {
	parsed, err := id.Parse(chi.URLParam(r, param))
	if err != nil {
		return id.Nil, apperr.Invalid("INVALID_ID", "That "+param+" is not valid.")
	}
	return parsed, nil
}
