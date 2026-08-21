// Package http is the notification module's transport adapter.
package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/notification/app"
	"github.com/zevoxa/chowk-backend/internal/modules/notification/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

type Handler struct {
	notifications *app.NotificationService
}

func NewHandler(notifications *app.NotificationService) *Handler {
	return &Handler{notifications: notifications}
}

// CustomerRoutes require an authenticated customer: a device token and a set of
// preferences belong to exactly one person.
func (h *Handler) CustomerRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/devices", h.RegisterDevice)
	r.Delete("/devices", h.UnregisterDevice)
	r.Get("/preferences", h.GetPreferences)
	r.Put("/preferences", h.SavePreferences)
	r.Get("/history", h.History)
	return r
}

type registerDeviceRequest struct {
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	DeviceName string `json:"device_name"`
}

func (r *registerDeviceRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please check the highlighted fields.")
	bad := false
	if r.Token == "" {
		err.WithField("token", "required")
		bad = true
	}
	switch r.Platform {
	case "android", "ios", "web":
	default:
		err.WithField("platform", "must_be_android_ios_or_web")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

// RegisterDevice is called by the app on launch and after a token refresh.
//
// Idempotent by design: the same token re-registered just refreshes it, and
// re-registering after a device changes hands moves it to the new user rather
// than pushing the previous owner's order updates to their phone.
func (h *Handler) RegisterDevice(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req registerDeviceRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.notifications.RegisterDevice(r.Context(), p.UserID, req.Token, req.Platform, req.DeviceName); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, map[string]any{"registered": true})
}

type unregisterDeviceRequest struct {
	Token string `json:"token"`
}

func (h *Handler) UnregisterDevice(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req unregisterDeviceRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.Token == "" {
		httpx.Error(w, r, apperr.Invalid("MISSING_TOKEN", "A device token is required."))
		return
	}

	if err := h.notifications.UnregisterDevice(r.Context(), req.Token); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

type preferencesResponse struct {
	PromotionalPush  bool   `json:"promotional_push"`
	PromotionalSMS   bool   `json:"promotional_sms"`
	PromotionalEmail bool   `json:"promotional_email"`
	QuietHoursStart  *int16 `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd    *int16 `json:"quiet_hours_end,omitempty"`
	// Surfaced so the UI can explain WHY there is no toggle for order updates,
	// rather than a customer hunting for one that does not exist.
	TransactionalNote string `json:"transactional_note"`
}

func toPreferencesResponse(p domain.Preferences) preferencesResponse {
	return preferencesResponse{
		PromotionalPush:   p.PromotionalPush,
		PromotionalSMS:    p.PromotionalSMS,
		PromotionalEmail:  p.PromotionalEmail,
		QuietHoursStart:   p.QuietHoursStart,
		QuietHoursEnd:     p.QuietHoursEnd,
		TransactionalNote: "Order updates are always sent and cannot be turned off.",
	}
}

func (h *Handler) GetPreferences(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	prefs, err := h.notifications.GetPreferences(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toPreferencesResponse(prefs))
}

type savePreferencesRequest struct {
	PromotionalPush  *bool  `json:"promotional_push"`
	PromotionalSMS   *bool  `json:"promotional_sms"`
	PromotionalEmail *bool  `json:"promotional_email"`
	QuietHoursStart  *int16 `json:"quiet_hours_start"`
	QuietHoursEnd    *int16 `json:"quiet_hours_end"`
}

// SavePreferences applies a partial update.
//
// Pointers rather than plain bools so an omitted field keeps its current value.
// With plain bools, a client sending only quiet hours would silently opt the user
// out of every promotional channel.
func (h *Handler) SavePreferences(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req savePreferencesRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	prefs, err := h.notifications.GetPreferences(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if req.PromotionalPush != nil {
		prefs.PromotionalPush = *req.PromotionalPush
	}
	if req.PromotionalSMS != nil {
		prefs.PromotionalSMS = *req.PromotionalSMS
	}
	if req.PromotionalEmail != nil {
		prefs.PromotionalEmail = *req.PromotionalEmail
	}
	prefs.QuietHoursStart = req.QuietHoursStart
	prefs.QuietHoursEnd = req.QuietHoursEnd

	if err := h.notifications.SavePreferences(r.Context(), prefs); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toPreferencesResponse(prefs))
}

type historyEntry struct {
	Channel  string `json:"channel"`
	Template string `json:"template"`
	Body     string `json:"body"`
	Status   string `json:"status"`
	SentAt   string `json:"sent_at,omitempty"`
}

// History lets a customer see what we sent them.
//
// Useful for support ("I never got a message") and it is their data.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	entries, err := h.notifications.History(r.Context(), p.UserID, httpx.QueryInt(r, "limit", 50))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]historyEntry, 0, len(entries))
	for _, e := range entries {
		item := historyEntry{
			Channel: string(e.Channel), Template: e.TemplateKey,
			Body: e.RenderedBody, Status: string(e.Status),
		}
		if e.SentAt != nil {
			item.SentAt = e.SentAt.Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, item)
	}
	httpx.OK(w, map[string]any{"notifications": out})
}
