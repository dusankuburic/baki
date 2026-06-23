package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseIntParam(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		def      int
		wantVal  int
		wantOK   bool
		wantCode int // response status written when ok is false
	}{
		{name: "empty uses default", raw: "", def: 50, wantVal: 50, wantOK: true},
		{name: "valid positive", raw: "10", def: 50, wantVal: 10, wantOK: true},
		{name: "valid zero", raw: "0", def: 50, wantVal: 0, wantOK: true},
		{name: "valid negative", raw: "-5", def: 50, wantVal: -5, wantOK: true},
		{name: "non-numeric rejected", raw: "abc", def: 50, wantOK: false, wantCode: http.StatusBadRequest},
		{name: "float rejected", raw: "1.5", def: 50, wantOK: false, wantCode: http.StatusBadRequest},
		{name: "trailing junk rejected", raw: "10x", def: 50, wantOK: false, wantCode: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			got, ok := parseIntParam(w, tc.raw, "limit", tc.def)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok {
				if got != tc.wantVal {
					t.Fatalf("value = %d, want %d", got, tc.wantVal)
				}
				if w.Code != http.StatusOK {
					t.Fatalf("no response should be written on success, got status %d", w.Code)
				}
				return
			}
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}
		})
	}
}
