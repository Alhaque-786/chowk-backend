package http

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/identity/app"
	"github.com/zevoxa/chowk-backend/internal/modules/identity/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type AddressHandler struct {
	addresses *app.AddressService
}

func NewAddressHandler(addresses *app.AddressService) *AddressHandler {
	return &AddressHandler{addresses: addresses}
}

// Routes are all authenticated: an address belongs to exactly one customer.
func (h *AddressHandler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Patch("/{addressID}", h.Update)
	r.Delete("/{addressID}", h.Delete)
	return r
}

type addressRequest struct {
	Type          string  `json:"type"`
	Label         string  `json:"label"`
	Line1         string  `json:"line1"`
	Line2         string  `json:"line2"`
	Landmark      string  `json:"landmark"`
	City          string  `json:"city"`
	State         string  `json:"state"`
	Pincode       string  `json:"pincode"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	IsDefault     bool    `json:"is_default"`
	ReceiverName  string  `json:"receiver_name"`
	ReceiverPhone string  `json:"receiver_phone"`
}

func (r *addressRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please complete the address.")
	bad := false

	if strings.TrimSpace(r.Line1) == "" {
		err.WithField("line1", "required")
		bad = true
	}
	if strings.TrimSpace(r.City) == "" {
		err.WithField("city", "required")
		bad = true
	}
	if strings.TrimSpace(r.Pincode) == "" {
		err.WithField("pincode", "required")
		bad = true
	}
	// Coordinates are mandatory, not optional: the delivery-radius check and
	// rider routing both need a point, and geocoding at save time keeps that
	// third-party call off the checkout path.
	if r.Latitude == 0 && r.Longitude == 0 {
		err.WithField("location", "required")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

type addressResponse struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Label         string  `json:"label"`
	Line1         string  `json:"line1"`
	Line2         string  `json:"line2,omitempty"`
	Landmark      string  `json:"landmark,omitempty"`
	City          string  `json:"city"`
	State         string  `json:"state,omitempty"`
	Pincode       string  `json:"pincode"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	IsDefault     bool    `json:"is_default"`
	ReceiverName  string  `json:"receiver_name,omitempty"`
	ReceiverPhone string  `json:"receiver_phone,omitempty"`
}

func toAddressResponse(a *domain.Address) addressResponse {
	return addressResponse{
		ID: a.ID.String(), Type: string(a.Type), Label: a.Label,
		Line1: a.Line1, Line2: a.Line2, Landmark: a.Landmark,
		City: a.City, State: a.State, Pincode: a.Pincode,
		Latitude: a.Latitude, Longitude: a.Longitude, IsDefault: a.IsDefault,
		ReceiverName: a.ReceiverName, ReceiverPhone: a.ReceiverPhone,
	}
}

func (h *AddressHandler) List(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	addrs, err := h.addresses.List(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]addressResponse, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, toAddressResponse(a))
	}
	httpx.OK(w, map[string]any{"addresses": out})
}

func (h *AddressHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req addressRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	addr, err := h.addresses.Save(r.Context(), req.toInput(p.UserID, nil))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, toAddressResponse(addr))
}

func (h *AddressHandler) Update(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	addressID, err := id.Parse(chi.URLParam(r, "addressID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That address id is not valid."))
		return
	}

	var req addressRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	addr, err := h.addresses.Save(r.Context(), req.toInput(p.UserID, &addressID))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toAddressResponse(addr))
}

func (h *AddressHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	addressID, err := id.Parse(chi.URLParam(r, "addressID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That address id is not valid."))
		return
	}
	if err := h.addresses.Delete(r.Context(), p.UserID, addressID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (r *addressRequest) toInput(userID id.ID, addressID *id.ID) app.SaveAddressInput {
	return app.SaveAddressInput{
		ID:            addressID,
		UserID:        userID,
		Type:          r.Type,
		Label:         strings.TrimSpace(r.Label),
		Line1:         strings.TrimSpace(r.Line1),
		Line2:         strings.TrimSpace(r.Line2),
		Landmark:      strings.TrimSpace(r.Landmark),
		City:          strings.TrimSpace(r.City),
		State:         strings.TrimSpace(r.State),
		Pincode:       strings.TrimSpace(r.Pincode),
		Latitude:      r.Latitude,
		Longitude:     r.Longitude,
		IsDefault:     r.IsDefault,
		ReceiverName:  strings.TrimSpace(r.ReceiverName),
		ReceiverPhone: strings.TrimSpace(r.ReceiverPhone),
	}
}
