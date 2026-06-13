package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecovery_PanicReturns500(t *testing.T) {
	handler := Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went wrong")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["error"] != "internal server error" {
		t.Errorf("expected 'internal server error', got %q", body["error"])
	}
}

func TestRecovery_PanicWithNilDereference(t *testing.T) {
	handler := Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p *int
		_ = *p
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rr.Code)
	}
}

func TestRecovery_NormalRequestPassesThrough(t *testing.T) {
	handler := Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Errorf("expected 'ok', got %q", rr.Body.String())
	}
}

func TestRecovery_LogsPanicDetails(t *testing.T) {
	buf := captureSlog(t)

	handler := Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic message")
	}))

	req := httptest.NewRequest(http.MethodDelete, "/api/items/42", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	logOutput := buf.String()
	if !contains(logOutput, "test panic message") {
		t.Errorf("expected log to contain panic message, got: %s", logOutput)
	}
	if !contains(logOutput, "DELETE") {
		t.Errorf("expected log to contain method DELETE, got: %s", logOutput)
	}
	if !contains(logOutput, "/api/items/42") {
		t.Errorf("expected log to contain path, got: %s", logOutput)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
