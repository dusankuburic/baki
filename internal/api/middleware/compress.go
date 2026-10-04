package middleware

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

// compressibleTypes is the set of response content types eligible for compression (chi's default allowlist).
// SSE (text/event-stream) is intentionally excluded: compressing it would buffer chunks and defeat token streaming; WebSocket upgrades (101, no content-type) are skipped too.
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

// pickEncoding chooses the best supported encoding from an Accept-Encoding header, honouring q-values (q=0 disqualifies); ties go brotli > gzip > deflate (~15-20% better text ratios for ~equal CPU). Returns "" when the client accepts none.
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

// NegotiateEncoding picks, from the codings on offer ("br", "gzip", "deflate"), the one Accept-Encoding ranks highest, for a handler that has the body already encoded (a precompressed file). A coding is acceptable at its own q-value, else the wildcard's; ties go brotli > gzip > deflate.
func NegotiateEncoding(accept string, offered ...string) string {
	if accept == "" || len(offered) == 0 {
		return ""
	}
	explicit := make(map[string]float64, 4)
	wildcard := -1.0
	for _, part := range strings.Split(accept, ",") {
		name, q, ok := parseAcceptPart(part)
		if !ok {
			continue
		}
		if name == "*" {
			wildcard = q
		} else {
			explicit[name] = q
		}
	}
	best, bestQ := "", 0.0
	for _, enc := range offered {
		q, ok := explicit[enc]
		if !ok {
			q = wildcard
		}
		if q > bestQ || (q == bestQ && q > 0 && pref(enc) > pref(best)) {
			best, bestQ = enc, q
		}
	}
	return best
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

// parseFloat is a tiny float parser for q-values, accepting the "0", "1",
// "0.8", "1.0" forms browsers actually emit.
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

// Pools reuse expensive encoder allocations across requests (brotli and gzip writers are the costly part; deflate reuses gzip's flate pool indirectly).
// brotliQuality is the dynamic-response level: q11 (the old value) is pathological on caller-influenced content — measured on a 2 MB body, q11 spends 40x the CPU of q5 for an IDENTICAL ratio on high-entropy input, and 10x for a 1.4x ratio on realistic JSON. q5 is what nginx and most CDNs use for dynamic content.
const brotliQuality = 5

// brotliQualityLarge is the level for large bodies: on real multi-megabyte bodies q5 runs ~50 MB/s for a 13x ratio while q1 runs ~280 MB/s for 10.6x — under a 40-user read mix brotli was 68% of all CPU, so large bodies take the fast level; small API JSON keeps q5.
const brotliQualityLarge = 1

// brotliWindowBits is the encoder window (LGWin): the library default of 22 grows a pooled encoder to 10-27 MiB after one large body, while 18 holds it at ~3.4 MiB flat for a marginal ratio loss on JSON.
const brotliWindowBits = 18

// largeBodySize is the body size from which the fast encoder level is used; the size is what is known when the encoder starts (Content-Length, or the bytes handed over so far — render.JSON writes the whole body in one call), so a body streaming in small pieces keeps the default level.
const largeBodySize = 256 << 10

// minCompressSize is the smallest body worth compressing: below it the encoder costs more than the bytes it saves, and framing can make a tiny body larger.
// Most responses carry no Content-Length, so the decision is lazy — a compressible response of unknown size holds back status and body until the body reaches the threshold, the handler returns (send as is), or it flushes (compress); holding back under 1 KB is unobservable (net/http buffers 4 KB anyway).
const minCompressSize = 1024

// Each pool holds *pooledWriter: an encoder permanently pointed at its own
// sink, which forwards to the current response (see pooledWriter).
var (
	brotliPool = sync.Pool{
		New: func() any {
			p := &pooledWriter{}
			p.w = brotli.NewWriterOptions(&p.sink, brotli.WriterOptions{Quality: brotliQuality, LGWin: brotliWindowBits})
			return p
		},
	}
	// brotliFastPool serves bodies of largeBodySize and up (brotliQualityLarge).
	brotliFastPool = sync.Pool{
		New: func() any {
			p := &pooledWriter{}
			p.w = brotli.NewWriterOptions(&p.sink, brotli.WriterOptions{Quality: brotliQualityLarge, LGWin: brotliWindowBits})
			return p
		},
	}
	gzipPool = sync.Pool{
		New: func() any {
			p := &pooledWriter{}
			p.w, _ = gzip.NewWriterLevel(&p.sink, gzip.DefaultCompression)
			return p
		},
	}
	gzipFastPool = sync.Pool{
		New: func() any {
			p := &pooledWriter{}
			p.w, _ = gzip.NewWriterLevel(&p.sink, gzip.BestSpeed)
			return p
		},
	}
	flatePool = sync.Pool{
		New: func() any {
			p := &pooledWriter{}
			p.w, _ = flate.NewWriter(&p.sink, flate.DefaultCompression)
			return p
		},
	}
	flateFastPool = sync.Pool{
		New: func() any {
			p := &pooledWriter{}
			p.w, _ = flate.NewWriter(&p.sink, flate.BestSpeed)
			return p
		},
	}
)

// encoderPoolFor picks the pool for an encoding: the fast level when the body
// is known to be at least largeBodySize at encoder start.
func encoderPoolFor(encoding string, fast bool) encoderPool {
	switch encoding {
	case "br":
		if fast {
			return &brotliFastPool
		}
		return &brotliPool
	case "gzip":
		if fast {
			return &gzipFastPool
		}
		return &gzipPool
	case "deflate":
		if fast {
			return &flateFastPool
		}
		return &flatePool
	}
	return nil
}

// Compress returns middleware that negotiates brotli > gzip > deflate from Accept-Encoding and applies the encoder only to compressible content types; non-compressible responses (SSE, WebSocket upgrades, binary blobs) pass through unchanged. Flusher/Hijacker/Pusher are delegated.
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

// compressWriter wraps the underlying ResponseWriter and lazily installs a compression encoder once it sees a compressible Content-Type at WriteHeader or the first Write; until then it is a transparent pass-through.
type compressWriter struct {
	http.ResponseWriter
	encoding string
	// encoder is concrete, not io.WriteCloser: Flush through an interface
	// assertion would fail silently and leave every flushed byte buffered.
	encoder *pooledWriter
	started bool // compression decided (or deferred, below)
	// deferring: compressible, size unknown and under minCompressSize so far.
	// status (0 = implicit 200) and buf are held back until the size decides.
	deferring bool
	status    int
	buf       []byte
	// fast records that the encoder started at the fast level (largeBodySize).
	fast bool
}

// init decides whether to compress based on Content-Type and, when known, Content-Length; called from WriteHeader, Write and Flush. A compressible body of unknown size defers the decision (see minCompressSize).
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
	if cl := c.Header().Get("Content-Length"); cl != "" {
		hint := int64(0)
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			if n < minCompressSize {
				return // known small: passthrough, Content-Length kept
			}
			hint = n
		}
		c.startEncoder(hint)
		return
	}
	c.deferring = true
}

// startEncoder sets Content-Encoding, drops Content-Length (now invalid), and primes the encoder; a sizeHint of largeBodySize or up gets the fast level.
func (c *compressWriter) startEncoder(sizeHint int64) {
	c.Header().Set("Content-Encoding", c.encoding)
	c.Header().Del("Content-Length")
	c.fast = sizeHint >= largeBodySize
	if pool := encoderPoolFor(c.encoding, c.fast); pool != nil {
		c.encoder = acquireEncoder(pool, c.ResponseWriter)
	}
}

// commitEncoded ends a deferral by compressing: the held-back status and
// bytes go out through the encoder (sizeHint = the bytes known so far).
func (c *compressWriter) commitEncoded(sizeHint int64) error {
	c.deferring = false
	c.startEncoder(sizeHint)
	if c.status != 0 {
		c.ResponseWriter.WriteHeader(c.status)
	}
	if len(c.buf) == 0 {
		return nil
	}
	_, err := c.encoder.Write(c.buf)
	c.buf = nil
	return err
}

// commitPlain ends a deferral without compressing: the body stayed under
// minCompressSize, so the held-back status and bytes go out as they are.
func (c *compressWriter) commitPlain() {
	c.deferring = false
	if c.status != 0 {
		c.ResponseWriter.WriteHeader(c.status)
	}
	if len(c.buf) > 0 {
		_, _ = c.ResponseWriter.Write(c.buf)
		c.buf = nil
	}
}

func (c *compressWriter) WriteHeader(code int) {
	// 1xx informational responses (103 Early Hints) go out at once; they are not the final status the compression decision belongs to. 101 is final: the connection switches protocols.
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		c.ResponseWriter.WriteHeader(code)
		return
	}
	c.init()
	if c.deferring {
		if c.status == 0 {
			c.status = code
		}
		return
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if !c.started {
		// No WriteHeader call: implicit 200. Decide compression now.
		c.init()
	}
	if c.deferring {
		if len(c.buf)+len(p) < minCompressSize {
			c.buf = append(c.buf, p...)
			return len(p), nil
		}
		if err := c.commitEncoded(int64(len(c.buf) + len(p))); err != nil {
			return 0, err
		}
	}
	if c.encoder == nil {
		return c.ResponseWriter.Write(p)
	}
	return c.encoder.Write(p)
}

// close sends a still-deferred small response as is, or flushes and returns the encoder to its pool. Safe to call when passthrough (encoder nil) — a no-op.
func (c *compressWriter) close() {
	if c.deferring {
		c.commitPlain()
		return
	}
	if c.encoder == nil {
		return
	}
	_ = c.encoder.Close()
}

// Flush delegates to the underlying writer; when compressing, it also flushes the encoder.
// Flushing commits the headers, so a Flush before the first Write decides compression then; a flush also ends a deferral by compressing — a handler that flushes is streaming, and later chunks may follow.
func (c *compressWriter) Flush() {
	c.init()
	if c.deferring {
		_ = c.commitEncoded(int64(len(c.buf)))
	}
	if c.encoder != nil {
		_ = c.encoder.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates so WebSocket upgrades work unchanged: upgrades send a 101
// with no Content-Type, so init() leaves the encoder nil before any Hijack.
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

// pooledWriter is a pooled encoder pointed for life at its own sink, which forwards to the response the encoder is currently serving.
// The sink keeps a pooled encoder from retaining the last response: gzip and flate keep their destination after Close, so a writer pooled while pointed at the ResponseWriter holds it (and the request, its context, the connection's buffers) until a later Get or GC; with the sink in between, Close only clears the sink's target, and the one Reset per response happens at acquire.
type pooledWriter struct {
	w    resettableEncoder
	sink encoderSink
	pool encoderPool // where Close returns the writer
}

// encoderPool is the part of sync.Pool the encoder pools are used through; an
// interface so a test can use a deterministic one.
type encoderPool interface {
	Get() any
	Put(any)
}

// encoderSink forwards an encoder's output to the response it is serving; nil
// between responses.
type encoderSink struct{ w io.Writer }

func (s *encoderSink) Write(p []byte) (int, error) { return s.w.Write(p) }

// acquireEncoder takes a writer from pool and points it at dst.
func acquireEncoder(pool encoderPool, dst io.Writer) *pooledWriter {
	p := pool.Get().(*pooledWriter)
	p.pool = pool
	p.sink.w = dst
	p.w.Reset(&p.sink)
	return p
}

// resettableEncoder is what the brotli, gzip and flate writers have in
// common: re-pointable at a new destination and flushable mid-stream.
type resettableEncoder interface {
	io.WriteCloser
	Reset(dst io.Writer)
	Flush() error
}

func (p *pooledWriter) Write(b []byte) (int, error) { return p.w.Write(b) }

// Flush writes out everything written so far in a form the client can decode
// now (a sync flush), without ending the stream.
func (p *pooledWriter) Flush() error { return p.w.Flush() }

// Close finishes the stream and pools the writer, detached from the response
// first (see pooledWriter).
func (p *pooledWriter) Close() error {
	err := p.w.Close()
	p.sink.w = nil
	p.pool.Put(p)
	return err
}

var errNotHijacker = hijackError("response writer does not implement http.Hijacker")

type hijackError string

func (e hijackError) Error() string { return string(e) }
