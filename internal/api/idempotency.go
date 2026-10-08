package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Idempotency-Key for create routes (ADR 0007). A route declared route.Idempotent() honours the header: the first
// request with a key runs, and its response is stored with the key in the request's own transaction, so the two
// commit together or vanish together; a retry with the key gets that response back, marked Idempotent-Replayed,
// without the route running again. Without the request transaction (the desktop and local mode) the header is
// ignored.

const (
	idempotencyKeyHeader     = "Idempotency-Key"
	idempotentReplayedHeader = "Idempotent-Replayed"
	maxIdempotencyKeyLen     = 255
	// maxStoredResponseBytes caps the body kept with a key. A larger response is sent as usual, and its record keeps
	// the status alone: a replay answers that status with an empty body.
	maxStoredResponseBytes = 256 << 10
)

// Machine codes of the Idempotency-Key refusals.
const (
	codeIdempotencyKeyInvalid = "IDEMPOTENCY_KEY_INVALID"
	codeIdempotencyKeyReused  = "IDEMPOTENCY_KEY_REUSED"
	codeIdempotencyInProgress = "IDEMPOTENCY_IN_PROGRESS"
)

var errIdempotencyRecordFailed = errors.New("the response could not be stored with its Idempotency-Key")

// idempotency honours Idempotency-Key on the routes that declare it. registerRoutes puts it right after
// rlsMiddleware, inside the request transaction: the claim, the route's writes and the stored response are one
// transaction. It decides on the resolved policy, as every layer does (ADR 0006); the endpoint guard after it refuses
// a request chi dispatched elsewhere, and that error response rolls the claim back. A concurrent request with a key
// in use is answered 409 at once rather than queued behind the first: waiting would hold a pooled connection, and
// the request's deadline, for as long as the first one runs.
func (rt *Router) idempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values(idempotencyKeyHeader)
		p, _ := middleware.RequestPolicy(r)
		if len(keys) == 0 || !p.Idempotent || !rt.caps.Has(capability.RowLevelSecurity) {
			next.ServeHTTP(w, r)
			return
		}
		store := rt.security.Idempotency
		claims := auth.ClaimsFromContext(r.Context())
		if store == nil || claims == nil {
			next.ServeHTTP(w, r)
			return
		}
		if len(keys) > 1 || !validIdempotencyKey(keys[0]) {
			render.ErrorWithCode(w, fmt.Errorf("%s must be one value of 1 to %d printable ASCII characters", idempotencyKeyHeader, maxIdempotencyKeyLen),
				http.StatusBadRequest, codeIdempotencyKeyInvalid)
			return
		}
		key := keys[0]
		body, err := io.ReadAll(r.Body)
		if err != nil {
			status := http.StatusBadRequest
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				status = http.StatusRequestEntityTooLarge
			}
			render.Error(w, fmt.Errorf("read the request body: %w", err), status)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		ctx := r.Context()
		prior, err := store.ClaimIdempotencyKey(ctx, claims.UserID, key, requestFingerprint(r, p, body))
		switch {
		case errors.Is(err, storageif.ErrIdempotencyUnavailable):
			// Outside the request transaction the key could not commit with the request's writes.
			next.ServeHTTP(w, r)
		case errors.Is(err, storageif.ErrIdempotencyKeyInProgress):
			w.Header().Set("Retry-After", "1")
			render.ErrorWithCode(w, errors.New("a request with this Idempotency-Key is still in progress; retry it shortly"),
				http.StatusConflict, codeIdempotencyInProgress)
		case errors.Is(err, storageif.ErrIdempotencyKeyReused):
			render.ErrorWithCode(w, errors.New("this Idempotency-Key was already used for a different request"),
				http.StatusUnprocessableEntity, codeIdempotencyKeyReused)
		case err != nil:
			render.Error(w, err, 0)
		case prior != nil:
			replayIdempotent(w, prior)
		default:
			iw := newIdempotentWriter(w, func(resp storageif.IdempotentResponse) error {
				// The route's work is done: its outcome is stored even if the deadline passes meanwhile.
				return store.CompleteIdempotencyKey(context.WithoutCancel(ctx), claims.UserID, key, resp)
			})
			next.ServeHTTP(iw, r)
			iw.finish()
		}
	})
}

// idempotentRouteError refuses an Idempotent declaration the mechanism cannot honour (finishRoutes): the record is
// kept per caller, in the request transaction, and a read needs no key.
func idempotentRouteError(p *route.Policy) error {
	switch {
	case !p.Idempotent:
		return nil
	case p.Access != route.AuthRequired:
		return fmt.Errorf("route policy: %s %s is Idempotent but not authenticated: keys are kept per user", p.Method, p.Pattern)
	case p.Tx != route.TxRequest:
		return fmt.Errorf("route policy: %s %s is Idempotent but runs without the request transaction: its record must commit with the route's writes", p.Method, p.Pattern)
	case p.Method == http.MethodGet || p.Method == http.MethodHead || p.Method == http.MethodOptions:
		return fmt.Errorf("route policy: %s %s is Idempotent, but a read needs no Idempotency-Key", p.Method, p.Pattern)
	}
	return nil
}

// validIdempotencyKey: 1 to 255 printable ASCII characters (space to tilde).
func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > maxIdempotencyKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < ' ' || c > '~' {
			return false
		}
	}
	return true
}

// requestFingerprint identifies what a key was first sent with: the method and route pattern, and a hash of the path,
// query and body. The path counts as well as the pattern: the same key sent for another flow's or organisation's
// record is another request, not a retry.
func requestFingerprint(r *http.Request, p *route.Policy, body []byte) string {
	h := sha256.New()
	h.Write([]byte(route.DispatchPath(r.URL) + "\x00" + r.URL.RawQuery + "\x00"))
	h.Write(body)
	return r.Method + " " + p.Pattern + " " + hex.EncodeToString(h.Sum(nil))
}

// replayIdempotent answers with a stored response: its status, content type and body; other headers the route set
// the first time are not kept.
func replayIdempotent(w http.ResponseWriter, resp *storageif.IdempotentResponse) {
	h := w.Header()
	if resp.ContentType != "" {
		h.Set("Content-Type", resp.ContentType)
	}
	h.Set(idempotentReplayedHeader, "true")
	w.WriteHeader(resp.Status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

// idempotentWriter holds a claimed request's response until its record is stored: the first byte that reaches the
// request's writer commits the transaction (ADR 0004), and the record must commit with it. An error response (400
// and above) goes straight through, since the transaction rolls back and the claim with it. A body that outgrows
// maxStoredResponseBytes has its status stored alone and then streams. It offers no Unwrap: a route that hijacks
// the connection or writes around it must not be declared Idempotent.
type idempotentWriter struct {
	w        http.ResponseWriter
	header   http.Header
	store    func(storageif.IdempotentResponse) error
	status   int
	buf      bytes.Buffer
	released bool // the response went to w; later writes follow it
	failed   bool // storing the record failed and an error went out instead; later writes are swallowed
}

func newIdempotentWriter(w http.ResponseWriter, store func(storageif.IdempotentResponse) error) *idempotentWriter {
	return &idempotentWriter{w: w, header: w.Header().Clone(), store: store}
}

func (iw *idempotentWriter) Header() http.Header { return iw.header }

func (iw *idempotentWriter) WriteHeader(code int) {
	if iw.status != 0 || code < http.StatusOK {
		return // a second status is dropped, as net/http would; a 1xx would go out ahead of the record
	}
	iw.status = code
	if code >= http.StatusBadRequest {
		iw.release()
	}
}

func (iw *idempotentWriter) Write(b []byte) (int, error) {
	if iw.status == 0 {
		iw.WriteHeader(http.StatusOK)
	}
	switch {
	case iw.failed:
		return 0, errIdempotencyRecordFailed
	case iw.released:
		return iw.w.Write(b)
	case iw.buf.Len()+len(b) <= maxStoredResponseBytes:
		return iw.buf.Write(b)
	}
	if !iw.complete(storageif.IdempotentResponse{Status: iw.status, BodyOmitted: true}) {
		return 0, errIdempotencyRecordFailed
	}
	iw.release()
	return iw.w.Write(b)
}

// Flush forwards only once the response is released; until then there is nothing the client may see.
func (iw *idempotentWriter) Flush() {
	if !iw.released {
		return
	}
	if f, ok := iw.w.(http.Flusher); ok {
		f.Flush()
	}
}

// finish stores and releases a response the route completed within the cap; it runs when the route returns.
func (iw *idempotentWriter) finish() {
	if iw.status == 0 {
		iw.status = http.StatusOK
	}
	if iw.released || iw.failed {
		return
	}
	if iw.complete(storageif.IdempotentResponse{Status: iw.status, ContentType: iw.header.Get("Content-Type"), Body: iw.buf.Bytes()}) {
		iw.release()
	}
}

// complete stores the record. When that fails the held response is dropped for an error, which rolls the request's
// writes back with the claim: a create the client is told about always has its record.
func (iw *idempotentWriter) complete(resp storageif.IdempotentResponse) bool {
	err := iw.store(resp)
	if err == nil {
		return true
	}
	iw.failed = true
	// An explicit code: render.Error would turn a wrapped ErrNotFound (no claim held) into a 404.
	code, machine := http.StatusInternalServerError, "INTERNAL_ERROR"
	if errors.Is(err, storageif.ErrRLSUnavailable) {
		code, machine = http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"
	}
	render.ErrorWithCode(iw.w, fmt.Errorf("%w: %w", errIdempotencyRecordFailed, err), code, machine)
	return false
}

// release sends the held headers, status and body to the request's writer.
func (iw *idempotentWriter) release() {
	iw.released = true
	h := iw.w.Header()
	for k := range h {
		if _, ok := iw.header[k]; !ok {
			delete(h, k)
		}
	}
	for k, v := range iw.header {
		h[k] = v
	}
	iw.w.WriteHeader(iw.status)
	if iw.buf.Len() > 0 {
		_, _ = iw.w.Write(iw.buf.Bytes())
		iw.buf.Reset()
	}
}
