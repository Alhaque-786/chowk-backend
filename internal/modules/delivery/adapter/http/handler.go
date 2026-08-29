// Package http is the delivery module's transport adapter.
package http

import (
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/app"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

const maxWebhookBody = 256 << 10

type Handler struct {
	locations *app.LocationService
	dispatch  *app.DispatchService
}

func NewHandler(dispatch *app.DispatchService, locations *app.LocationService) *Handler {
	return &Handler{dispatch: dispatch, locations: locations}
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
	r.Get("/by-order/{orderID}/location", h.TrackLocation)
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

// ---------------------------------------------------------------------------
// Rider location
// ---------------------------------------------------------------------------

/*
RiderRoutes is the rider app's surface.

Small on purpose: report where you are, and see what you are carrying. Marking
a pickup or a drop still goes through the kitchen's MarkStatus, because with
the manual provider that is the one path staff already use and a second way to
move the same state machine is a second thing to keep consistent.

Mounted behind RequireRole(rider) in the composition root. Nothing here trusts
a rider id from the request: it comes from the token, every time.
*/
func (h *Handler) RiderRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/deliveries", h.MyDeliveries)
	r.Post("/deliveries/{deliveryID}/location", h.ReportLocation)
	return r
}

type reportLocationRequest struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// AccuracyM is what the handset claimed, in metres. Optional, and worth
	// sending: it is the only way to tell a rider who moved from a phone that lost
	// its fix and guessed.
	AccuracyM float64 `json:"accuracy_m"`
	// RecordedAt is when the DEVICE took the reading, RFC3339. A rider out of
	// signal uploads a burst on reconnect, and ordering by arrival draws a track
	// that teleports. Omitted means now.
	RecordedAt string `json:"recorded_at"`
}

func (h *Handler) ReportLocation(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	deliveryID, err := id.Parse(chi.URLParam(r, "deliveryID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That delivery id is not valid."))
		return
	}

	var req reportLocationRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// A malformed timestamp is treated as absent rather than rejected. The rider
	// cannot fix their handset mid-shift, and refusing their position over a
	// clock format would take them off the map entirely.
	var recordedAt time.Time
	if req.RecordedAt != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, req.RecordedAt); parseErr == nil {
			recordedAt = parsed
		}
	}

	if err := h.locations.Report(r.Context(), app.ReportInput{
		DeliveryID: deliveryID,
		// From the token, never the body.
		RiderID:    p.UserID,
		Latitude:   req.Latitude,
		Longitude:  req.Longitude,
		AccuracyM:  req.AccuracyM,
		RecordedAt: recordedAt,
	}); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// 204 rather than an echo. This is called every few seconds on mobile data;
	// the smallest useful response is the right one, and there is nothing the app
	// does with a body.
	w.WriteHeader(http.StatusNoContent)
}

// MyDeliveries is what a rider is carrying right now.
func (h *Handler) MyDeliveries(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	deliveries, err := h.locations.ActiveForRider(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]deliveryResponse, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, toDeliveryResponse(d))
	}
	httpx.OK(w, map[string]any{"deliveries": out})
}

type liveLocationResponse struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// RecordedAt lets the client say "as of a minute ago" rather than implying the
	// dot is accurate to the second.
	RecordedAt string `json:"recorded_at"`
}

/*
TrackLocation is the customer's live dot.

Returns 204 rather than 404 for every ordinary "nothing to show" case: no
rider yet, nothing reported, the position gone stale, or the order already
delivered. A 404 would be indistinguishable from a wrong url, and the client
would have to guess whether to keep polling. 204 says plainly: nothing now,
ask again.
*/
func (h *Handler) TrackLocation(w http.ResponseWriter, r *http.Request) {
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

	staff := p.HasRole(httpx.RoleSupport, httpx.RolePlatformAdmin)
	position, err := h.locations.ForOrder(r.Context(), orderID, p.UserID, staff)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if position == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	httpx.OK(w, liveLocationResponse{
		Latitude:   position.Latitude,
		Longitude:  position.Longitude,
		RecordedAt: position.RecordedAt.Format(time.RFC3339),
	})
}
