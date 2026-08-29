// Package http is the delivery module's transport adapter.
package http

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/app"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

const maxWebhookBody = 256 << 10

type Handler struct {
	dispatch *app.DispatchService
}

func NewHandler(dispatch *app.DispatchService) *Handler {
	return &Handler{dispatch: dispatch}
}

// WebhookRoutes are public and authenticated by HMAC, like payment's. A 3PL has
// no bearer token.
func (h *Handler) WebhookRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.HandleWebhook)
	return r
}

func (h *Handler) CustomerRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/by-order/{orderID}", h.TrackOrder)
	return r
}

func (h *Handler) KitchenRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/board", h.DispatchBoard)
	r.Post("/by-order/{orderID}/status", h.MarkStatus)
	return r
}

// HandleWebhook reads the RAW body, because the HMAC covers the exact bytes sent.
func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("UNREADABLE_BODY", "Could not read the request body.").WithCause(err))
		return
	}

	if err := h.dispatch.ProcessWebhook(r.Context(), rawBody, r.Header.Get("X-Delivery-Signature")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "received"})
}

type deliveryResponse struct {
	Status        string `json:"status"`
	Mode          string `json:"mode"`
	RiderName     string `json:"rider_name,omitempty"`
	RiderPhone    string `json:"rider_phone,omitempty"`
	TrackingURL   string `json:"tracking_url,omitempty"`
	DropETA       string `json:"drop_eta,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	// Provider name and cost are internal: the customer does not need to know
	// which 3PL we use or what we paid.
}

func toDeliveryResponse(d *domain.Delivery) deliveryResponse {
	resp := deliveryResponse{
		Status: string(d.Status), Mode: string(d.Mode),
		RiderName: d.RiderName, RiderPhone: d.RiderPhone,
		TrackingURL: d.TrackingURL, FailureReason: d.FailureReason,
	}
	if d.DropETA != nil {
		resp.DropETA = d.DropETA.Format("2006-01-02T15:04:05Z07:00")
	}
	return resp
}

// TrackOrder is the customer's live tracking view.
func (h *Handler) TrackOrder(w http.ResponseWriter, r *http.Request) {
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

	// The principal's own id, never a value from the request. There is no
	// parameter for "whose order" because there must not be one.
	//
	// Support and platform admins may view any delivery: answering "where is my
	// food" is the job, and they cannot do it through the customer's own account.
	staff := p.HasRole(httpx.RoleSupport, httpx.RolePlatformAdmin)

	delivery, err := h.dispatch.GetForOrder(r.Context(), orderID, p.UserID, staff)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toDeliveryResponse(delivery))
}

// DispatchBoard shows staff what is out and what still needs a rider.
func (h *Handler) DispatchBoard(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if id.IsNil(p.KitchenID) {
		httpx.Error(w, r, apperr.Forbidden("NO_KITCHEN_SCOPE", "Your account is not linked to a kitchen."))
		return
	}

	deliveries, err := h.dispatch.ListActiveForKitchen(r.Context(), p.KitchenID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, map[string]any{
			"order_id":   d.OrderID.String(),
			"status":     string(d.Status),
			"mode":       string(d.Mode),
			"rider_name": d.RiderName,
			"cost_minor": d.Cost.Minor(),
		})
	}
	httpx.OK(w, map[string]any{"deliveries": out})
}

type markStatusRequest struct {
	Status     string `json:"status"`
	RiderName  string `json:"rider_name"`
	RiderPhone string `json:"rider_phone"`
}

// MarkStatus lets staff drive a delivery by hand.
//
// With the manual provider this is the ONLY way a delivery progresses, and even
// with a 3PL a provider outage must not leave staff unable to record that food
// went out.
func (h *Handler) MarkStatus(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if id.IsNil(p.KitchenID) {
		httpx.Error(w, r, apperr.Forbidden("NO_KITCHEN_SCOPE", "Your account is not linked to a kitchen."))
		return
	}
	orderID, err := id.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That order id is not valid."))
		return
	}

	var req markStatusRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	next := domain.Status(req.Status)
	switch next {
	case domain.StatusAssigned, domain.StatusAtPickup, domain.StatusPickedUp,
		domain.StatusDelivered, domain.StatusCancelled, domain.StatusFailed:
	default:
		httpx.Error(w, r, apperr.Invalid("INVALID_STATUS", "That delivery status is not recognised."))
		return
	}

	delivery, err := h.dispatch.MarkStatusManually(r.Context(), orderID, next, req.RiderName, req.RiderPhone)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toDeliveryResponse(delivery))
}
