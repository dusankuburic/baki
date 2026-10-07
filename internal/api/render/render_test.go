package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/platform/apperr"
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
		{"ErrScopeDenied auto-maps to 403", service.ErrScopeDenied, 0, http.StatusForbidden, "FORBIDDEN", false},
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

// An *apperr.Error decides its own status, code and details. The wire carries its message, never its cause or
// the context it was wrapped in, and a 5xx masks the message as it does for any error but keeps the details.
func TestError_AppErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		code        int
		wantStatus  int
		wantCode    string
		wantMessage string
		wantDetails map[string]any
	}{
		{"its kind and code", apperr.NotFound("FLOW_NOT_FOUND", "flow not found"), 0, 404, "FLOW_NOT_FOUND", "flow not found", nil},
		{"the kind's default code, and details", apperr.Invalid("", "name is required").WithDetails(map[string]any{"field": "name"}), 0, 400, "BAD_REQUEST", "name is required", map[string]any{"field": "name"}},
		{"found through wrapping, which stays off the wire", fmt.Errorf("rename f-1: %w", apperr.Conflict("FLOW_LOCKED", "the flow is locked")), 0, 409, "FLOW_LOCKED", "the flow is locked", nil},
		{"the cause stays off the wire", apperr.Forbidden("", "not your flow").Wrap(errors.New("owner u-2, org o-9")), 0, 403, "FORBIDDEN", "not your flow", nil},
		{"it decides over a wrapped sentinel", apperr.Conflict("STALE_VERSION", "reload first").Wrap(service.ErrNotFound), 0, 409, "STALE_VERSION", "reload first", nil},
		{"it decides over an explicit status", apperr.NotFound("", "gone"), http.StatusBadRequest, 404, "NOT_FOUND", "gone", nil},
		{"an Error without a code takes the kind's", &apperr.Error{Kind: apperr.KindRateLimited, Message: "slow down"}, 0, 429, "RATE_LIMITED", "slow down", nil},
		{"an Error without a message reads as its kind", &apperr.Error{Kind: apperr.KindNotFound}, 0, 404, "NOT_FOUND", "not found", nil},
		{"a 5xx masks the message and keeps the details", apperr.Unavailable("AI_PROVIDER_DOWN", "provider down").WithDetails(map[string]any{"retryAfter": 30}).Wrap(errors.New("dial tcp 10.0.0.7:443")), 0, 503, "AI_PROVIDER_DOWN", "internal server error", map[string]any{"retryAfter": float64(30)}},
		{"internal", apperr.Internal("", "boom"), 0, 500, "INTERNAL_ERROR", "internal server error", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			w.Header().Set("X-Request-ID", "req-7")
			Error(w, tt.err, tt.code)

			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type %q", ct)
			}
			var body ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if w.Code != tt.wantStatus || body.Code != tt.wantCode || body.Message != tt.wantMessage || body.RequestID != "req-7" {
				t.Errorf("got %d %+v, want %d %s %q with requestId req-7", w.Code, body, tt.wantStatus, tt.wantCode, tt.wantMessage)
			}
			if !reflect.DeepEqual(body.Details, tt.wantDetails) {
				t.Errorf("details %v, want %v", body.Details, tt.wantDetails)
			}
		})
	}

	// The envelope is the one every other error gets, byte for byte.
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "req-7")
	Error(w, apperr.NotFound("FLOW_NOT_FOUND", "flow not found"), 0)
	if got, want := w.Body.String(), `{"code":"FLOW_NOT_FOUND","message":"flow not found","requestId":"req-7"}`+"\n"; got != want {
		t.Errorf("body %s, want %s", got, want)
	}
}

// recordingHandler keeps the "error" attribute of every record it sees.
type recordingHandler struct {
	mu     sync.Mutex
	errors []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			h.errors = append(h.errors, a.Value.String())
		}
		return true
	})
	return nil
}

func (h *recordingHandler) lines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.errors...)
}

// A 5xx *apperr.Error is logged by Error like any 5xx, its cause included, and ErrorUnlogged writes the same bytes
// without the log. A 4xx is not logged. Not parallel: it swaps the process-wide slog default.
func TestError_AppErrLogging(t *testing.T) {
	logs := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	down := apperr.Unavailable("AI_PROVIDER_DOWN", "provider down").Wrap(errors.New("dial tcp 10.0.0.7:443"))
	logged := httptest.NewRecorder()
	Error(logged, down, 0)
	if got := logs.lines(); len(got) != 1 || got[0] != "provider down: dial tcp 10.0.0.7:443" {
		t.Errorf("Error logged %q, want one line carrying the cause", got)
	}

	unlogged := httptest.NewRecorder()
	ErrorUnlogged(unlogged, down, 0)
	if got := logs.lines(); len(got) != 1 {
		t.Errorf("ErrorUnlogged logged: %q", got[1:])
	}
	if unlogged.Code != logged.Code || unlogged.Body.String() != logged.Body.String() {
		t.Errorf("ErrorUnlogged wrote %d %s, Error wrote %d %s", unlogged.Code, unlogged.Body, logged.Code, logged.Body)
	}

	Error(httptest.NewRecorder(), apperr.Conflict("FLOW_LOCKED", "the flow is locked").Wrap(errors.New("row locked")), 0)
	if got := logs.lines(); len(got) != 1 {
		t.Errorf("a 4xx was logged: %q", got[1:])
	}
}

// Every kind's default code is the code statusCode gives the kind's status, so an endpoint that moves onto apperr
// keeps the codes its clients switch on. Unknown kinds are internal errors on both sides.
func TestAppErrKinds_DefaultCodesAreTheStatusCodes(t *testing.T) {
	t.Parallel()
	for k := range 256 {
		kind := apperr.Kind(k)
		if got := statusCode(kind.Status()); got != kind.Code() {
			t.Errorf("%v: default code %q, but render gives status %d the code %q", kind, kind.Code(), kind.Status(), got)
		}
	}
}

// The explicit-code writers keep their own status and code for an *apperr.Error too, and still never put its
// cause on the wire.
func TestErrorWithCode_AppErrKeepsItsMessage(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	ErrorWithCode(w, apperr.Invalid("", "too many chats").Wrap(errors.New("pool: 3 of 3 slots busy")), http.StatusTooManyRequests, "CHAT_CAPACITY_REACHED")
	var body ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if w.Code != http.StatusTooManyRequests || body.Code != "CHAT_CAPACITY_REACHED" || body.Message != "too many chats" {
		t.Errorf("got %d %+v", w.Code, body)
	}
}
