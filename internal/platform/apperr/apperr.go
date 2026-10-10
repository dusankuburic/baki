// Package apperr is the API's typed error. An Error carries a Kind, which decides the HTTP status; a stable
// SCREAMING_SNAKE code that clients switch on; a message the caller may see; structured details; and the cause,
// which the logs show and the client never does. render.Error writes it as the standard error envelope, while
// plain errors and the service sentinels keep render's own mapping.
//
// The package uses only the standard library and sits in internal/platform so both layers may use it: a service
// returns an Error where it knows the client-facing kind and code, and request validation and the functions behind
// api.JSON and api.Query do the same. Services keep their sentinels where render's mapping already fits.
package apperr

import (
	"cmp"
	"errors"
	"maps"
	"net/http"
)

// Kind is the class of an Error: it decides the HTTP status and the default code.
type Kind uint8

const (
	// KindInternal is the zero Kind, so an Error built without one is a 500 and its message is masked.
	KindInternal Kind = iota
	KindInvalid
	KindUnauthenticated
	KindForbidden
	KindNotFound
	KindConflict
	KindUnprocessable
	KindRateLimited
	KindUnavailable
	KindNotImplemented
	KindBadGateway
)

type kindInfo struct {
	status int
	code   string
	name   string
}

// kinds gives each Kind its status, default code and name. Each default code is the one render derives from the
// status for a plain error, so an endpoint moved onto apperr keeps the codes its clients see (render's tests pin
// this).
var kinds = [...]kindInfo{
	KindInternal:        {http.StatusInternalServerError, "INTERNAL_ERROR", "internal error"},
	KindInvalid:         {http.StatusBadRequest, "BAD_REQUEST", "invalid request"},
	KindUnauthenticated: {http.StatusUnauthorized, "UNAUTHORIZED", "unauthenticated"},
	KindForbidden:       {http.StatusForbidden, "FORBIDDEN", "forbidden"},
	KindNotFound:        {http.StatusNotFound, "NOT_FOUND", "not found"},
	KindConflict:        {http.StatusConflict, "CONFLICT", "conflict"},
	KindUnprocessable:   {http.StatusUnprocessableEntity, "UNPROCESSABLE", "unprocessable"},
	KindRateLimited:     {http.StatusTooManyRequests, "RATE_LIMITED", "rate limited"},
	KindUnavailable:     {http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "unavailable"},
	KindNotImplemented:  {http.StatusNotImplemented, "NOT_IMPLEMENTED", "not implemented"},
	KindBadGateway:      {http.StatusBadGateway, "INTERNAL_ERROR", "bad gateway"},
}

// info returns the kind's entry; an unknown kind is treated as internal.
func (k Kind) info() kindInfo {
	if int(k) < len(kinds) {
		return kinds[k]
	}
	return kinds[KindInternal]
}

// Status is the HTTP status of the kind.
func (k Kind) Status() int { return k.info().status }

// Code is the kind's default machine code.
func (k Kind) Code() string { return k.info().code }

func (k Kind) String() string { return k.info().name }

// Error is an API error. Build one with a constructor (Invalid, NotFound, …), which fills an empty code with the
// kind's default.
type Error struct {
	Kind Kind
	// Code is the stable SCREAMING_SNAKE code clients switch on, e.g. FLOW_LOCKED.
	Code string
	// Message is for the caller, so it never carries internals. render masks it on a 5xx.
	Message string
	// Details are structured fields the client needs to recover, such as a field name or a resume cursor. They
	// reach the client on a 5xx too, so only put what may be shown.
	Details map[string]any
	// Err is the cause. errors.Is and errors.As see it, and the logs show it.
	Err error
}

// New returns an Error of kind with code, or the kind's default code when code is empty, and msg.
func New(kind Kind, code, msg string) *Error {
	return &Error{Kind: kind, Code: cmp.Or(code, kind.Code()), Message: msg}
}

// Invalid is a request that is malformed or fails validation (400).
func Invalid(code, msg string) *Error { return New(KindInvalid, code, msg) }

// Unauthenticated is a caller who is not signed in (401).
func Unauthenticated(code, msg string) *Error { return New(KindUnauthenticated, code, msg) }

// Forbidden is a caller who may not do this (403).
func Forbidden(code, msg string) *Error { return New(KindForbidden, code, msg) }

// NotFound is a resource that does not exist, or that the caller may not know exists (404).
func NotFound(code, msg string) *Error { return New(KindNotFound, code, msg) }

// Conflict is a request that conflicts with the resource's current state, such as a stale version (409).
func Conflict(code, msg string) *Error { return New(KindConflict, code, msg) }

// Unprocessable is a well-formed request the server cannot act on (422).
func Unprocessable(code, msg string) *Error { return New(KindUnprocessable, code, msg) }

// RateLimited is a caller who must slow down (429).
func RateLimited(code, msg string) *Error { return New(KindRateLimited, code, msg) }

// Unavailable is a deployment or dependency that cannot serve the request now (503).
func Unavailable(code, msg string) *Error { return New(KindUnavailable, code, msg) }

// NotImplemented is a feature this deployment does not offer (501).
func NotImplemented(code, msg string) *Error { return New(KindNotImplemented, code, msg) }

// BadGateway is an upstream the server called on the caller's behalf that failed, such as a notification channel's
// endpoint (502). render masks the message, so put what the caller may see in Details.
func BadGateway(code, msg string) *Error { return New(KindBadGateway, code, msg) }

// Internal is a failure the caller cannot fix (500).
func Internal(code, msg string) *Error { return New(KindInternal, code, msg) }

// Error returns the message followed by the cause, for the logs.
func (e *Error) Error() string {
	msg := cmp.Or(e.Message, e.Kind.String())
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is an *Error of the same kind and code. The copies made by WithDetails and Wrap then
// still match the value they came from: errors.Is(err, errFlowLocked).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind && cmp.Or(t.Code, t.Kind.Code()) == cmp.Or(e.Code, e.Kind.Code())
}

// WithDetails returns a copy of e with details added to its own. e itself is left unchanged, so a package-level
// Error is safe to share between requests.
func (e *Error) WithDetails(details map[string]any) *Error {
	c := *e
	c.Details = make(map[string]any, len(e.Details)+len(details))
	maps.Copy(c.Details, e.Details)
	maps.Copy(c.Details, details)
	return &c
}

// Wrap returns a copy of e with err as its cause.
func (e *Error) Wrap(err error) *Error {
	c := *e
	c.Err = err
	return &c
}

// As returns the first *Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
