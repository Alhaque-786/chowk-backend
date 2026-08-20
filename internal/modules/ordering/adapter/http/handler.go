// Package http is the ordering module's transport adapter.
package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/ordering/app"
	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type Handler struct {
	carts    *app.CartService
	checkout *app.CheckoutService
	kitchen  *app.KitchenOrderService
}

func NewHandler(carts *app.CartService, checkout *app.CheckoutService, kitchen *app.KitchenOrderService) *Handler {
	return &Handler{carts: carts, checkout: checkout, kitchen: kitchen}
}

// CustomerRoutes require an authenticated customer.
func (h *Handler) CustomerRoutes() http.Handler {
	r := chi.NewRouter()

	r.Get("/cart", h.GetCart)
	r.Post("/cart/items", h.AddToCart)
	r.Patch("/cart/items/{lineID}", h.SetQuantity)
	r.Delete("/cart", h.ClearCart)

	r.Post("/checkout", h.Checkout)
	r.Get("/orders/{orderID}", h.GetOrder)
	r.Post("/orders/{orderID}/cancel", h.CancelOrder)
	return r
}

// KitchenRoutes are the staff app.
func (h *Handler) KitchenRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/queue", h.Queue)
	r.Post("/orders/{orderID}/accept", h.transitionTo(domain.StatusConfirmed))
	r.Post("/orders/{orderID}/start", h.transitionTo(domain.StatusPreparing))
	r.Post("/orders/{orderID}/ready", h.transitionTo(domain.StatusReady))
	r.Post("/orders/{orderID}/handover", h.transitionTo(domain.StatusPickedUp))
	r.Post("/orders/{orderID}/delivered", h.transitionTo(domain.StatusDelivered))
	r.Post("/orders/{orderID}/reject", h.transitionTo(domain.StatusRejected))
	return r
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type cartLineResponse struct {
	LineID         string   `json:"line_id"`
	MenuItemID     string   `json:"menu_item_id"`
	Name           string   `json:"name"`
	Quantity       int32    `json:"quantity"`
	UnitPriceMinor int64    `json:"unit_price_minor"`
	LineTotalMinor int64    `json:"line_total_minor"`
	Modifiers      []string `json:"modifiers,omitempty"`
	IsAvailable    bool     `json:"is_available"`
}

type cartResponse struct {
	KitchenID      string             `json:"kitchen_id,omitempty"`
	Lines          []cartLineResponse `json:"lines"`
	ItemCount      int32              `json:"item_count"`
	SubtotalMinor  int64              `json:"subtotal_minor"`
	PackagingMinor int64              `json:"packaging_fee_minor"`
	DeliveryMinor  int64              `json:"delivery_fee_minor"`
	TaxMinor       int64              `json:"tax_minor"`
	TotalMinor     int64              `json:"total_minor"`
	// Unavailable lists items blocking checkout, so the UI can highlight exactly
	// what to remove rather than saying "something is wrong".
	Unavailable  []string `json:"unavailable,omitempty"`
	KitchenOpen  bool     `json:"kitchen_open"`
	ClosedReason string   `json:"closed_reason,omitempty"`
}

func toCartResponse(p *app.CartPreview) cartResponse {
	resp := cartResponse{
		Lines:          make([]cartLineResponse, 0, len(p.Lines)),
		ItemCount:      p.Cart.TotalQuantity(),
		SubtotalMinor:  p.Subtotal.Minor(),
		PackagingMinor: p.Packaging.Minor(),
		DeliveryMinor:  p.Delivery.Minor(),
		TaxMinor:       p.Tax.Minor(),
		TotalMinor:     p.Total.Minor(),
		Unavailable:    p.Unavailable,
		KitchenOpen:    p.KitchenOpen,
		ClosedReason:   p.ClosedReason,
	}
	if !id.IsNil(p.Cart.KitchenID) {
		resp.KitchenID = p.Cart.KitchenID.String()
	}
	for _, l := range p.Lines {
		resp.Lines = append(resp.Lines, cartLineResponse{
			LineID:         l.LineID.String(),
			MenuItemID:     l.MenuItemID.String(),
			Name:           l.Name,
			Quantity:       l.Quantity,
			UnitPriceMinor: l.UnitPrice.Minor(),
			LineTotalMinor: l.LineTotal.Minor(),
			Modifiers:      l.Modifiers,
			IsAvailable:    l.IsAvailable,
		})
	}
	return resp
}

type orderLineResponse struct {
	Name           string   `json:"name"`
	Quantity       int32    `json:"quantity"`
	UnitPriceMinor int64    `json:"unit_price_minor"`
	LineTotalMinor int64    `json:"line_total_minor"`
	Modifiers      []string `json:"modifiers,omitempty"`
	Note           string   `json:"note,omitempty"`
}

type orderResponse struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Status string `json:"status"`

	Lines []orderLineResponse `json:"lines"`

	SubtotalMinor  int64 `json:"subtotal_minor"`
	PackagingMinor int64 `json:"packaging_fee_minor"`
	DeliveryMinor  int64 `json:"delivery_fee_minor"`
	DiscountMinor  int64 `json:"discount_minor"`
	TaxMinor       int64 `json:"tax_minor"`
	TipMinor       int64 `json:"tip_minor"`
	TotalMinor     int64 `json:"total_minor"`

	PaymentMethod string `json:"payment_method"`
	CouponCode    string `json:"coupon_code,omitempty"`

	DeliveryAddress string `json:"delivery_address"`
	CustomerNote    string `json:"customer_note,omitempty"`

	PromisedAt   *time.Time `json:"promised_at,omitempty"`
	ConfirmedAt  *time.Time `json:"confirmed_at,omitempty"`
	DeliveredAt  *time.Time `json:"delivered_at,omitempty"`
	CancelledAt  *time.Time `json:"cancelled_at,omitempty"`
	CancelReason string     `json:"cancel_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`

	// CanCancel lets the UI show or hide the cancel button without duplicating
	// the business rule client-side.
	CanCancel bool `json:"can_cancel"`
}

func toOrderResponse(o *domain.Order) orderResponse {
	resp := orderResponse{
		ID:             o.ID.String(),
		Number:         o.Number,
		Status:         string(o.Status),
		Lines:          make([]orderLineResponse, 0, len(o.Lines)),
		SubtotalMinor:  o.Subtotal.Minor(),
		PackagingMinor: o.PackagingFee.Minor(),
		DeliveryMinor:  o.DeliveryFee.Minor(),
		DiscountMinor:  o.Discount.Minor(),
		TaxMinor:       o.TaxAmount.Minor(),
		TipMinor:       o.Tip.Minor(),
		TotalMinor:     o.Total.Minor(),
		PaymentMethod:  string(o.PaymentMethod),
		CouponCode:     o.CouponCode,
		CustomerNote:   o.CustomerNote,
		PromisedAt:     o.PromisedAt,
		ConfirmedAt:    o.ConfirmedAt,
		DeliveredAt:    o.DeliveredAt,
		CancelledAt:    o.CancelledAt,
		CancelReason:   o.CancelReason,
		CreatedAt:      o.CreatedAt,
		CanCancel:      o.CanCustomerCancel(time.Now()) == nil,
	}

	parts := []string{o.DeliveryLine1, o.DeliveryLine2, o.DeliveryLandmark, o.DeliveryCity, o.DeliveryPincode}
	nonEmpty := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	resp.DeliveryAddress = strings.Join(nonEmpty, ", ")

	for _, l := range o.Lines {
		lr := orderLineResponse{
			Name:           l.Name,
			Quantity:       l.Quantity,
			UnitPriceMinor: l.UnitPrice.Minor(),
			LineTotalMinor: l.LineTotal.Minor(),
			Note:           l.Note,
		}
		for _, m := range l.Modifiers {
			lr.Modifiers = append(lr.Modifiers, m.Name)
		}
		resp.Lines = append(resp.Lines, lr)
	}
	return resp
}

// ---------------------------------------------------------------------------
// Cart handlers
// ---------------------------------------------------------------------------

func (h *Handler) GetCart(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	preview, err := h.carts.Preview(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toCartResponse(preview))
}

type addToCartRequest struct {
	KitchenID   string   `json:"kitchen_id"`
	MenuItemID  string   `json:"menu_item_id"`
	Quantity    int32    `json:"quantity"`
	ModifierIDs []string `json:"modifier_ids"`
	Note        string   `json:"note"`
}

func (r *addToCartRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please check the highlighted fields.")
	bad := false
	if r.KitchenID == "" {
		err.WithField("kitchen_id", "required")
		bad = true
	}
	if r.MenuItemID == "" {
		err.WithField("menu_item_id", "required")
		bad = true
	}
	if r.Quantity <= 0 {
		// Default rather than reject: "add to cart" without a quantity means one.
		r.Quantity = 1
	}
	if bad {
		return err
	}
	return nil
}

func (h *Handler) AddToCart(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req addToCartRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	kitchenID, err := id.Parse(req.KitchenID)
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That kitchen id is not valid."))
		return
	}
	menuItemID, err := id.Parse(req.MenuItemID)
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That item id is not valid."))
		return
	}

	modifierIDs := make([]id.ID, 0, len(req.ModifierIDs))
	for _, raw := range req.ModifierIDs {
		m, err := id.Parse(raw)
		if err != nil {
			httpx.Error(w, r, apperr.Invalid("INVALID_ID", "A modifier id is not valid."))
			return
		}
		modifierIDs = append(modifierIDs, m)
	}

	if _, err := h.carts.AddToCart(r.Context(), app.AddToCartInput{
		UserID:      p.UserID,
		KitchenID:   kitchenID,
		MenuItemID:  menuItemID,
		Quantity:    req.Quantity,
		ModifierIDs: modifierIDs,
		Note:        req.Note,
	}); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Return the priced cart, not just the raw one: the client needs the new
	// total immediately and should not have to make a second call for it.
	preview, err := h.carts.Preview(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toCartResponse(preview))
}

type setQuantityRequest struct {
	Quantity int32 `json:"quantity"`
}

func (h *Handler) SetQuantity(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	lineID, err := id.Parse(chi.URLParam(r, "lineID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That cart line id is not valid."))
		return
	}

	var req setQuantityRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if _, err := h.carts.SetQuantity(r.Context(), p.UserID, lineID, req.Quantity); err != nil {
		httpx.Error(w, r, err)
		return
	}
	preview, err := h.carts.Preview(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toCartResponse(preview))
}

func (h *Handler) ClearCart(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.carts.Clear(r.Context(), p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// ---------------------------------------------------------------------------
// Checkout
// ---------------------------------------------------------------------------

type checkoutRequest struct {
	AddressID     string `json:"address_id"`
	PaymentMethod string `json:"payment_method"`
	CouponCode    string `json:"coupon_code"`
	TipMinor      int64  `json:"tip_minor"`
	CustomerNote  string `json:"customer_note"`
}

func (r *checkoutRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please check the highlighted fields.")
	bad := false

	if r.AddressID == "" {
		err.WithField("address_id", "required")
		bad = true
	}
	switch domain.PaymentMethod(r.PaymentMethod) {
	case domain.PaymentUPI, domain.PaymentCard, domain.PaymentNetBanking,
		domain.PaymentWallet, domain.PaymentCOD:
	case "":
		err.WithField("payment_method", "required")
		bad = true
	default:
		err.WithField("payment_method", "unsupported")
		bad = true
	}
	if r.TipMinor < 0 {
		err.WithField("tip_minor", "must_not_be_negative")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

type checkoutResponse struct {
	Order orderResponse `json:"order"`
	// Payment is present only for prepaid orders; the client hands ClientSecret
	// to the gateway SDK.
	Payment *paymentIntentResponse `json:"payment,omitempty"`
}

type paymentIntentResponse struct {
	PaymentID      string `json:"payment_id"`
	ProviderRef    string `json:"provider_ref"`
	ClientSecret   string `json:"client_secret"`
	Status         string `json:"status"`
	RequiresAction bool   `json:"requires_action"`
}

// Checkout turns the cart into an order.
//
// Mounted behind the Idempotency middleware: a retried request on a flaky mobile
// network must replay the first response, not create a second order.
func (h *Handler) Checkout(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req checkoutRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	addressID, err := id.Parse(req.AddressID)
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That address id is not valid."))
		return
	}

	result, err := h.checkout.Checkout(r.Context(), app.CheckoutInput{
		UserID:        p.UserID,
		AddressID:     addressID,
		PaymentMethod: domain.PaymentMethod(req.PaymentMethod),
		CouponCode:    strings.ToUpper(strings.TrimSpace(req.CouponCode)),
		TipMinor:      req.TipMinor,
		CustomerNote:  req.CustomerNote,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	resp := checkoutResponse{Order: toOrderResponse(result.Order)}
	if result.Payment != nil {
		resp.Payment = &paymentIntentResponse{
			PaymentID:      result.Payment.PaymentID.String(),
			ProviderRef:    result.Payment.ProviderRef,
			ClientSecret:   result.Payment.ClientSecret,
			Status:         result.Payment.Status,
			RequiresAction: result.Payment.RequiresAction,
		}
	}
	httpx.Created(w, resp)
}

func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	orderID, err := id.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That order id is not valid."))
		return
	}

	order, err := h.checkout.GetOrder(r.Context(), p.UserID, orderID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toOrderResponse(order))
}

type cancelRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	orderID, err := id.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That order id is not valid."))
		return
	}

	var req cancelRequest
	// A body is optional here: cancelling without giving a reason is fine.
	_ = httpx.Decode(r, &req)

	order, err := h.checkout.CancelByCustomer(r.Context(), p.UserID, orderID, req.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toOrderResponse(order))
}

// ---------------------------------------------------------------------------
// Kitchen handlers
// ---------------------------------------------------------------------------

func (h *Handler) Queue(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if id.IsNil(p.KitchenID) {
		httpx.Error(w, r, apperr.Forbidden("NO_KITCHEN_SCOPE",
			"Your account is not linked to a kitchen."))
		return
	}

	orders, err := h.kitchen.Queue(r.Context(), p.KitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]orderResponse, 0, len(orders))
	for _, o := range orders {
		out = append(out, toOrderResponse(o))
	}
	httpx.OK(w, map[string]any{"orders": out})
}

type transitionRequest struct {
	Reason string `json:"reason"`
}

// transitionTo builds a handler for one status transition.
//
// A closure rather than six near-identical handlers: the only thing that varies
// is the target status, and the domain state machine already owns legality.
func (h *Handler) transitionTo(next domain.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := httpx.MustPrincipal(r.Context())
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if id.IsNil(p.KitchenID) {
			httpx.Error(w, r, apperr.Forbidden("NO_KITCHEN_SCOPE",
				"Your account is not linked to a kitchen."))
			return
		}
		orderID, err := id.Parse(chi.URLParam(r, "orderID"))
		if err != nil {
			httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That order id is not valid."))
			return
		}

		var req transitionRequest
		_ = httpx.Decode(r, &req)

		// A rejection without a reason is unauditable, and the DB constraint would
		// reject it anyway — fail here with a message a human can act on.
		if next == domain.StatusRejected && strings.TrimSpace(req.Reason) == "" {
			httpx.Error(w, r, apperr.Invalid("REASON_REQUIRED",
				"Please give a reason for rejecting this order."))
			return
		}

		actorRole := "kitchen_staff"
		if p.HasRole(httpx.RoleKitchenAdmin) {
			actorRole = "kitchen_admin"
		}

		order, err := h.kitchen.Transition(r.Context(), app.TransitionInput{
			OrderID:    orderID,
			KitchenID:  p.KitchenID,
			NextStatus: next,
			Reason:     req.Reason,
			ActorID:    p.UserID,
			ActorRole:  actorRole,
		})
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, toOrderResponse(order))
	}
}
