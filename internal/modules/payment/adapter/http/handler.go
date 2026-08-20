// Package http is the payment module's transport adapter.
package http

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/payment/app"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// maxWebhookBody bounds an inbound webhook. The endpoint is public, so without a
// limit anyone can stream an unbounded body at it.
const maxWebhookBody = 512 << 10 // 512 KiB

type Handler struct {
	payments *app.PaymentService
	// signatureHeader is provider-specific, so it is injected rather than
	// hardcoded: Razorpay uses X-Razorpay-Signature, Stripe uses Stripe-Signature.
	signatureHeader string
}

func NewHandler(payments *app.PaymentService, signatureHeader string) *Handler {
	if signatureHeader == "" {
		signatureHeader = "X-Razorpay-Signature"
	}
	return &Handler{payments: payments, signatureHeader: signatureHeader}
}

// WebhookRoutes are PUBLIC and unauthenticated by necessity — the gateway has no
// JWT. Authentication is the HMAC signature, checked inside the handler.
//
// Deliberately NOT behind:
//   - Authenticate: a gateway cannot present a bearer token.
//   - Idempotency middleware: it buffers and fingerprints the body, and the
//     module already dedupes on the provider's event id, which is stronger.
//   - Aggressive rate limiting: throttling a gateway means dropping payment
//     notifications, and a dropped capture is an order the customer paid for that
//     nobody cooks.
func (h *Handler) WebhookRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.HandleWebhook)
	return r
}

// CustomerRoutes let a customer see their payment status.
//
// Mounted under its own /payments prefix rather than nesting under /orders,
// because ordering already owns that subtree and chi refuses two Mounts on one
// path. "by-order" is spelled out so the path parameter is unambiguously an
// order id and not a payment id.
func (h *Handler) CustomerRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/by-order/{orderID}", h.GetPaymentForOrder)
	return r
}

// HandleWebhook receives a gateway callback.
//
// It reads the RAW body and never decodes before verifying: the HMAC is computed
// over the exact bytes sent, so parsing and re-serialising would change key order
// and whitespace and the signature would never match.
func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	log := logger.From(r.Context())

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("UNREADABLE_BODY", "Could not read the request body.").WithCause(err))
		return
	}

	signature := r.Header.Get(h.signatureHeader)

	if err := h.payments.ProcessWebhook(r.Context(), rawBody, signature); err != nil {
		// Status code choice matters, because it decides whether the gateway
		// retries:
		//
		//   401 (bad signature)  -> do NOT retry. The caller is not the gateway,
		//                           or the secret is misconfigured. Retrying an
		//                           attack is pointless.
		//   4xx (bad payload)    -> do NOT retry. It will never parse.
		//   5xx (our failure)    -> DO retry. Our database was down, not their
		//                           problem, and the event must not be lost.
		if apperr.KindOf(err) == apperr.KindUnauthorized {
			log.Warn("rejected webhook with an invalid signature",
				"provider_header", h.signatureHeader, "remote", r.RemoteAddr)
		} else {
			log.Error("webhook processing failed", "error", err)
		}
		httpx.Error(w, r, err)
		return
	}

	// 200 with a trivial body. Gateways treat any 2xx as delivered and stop
	// retrying, which is what we want once the event is recorded.
	httpx.OK(w, map[string]string{"status": "received"})
}

type paymentResponse struct {
	PaymentID     string `json:"payment_id"`
	Status        string `json:"status"`
	Method        string `json:"method"`
	AmountMinor   int64  `json:"amount_minor"`
	FailureReason string `json:"failure_reason,omitempty"`
	// Provider refs and raw payloads are deliberately NOT exposed: they are
	// internal reconciliation data and leaking them helps nobody but an attacker.
}

func (h *Handler) GetPaymentForOrder(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	orderID, err := id.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.Error(w, r, apperr.Invalid("INVALID_ID", "That order id is not valid."))
		return
	}

	payment, err := h.payments.GetForOrder(r.Context(), orderID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Ownership check. Without it, iterating order ids reveals other customers'
	// payment status.
	p, _ := httpx.PrincipalFrom(r.Context())
	if payment.UserID != p.UserID && !p.HasRole(httpx.RoleSupport) {
		httpx.Error(w, r, apperr.NotFound("PAYMENT_NOT_FOUND", "That payment could not be found."))
		return
	}

	httpx.OK(w, paymentResponse{
		PaymentID:     payment.ID.String(),
		Status:        string(payment.Status),
		Method:        payment.Method,
		AmountMinor:   payment.Amount.Minor(),
		FailureReason: payment.FailureReason,
	})
}
