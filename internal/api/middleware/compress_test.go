package middleware

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

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

func TestNegotiateEncoding(t *testing.T) {
	cases := []struct {
		accept  string
		offered []string
		want    string
	}{
		{"", []string{"br", "gzip"}, ""},                         // no header → identity
		{"br, gzip", []string{"br", "gzip"}, "br"},               // tie → brotli
		{"gzip, br", []string{"gzip", "br"}, "br"},               // order of offer is irrelevant
		{"gzip", []string{"br", "gzip"}, "gzip"},                 // only gzip accepted
		{"br", []string{"gzip"}, ""},                             // accepted one not on offer
		{"br;q=0, gzip", []string{"br", "gzip"}, "gzip"},         // q=0 refuses brotli
		{"gzip;q=0.9, br;q=0.5", []string{"br", "gzip"}, "gzip"}, // q wins over preference
		{"*", []string{"br", "gzip"}, "br"},                      // wildcard accepts all
		{"*;q=0", []string{"br", "gzip"}, ""},                    // wildcard refuses all
		{"gzip;q=0, *", []string{"br", "gzip"}, "br"},            // explicit q=0 beats wildcard
		{"gzip;q=0, *", []string{"gzip"}, ""},                    //
		{"identity", []string{"br", "gzip"}, ""},                 // identity only
		{"BR;Q=1", []string{"br"}, "br"},                         // case-insensitive name
		{"br;q=x, gzip", []string{"br", "gzip"}, "gzip"},         // malformed part ignored
	}
	for _, c := range cases {
		if got := NegotiateEncoding(c.accept, c.offered...); got != c.want {
			t.Errorf("NegotiateEncoding(%q, %v) = %q, want %q", c.accept, c.offered, got, c.want)
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

// largeJSON is a JSON body comfortably over minCompressSize, which tiny
// bodies are not compressed below.
var largeJSON = `{"message":"hello world","repeated":"` + repeat("aaaaaaaaaabbbbbbbbbbcccccccccc", 50) + `"}`

func TestCompress_BrotliAppliedToJSON(t *testing.T) {
	body := largeJSON
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
	body := `{"x":"` + repeat("y", 2000) + `"}`
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
	rr := roundTrip(t, "br, gzip", "application/json", largeJSON)
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
	rr := roundTrip(t, "br", "application/json; charset=utf-8", largeJSON)
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

// TestCompress_FlushReachesTheClient: bytes written before a Flush reach the client while the handler is still running, on every encoding.
// Regression: Flush looked for a Flush method on the encoder, which pooledWriter did not have, so the check failed silently — the ResponseWriter was flushed but the handler's bytes sat in the encoder until Close. TestCompress_FlusherDelegated cannot see that (it reads the body after the handler finished), so this one holds the handler open on a real connection.
func TestCompress_FlushReachesTheClient(t *testing.T) {
	const first, second = `{"progress":[1,2,3],`, `"done":true}`
	for _, enc := range []string{"br", "gzip", "deflate"} {
		t.Run(enc, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, first)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, second)
			})))
			defer srv.Close()
			var once sync.Once
			releaseHandler := func() { once.Do(func() { close(release) }) }
			defer releaseHandler() // before srv.Close, which waits for the handler

			type result struct {
				resp *http.Response
				dec  io.Reader
				got  string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
				// Set explicitly, so the transport leaves the body encoded.
				req.Header.Set("Accept-Encoding", enc)
				resp, err := srv.Client().Do(req)
				if err != nil {
					done <- result{err: err}
					return
				}
				dec, err := newDecoder(enc, resp.Body)
				if err != nil {
					done <- result{resp: resp, err: err}
					return
				}
				buf := make([]byte, len(first))
				_, err = io.ReadFull(dec, buf)
				done <- result{resp: resp, dec: dec, got: string(buf), err: err}
			}()

			var res result
			select {
			case res = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the flushed part of the body did not reach the client while the handler was running")
			}
			if res.resp != nil {
				defer res.resp.Body.Close()
			}
			if res.err != nil {
				t.Fatalf("reading the flushed part: %v", res.err)
			}
			if ce := res.resp.Header.Get("Content-Encoding"); ce != enc {
				t.Fatalf("Content-Encoding = %q, want %q", ce, enc)
			}
			if res.got != first {
				t.Fatalf("flushed part = %q, want %q", res.got, first)
			}

			releaseHandler()
			rest, err := io.ReadAll(res.dec)
			if err != nil {
				t.Fatalf("reading the rest: %v", err)
			}
			if string(rest) != second {
				t.Errorf("rest of the body = %q, want %q", rest, second)
			}
		})
	}
}

// newDecoder opens a decoding reader for a Content-Encoding the middleware
// negotiates.
func newDecoder(enc string, r io.Reader) (io.Reader, error) {
	switch enc {
	case "br":
		return brotli.NewReader(r), nil
	case "gzip":
		return gzip.NewReader(r)
	case "deflate":
		return flate.NewReader(r), nil
	}
	return nil, fmt.Errorf("no decoder for %q", enc)
}

// TestCompress_FlushBeforeFirstWrite: a Flush before any Write or WriteHeader commits the headers, so the compression decision has to be made then. Flush used to pass straight through, committing a response without Content-Encoding while the first Write then chose to compress — the client got an encoded body labelled as plain JSON.
func TestCompress_FlushBeforeFirstWrite(t *testing.T) {
	const body = `{"a":"b"}`
	handler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Result carries the headers as they were when the flush committed them.
	res := rr.Result()
	if ce := res.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("committed Content-Encoding = %q, want gzip (body: %q)", ce, rr.Body.Bytes())
	}
	gz, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	if dec, _ := io.ReadAll(gz); string(dec) != body {
		t.Errorf("decompressed body = %q, want %q", dec, body)
	}
}

func TestCompress_HijackDelegated(t *testing.T) {
	// The wrapper must implement http.Hijacker so WebSocket upgrades work: a browser upgrade request carries Accept-Encoding, so the compress path wraps the writer; the inner handler must still see a Hijacker.
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

// TestCompress_PooledEncoderReleasesTheResponse: an encoder goes back to its pool holding no reference to the response it just finished — gzip and flate keep their destination after Close, so the pooled writer kept the last ResponseWriter (and through it the request) alive until a later Get or pool aging; brotli's Close drops it already, its case is covered all the same.
// The pool itself is bypassed (put captures the encoder, held across the GC): sync.Pool drops a random quarter of Puts under -race, and another pooled encoder could come back from Get — either would let the old behaviour pass by luck. The encoders come from the production pools' New, so they are configured exactly as served.
func TestCompress_PooledEncoderReleasesTheResponse(t *testing.T) {
	for _, tc := range []struct {
		enc  string
		pool *sync.Pool
	}{
		{"br", &brotliPool},
		{"gzip", &gzipPool},
		{"deflate", &flatePool},
	} {
		t.Run(tc.enc, func(t *testing.T) {
			pool := &capturePool{}
			response := finishOneCompressedResponse(t, tc.pool.New().(*pooledWriter), pool)
			if pool.pooled == nil {
				t.Fatal("Close did not return the encoder to its pool")
			}
			runtime.GC()
			if response.Value() != nil {
				t.Errorf("the pooled %s encoder still references the finished response's ResponseWriter", tc.enc)
			}
			runtime.KeepAlive(pool.pooled)
		})
	}
}

// capturePool stands in for a sync.Pool, deterministically (sync.Pool drops
// Puts at random under -race): it holds the one writer Put last.
type capturePool struct{ pooled any }

func (c *capturePool) Get() any  { return c.pooled }
func (c *capturePool) Put(v any) { c.pooled = v }

// finishOneCompressedResponse writes one compressed body through a pooledWriter onto a fresh ResponseWriter and closes it, returning only a weak pointer to the ResponseWriter so nothing in the caller keeps it alive. It points the writer at the response exactly as acquireEncoder does.
func finishOneCompressedResponse(t *testing.T, pw *pooledWriter, pool encoderPool) weak.Pointer[httptest.ResponseRecorder] {
	t.Helper()
	rec := httptest.NewRecorder()
	pw.pool = pool
	pw.sink.w = rec
	pw.w.Reset(&pw.sink)
	if _, err := io.WriteString(pw, `{"a":"b"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the encoder wrote nothing to the response — the check below would prove nothing")
	}
	return weak.Make(rec)
}

func repeat(s string, n int) string {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.WriteString(s)
	}
	return b.String()
}

// countingEncoder is a resettableEncoder that counts Resets.
type countingEncoder struct {
	resettableEncoder
	resets int
}

func (c *countingEncoder) Reset(dst io.Writer) {
	c.resets++
	c.resettableEncoder.Reset(dst)
}

// TestCompress_OneEncoderResetPerResponse: gzip and flate clear their hash tables on every Reset (~13 µs), and a response paid for two — one to start it and one onto io.Discard at Close, to drop the pooled encoder's reference to the response. The sink between encoder and response drops it instead; TestCompress_PooledEncoderReleasesTheResponse holds the other half.
func TestCompress_OneEncoderResetPerResponse(t *testing.T) {
	pw := &pooledWriter{}
	gz, _ := gzip.NewWriterLevel(&pw.sink, gzip.DefaultCompression)
	enc := &countingEncoder{resettableEncoder: gz}
	pw.w = enc
	pool := &capturePool{pooled: pw}

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		got := acquireEncoder(pool, rec)
		_, _ = io.WriteString(got, largeJSON)
		if err := got.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("response %d: gzip reader: %v", i, err)
		}
		if dec, _ := io.ReadAll(zr); string(dec) != largeJSON {
			t.Fatalf("response %d: decoded body differs", i)
		}
	}
	if enc.resets != 3 {
		t.Errorf("3 responses cost %d encoder Resets, want 3", enc.resets)
	}
}

// TestCompress_SmallBodyIsNotCompressed: a body under minCompressSize goes out as is — compressing it cost 20-40 µs, more than the rest of the pipeline, to save bytes that fit one TCP segment either way. The decision waits for the body (render.JSON sets no Content-Length), so the status must survive the wait.
func TestCompress_SmallBodyIsNotCompressed(t *testing.T) {
	const body = `{"id":"abc","ok":true}`
	for _, enc := range []string{"br", "gzip", "deflate"} {
		t.Run(enc, func(t *testing.T) {
			handler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", enc)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusCreated {
				t.Errorf("status = %d, want 201", rr.Code)
			}
			if ce := rr.Header().Get("Content-Encoding"); ce != "" {
				t.Errorf("Content-Encoding = %q on a %d-byte body, want none", ce, len(body))
			}
			if rr.Body.String() != body {
				t.Errorf("body = %q, want %q", rr.Body.String(), body)
			}
			if got := rr.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q, want Accept-Encoding", got)
			}
		})
	}
}

// TestCompress_SmallContentLengthIsNotCompressed: a known Content-Length under
// the threshold decides at once, and the header is kept.
func TestCompress_SmallContentLengthIsNotCompressed(t *testing.T) {
	const body = `{"a":1}`
	handler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if ce := rr.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none", ce)
	}
	if cl := rr.Header().Get("Content-Length"); cl != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(body))
	}
	if rr.Body.String() != body {
		t.Errorf("body = %q, want %q", rr.Body.String(), body)
	}
}

// TestCompress_LargeContentLengthIsCompressed: a known Content-Length at or over the threshold compresses from the start, and the now-wrong header is dropped.
func TestCompress_LargeContentLengthIsCompressed(t *testing.T) {
	handler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(largeJSON)))
		_, _ = io.WriteString(w, largeJSON)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if ce := rr.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q kept on a compressed body", cl)
	}
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	if dec, _ := io.ReadAll(zr); string(dec) != largeJSON {
		t.Error("decompressed body mismatch")
	}
}

// TestCompress_BodyWrittenInPiecesCrossesTheThreshold: a body that reaches minCompressSize over several small writes is compressed whole — the bytes held back while it was small included — with the status it was given.
func TestCompress_BodyWrittenInPiecesCrossesTheThreshold(t *testing.T) {
	piece := `{"k":"` + repeat("v", 90) + `"},`
	handler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		for i := 0; i < 30; i++ {
			_, _ = io.WriteString(w, piece)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rr.Code)
	}
	if ce := rr.Header().Get("Content-Encoding"); ce != "br" {
		t.Fatalf("Content-Encoding = %q, want br", ce)
	}
	dec, _ := io.ReadAll(brotli.NewReader(rr.Body))
	if want := repeat(piece, 30); string(dec) != want {
		t.Errorf("decompressed %d bytes, want %d", len(dec), len(want))
	}
}

// TestCompress_DeferredBodilessStatus: a compressible response with no body (204) passes through with its status and no Content-Encoding — an encoder would add framing bytes a 204 must not carry.
func TestCompress_DeferredBodilessStatus(t *testing.T) {
	handler := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rr.Code)
	}
	if ce := rr.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q on a 204", ce)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("204 carried %d body bytes", rr.Body.Len())
	}
}
