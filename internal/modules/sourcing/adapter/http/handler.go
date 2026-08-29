// Package http is the sourcing module's transport adapter.
package http

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/app"
	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type Handler struct {
	sourcing *app.SourcingService
}

func NewHandler(sourcing *app.SourcingService) *Handler {
	return &Handler{sourcing: sourcing}
}

// PublicRoutes is the submission form's endpoint.
//
// UNAUTHENTICATED BY DESIGN, and this is the decision most likely to be
// second-guessed. Requiring an account to tell us about a juice cart would lose
// most of the leads this module exists to collect: the person best placed to
// refer a cart is the customer who walks past it, and they will not create an
// account first. The composition root mounts this under OptionalAuth so that a
// customer who IS signed in gets attributed, without anyone else being blocked.
//
// The abuse surface is real and handled one layer up, by the rate limiter, which
// is the right tool for it. Nothing here is trusted: every field is validated in
// the domain and again by a database CHECK.
func (h *Handler) PublicRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.Submit)
	return r
}

// AdminRoutes is the queue our own team works.
func (h *Handler) AdminRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Patch("/{leadID}/status", h.UpdateStatus)
	r.Post("/{leadID}/reopen", h.Reopen)
	r.Delete("/{leadID}", h.Discard)
	return r
}

type submitRequest struct {
	Kind         string `json:"kind"`
	BusinessName string `json:"business_name"`
	ContactName  string `json:"contact_name"`
	ContactPhone string `json:"contact_phone"`
	Area         string `json:"area"`
	VendorKind   string `json:"vendor_kind"`
	Note         string `json:"note"`
}

// submitResponse deliberately returns almost nothing.
//
// An unauthenticated caller learns that we received the lead and nothing else. In
// particular it does NOT return the mention count, which would turn a public
// endpoint into an oracle for "is this vendor already talking to you" — commercial
// information a competitor would enjoy, and about a third party who never agreed
// to have it disclosed.
type submitResponse struct {
	ID       string `json:"id"`
	Received bool   `json:"received"`
}

func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Attribution when we happen to know who this is, and nothing more. The route
	// runs under OptionalAuth, so an absent principal is the ordinary case.
	var referredBy *id.ID
	if p, ok := httpx.PrincipalFrom(r.Context()); ok && !id.IsNil(p.UserID) {
		uid := p.UserID
		referredBy = &uid
	}

	lead, err := h.sourcing.Submit(r.Context(), app.SubmitInput{
		Kind:         domain.Kind(req.Kind),
		BusinessName: req.BusinessName,
		ContactName:  req.ContactName,
		ContactPhone: req.ContactPhone,
		Area:         req.Area,
		VendorKind:   domain.VendorKind(req.VendorKind),
		Note:         req.Note,
		ReferredBy:   referredBy,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	httpx.Created(w, submitResponse{ID: lead.ID.String(), Received: true})
}

// leadResponse is the admin view, which carries everything.
type leadResponse struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	BusinessName string `json:"business_name"`
	ContactName  string `json:"contact_name"`
	ContactPhone string `json:"contact_phone"`
	Area         string `json:"area"`
	VendorKind   string `json:"vendor_kind"`
	Note         string `json:"note"`
	Status       string `json:"status"`
	// Mentions is how many people have named this number. One is ordinary;
	// anything more is the queue's real priority order.
	Mentions  int    `json:"mentions"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toLeadResponse(l app.LeadWithSignal) leadResponse {
	return leadResponse{
		ID:           l.Lead.ID.String(),
		Kind:         string(l.Lead.Kind),
		BusinessName: l.Lead.BusinessName,
		ContactName:  l.Lead.ContactName,
		// Unmasked here and only here. Our team has to ring the vendor, which is
		// the entire point of the queue.
		ContactPhone: l.Lead.ContactPhone.String(),
		Area:         l.Lead.Area,
		VendorKind:   string(l.Lead.VendorKind),
		Note:         l.Lead.Note,
		Status:       string(l.Lead.Status),
		Mentions:     l.Mentions,
		CreatedAt:    l.Lead.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    l.Lead.UpdatedAt.Format(time.RFC3339),
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	status, err := app.ParseStatus(r.URL.Query().Get("status"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	leads, err := h.sourcing.List(r.Context(), status, limit, offset)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Non-nil so the field marshals as [] rather than null: an empty queue is a
	// normal state and a client should not need a third case for it.
	out := make([]leadResponse, 0, len(leads))
	for _, l := range leads {
		out = append(out, toLeadResponse(l))
	}
	httpx.OK(w, map[string]any{"leads": out})
}

type statusRequest struct {
	Status string `json:"status"`
}

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	leadID, err := httpx.URLParamID(r, "leadID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req statusRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	next, err := app.ParseStatus(req.Status)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if next == "" {
		httpx.Error(w, r, apperr.Invalid("MISSING_STATUS", "Say which status to move this lead to.").
			WithField("status", "required"))
		return
	}

	lead, err := h.sourcing.MoveTo(r.Context(), leadID, next)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toLeadResponse(app.LeadWithSignal{Lead: lead}))
}

func (h *Handler) Reopen(w http.ResponseWriter, r *http.Request) {
	leadID, err := httpx.URLParamID(r, "leadID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	lead, err := h.sourcing.Reopen(r.Context(), leadID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toLeadResponse(app.LeadWithSignal{Lead: lead}))
}

// Discard marks a lead as junk. DELETE is the verb because that is what it means
// to the person clicking it, but nothing is removed -- see migration 0018 for why
// junk and rejection are kept apart.
func (h *Handler) Discard(w http.ResponseWriter, r *http.Request) {
	leadID, err := httpx.URLParamID(r, "leadID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.sourcing.Discard(r.Context(), leadID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
