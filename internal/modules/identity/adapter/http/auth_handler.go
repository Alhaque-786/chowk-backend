// Package http is the identity module's transport adapter.
//
// Handlers are deliberately thin: decode, delegate to the app layer, encode. Any
// logic that appears here is logic that cannot be unit-tested without an HTTP
// request and cannot be reused by a future gRPC or CLI entry point, so it belongs
// in app instead.
package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/cloud-kitchen-backend/internal/modules/identity/app"
	"github.com/zevoxa/cloud-kitchen-backend/internal/modules/identity/domain"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/httpx"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/apperr"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/id"
)

type AuthHandler struct {
	auth *app.AuthService
	// exposeOTP is true only outside production, so local development does not
	// need a real SMS provider. Guarded by config, never by a request parameter.
	exposeOTP bool
}

func NewAuthHandler(auth *app.AuthService, exposeOTP bool) *AuthHandler {
	return &AuthHandler{auth: auth, exposeOTP: exposeOTP}
}

// Routes returns the whole /auth subtree. Returning an http.Handler rather than
// mutating a shared router keeps the module self-contained -- it declares its own
// URL space and the composition root decides where to mount it.
//
// The authenticate middleware is injected rather than imported, for two reasons.
// It keeps the public and authenticated routes in ONE subtree, so both can live
// under /auth (chi refuses two Mounts on the same path, and splitting logout onto
// a different prefix to dodge that would be the tail wagging the dog). And it
// leaves the choice of how a caller is authenticated with the composition root,
// so this module never depends on the token implementation.
func (h *AuthHandler) Routes(authenticate func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()

	// Public: these are how you obtain a token, so they cannot require one.
	r.Post("/register", h.Register)
	r.Post("/login", h.Login)
	r.Post("/otp/request", h.RequestOTP)
	r.Post("/otp/verify", h.VerifyOTP)
	// Refresh authenticates via the refresh token in the body, not a bearer
	// header -- by the time a client calls this, its access token has expired.
	r.Post("/refresh", h.Refresh)

	// Authenticated: revoking a session requires knowing which session.
	r.Group(func(r chi.Router) {
		r.Use(authenticate)
		r.Post("/logout", h.Logout)
		r.Post("/logout-all", h.LogoutEverywhere)
	})
	return r
}

// ---------------------------------------------------------------------------
// DTOs
//
// Request and response shapes are separate types from domain entities on
// purpose. If handlers returned *domain.User directly, adding an internal field
// would silently leak it to every client -- which is exactly how password
// hashes end up in API responses.
// ---------------------------------------------------------------------------

type registerRequest struct {
	Phone        string `json:"phone"`
	Email        string `json:"email"`
	FullName     string `json:"full_name"`
	Password     string `json:"password"`
	ReferralCode string `json:"referral_code"`
}

func (r *registerRequest) Validate() error {
	err := apperr.Invalid("VALIDATION_FAILED", "Please check the highlighted fields.")
	bad := false

	if strings.TrimSpace(r.Phone) == "" {
		err.WithField("phone", "required")
		bad = true
	}
	if name := strings.TrimSpace(r.FullName); name == "" {
		err.WithField("full_name", "required")
		bad = true
	} else if len(name) < 2 || len(name) > 100 {
		err.WithField("full_name", "must_be_2_to_100_chars")
		bad = true
	}
	if r.Password == "" {
		err.WithField("password", "required")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

type loginRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

func (r *loginRequest) Validate() error {
	if strings.TrimSpace(r.Phone) == "" || r.Password == "" {
		return apperr.Invalid("VALIDATION_FAILED", "Phone number and password are both required.")
	}
	return nil
}

type otpRequestRequest struct {
	Phone   string `json:"phone"`
	Purpose string `json:"purpose"` // "login" | "verify_phone" | "reset_password"
}

func (r *otpRequestRequest) Validate() error {
	if strings.TrimSpace(r.Phone) == "" {
		return apperr.Invalid("VALIDATION_FAILED", "A phone number is required.").
			WithField("phone", "required")
	}
	switch r.Purpose {
	case "", "login":
		r.Purpose = "login"
	case "verify_phone", "reset_password":
	default:
		return apperr.Invalid("INVALID_PURPOSE", "That verification purpose is not supported.").
			WithField("purpose", "unsupported")
	}
	return nil
}

type otpVerifyRequest struct {
	Phone   string `json:"phone"`
	Code    string `json:"code"`
	Purpose string `json:"purpose"`
}

func (r *otpVerifyRequest) Validate() error {
	if strings.TrimSpace(r.Phone) == "" || strings.TrimSpace(r.Code) == "" {
		return apperr.Invalid("VALIDATION_FAILED", "Phone number and code are both required.")
	}
	if r.Purpose == "" {
		r.Purpose = "login"
	}
	return nil
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type userResponse struct {
	ID            string    `json:"id"`
	Phone         string    `json:"phone"`
	Email         string    `json:"email,omitempty"`
	FullName      string    `json:"full_name"`
	Status        string    `json:"status"`
	Roles         []string  `json:"roles"`
	PhoneVerified bool      `json:"phone_verified"`
	ReferralCode  string    `json:"referral_code"`
	KitchenID     string    `json:"kitchen_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// toUserResponse is the projection boundary. Note what it does NOT copy:
// PasswordHash. That omission is the whole reason this function exists.
func toUserResponse(u *domain.User) userResponse {
	roles := make([]string, len(u.Roles))
	for i, r := range u.Roles {
		roles[i] = string(r)
	}
	resp := userResponse{
		ID:            u.ID.String(),
		Phone:         u.Phone.String(),
		Email:         u.Email,
		FullName:      u.FullName,
		Status:        string(u.Status),
		Roles:         roles,
		PhoneVerified: u.PhoneVerifiedAt != nil,
		ReferralCode:  u.ReferralCode,
		CreatedAt:     u.CreatedAt,
	}
	if !id.IsNil(u.KitchenID) {
		resp.KitchenID = u.KitchenID.String()
	}
	return resp
}

type authResponse struct {
	User         userResponse `json:"user"`
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	TokenType    string       `json:"token_type"`
	ExpiresAt    time.Time    `json:"expires_at"`
}

func toAuthResponse(r *app.AuthResult) authResponse {
	return authResponse{
		User:         toUserResponse(r.User),
		AccessToken:  r.Tokens.AccessToken,
		RefreshToken: r.Tokens.RefreshToken,
		TokenType:    r.Tokens.TokenType,
		ExpiresAt:    r.Tokens.ExpiresAt,
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.auth.Register(r.Context(), app.RegisterInput{
		Phone:        req.Phone,
		Email:        req.Email,
		FullName:     strings.TrimSpace(req.FullName),
		Password:     req.Password,
		ReferralCode: strings.ToUpper(strings.TrimSpace(req.ReferralCode)),
		Device:       deviceFrom(r),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, toAuthResponse(result))
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.auth.Login(r.Context(), app.LoginInput{
		Phone:    req.Phone,
		Password: req.Password,
		Device:   deviceFrom(r),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toAuthResponse(result))
}

func (h *AuthHandler) RequestOTP(w http.ResponseWriter, r *http.Request) {
	var req otpRequestRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	code, err := h.auth.RequestOTP(r.Context(), req.Phone, req.Purpose)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	resp := map[string]any{
		"sent":       true,
		"expires_in": 300,
	}
	// Never in production: returning the code would make the OTP meaningless.
	if h.exposeOTP {
		resp["debug_code"] = code
	}
	httpx.OK(w, resp)
}

func (h *AuthHandler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req otpVerifyRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.auth.VerifyOTP(r.Context(), req.Phone, req.Code, req.Purpose, deviceFrom(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toAuthResponse(result))
}

func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.auth.Refresh(r.Context(), req.RefreshToken, deviceFrom(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, toAuthResponse(result))
}

// Logout revokes the calling session. Mounted behind auth middleware.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.auth.Logout(r.Context(), p.SessionID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// LogoutEverywhere revokes every session for the caller.
func (h *AuthHandler) LogoutEverywhere(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.auth.LogoutEverywhere(r.Context(), p.UserID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// deviceFrom extracts client details for the session record.
func deviceFrom(r *http.Request) app.DeviceInfo {
	return app.DeviceInfo{
		UserAgent:  truncate(r.UserAgent(), 512),
		IPAddress:  clientIP(r),
		DeviceName: truncate(r.Header.Get("X-Device-Name"), 100),
	}
}

// clientIP resolves the caller's address behind a proxy.
//
// X-Forwarded-For is a client-controlled header and can be forged, so this is
// only trustworthy because the load balancer overwrites it. It is used for
// display and rate limiting, never for authorization.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Leftmost entry is the original client; the rest are proxy hops.
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		return strings.TrimSpace(rip)
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
