// Package apperr defines the single error vocabulary every module speaks.
//
// Why this exists: domain and application code must be able to say "this was
// the caller's fault" or "this conflicts with current state" without importing
// net/http. The transport layer is the only place that maps these kinds onto
// status codes. That keeps modules transport-agnostic, which is what lets a
// module be lifted out behind gRPC later without touching its logic.
package apperr

import (
	"errors"
	"fmt"
)

// Kind is a transport-independent error classification.
type Kind string

const (
	KindInvalid      Kind = "invalid"      // malformed or failed validation
	KindUnauthorized Kind = "unauthorized" // no or bad credentials
	KindForbidden    Kind = "forbidden"    // authenticated but not permitted
	KindNotFound     Kind = "not_found"    // referenced entity does not exist
	KindConflict     Kind = "conflict"     // violates a uniqueness or state rule
	KindRateLimited  Kind = "rate_limited" // caller exceeded a quota
	KindUnavailable  Kind = "unavailable"  // a dependency is down
	KindInternal     Kind = "internal"     // our bug; never expose details
)

// Error carries a machine-readable code for clients, a safe human message,
// optional per-field validation details, and a wrapped cause for logs.
//
// Message is returned to the caller. Cause never is.
type Error struct {
	Kind    Kind
	Code    string            // stable, screaming-snake identifier for clients
	Message string            // safe to show a user
	Fields  map[string]string // field -> reason, for form validation
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s(%s): %s: %v", e.Kind, e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s(%s): %s", e.Kind, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// WithField attaches a field-level validation reason and returns the receiver
// so constructors can be chained fluently.
func (e *Error) WithField(field, reason string) *Error {
	if e.Fields == nil {
		e.Fields = make(map[string]string, 4)
	}
	e.Fields[field] = reason
	return e
}

// WithCause attaches the underlying error for logging without changing what
// the client sees.
func (e *Error) WithCause(err error) *Error {
	e.Cause = err
	return e
}

func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

func Invalid(code, message string) *Error  { return New(KindInvalid, code, message) }
func NotFound(code, message string) *Error { return New(KindNotFound, code, message) }
func Conflict(code, message string) *Error { return New(KindConflict, code, message) }

func Unauthorized(code, message string) *Error {
	return New(KindUnauthorized, code, message)
}

func Forbidden(code, message string) *Error {
	return New(KindForbidden, code, message)
}

func RateLimited(code, message string) *Error {
	return New(KindRateLimited, code, message)
}

func Unavailable(code, message string) *Error {
	return New(KindUnavailable, code, message)
}

// Internal wraps an unexpected failure. The message is deliberately generic --
// the real cause goes to logs only.
func Internal(cause error) *Error {
	return &Error{
		Kind:    KindInternal,
		Code:    "INTERNAL_ERROR",
		Message: "Something went wrong on our end. Please try again.",
		Cause:   cause,
	}
}

// CodeOf reports the stable machine-readable code of an error, or "" if the
// error is not an *Error.
//
// Kind alone is often too coarse to assert on. Two conflicts can mean completely
// different things -- "this coupon is used up" and "too many people are doing
// this at once" are both KindConflict, and a test that only checks the kind
// cannot tell them apart. The code can.
func CodeOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}

// KindOf reports the classification of any error, defaulting to KindInternal
// so an unclassified error can never accidentally read as a client mistake.
func KindOf(err error) Kind {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	return KindInternal
}

// As extracts the *Error from a chain, reporting whether one was present.
//
// errors.AsType rather than errors.As with an out-parameter: it returns the
// typed value directly, so there is no zero-valued variable in scope that a
// later edit could read on the false branch. Same semantics, one fewer way to
// get it wrong.
func As(err error) (*Error, bool) {
	return errors.AsType[*Error](err)
}
