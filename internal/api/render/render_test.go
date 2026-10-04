package render

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// countingHandler is a slog.Handler that only counts the records it sees.
type countingHandler struct{ n atomic.Int64 }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h *countingHandler) Handle(context.Context, slog.Record) error { h.n.Add(1); return nil }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *countingHandler) WithGroup(string) slog.Handler             { return h }

// TestErrorUnlogged_SameResponseNoLog: ErrorUnlogged exists so a public
// endpoint's configuration 503 stops writing an ERROR line per request. It
// must put exactly the bytes Error would on the wire — status mapping,
// machine code, 5xx masking, request ID — and log nothing. Not parallel: it
// swaps the process-wide slog default, which the parallel Error cases log to.
func TestErrorUnlogged_SameResponseNoLog(t *testing.T) {
	logs := &countingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := []struct {
		name string
		err  error
		code int
	}{
		{"503 is masked", errors.New("CI webhook not configured: set PAD_CI_WEBHOOK_SECRET"), http.StatusServiceUnavailable},
		{"zero status defaults to 500", errors.New("boom"), 0},
		{"domain error auto-maps", service.ErrNotFound, 0},
		{"4xx keeps its message", errors.New("flowId is required"), http.StatusBadRequest},
	}
	for _, c := range cases {
		want := httptest.NewRecorder()
		want.Header().Set("X-Request-ID", "req-1")
		Error(want, c.err, c.code)

		before := logs.n.Load()
		got := httptest.NewRecorder()
		got.Header().Set("X-Request-ID", "req-1")
		ErrorUnlogged(got, c.err, c.code)

		if n := logs.n.Load() - before; n != 0 {
			t.Errorf("%s: ErrorUnlogged logged %d records, want none", c.name, n)
		}
		if got.Code != want.Code {
			t.Errorf("%s: status %d, Error writes %d", c.name, got.Code, want.Code)
		}
		if g, w := got.Header().Get("Content-Type"), want.Header().Get("Content-Type"); g != w {
			t.Errorf("%s: Content-Type %q, Error writes %q", c.name, g, w)
		}
		if got.Body.String() != want.Body.String() {
			t.Errorf("%s: body %q, Error writes %q", c.name, got.Body.String(), want.Body.String())
		}
	}
}

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
