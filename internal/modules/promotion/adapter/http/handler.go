// Package http is the promotion module's transport adapter.
package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion/app"
	"github.com/zevoxa/chowk-backend/internal/modules/promotion/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

type Handler struct {
	promotions *app.PromotionService
}

func NewHandler(promotions *app.PromotionService) *Handler {
	return &Handler{promotions: promotions}
}

func (h *Handler) CustomerRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.ListOffers)
	r.Post("/check", h.CheckCode)
	return r
}

func (h *Handler) AdminRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.ListAll)
	r.Post("/", h.Create)
	r.Delete("/{couponID}", h.Deactivate)
	return r
}

type offerResponse struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	// MinOrderMinor lets the UI say "add Rs50 more to use this" rather than just
	// greying the offer out with no explanation.
	MinOrderMinor  int64  `json:"min_order_minor"`
	FirstOrderOnly bool   `json:"first_order_only"`
	ValidUntil     string `json:"valid_until,omitempty"`
	// Discount internals (type, bps, funding) are NOT exposed: how a discount is
	// computed and who pays for it is commercial information.
}

func toOfferResponse(c *domain.Coupon) offerResponse {
	resp := offerResponse{
		Code: c.Code, Description: c.Description,
		MinOrderMinor: c.MinOrder.Minor(), FirstOrderOnly: c.FirstOrderOnly,
	}
	if c.ValidUntil != nil {
		resp.ValidUntil = c.ValidUntil.Format(time.RFC3339)
	}
	return resp
}

// ListOffers is the customer-facing offers screen.
func (h *Handler) ListOffers(w http.ResponseWriter, r *http.Request) {
	kitchenID := id.Nil
	if raw := r.URL.Query().Get("kitchen_id"); raw != "" {
		parsed, err := id.Parse(raw)
		if err != nil {
			httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That kitchen id is not valid."))
			return
		}
		kitchenID = parsed
	}

	coupons, err := h.promotions.ListForCustomer(r.Context(), kitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]offerResponse, 0, len(coupons))
	for _, c := range coupons {
		out = append(out, toOfferResponse(c))
	}
	httpx.OK(w, map[string]any{"offers": out})
}

type checkCodeRequest struct {
	Code          string `json:"code"`
	KitchenID     string `json:"kitchen_id"`
	SubtotalMinor int64  `json:"subtotal_minor"`
}

func (r *checkCodeRequest) Validate() error {
	if strings.TrimSpace(r.Code) == "" {
		return apperr.Invalid("MISSING_CODE", "Please enter a code.").WithField("code", "required")
	}
	if r.SubtotalMinor <= 0 {
		return apperr.Invalid("INVALID_SUBTOTAL", "Your cart is empty.")
	}
	return nil
}

type checkCodeResponse struct {
	Valid         bool   `json:"valid"`
	Code          string `json:"code,omitempty"`
	Description   string `json:"description,omitempty"`
	DiscountMinor int64  `json:"discount_minor"`
	Reason        string `json:"reason,omitempty"`
}

// CheckCode previews a coupon WITHOUT consuming it.
//
// This endpoint exists precisely because Calculate is read-only: a customer
// typing a code to see what it does must not burn one of the available uses.
// Merging preview and redemption is how a flash sale sells out to people who
// never ordered.
func (h *Handler) CheckCode(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req checkCodeRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	kitchenID := id.Nil
	if req.KitchenID != "" {
		parsed, err := id.Parse(req.KitchenID)
		if err != nil {
			httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That kitchen id is not valid."))
			return
		}
		kitchenID = parsed
	}

	result, err := h.promotions.Calculate(r.Context(), contracts.DiscountRequest{
		UserID:     p.UserID,
		KitchenID:  kitchenID,
		CouponCode: req.Code,
		Subtotal:   money.FromMinor(req.SubtotalMinor, money.INR),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// 200 with valid=false, not an error: an ineligible coupon is expected traffic
	// and the client needs the reason to display, not an exception to handle.
	httpx.OK(w, checkCodeResponse{
		Valid:         result.Applied,
		Code:          result.Code,
		Description:   result.Description,
		DiscountMinor: result.DiscountAmount.Minor(),
		Reason:        result.RejectReason,
	})
}

type createCouponRequest struct {
	Code                  string   `json:"code"`
	Description           string   `json:"description"`
	DiscountType          string   `json:"discount_type"`
	DiscountValue         int64    `json:"discount_value"`
	MaxDiscountMinor      *int64   `json:"max_discount_minor"`
	MinOrderMinor         int64    `json:"min_order_minor"`
	KitchenIDs            []string `json:"kitchen_ids"`
	FirstOrderOnly        bool     `json:"first_order_only"`
	ValidUntil            *string  `json:"valid_until"`
	MaxRedemptions        *int32   `json:"max_redemptions"`
	MaxRedemptionsPerUser int32    `json:"max_redemptions_per_user"`
	FundedBy              string   `json:"funded_by"`
}

func (r *createCouponRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please check the highlighted fields.")
	bad := false

	if strings.TrimSpace(r.Code) == "" {
		err.WithField("code", "required")
		bad = true
	}
	switch r.DiscountType {
	case "percentage":
		// Refused here as well as in the domain and the database. A percentage
		// coupon with no cap is the easiest way to lose a lot of money quickly,
		// so it is worth being unmissable.
		if r.MaxDiscountMinor == nil {
			err.WithField("max_discount_minor", "required_for_percentage")
			bad = true
		}
	case "flat", "free_delivery":
	default:
		err.WithField("discount_type", "must_be_percentage_flat_or_free_delivery")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req createCouponRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	kitchenIDs := make([]id.ID, 0, len(req.KitchenIDs))
	for _, raw := range req.KitchenIDs {
		parsed, err := id.Parse(raw)
		if err != nil {
			httpx.Error(w, r, apperr.Invalid("INVALID_ID", "A kitchen id is not valid."))
			return
		}
		kitchenIDs = append(kitchenIDs, parsed)
	}

	in := app.CreateCouponInput{
		Code: req.Code, Description: req.Description,
		DiscountType: req.DiscountType, DiscountValue: req.DiscountValue,
		MaxDiscountMinor: req.MaxDiscountMinor, MinOrderMinor: req.MinOrderMinor,
		KitchenIDs: kitchenIDs, FirstOrderOnly: req.FirstOrderOnly,
		MaxRedemptions:        req.MaxRedemptions,
		MaxRedemptionsPerUser: req.MaxRedemptionsPerUser,
		FundedBy:              req.FundedBy, CreatedBy: p.UserID,
	}
	if req.ValidUntil != nil && *req.ValidUntil != "" {
		parsed, err := time.Parse(time.RFC3339, *req.ValidUntil)
		if err != nil {
			httpx.Error(w, r, apperr.Invalid("INVALID_DATE", "valid_until must be an RFC3339 timestamp."))
			return
		}
		in.ValidUntil = &parsed
	}

	coupon, err := h.promotions.CreateCoupon(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, adminCouponResponse(coupon))
}

func (h *Handler) ListAll(w http.ResponseWriter, r *http.Request) {
	coupons, err := h.promotions.ListAll(r.Context(), httpx.QueryInt(r, "limit", 50))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(coupons))
	for _, c := range coupons {
		out = append(out, adminCouponResponse(c))
	}
	httpx.OK(w, map[string]any{"coupons": out})
}

// Deactivate stops a coupon. It is never deleted: existing redemptions reference
// it, and what a customer was promised must remain on record.
func (h *Handler) Deactivate(w http.ResponseWriter, r *http.Request) {
	couponID, err := id.Parse(chi.URLParam(r, "couponID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That coupon id is not valid."))
		return
	}
	if err := h.promotions.Deactivate(r.Context(), couponID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// adminCouponResponse exposes the full picture, including usage and funding,
// which the customer view deliberately hides.
func adminCouponResponse(c *domain.Coupon) map[string]any {
	out := map[string]any{
		"id":                       c.ID.String(),
		"code":                     c.Code,
		"description":              c.Description,
		"discount_type":            string(c.DiscountType),
		"discount_value":           c.DiscountValue,
		"min_order_minor":          c.MinOrder.Minor(),
		"first_order_only":         c.FirstOrderOnly,
		"max_redemptions_per_user": c.MaxRedemptionsPerUser,
		"redemption_count":         c.RedemptionCount,
		"funded_by":                string(c.FundedBy),
		"is_active":                c.IsActive,
	}
	if c.MaxDiscount != nil {
		out["max_discount_minor"] = c.MaxDiscount.Minor()
	}
	if c.MaxRedemptions != nil {
		out["max_redemptions"] = *c.MaxRedemptions
	}
	if c.ValidUntil != nil {
		out["valid_until"] = c.ValidUntil.Format(time.RFC3339)
	}
	return out
}
