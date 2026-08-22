package render

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

func TestError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		code          int
		wantStatus    int
		wantCode      string
		wantMsgHidden bool
	}{
		{"service ErrNotFound auto-maps to 404", service.ErrNotFound, 0, http.StatusNotFound, "NOT_FOUND", false},
		{"storage ErrNotFound auto-maps to 404", storageif.ErrNotFound, 0, http.StatusNotFound, "NOT_FOUND", false},
		{"ErrPermissionDenied auto-maps to 403", service.ErrPermissionDenied, 0, http.StatusForbidden, "FORBIDDEN", false},
		{"ErrInvalidInput auto-maps to 400", service.ErrInvalidInput, 0, http.StatusBadRequest, "BAD_REQUEST", false},
		{"ErrConflict auto-maps to 409", service.ErrConflict, 0, http.StatusConflict, "CONFLICT", false},
		{"ErrVersionConflict auto-maps to 409", storageif.ErrVersionConflict, 0, http.StatusConflict, "CONFLICT", false},
		{"ErrEmailExists auto-maps to 409", storageif.ErrEmailExists, 0, http.StatusConflict, "CONFLICT", false},
		{"ErrUninitialized auto-maps to 400", service.ErrUninitialized, 0, http.StatusBadRequest, "BAD_REQUEST", false},
		{"explicit 400 overrides mapping", errors.New("some error"), http.StatusBadRequest, http.StatusBadRequest, "BAD_REQUEST", false},
		{"generic error maps to 500", errors.New("something broke"), 0, http.StatusInternalServerError, "INTERNAL_ERROR", true},
		{"explicit 502 hides message", errors.New("secret details"), http.StatusBadGateway, http.StatusBadGateway, "INTERNAL_ERROR", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			Error(w, tt.err, tt.code)

			if w.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", w.Code, tt.wantStatus)
			}

			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content-type: got %q, want application/json", ct)
			}

			var body ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Code != tt.wantCode {
				t.Fatalf("code: got %q, want %q", body.Code, tt.wantCode)
			}
			if tt.wantMsgHidden && body.Message != "internal server error" {
				t.Fatalf("expected hidden message, got %q", body.Message)
			}
			if !tt.wantMsgHidden && body.Message == "internal server error" {
				t.Fatal("expected original error message, got hidden")
			}
		})
	}
}

func TestErrorWithCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		code       int
		machine    string
		wantStatus int
		wantMsg    string
	}{
		{
			"explicit 4xx keeps message and custom code",
			errors.New("too many chat responses running at once (max 3)"),
			http.StatusTooManyRequests,
			"CHAT_CAPACITY_REACHED",
			http.StatusTooManyRequests,
			"too many chat responses running at once (max 3)",
		},
		{
			"explicit 5xx still masks the message",
			errors.New("secret details"),
			http.StatusInternalServerError,
			"DOMAIN_CODE",
			http.StatusInternalServerError,
			"internal server error",
		},
		{
			"zero status defaults to 500",
			errors.New("boom"),
			0,
			"DOMAIN_CODE",
			http.StatusInternalServerError,
			"internal server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			w.Header().Set("X-Request-ID", "req-123")
			ErrorWithCode(w, tt.err, tt.code, tt.machine)

			if w.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", w.Code, tt.wantStatus)
			}

			var body ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Code != tt.machine {
				t.Fatalf("code: got %q, want %q", body.Code, tt.machine)
			}
			if body.Message != tt.wantMsg {
				t.Fatalf("message: got %q, want %q", body.Message, tt.wantMsg)
			}
			if body.RequestID != "req-123" {
				t.Fatalf("requestId: got %q, want %q", body.RequestID, "req-123")
			}
		})
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	JSON(w, map[string]string{"hello": "world"})

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["hello"] != "world" {
		t.Fatalf("body: got %v", body)
	}
}
