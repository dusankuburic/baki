package middleware

import (
	"bytes"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
)

// TestCompress_AdversarialBodyIsNotEncodedAtMaxQuality is the resource gate on
// the response path.
//
// The brotli writer pool was built at brotli.BestCompression (q11), which is
// pathological on content that does not compress — and response bodies carry
// caller-influenced text (rule metadata, parse-error snippets, block names from
// an uploaded flow). Measured on a 2 MB high-entropy body: q11 took 2.601s for a
// 1.3x ratio; the configured level takes ~65ms for the SAME 1.3x. The extra
// effort buys literally nothing on incompressible input, and at ~0.8 MB/s a
// few-megabyte response pins a core for seconds.
//
// The gate is deterministic: the configured level is pinned, and the
// middleware's output must be byte-identical to a direct encode at that level
// (brotli is deterministic for a given level and input) — so a pool built at
// another level fails however fast the machine is. The q6-vs-q11 cost ratio
// itself is a property of the encoder, measured by BenchmarkBrotli_AdversarialBody
// instead of by wall clock here: under -race, which is how CI runs the suite,
// the q11 reference encode alone took ~34 s.
func TestCompress_AdversarialBodyIsNotEncodedAtMaxQuality(t *testing.T) {
	if brotliQuality > 6 {
		t.Fatalf("brotliQuality is %d — q7+ costs order-of-magnitude more CPU on incompressible "+
			"input for no ratio gain, and response bodies carry caller-influenced text",
			brotliQuality)
	}

	body := highEntropyBody(64 << 10)
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc != "br" {
		t.Fatalf("Content-Encoding = %q, want br", enc)
	}

	if got, want := rec.Body.Bytes(), brotliEncode(body, brotliQuality); !bytes.Equal(got, want) {
		t.Errorf("the middleware's brotli output (%d bytes) differs from a q%d encode (%d bytes) — "+
			"the writer pool is not encoding at brotliQuality", len(got), brotliQuality, len(want))
	}
	// The comparison has teeth: max quality encodes this body differently.
	if bytes.Equal(rec.Body.Bytes(), brotliEncode(body, brotli.BestCompression)) {
		t.Fatal("q11 output equals the configured level's — the byte comparison proves nothing on this input")
	}
}

// BenchmarkBrotli_AdversarialBody measures the cost the level pin above
// protects against — the configured level vs q11 on a 2 MB high-entropy body:
//
//	go test -run '^$' -bench Brotli_AdversarialBody ./internal/api/middleware/
func BenchmarkBrotli_AdversarialBody(b *testing.B) {
	body := highEntropyBody(2 << 20)
	for _, level := range []int{brotliQuality, brotli.BestCompression} {
		b.Run(fmt.Sprintf("q%d", level), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				brotliEncode(body, level)
			}
		})
	}
}

// highEntropyBody returns size bytes of base64-alphabet noise: text a JSON
// response could carry, which no encoder level compresses much.
func highEntropyBody(size int) []byte {
	r := rand.New(rand.NewSource(1))
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	body := make([]byte, size)
	for i := range body {
		body[i] = alpha[r.Intn(len(alpha))]
	}
	return body
}

// brotliEncode is the reference encode at a level, with the middleware's own
// window (brotliWindowBits): the window changes the output bytes too, and the
// comparison below is about the LEVEL.
func brotliEncode(body []byte, level int) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: level, LGWin: brotliWindowBits})
	_, _ = w.Write(body)
	_ = w.Close()
	return buf.Bytes()
}

// TestCompress_StillCompressesRealPayloads keeps the change from being a
// silent removal of compression: a realistic JSON body must still shrink a lot.
func TestCompress_StillCompressesRealPayloads(t *testing.T) {
	unit := `{"ruleId":"parse-error","severity":"error","title":"Parse error (line 1)"},`
	body := "["
	for len(body) < 512*1024 {
		body += unit
	}
	body += "]"

	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := rec.Body.Len()
	if out == 0 || out >= len(body)/10 {
		t.Errorf("compressed %d bytes to %d — the encoder is no longer doing useful work",
			len(body), out)
	}
}
