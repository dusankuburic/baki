package middleware

import (
	"bytes"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
// The assertion is RELATIVE, measured in this process against q11 on the same
// input. An absolute wall-clock budget is not usable here: the suite runs 36
// packages in parallel under -race, where the same 65ms encode was observed at
// 1.4s purely from contention. A ratio is immune to that.
func TestCompress_AdversarialBodyIsNotEncodedAtMaxQuality(t *testing.T) {
	// The configured level is the thing under test; pin it explicitly so a
	// regression to q11 fails deterministically, independent of any timing.
	if brotliQuality > 6 {
		t.Fatalf("brotliQuality is %d — q7+ costs order-of-magnitude more CPU on incompressible "+
			"input for no ratio gain, and response bodies carry caller-influenced text",
			brotliQuality)
	}

	const size = 2 << 20
	r := rand.New(rand.NewSource(1))
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	body := make([]byte, size)
	for i := range body {
		body[i] = alpha[r.Intn(len(alpha))]
	}

	encode := func(level int) time.Duration {
		var buf bytes.Buffer
		w := brotli.NewWriterLevel(&buf, level)
		start := time.Now()
		_, _ = w.Write(body)
		_ = w.Close()
		return time.Since(start)
	}

	configured := encode(brotliQuality)
	maxQuality := encode(11)

	t.Logf("2 MB high-entropy: q%d=%v  q11=%v", brotliQuality,
		configured.Round(time.Millisecond), maxQuality.Round(time.Millisecond))

	if configured*5 > maxQuality {
		t.Errorf("the configured level (q%d, %v) is not materially cheaper than q11 (%v) — "+
			"the encoder is still doing max-quality work on content that cannot compress",
			brotliQuality, configured.Round(time.Millisecond), maxQuality.Round(time.Millisecond))
	}
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
