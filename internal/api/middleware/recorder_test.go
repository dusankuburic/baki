package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponseRecorder_DefaultStatus(t *testing.T) {
	rr := NewResponseRecorder(httptest.NewRecorder())
	if rr.Status() != http.StatusOK {
		t.Errorf("expected default 200, got %d", rr.Status())
	}
}

func TestResponseRecorder_WriteHeaderCapturesStatus(t *testing.T) {
	inner := httptest.NewRecorder()
	rr := NewResponseRecorder(inner)
	rr.WriteHeader(http.StatusNotFound)
	if rr.Status() != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Status())
	}
	if inner.Code != http.StatusNotFound {
		t.Errorf("expected inner recorder 404, got %d", inner.Code)
	}
}

func TestResponseRecorder_WriteSetsDefaultStatus(t *testing.T) {
	inner := httptest.NewRecorder()
	rr := NewResponseRecorder(inner)
	rr.Write([]byte("hello"))
	if rr.Status() != http.StatusOK {
		t.Errorf("expected implicit 200 after Write, got %d", rr.Status())
	}
}

func TestResponseRecorder_ByteCount(t *testing.T) {
	inner := httptest.NewRecorder()
	rr := NewResponseRecorder(inner)
	n, _ := rr.Write([]byte("hello world"))
	if n != 11 {
		t.Errorf("expected 11 bytes written, got %d", n)
	}
	if rr.Bytes() != 11 {
		t.Errorf("expected Bytes()=11, got %d", rr.Bytes())
	}

	rr.Write([]byte("more"))
	if rr.Bytes() != 15 {
		t.Errorf("expected cumulative Bytes()=15, got %d", rr.Bytes())
	}
}

func TestResponseRecorder_WriteHeaderOnlyOnce(t *testing.T) {
	inner := httptest.NewRecorder()
	rr := NewResponseRecorder(inner)
	rr.WriteHeader(http.StatusTeapot)
	rr.WriteHeader(http.StatusOK) // should not override
	if rr.Status() != http.StatusTeapot {
		t.Errorf("expected first status (418) preserved, got %d", rr.Status())
	}
}

func TestResponseRecorder_FlushDelegates(t *testing.T) {
	flusher := &fakeFlusher{ResponseRecorder: NewResponseRecorder(httptest.NewRecorder())}
	flusher.Flush()
	if !flusher.flushed {
		t.Error("expected Flush to delegate to underlying Flusher")
	}
}

func TestResponseRecorder_Unwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	rr := NewResponseRecorder(inner)
	if rr.Unwrap() != inner {
		t.Error("expected Unwrap to return underlying ResponseWriter")
	}
}

type fakeFlusher struct {
	*ResponseRecorder
	flushed bool
}

func (f *fakeFlusher) Flush() {
	f.flushed = true
}
