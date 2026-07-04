package middleware

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

// compressibleTypes is the set of response content types eligible for
// compression. It mirrors chi middleware.Compress's default allowlist. SSE
// (text/event-stream) is intentionally excluded: compressing it would buffer
// chunks and defeat token streaming. WebSocket upgrades (101 with no
// content-type) are likewise skipped.
var compressibleTypes = map[string]struct{}{
	"text/html":                {},
	"text/css":                 {},
	"text/plain":               {},
	"text/javascript":          {},
	"text/xml":                 {},
	"application/javascript":   {},
	"application/x-javascript": {},
	"application/json":         {},
	"application/atom+xml":     {},
	"application/rss+xml":      {},
	"image/svg+xml":            {},
}

func isCompressibleContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	_, ok := compressibleTypes[strings.ToLower(strings.TrimSpace(ct))]
	return ok
}

// pickEncoding chooses the best supported encoding from an Accept-Encoding
// header, honouring q-values (q=0 disqualifies). Preference order on a tie is
// brotli > gzip > deflate, since brotli yields ~15-20% better ratios on text
// than gzip for ~equal CPU. Returns "" when the client accepts none of them.
func pickEncoding(accept string) string {
	if accept == "" {
		return ""
	}
	bestQ := 0.0
	bestEnc := ""
	wildcard := -1.0 // unknown until seen; valid q-values are >= 0

	for _, part := range strings.Split(accept, ",") {
		name, q, ok := parseAcceptPart(part)
		if !ok {
			continue
		}
		switch name {
		case "*":
			wildcard = q
		case "br", "gzip", "deflate":
			// Pick the encoding with the highest client q-value; break ties by
			// our preference order (br > gzip > deflate).
			if q > bestQ || (q == bestQ && pref(name) > pref(bestEnc)) {
				bestQ, bestEnc = q, name
			}
		case "identity":
			// Tracked implicitly: if identity has a higher q than any encoding,
			// bestEnc stays "" and we return no compression below.
		}
	}
	if bestEnc != "" && bestQ > 0 {
		return bestEnc
	}
	// No explicit supported encoding with q>0. A wildcard with q>0 grants
	// permission to use any encoding — pick our preferred.
	if wildcard > 0 {
		return "br"
	}
	return ""
}

func pref(enc string) int {
	switch enc {
	case "br":
		return 3
	case "gzip":
		return 2
	case "deflate":
		return 1
	default:
		return 0
	}
}

// parseAcceptPart parses a single "gzip;q=0.8" token into name and q (default 1).
// ok is false for malformed tokens.
func parseAcceptPart(part string) (string, float64, bool) {
	fields := strings.Split(strings.TrimSpace(part), ";")
	name := strings.ToLower(strings.TrimSpace(fields[0]))
	if name == "" {
		return "", 0, false
	}
	q := 1.0
	for _, f := range fields[1:] {
		f = strings.TrimSpace(f)
		if strings.HasPrefix(f, "q=") {
			val := strings.TrimSpace(f[2:])
			parsed, ok := parseFloat(val)
			if !ok {
				return name, 0, false
			}
			q = parsed
		}
	}
	return name, q, true
}

// parseFloat is a tiny float parser for q-values (avoids pulling strconv for
// the one place we need it and keeps the error path explicit). Accepts the
// "0", "1", "0.8", "1.0" forms browsers actually emit.
func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	intPart := ""
	fracPart := ""
	dotSeen := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if dotSeen {
				fracPart += string(r)
			} else {
				intPart += string(r)
			}
		case r == '.' && !dotSeen:
			dotSeen = true
		default:
			return 0, false
		}
	}
	if intPart == "" && fracPart == "" {
		return 0, false
	}
	val := 0.0
	for _, r := range intPart {
		val = val*10 + float64(r-'0')
	}
	div := 1.0
	for _, r := range fracPart {
		val = val*10 + float64(r-'0')
		div *= 10
	}
	val /= div
	if neg {
		val = -val
	}
	return val, true
}

// Pools reuse expensive encoder allocations across requests. Brotli and gzip
// writers are the costly part; deflate reuses gzip's flate pool indirectly.
var (
	brotliPool = sync.Pool{
		New: func() any { return brotli.NewWriterLevel(nil, brotli.BestCompression) },
	}
	gzipPool = sync.Pool{
		New: func() any {
			w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
			return w
		},
	}
	flatePool = sync.Pool{
		New: func() any {
			w, _ := flate.NewWriter(nil, flate.DefaultCompression)
			return w
		},
	}
)

// Compress returns middleware that negotiates brotli > gzip > deflate from the
// request's Accept-Encoding and applies the encoder only to compressible
// content types. Brotli is preferred because it beats gzip ~15-20% on text
// payloads. Non-compressible responses (SSE, WebSocket upgrades, binary blobs)
// pass through unchanged. The Flusher/Hijacker/Pusher interfaces are delegated
// so streaming and protocol-upgrade handlers keep working.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := pickEncoding(r.Header.Get("Accept-Encoding"))
		// Always advertise that the response varies by Accept-Encoding so
		// caches don't serve a brotli body to a gzip-only client (or vice versa).
		w.Header().Add("Vary", "Accept-Encoding")
		if enc == "" {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w, encoding: enc}
		defer cw.close()
		next.ServeHTTP(cw, r)
	})
}

// compressWriter wraps the underlying ResponseWriter and lazily installs a
// compression encoder once it sees a compressible Content-Type at WriteHeader
// or the first Write. Until then it is a transparent pass-through, so
// non-compressible responses (SSE, upgrades, binary) behave exactly as without
// the middleware.
type compressWriter struct {
	http.ResponseWriter
	encoding string
	encoder  io.WriteCloser
	started  bool // compression initialised
}

// init decides whether to compress based on the response's Content-Type. It is
// called from WriteHeader and Write. When it compresses it sets Content-Encoding,
// drops Content-Length (now invalid), and primes the encoder.
func (c *compressWriter) init() {
	if c.started {
		return
	}
	c.started = true
	// Don't double-compress: if the handler already set a Content-Encoding
	// (e.g. a pre-compressed blob), leave the body untouched.
	if c.Header().Get("Content-Encoding") != "" {
		return
	}
	ct := c.Header().Get("Content-Type")
	if !isCompressibleContentType(ct) {
		return // leave encoder nil → passthrough
	}
	c.Header().Set("Content-Encoding", c.encoding)
	c.Header().Del("Content-Length")
	switch c.encoding {
	case "br":
		w := brotliPool.Get().(*brotli.Writer)
		w.Reset(c.ResponseWriter)
		c.encoder = &pooledWriter{w: w, put: func(v any) { brotliPool.Put(v) }}
	case "gzip":
		w := gzipPool.Get().(*gzip.Writer)
		w.Reset(c.ResponseWriter)
		c.encoder = &pooledWriter{w: w, put: func(v any) { gzipPool.Put(v) }}
	case "deflate":
		w := flatePool.Get().(*flate.Writer)
		w.Reset(c.ResponseWriter)
		c.encoder = &pooledWriter{w: w, put: func(v any) { flatePool.Put(v) }}
	}
}

func (c *compressWriter) WriteHeader(code int) {
	c.init()
	c.ResponseWriter.WriteHeader(code)
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if !c.started {
		// No WriteHeader call: implicit 200. Decide compression now.
		c.init()
	}
	if c.encoder == nil {
		return c.ResponseWriter.Write(p)
	}
	return c.encoder.Write(p)
}

// close flushes and returns the encoder to its pool. Safe to call when
// passthrough (encoder nil) — a no-op.
func (c *compressWriter) close() {
	if c.encoder == nil {
		return
	}
	_ = c.encoder.Close()
}

// Flush delegates to the underlying writer; when compressing, it also flushes
// the encoder so already-buffered bytes reach the client (e.g. a handler that
// flushes JSON progressively).
func (c *compressWriter) Flush() {
	if c.encoder != nil {
		if f, ok := c.encoder.(interface{ Flush() error }); ok {
			_ = f.Flush()
		}
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates so WebSocket upgrades and other connection takeovers work
// unchanged. Compression never applies to a hijacked connection: upgrades send
// a 101 with no Content-Type, so init() leaves the encoder nil before any
// Hijack can occur.
func (c *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := c.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errNotHijacker
}

// Push delegates HTTP/2 server push if the underlying writer supports it.
func (c *compressWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := c.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap exposes the underlying writer for handlers that introspect it (e.g.
// http.ResponseController). Requires Go 1.20+.
func (c *compressWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// pooledWriter pairs an encoder with the pool it came from so Close returns it.
type pooledWriter struct {
	w   io.WriteCloser
	put func(any)
}

func (p *pooledWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pooledWriter) Close() error {
	err := p.w.Close()
	p.put(p.w)
	return err
}

var errNotHijacker = hijackError("response writer does not implement http.Hijacker")

type hijackError string

func (e hijackError) Error() string { return string(e) }
