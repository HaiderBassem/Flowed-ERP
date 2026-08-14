// Package shared holds the vocabulary every other domain package depends on:
// error kinds, identifiers, and the clock abstraction. It imports nothing from
// the rest of the system, which keeps the domain layer free of infrastructure.
package shared

import (
	"errors"
	"fmt"
)

// Kind classifies a domain error so that transport layers can map it to a
// status code without inspecting error strings. Handlers switch on the kind;
// they never parse messages.
type Kind uint8

const (
	// KindInternal is an unexpected failure — a bug or an infrastructure fault.
	KindInternal Kind = iota
	// KindValidation is malformed input: a negative amount, a missing field.
	KindValidation
	// KindNotFound is a reference to something that does not exist.
	KindNotFound
	// KindConflict is a uniqueness or concurrency clash: a duplicate student
	// number, a stale optimistic-lock version.
	KindConflict
	// KindPreconditionFailed is a legitimate request against state that
	// forbids it: paying into a financially closed year, voiding a payment
	// that already carries a refund.
	KindPreconditionFailed
	// KindForbidden is an authenticated actor lacking authority for the command.
	KindForbidden
	// KindUnauthorized is a missing or invalid credential.
	KindUnauthorized
	// KindInvariantViolation is an internal consistency breach detected before
	// commit — installments not summing to the net, allocations exceeding a
	// payment. These must abort the transaction and page someone.
	KindInvariantViolation
)

// String renders the kind for logs.
func (k Kind) String() string {
	switch k {
	case KindValidation:
		return "validation"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindPreconditionFailed:
		return "precondition_failed"
	case KindForbidden:
		return "forbidden"
	case KindUnauthorized:
		return "unauthorized"
	case KindInvariantViolation:
		return "invariant_violation"
	default:
		return "internal"
	}
}

// Error is a domain error carrying a machine-readable code, a human message,
// structured details, and an optional wrapped cause.
//
// The Code is a stable identifier clients may branch on (for example
// "payment.duplicate_idempotency_key"); the Message is prose for an operator.
// Details carry the specifics a cashier needs on screen — which installment,
// how much remains — without forcing the client to parse the message.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Details map[string]any
	cause   error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// WithDetail attaches one structured detail and returns the error for chaining.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 4)
	}
	e.Details[key] = value
	return e
}

// WithCause wraps an underlying error, preserving it for logs while keeping
// the domain-facing code and message intact.
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

func newError(kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Validation builds a KindValidation error.
func Validation(code, format string, args ...any) *Error {
	return newError(KindValidation, code, format, args...)
}

// NotFound builds a KindNotFound error.
func NotFound(code, format string, args ...any) *Error {
	return newError(KindNotFound, code, format, args...)
}

// Conflict builds a KindConflict error.
func Conflict(code, format string, args ...any) *Error {
	return newError(KindConflict, code, format, args...)
}

// PreconditionFailed builds a KindPreconditionFailed error.
func PreconditionFailed(code, format string, args ...any) *Error {
	return newError(KindPreconditionFailed, code, format, args...)
}

// Forbidden builds a KindForbidden error.
func Forbidden(code, format string, args ...any) *Error {
	return newError(KindForbidden, code, format, args...)
}

// Unauthorized builds a KindUnauthorized error.
func Unauthorized(code, format string, args ...any) *Error {
	return newError(KindUnauthorized, code, format, args...)
}

// InvariantViolation builds a KindInvariantViolation error. Reaching for this
// means the system caught itself about to persist an inconsistent state.
func InvariantViolation(code, format string, args ...any) *Error {
	return newError(KindInvariantViolation, code, format, args...)
}

// Internal builds a KindInternal error wrapping a cause.
func Internal(code string, cause error, format string, args ...any) *Error {
	return newError(KindInternal, code, format, args...).WithCause(cause)
}

// KindOf reports the Kind of any error, defaulting to KindInternal for errors
// that did not originate in the domain.
func KindOf(err error) Kind {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr.Kind
	}
	return KindInternal
}

// AsDomain extracts the domain error from a chain, if present.
func AsDomain(err error) (*Error, bool) {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr, true
	}
	return nil, false
}

// CodeOf returns the stable error code, or "internal_error" for foreign errors.
func CodeOf(err error) string {
	if domainErr, ok := AsDomain(err); ok {
		return domainErr.Code
	}
	return "internal_error"
}
