package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// IdempotencyStore persists the outcome of a keyed request.
//
// Declared here rather than in a module because idempotency is a transport
// concern: it is about HTTP retries, not about what ordering or payment mean.
type IdempotencyStore interface {
	// Claim atomically reserves a key.
	//
	// Returns (nil, true, nil) when this caller won the race and should do the
	// work; (response, false, nil) when the key was already completed and the
	// stored response should be replayed; and an ErrInProgress-style conflict
	// when another request holds the key right now.
	Claim(ctx context.Context, key, fingerprint, endpoint string, userID string, now time.Time) (stored []byte, claimed bool, err error)
	// Complete stores the response so retries replay it.
	Complete(ctx context.Context, key string, status int, response []byte) error
	// Release frees a claim whose work failed, so the client may genuinely retry
	// rather than being permanently locked out by a key that will never complete.
	Release(ctx context.Context, key string) error
}

// idempotentMethods are the ones worth protecting. GET/HEAD are already
// idempotent by definition; DELETE is naturally so.
var idempotentMethods = map[string]bool{
	http.MethodPost:  true,
	http.MethodPatch: true,
	http.MethodPut:   true,
}

// Idempotency replays the original response for a repeated Idempotency-Key.
//
// The problem: mobile clients retry on flaky networks. A retried POST /checkout
// without this creates a second order and a second charge — and the customer
// never saw the first response, so they cannot tell it worked.
//
// The contract:
//   - Same key + same body     -> the first response is replayed verbatim.
//   - Same key + DIFFERENT body -> 422. This is a client bug (a key was reused
//     for a different request) and silently serving the old response would hide
//     it while producing a baffling result.
//   - Key currently in flight   -> 409. The client should wait and retry.
//
// Only successful responses (2xx) are stored. A failed request must be genuinely
// retryable, so caching a 500 against the key would make a transient failure
// permanent.
func Idempotency(store IdempotencyStore, required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !idempotentMethods[r.Method] {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				if required {
					Error(w, r, apperr.Invalid("IDEMPOTENCY_KEY_REQUIRED",
						"This endpoint requires an Idempotency-Key header."))
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			// Bound the key: it is a database primary key, and an unbounded
			// header would let a caller write arbitrarily large rows.
			if len(key) < 8 || len(key) > 200 {
				Error(w, r, apperr.Invalid("INVALID_IDEMPOTENCY_KEY",
					"Idempotency-Key must be between 8 and 200 characters."))
				return
			}

			// Read the body so it can be fingerprinted, then restore it for the
			// handler. Without buffering, the handler would find an empty body.
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
			if err != nil {
				Error(w, r, apperr.Invalid("BODY_TOO_LARGE", "Request body exceeds the 1 MB limit.").WithCause(err))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			// Fingerprint the body so key reuse with different content is
			// detectable rather than silently served stale.
			sum := sha256.Sum256(body)
			fingerprint := hex.EncodeToString(sum[:])

			var userID string
			if p, ok := PrincipalFrom(r.Context()); ok {
				userID = p.UserID.String()
			}

			stored, claimed, err := store.Claim(r.Context(), key, fingerprint, r.URL.Path, userID, time.Now().UTC())
			if err != nil {
				Error(w, r, err)
				return
			}

			if !claimed {
				// Replay. The client gets byte-identical output to the original,
				// which is what makes the retry transparent.
				logger.From(r.Context()).Info("replaying idempotent response",
					"key", key, "path", r.URL.Path)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Idempotent-Replay", "true")

				var replay storedResponse
				if err := json.Unmarshal(stored, &replay); err != nil {
					// Corrupt record: fail loudly rather than serve garbage, and
					// certainly rather than re-run a payment.
					Error(w, r, apperr.Internal(err))
					return
				}
				w.WriteHeader(replay.Status)
				_, _ = w.Write(replay.Body)
				return
			}

			// We own the key. Capture the response so it can be stored.
			rec := &bufferingWriter{ResponseWriter: w, buf: &bytes.Buffer{}}
			next.ServeHTTP(rec, r)

			if rec.status >= 200 && rec.status < 300 {
				if err := store.Complete(r.Context(), key, rec.status, rec.buf.Bytes()); err != nil {
					// The response already went to the client; a storage failure
					// only costs replay protection on a future retry.
					logger.From(r.Context()).Error("storing idempotent response failed",
						"key", key, "error", err)
				}
				return
			}

			// Non-2xx: release so the client can retry for real.
			if err := store.Release(r.Context(), key); err != nil {
				logger.From(r.Context()).Error("releasing idempotency key failed",
					"key", key, "error", err)
			}
		})
	}
}

// storedResponse is the persisted shape of a completed idempotent request.
type storedResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// MarshalStoredResponse is used by the store implementation to build the record.
func MarshalStoredResponse(status int, body []byte) ([]byte, error) {
	if len(body) == 0 {
		body = []byte("null")
	}
	return json.Marshal(storedResponse{Status: status, Body: body})
}

// bufferingWriter tees the response so it can be both sent and stored.
//
// It buffers rather than streams because the body must be complete before it can
// be persisted. That is fine for JSON API responses; it would be wrong for a
// large download, which is why this middleware is only mounted on write routes.
type bufferingWriter struct {
	http.ResponseWriter
	buf    *bytes.Buffer
	status int
}

func (w *bufferingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *bufferingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *bufferingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
