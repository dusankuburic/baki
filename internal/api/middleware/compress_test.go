package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestPickEncoding(t *testing.T) {
	cases := []struct {
		accept string
		want   string
	}{
		{"", ""},                         // no header → passthrough
		{"br", "br"},                     // brotli preferred
		{"gzip", "gzip"},                 // gzip only
		{"br, gzip", "br"},               // tie at q=1 → prefer br
		{"gzip;q=0.9, br;q=0.8", "gzip"}, // higher q wins over preference
		{"gzip;q=1.0, br;q=0.5", "gzip"}, // q dominates
		{"br;q=0", ""},                   // q=0 disqualifies
		{"identity;q=1, *;q=0", ""},      // wildcard q=0 forbids all
		{"*", "br"},                      // wildcard allows → preferred
		{"deflate", "deflate"},           // deflate only
		{"br, gzip, deflate", "br"},      // all present → preference order
		{"gzip, deflate;q=0.5", "gzip"},  // gzip q=1 beats deflate q=0.5
		{"unknown-enc, x-custom", ""},    // nothing supported
		{"br;q=0.8, gzip;q=0.8", "br"},   // equal q → preference br
	}
	for _, c := range cases {
		if got := pickEncoding(c.accept); got != c.want {
			t.Errorf("pickEncoding(%q) = %q, want %q", c.accept, got, c.want)
		}
	}
}

// roundTrip runs a request through Compress wrapping a handler that writes the
// given content-type + body, returning the recorded response.
func roundTrip(t *testing.T, accept, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestCompress_BrotliAppliedToJSON(t *testing.T) {
	body := `{"message":"hello world","repeated":"aaaaaaaaaabbbbbbbbbbcccccccccc"}`
	rr := roundTrip(t, "br", "application/json", body)

	if got := rr.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	if rr.Header().Get("Content-Length") != "" {
		t.Error("Content-Length should be dropped when compressing")
	}
	dec, _ := io.ReadAll(brotli.NewReader(rr.Result().Body))
	if string(dec) != body {
		t.Errorf("decompressed body mismatch: got %q want %q", dec, body)
	}
}

func TestCompress_GzipFallback(t *testing.T) {
	body := `{"x":"` + repeat("y", 500) + `"}`
	rr := roundTrip(t, "gzip", "application/json", body)
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	gz, err := gzip.NewReader(rr.Result().Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	dec, _ := io.ReadAll(gz)
	if string(dec) != body {
		t.Errorf("decompressed body mismatch")
	}
}

func TestCompress_BrotliPreferredOverGzip(t *testing.T) {
	rr := roundTrip(t, "br, gzip", "application/json", `{"a":1}`)
	if got := rr.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("expected br preferred, got %q", got)
	}
}

func TestCompress_SSEIsNotCompressed(t *testing.T) {
	// SSE must pass through: compressing would buffer chunks and break streaming.
	body := "data: hello\n\n"
	rr := roundTrip(t, "br", "text/event-stream", body)
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("SSE must not be compressed, got Content-Encoding %q", got)
	}
	if rr.Body.String() != body {
		t.Errorf("SSE body should pass through unchanged, got %q", rr.Body.String())
	}
}

func TestCompress_ContentTypeWithCharset(t *testing.T) {
	// "application/json; charset=utf-8" must still be detected as compressible.
	rr := roundTrip(t, "br", "application/json; charset=utf-8", `{"a":1}`)
	if got := rr.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want br for json;charset", got)
	}
}

func TestCompress_NoAcceptEncodingPassthrough(t *testing.T) {
	body := `{"a":1}`
	rr := roundTrip(t, "", "application/json", body)
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("expected no compression without Accept-Encoding, got %q", got)
	}
	if rr.Body.String() != body {
		t.Errorf("body should pass through unchanged")
	}
	if got := rr.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
}

func TestCompress_NonCompressibleTypePassthrough(t *testing.T) {
	// Binary/octet content-types are skipped even when br is accepted.
	rr := roundTrip(t, "br", "application/octet-stream", "\x00\x01\x02\x03")
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("octet-stream must not be compressed, got %q", got)
	}
}

func TestCompress_VaryAlwaysSet(t *testing.T) {
	// Even when not compressing, Vary must be advertised so caches key correctly.
	rr := roundTrip(t, "br", "text/event-stream", "data:x\n\n")
	if got := rr.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
}

func TestCompress_FlusherDelegated(t *testing.T) {
	// A handler that flushes (e.g. progressive JSON) must not deadlock and the
	// compressed bytes must reach the client.
	flushHandler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"a":"`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, `b"}`)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br")
	rr := httptest.NewRecorder()
	flushHandler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	dec, _ := io.ReadAll(brotli.NewReader(rr.Result().Body))
	if string(dec) != `{"a":"b"}` {
		t.Errorf("decompressed body = %q, want {\"a\":\"b\"}", dec)
	}
}

func TestCompress_HijackDelegated(t *testing.T) {
	// The wrapper must implement http.Hijacker so WebSocket upgrades work.
	// A browser upgrade request carries Accept-Encoding, so the compress path
	// wraps the writer; the inner handler must still see a Hijacker.
	wrapper := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("compressWriter must expose http.Hijacker")
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br")
	rr := httptest.NewRecorder()
	wrapper.ServeHTTP(rr, req)
}

func TestCompress_LargeJSONPayloadShrinks(t *testing.T) {
	// Sanity: brotli must actually reduce a large repetitive JSON payload.
	body := `{"data":"` + repeat("abcdefghij", 1000) + `"}`
	rr := roundTrip(t, "br", "application/json", body)
	compressed := rr.Body.Len()
	if compressed >= len(body) {
		t.Errorf("brotli did not shrink payload: %d >= %d", compressed, len(body))
	}
	dec, _ := io.ReadAll(brotli.NewReader(rr.Result().Body))
	if !bytes.Equal(dec, []byte(body)) {
		t.Error("decompressed body mismatch")
	}
}

func repeat(s string, n int) string {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.WriteString(s)
	}
	return b.String()
}
