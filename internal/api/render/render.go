package render

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"

	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// PagedResponse is the standard envelope for paginated list endpoints: Items holds the page;
// Total is the full collection size; Offset/Limit echo the request pagination so clients can
// build next/prev links. HasMore reports items after the page, and NextCursor is the ?cursor= of
// the next page where the list pages by keyset (omitted on the last page). TotalCapped marks a
// Total cut at the list's cap: more items match ("10 000+").
type PagedResponse[T any] struct {
	Items       []T    `json:"items"`
	Total       int    `json:"total"`
	Offset      int    `json:"offset"`
	Limit       int    `json:"limit"`
	HasMore     bool   `json:"hasMore"`
	NextCursor  string `json:"nextCursor,omitempty"`
	TotalCapped bool   `json:"totalCapped"`
}

// JSON encodes data as JSON and writes it to w.
func JSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Warn("render.JSON: encode response", "error", err)
	}
}

// JSONStatus encodes data as JSON with the given status code. Use this instead of
// w.WriteHeader(code) followed by JSON(w, ...): that order sets Content-Type after the header
// block is flushed, so the 201 ships without Content-Type and Encode triggers a superfluous 200.
func JSONStatus(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Warn("render.JSONStatus: encode response", "error", err)
	}
}

// ErrorResponse is the standard error envelope: Code is a stable machine-readable string clients
// switch on; Message human-readable; RequestID links to traces; Details carries structured fields
// a client needs to recover from the error — never secret material, only what may be shown.
type ErrorResponse struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"requestId,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// statusCode maps an HTTP status code to its machine-readable code string.
func statusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "BAD_REQUEST"
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "CONFLICT"
	case http.StatusUnprocessableEntity:
		return "UNPROCESSABLE"
	case http.StatusTooManyRequests:
		return "RATE_LIMITED"
	case http.StatusNotImplemented:
		return "NOT_IMPLEMENTED"
	case http.StatusServiceUnavailable:
		return "SERVICE_UNAVAILABLE"
	default:
		if status >= 500 {
			return "INTERNAL_ERROR"
		}
		return "ERROR"
	}
}

// Error writes a structured JSON error response. An *apperr.Error in err's chain decides the status, the code and
// the details itself, whatever code says, and so does a secret store failure (service.SecretStoreError: 503
// SECRET_STORAGE_UNAVAILABLE, 422 KEY_UNREADABLE). For any other error, pass code=0 to auto-map domain errors
// (ErrNotFound → 404, ErrPermissionDenied → 403, etc.).
func Error(w http.ResponseWriter, err error, code int) {
	err = service.SecretStoreError(err)
	code, machineCode, details := resolve(err, code)
	writeErrorWithDetails(w, err, code, machineCode, details)
}

// ErrorUnlogged writes exactly the response Error would — same status mapping, machine code and
// 5xx masking — but without the per-request ERROR log every 5xx otherwise emits. For 5xx
// conditions that are configuration, not failure, and that an unauthenticated caller can trigger
// at will: the unconfigured CI webhook once answered each call with a 503 AND an ERROR line, so
// anyone could flood the error log and its alerting. The caller owns reporting, once per process.
func ErrorUnlogged(w http.ResponseWriter, err error, code int) {
	err = service.SecretStoreError(err)
	code, machineCode, details := resolve(err, code)
	writeErrorBody(w, err, code, machineCode, details)
}

// resolve returns the status, machine code and details that Error and ErrorUnlogged write: an *apperr.Error's
// own, otherwise the status errorStatus resolves and that status's code.
func resolve(err error, code int) (int, string, map[string]any) {
	if ae, ok := apperr.As(err); ok {
		return ae.Kind.Status(), cmp.Or(ae.Code, ae.Kind.Code()), ae.Details
	}
	code = errorStatus(err, code)
	return code, statusCode(code), nil
}

// errorStatus resolves the status of an error that is not an *apperr.Error: code=0 means 500, and a 500 is
// narrowed to the status a known domain error implies.
func errorStatus(err error, code int) int {
	if code == 0 {
		code = http.StatusInternalServerError
	}
	// Map domain errors to HTTP status codes.
	if code == http.StatusInternalServerError {
		switch {
		case errors.Is(err, service.ErrNotFound) || errors.Is(err, storageif.ErrNotFound):
			code = http.StatusNotFound
		case errors.Is(err, service.ErrPermissionDenied) || errors.Is(err, storageif.ErrNotCommentAuthor):
			code = http.StatusForbidden
		case errors.Is(err, service.ErrInvalidInput) || errors.Is(err, service.ErrUninitialized):
			code = http.StatusBadRequest
		case errors.Is(err, service.ErrConflict) || errors.Is(err, storageif.ErrEmailExists) || errors.Is(err, storageif.ErrOrgInviteExists) || errors.Is(err, storageif.ErrVersionConflict):
			code = http.StatusConflict
		case errors.Is(err, storageif.ErrRLSUnavailable):
			// The request's RLS transaction could not be started: a transient
			// pool/server condition the client retries.
			code = http.StatusServiceUnavailable
		}
	}
	return code
}

// ErrorWithCode writes the standard envelope with an explicit machine-readable code (e.g.
// "CHAT_CAPACITY_REACHED") instead of the HTTP-status-derived default — decoupling frontend
// classification from the message (masked on 5xx, may be reworded). Prefer a 4xx status so the
// message survives the internal-error masking.
func ErrorWithCode(w http.ResponseWriter, err error, code int, machineCode string) {
	if code == 0 {
		code = http.StatusInternalServerError
	}
	writeError(w, err, code, machineCode)
}

// ErrorWithCodeAndDetails is ErrorWithCode with a structured details object — machine-switchable
// fields the client needs to recover (e.g. a resumable re-index's nextCursor). The 5xx message
// masking does NOT cover Details: only put data that may be shown to the caller in it.
func ErrorWithCodeAndDetails(w http.ResponseWriter, err error, code int, machineCode string, details map[string]any) {
	if code == 0 {
		code = http.StatusInternalServerError
	}
	writeErrorWithDetails(w, err, code, machineCode, details)
}

func writeError(w http.ResponseWriter, err error, code int, machineCode string) {
	writeErrorWithDetails(w, err, code, machineCode, nil)
}

func writeErrorWithDetails(w http.ResponseWriter, err error, code int, machineCode string, details map[string]any) {
	if code >= 500 {
		logger.Error("internal server error", "error", err)
	}
	writeErrorBody(w, err, code, machineCode, details)
}

// writeErrorBody writes the error envelope without logging; every render error path funnels
// through it so the wire shape (and the 5xx masking) is defined in one place.
func writeErrorBody(w http.ResponseWriter, err error, code int, machineCode string, details map[string]any) {
	msg := err.Error()
	// An *apperr.Error's message is the one meant for the caller; its cause and any wrapping context are for
	// the logs.
	if ae, ok := apperr.As(err); ok {
		msg = cmp.Or(ae.Message, ae.Kind.String())
	}
	if code >= 500 {
		msg = "internal server error"
	}

	// X-Request-ID is stamped by middleware.RequestInfo (AccessLog when that did not run) before the
	// handler runs, so it is already present in the response header here.
	reqID := w.Header().Get("X-Request-ID")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Code:      machineCode,
		Message:   msg,
		RequestID: reqID,
		Details:   details,
	})
}
