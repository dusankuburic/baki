package middleware

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

// jsonBodyOf builds a compressible JSON-ish body of about n bytes.
func jsonBodyOf(n int) string {
	var sb strings.Builder
	sb.WriteString(`{"blocks":[`)
	for i := 0; sb.Len() < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":"b` + strconv.Itoa(i) + `","type":"action","properties":{"Message":"Message ` + strconv.Itoa(i*7919%10007) + `","Icon":"Display.Icon.None"},"line":` + strconv.Itoa(i) + `}`)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// serveCompressed runs handler behind Compress for the given Accept-Encoding
// and returns the recorder plus the compressWriter the handler observed.
func serveCompressed(t *testing.T, accept string, handler func(w http.ResponseWriter)) (*httptest.ResponseRecorder, *compressWriter) {
	t.Helper()
	var seen *compressWriter
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w)
		cw, ok := w.(*compressWriter)
		if !ok {
			t.Fatalf("handler saw %T, want *compressWriter", w)
		}
		seen = cw
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", accept)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr, seen
}

// TestCompress_LargeBodyUsesFastLevel: a body of largeBodySize and up, handed
// over in one Write (render.JSON and the content handlers do that), is
// encoded at the fast level; a small body keeps the default level. Measured
// on a 5.6 MB flow document, brotli q5 cost 106 ms per response and q1 20 ms
// for a fifth more output — under a 40-user read mix the q5 encoding of
// multi-megabyte flow bodies was two thirds of all CPU.
func TestCompress_LargeBodyUsesFastLevel(t *testing.T) {
	large := jsonBodyOf(largeBodySize + 1024)
	small := jsonBodyOf(16 << 10)

	for _, tc := range []struct {
		name     string
		accept   string
		body     string
		wantFast bool
		wantPool encoderPool
	}{
		{"brotli large", "br", large, true, &brotliFastPool},
		{"brotli small", "br", small, false, &brotliPool},
		{"gzip large", "gzip", large, true, &gzipFastPool},
		{"gzip small", "gzip", small, false, &gzipPool},
		{"deflate large", "deflate", large, true, &flateFastPool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var used encoderPool
			rr, _ := serveCompressed(t, tc.accept, func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(tc.body))
				cw := w.(*compressWriter)
				if cw.fast != tc.wantFast {
					t.Errorf("fast = %v, want %v", cw.fast, tc.wantFast)
				}
				if cw.encoder == nil {
					t.Fatal("no encoder started")
				}
				used = cw.encoder.pool
			})
			if used != tc.wantPool {
				t.Errorf("encoder came from the wrong pool (fast=%v)", tc.wantFast)
			}
			if got := rr.Header().Get("Content-Encoding"); got != tc.accept {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.accept)
			}
			if !bytes.Equal(decode(t, tc.accept, rr.Body.Bytes()), []byte(tc.body)) {
				t.Error("decoded body differs from the original")
			}
		})
	}
}

// TestCompress_ContentLengthDecidesTheLevel: a streamed body with a declared
// Content-Length at or above largeBodySize takes the fast level from its
// first byte; without a declared length a body that arrives in small pieces
// is judged by its first pieces and keeps the default level (documented).
func TestCompress_ContentLengthDecidesTheLevel(t *testing.T) {
	body := jsonBodyOf(largeBodySize + 1024)
	piece := 4 << 10

	t.Run("declared large", func(t *testing.T) {
		rr, cw := serveCompressed(t, "br", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			for i := 0; i < len(body); i += piece {
				end := min(i+piece, len(body))
				_, _ = w.Write([]byte(body[i:end]))
			}
		})
		if !cw.fast {
			t.Error("declared large body did not take the fast level")
		}
		if !bytes.Equal(decode(t, "br", rr.Body.Bytes()), []byte(body)) {
			t.Error("decoded body differs")
		}
	})
	t.Run("undeclared pieces", func(t *testing.T) {
		rr, cw := serveCompressed(t, "br", func(w http.ResponseWriter) {
			for i := 0; i < len(body); i += piece {
				end := min(i+piece, len(body))
				_, _ = w.Write([]byte(body[i:end]))
			}
		})
		if cw.fast {
			t.Error("a body streamed in small pieces must keep the default level (size unknown at start)")
		}
		if !bytes.Equal(decode(t, "br", rr.Body.Bytes()), []byte(body)) {
			t.Error("decoded body differs")
		}
	})
}

// TestCompress_FastLevelIsCheaperAndStillShrinks: the fast level must still
// compress JSON well (the ratio is the reason to compress at all) — a sanity
// bound rather than a benchmark.
func TestCompress_FastLevelIsCheaperAndStillShrinks(t *testing.T) {
	body := jsonBodyOf(2 << 20)
	rr, cw := serveCompressed(t, "br", func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(body))
	})
	if !cw.fast {
		t.Fatal("2 MiB body did not take the fast level")
	}
	if ratio := float64(len(body)) / float64(rr.Body.Len()); ratio < 4 {
		t.Errorf("fast level ratio = %.1fx, want >= 4x on JSON", ratio)
	}
}

func decode(t *testing.T, encoding string, b []byte) []byte {
	t.Helper()
	var r io.Reader
	switch encoding {
	case "br":
		r = brotli.NewReader(bytes.NewReader(b))
	case "gzip":
		gr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		r = gr
	case "deflate":
		r = flate.NewReader(bytes.NewReader(b))
	default:
		t.Fatalf("unknown encoding %q", encoding)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode %s: %v", encoding, err)
	}
	return out
}
