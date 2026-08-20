package httpx

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// TokenVerifier is implemented by the token manager. Declaring it here rather
// than importing the token package keeps the dependency pointing one way and
// avoids an import cycle (token already imports httpx for Principal).
type TokenVerifier interface {
	VerifyAccessToken(raw string) (Principal, error)
}

// RequestID assigns or propagates a correlation id.
//
// It reuses an inbound X-Request-ID so a trace spans the load balancer, the API
// and the worker that later processes the order's outbox event -- without that,
// debugging a failed order means correlating by timestamp.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-Request-ID")
		if traceID == "" || len(traceID) > 128 {
			traceID = id.New().String()
		}
		w.Header().Set("X-Request-ID", traceID)
		next.ServeHTTP(w, r.WithContext(WithTraceID(r.Context(), traceID)))
	})
}

// statusRecorder captures the status code, which net/http otherwise does not
// expose after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader implies 200.
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which is what
// keeps flushing and hijacking working for SSE order tracking and websockets.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RequestLogger logs one structured line per request and attaches a
// request-scoped logger to the context.
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health checks fire every few seconds on every replica; logging them
			// buries real traffic.
			if r.URL.Path == "/health" || r.URL.Path == "/ready" {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			reqLog := log.With("trace_id", TraceIDFrom(r.Context()))
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r.WithContext(logger.Into(r.Context(), reqLog)))

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes", rec.bytes,
			}
			// Attribute the request to a user when authenticated, so a support
			// query can find everything one customer did.
			if p, ok := PrincipalFrom(r.Context()); ok {
				attrs = append(attrs, "user_id", p.UserID.String())
			}

			switch {
			case rec.status >= 500:
				reqLog.Error("request", attrs...)
			case rec.status >= 400:
				reqLog.Warn("request", attrs...)
			default:
				reqLog.Info("request", attrs...)
			}
		})
	}
}

// Recoverer converts a panic into a 500 instead of killing the process.
//
// Without this, one nil dereference in one handler takes down every in-flight
// request on that instance.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way to abandon a response;
			// re-panic so net/http handles it as intended.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			logger.From(r.Context()).Error("panic recovered",
				"panic", rec,
				"path", r.URL.Path,
				"method", r.Method,
				"stack", string(debug.Stack()))

			Error(w, r, apperr.Internal(nil))
		}()
		next.ServeHTTP(w, r)
	})
}

// Authenticate verifies the bearer token and attaches the principal.
//
// Use on routes that require a signed-in caller. It does no database lookup: the
// token is self-contained, which is what keeps authentication off the hot path.
func Authenticate(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := bearerToken(r)
			if err != nil {
				Error(w, r, err)
				return
			}
			principal, err := verifier.VerifyAccessToken(raw)
			if err != nil {
				Error(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// OptionalAuth attaches the principal when a valid token is present and
// otherwise continues anonymously.
//
// Needed for endpoints that behave differently when signed in but must still
// work when not -- browsing a menu should show a returning customer their
// previous order, without requiring login to see the menu at all.
func OptionalAuth(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if raw, err := bearerToken(r); err == nil {
				if principal, err := verifier.VerifyAccessToken(raw); err == nil {
					r = r.WithContext(WithPrincipal(r.Context(), principal))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole authorizes by role. Apply after Authenticate.
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := MustPrincipal(r.Context())
			if err != nil {
				Error(w, r, err)
				return
			}
			if !principal.HasRole(roles...) {
				Error(w, r, apperr.Forbidden("INSUFFICIENT_PERMISSIONS",
					"You do not have permission to do this."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", apperr.Unauthorized("MISSING_TOKEN", "You must be signed in to do this.")
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", apperr.Unauthorized("MALFORMED_AUTH_HEADER",
			"Authorization header must be of the form: Bearer <token>.")
	}
	return token, nil
}

// SecurityHeaders sets defensive response headers.
//
// This is a JSON API, so the CSP is maximally restrictive -- an API response
// should never be able to execute anything if a browser is tricked into
// rendering it.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		// HSTS is only meaningful over TLS, and setting it on a plaintext local
		// server would pin localhost to https in the developer's browser.
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// Timeout bounds handler duration so one slow query cannot hold a connection
// open indefinitely and exhaust the pool.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"error":{"code":"REQUEST_TIMEOUT","message":"The request took too long. Please try again."}}`)
	}
}
