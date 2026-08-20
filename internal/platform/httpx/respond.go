// Package httpx holds the transport-layer plumbing: request decoding, the
// response envelope, and the single place where domain errors become HTTP
// status codes.
//
// Keeping that mapping here -- and nowhere else -- is what lets every module
// stay ignorant of HTTP. A handler returns an apperr; this package decides
// whether that is a 404 or a 409.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/platform/tracing"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// maxBodyBytes caps request bodies. Without this, a single client can exhaust
// server memory by streaming an unbounded JSON body.
const maxBodyBytes = 1 << 20 // 1 MiB

// Envelope is the shape of every response. A consistent envelope means the
// frontend writes one API client and one error handler, not one per endpoint.
type Envelope struct {
	Data  any        `json:"data,omitempty"`
	Error *ErrorBody `json:"error,omitempty"`
	Meta  any        `json:"meta,omitempty"`
}

type ErrorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
	TraceID string            `json:"trace_id,omitempty"`
}

// JSON writes a success response.
func JSON(w http.ResponseWriter, status int, data any) {
	write(w, status, Envelope{Data: data})
}

// OK is the common case: 200 with a body.
func OK(w http.ResponseWriter, data any) { JSON(w, http.StatusOK, data) }

// Created is 201 with the new resource.
func Created(w http.ResponseWriter, data any) { JSON(w, http.StatusCreated, data) }

// NoContent is 204 -- used for deletes and idempotent state changes.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error translates any error into a response.
//
// Internal errors are logged with their cause and returned with a generic
// message: leaking "pq: duplicate key value violates unique constraint
// users_phone_key" tells an attacker your schema.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	traceID := TraceIDFrom(r.Context())
	log := logger.From(r.Context())

	appErr, ok := apperr.As(err)
	if !ok {
		appErr = apperr.Internal(err)
	}

	// Count every error response exactly once, here, so no handler has to
	// remember to instrument its own failure paths.
	RecordError(appErr)

	// Mark the trace span failed, or a trace shows a span that took 3 seconds
	// and returned with no indication anything went wrong.
	tracing.RecordError(r.Context(), appErr)

	status := statusFor(appErr.Kind)
	if status >= http.StatusInternalServerError {
		log.Error("request failed",
			"code", appErr.Code, "kind", string(appErr.Kind),
			"error", appErr.Error(), "path", r.URL.Path, "method", r.Method)
	} else {
		log.Info("request rejected",
			"code", appErr.Code, "kind", string(appErr.Kind),
			"message", appErr.Message, "path", r.URL.Path, "method", r.Method)
	}

	write(w, status, Envelope{Error: &ErrorBody{
		Code:    appErr.Code,
		Message: appErr.Message,
		Fields:  appErr.Fields,
		TraceID: traceID,
	}})
}

// statusFor is the sole apperr.Kind -> HTTP status mapping in the codebase.
func statusFor(kind apperr.Kind) int {
	switch kind {
	case apperr.KindInvalid:
		return http.StatusBadRequest
	case apperr.KindUnauthorized:
		return http.StatusUnauthorized
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindRateLimited:
		return http.StatusTooManyRequests
	case apperr.KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func write(w http.ResponseWriter, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if status == http.StatusNoContent {
		return
	}
	// The response is already committed at this point, so a marshal failure can
	// only be logged, not corrected.
	_ = json.NewEncoder(w).Encode(body)
}

// Validator is implemented by request DTOs that can check themselves. Keeping
// validation on the DTO -- rather than in the handler -- means the rules live
// next to the shape they describe.
type Validator interface {
	Validate() error
}

// Decode reads and validates a JSON request body.
//
// DisallowUnknownFields is deliberate: a typo'd field name like "quantiy" would
// otherwise be silently ignored and the caller would get a default value. For a
// checkout request, that is an order at the wrong price.
func Decode[T any](r *http.Request, dst *T) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return apperr.Invalid("UNSUPPORTED_MEDIA_TYPE", "Request body must be application/json.")
	}

	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return apperr.Invalid("EMPTY_BODY", "A request body is required.")
		case strings.Contains(err.Error(), "unknown field"):
			field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return apperr.Invalid("UNKNOWN_FIELD", "Unrecognised field: "+field).WithCause(err)
		case strings.Contains(err.Error(), "http: request body too large"):
			return apperr.Invalid("BODY_TOO_LARGE", "Request body exceeds the 1 MB limit.").WithCause(err)
		default:
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				return apperr.Invalid("INVALID_FIELD_TYPE",
					"Field "+typeErr.Field+" must be a "+typeErr.Type.String()+".").
					WithField(typeErr.Field, "expected "+typeErr.Type.String()).
					WithCause(err)
			}
			return apperr.Invalid("MALFORMED_JSON", "Request body is not valid JSON.").WithCause(err)
		}
	}

	// Reject trailing content so `{"a":1}{"b":2}` cannot smuggle a second object.
	if dec.More() {
		return apperr.Invalid("MALFORMED_JSON", "Request body must contain a single JSON object.")
	}

	if v, ok := any(dst).(Validator); ok {
		return v.Validate()
	}
	return nil
}

// QueryInt reads an integer query parameter, falling back on absence or garbage.
// Query params are user input; a bad `limit` should not 500.
func QueryInt(r *http.Request, key string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(key)))
	if err != nil {
		return fallback
	}
	return v
}

func QueryString(r *http.Request, key, fallback string) string {
	if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
		return v
	}
	return fallback
}

func QueryBool(r *http.Request, key string, fallback bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(r.URL.Query().Get(key)))
	if err != nil {
		return fallback
	}
	return v
}
