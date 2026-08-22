package render

import (
	"encoding/json"
	"errors"
	"net/http"

	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// PagedResponse is the standard envelope for paginated list endpoints.
// Items holds the page contents; Total is the full collection size (may be
// len(Items) when a count query is unavailable); Offset and Limit echo the
// request pagination parameters so clients can construct next/prev links.
type PagedResponse[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// JSON encodes data as JSON and writes it to w.
func JSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Warn("render.JSON: encode response", "error", err)
	}
}

// JSONStatus encodes data as JSON and writes it with the given status code.
// Use this instead of w.WriteHeader(code) followed by JSON(w, ...): that
// sequence sets Content-Type AFTER the header block is flushed (WriteHeader
// sends the headers), so the 201 ships without Content-Type and the subsequent
// Encode triggers a superfluous WriteHeader(200).
func JSONStatus(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Warn("render.JSONStatus: encode response", "error", err)
	}
}

// ErrorResponse is the standard error envelope returned by all API endpoints.
// Code is a stable machine-readable string clients can switch on;
// Message is human-readable; RequestID links to distributed traces.
type ErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
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

// Error writes a structured JSON error response. Pass code=0 to auto-map
// domain errors (ErrNotFound → 404, ErrPermissionDenied → 403, etc.).
func Error(w http.ResponseWriter, err error, code int) {
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
		}
	}
	writeError(w, err, code, statusCode(code))
}

// ErrorWithCode writes the standard error envelope with an explicit
// machine-readable code (e.g. "CHAT_CAPACITY_REACHED") instead of the
// HTTP-status-derived default. Use it for domain errors that clients switch
// on — it decouples frontend classification from the human-readable message
// (which is masked on 5xx and may be reworded). Prefer a 4xx status so the
// message survives the internal-error masking in writeError.
func ErrorWithCode(w http.ResponseWriter, err error, code int, machineCode string) {
	if code == 0 {
		code = http.StatusInternalServerError
	}
	writeError(w, err, code, machineCode)
}

func writeError(w http.ResponseWriter, err error, code int, machineCode string) {
	if code >= 500 {
		logger.Error("internal server error", "error", err)
	}

	msg := err.Error()
	if code >= 500 {
		msg = "internal server error"
	}

	// X-Request-ID is stamped by the access-log middleware before the handler
	// runs, so it is already present in the response header by the time we
	// reach here.
	reqID := w.Header().Get("X-Request-ID")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Code:      machineCode,
		Message:   msg,
		RequestID: reqID,
	})
}
