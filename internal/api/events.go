package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	"pad-core/logger"
	"pad-core/models"
)

type Event struct {
	Name       string `json:"name"`
	Data       any    `json:"data"`
	TargetUser string `json:"-"` // empty = broadcast to all; non-empty = only this user
}

// sseHeartbeatInterval is how often HandleEvents writes an SSE comment frame
// on an otherwise-quiet connection. Without it, a half-dead socket (laptop
// suspend/resume, sidecar restart) is indistinguishable from a healthy idle
// one on both ends: the client's read blocks forever and the server never
// touches the socket. The client's read-inactivity watchdog is ~2× this
// interval, so keep the two in sync.
const sseHeartbeatInterval = 20 * time.Second

// sseWriteTimeout bounds each individual write (frame or heartbeat ping) on
// the SSE stream. The server deliberately sets no global WriteTimeout — that
// would kill healthy idle connections — so without a per-write deadline a
// client that stops reading while holding the connection open (suspended
// laptop, zero TCP window) blocks w.Write forever: the write never errors, so
// not even the heartbeat notices, the handler goroutine is pinned outside its
// select (revoke/expiry/shutdown cannot run), and its 512-slot buffer stays
// full. With the deadline, a stuck write fails within this timeout and the
// normal write-error cleanup path tears the connection down.
const sseWriteTimeout = 30 * time.Second

// maxFrameBytes bounds one encoded SSE frame. Each client's channel buffers
// up to 512 frames, so an unbounded frame size lets one large event (a
// document-carrying event, a chat chunk quoting a pasted file) pin frame-size
// × 512 bytes per slow connection. 256 KiB leaves ordinary events (chat
// chunks, progress ticks, summaries) untouched and only caps pathological
// ones; oversized document-carrying events are swapped for their lightweight
// form rather than dropped outright.
const maxFrameBytes = 256 << 10

// EventManager manages Server-Sent Events (SSE) clients and implements
// service.EventNotifier to allow internal services to emit events to the frontend.
type EventManager struct {
	clients      map[chan []byte]string // pre-encoded SSE frame channel → userID ("" in local mode)
	clientsMu    sync.Mutex
	sseConnCount map[string]int // per-client SSE connection counter (keyed by clientKey)
	shutdownCh   <-chan struct{}
	// allowOrigin/clientKey/isRevoked are injected by the Router. Concurrency
	// invariant: the Set* writers are called ONLY from NewRouter, which
	// completes before http.Serve (so there's a happens-before edge over the
	// reads from handler goroutines). They are plain fields rather than
	// atomic.Pointer because no post-startup setter exists; if one is ever
	// added (e.g. hot-reloading the CORS allowlist), these MUST become
	// atomic.Pointer[func] to avoid a data race flagged by -race.
	allowOrigin func(string) bool          // injected by Router; nil = deny all cross-origin
	clientKey   func(*http.Request) string // injected by Router; nil = fall back to remote IP
	isRevoked   func(string) bool          // injected by Router; nil = skip blacklist re-check

	heartbeatInterval time.Duration // test override; 0 ⇒ sseHeartbeatInterval
}

// heartbeatTick returns the SSE heartbeat interval, honouring the test
// override so heartbeat tests don't sleep for real 20s ticks.
func (m *EventManager) heartbeatTick() time.Duration {
	if m.heartbeatInterval > 0 {
		return m.heartbeatInterval
	}
	return sseHeartbeatInterval
}

func NewEventManager(shutdownCh chan struct{}) *EventManager {
	return &EventManager{
		clients:      make(map[chan []byte]string),
		sseConnCount: make(map[string]int),
		shutdownCh:   shutdownCh,
	}
}

// SetOriginChecker injects the router's CORS logic so the SSE endpoint
// respects the same origin allowlist as every other route.
func (m *EventManager) SetOriginChecker(fn func(string) bool) {
	m.allowOrigin = fn
}

// SetClientKeyFunc injects the router's connection-limit key function. In JWT
// mode the Router keys per authenticated user; otherwise it keys per
// proxy-aware client IP. This prevents all clients behind a reverse proxy from
// collapsing onto a single IP bucket (which would let one user exhaust the
// shared connection cap for everyone).
func (m *EventManager) SetClientKeyFunc(fn func(*http.Request) string) {
	m.clientKey = fn
}

// SetRevocationChecker injects the auth manager's blacklist check so the SSE
// event loop can periodically verify the access token hasn't been blacklisted
// (logout / explicit revoke) after the initial middleware upgrade. Note:
// password change and refresh-replay revoke only refresh tokens, not access
// tokens; those sessions end when the access token expires, which the SSE loop
// enforces directly via the token's expiry (independent of this check).
func (m *EventManager) SetRevocationChecker(fn func(string) bool) {
	m.isRevoked = fn
}

// Emit satisfies the service.EventNotifier interface. It broadcasts to ALL
// connected SSE clients — use only for events that are truly global or in
// local mode where only one client exists.
func (m *EventManager) Emit(name string, data any) {
	m.deliver(Event{Name: name, Data: data})
}

// EmitTo satisfies the service.EventNotifier interface. It delivers the event
// only to SSE clients associated with userID. In local mode (userID="")
// this behaves identically to Emit. Use EmitTo for any user-scoped event
// (chat, analysis, settings, flow load) to prevent cross-tenant data leaks.
func (m *EventManager) EmitTo(userID, name string, data any) {
	m.deliver(Event{Name: name, Data: data, TargetUser: userID})
}

// HasSubscriber satisfies the service.EventNotifier interface. It reports whether
// EmitTo(userID, …) would currently reach at least one connected SSE client;
// a "" userID matches any client, mirroring deliver's local-mode broadcast
// semantics. Services use it to detect abandoned work — e.g. a chat stream
// whose tab closed without sending /cancel.
func (m *EventManager) HasSubscriber(userID string) bool {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	for _, clientUser := range m.clients {
		if userID == "" || clientUser == userID {
			return true
		}
	}
	return false
}

// Event delivery priority. The SSE channel is shared by every event class,
// and chat streams sit behind the same 512-slot buffer as analysis progress —
// which arrives in bursts of hundreds during a large analysis. A burst used to
// push the buffer to capacity and silently drop whatever came next, including
// chat chunks; the client noticed via the done-event's chunk count and
// recovered with a full-buffer resume, i.e. the whole answer re-rendered and
// reflowed at the end of the stream.
//
// Shedding is by class, at enqueue time: once the buffer crosses the
// high-water mark, droppable events are discarded to keep headroom for the
// ones that cannot be reconstructed from a later message.
//
// sheddableNum/sheddableDen express the high-water mark as a fraction of the
// buffer: droppable events are shed once it is more than two-thirds full,
// leaving the last third as headroom for chat.
const (
	sheddableNum = 2
	sheddableDen = 3
)

// sheddable reports whether an event may be dropped early to protect the
// buffer. Progress ticks are pure UI polish and each is superseded by the
// next; a completion or a chat frame is not recoverable this way.
func sheddable(name string) bool {
	switch name {
	case "analysis:progress":
		return true
	default:
		return false
	}
}

func (m *EventManager) deliver(ev Event) {
	// Encode the SSE frame ONCE for all recipients: encoding per connected
	// client would multiply encode cost (and its allocations) by the number of
	// live SSE tabs for every chat chunk / progress event.
	frame, ok := encodeEventFrame(ev)
	if !ok {
		return
	}

	droppable := sheddable(ev.Name)

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	for ch, clientUser := range m.clients {
		// Filter by target user when one is specified.
		if ev.TargetUser != "" && clientUser != ev.TargetUser {
			continue
		}
		// Shed low-priority events before the buffer is exhausted so a
		// progress burst cannot starve a chat chunk queued right behind it.
		if droppable && len(ch)*sheddableDen > cap(ch)*sheddableNum {
			continue
		}
		select {
		case ch <- frame:
		default:
			logger.Warn("SSE client dropped event: send buffer full", "event", ev.Name)
		}
	}
}

// marshalFrame encodes ev into its "data: …\n\n" SSE wire frame, or nil when
// the JSON marshal fails (already logged).
func marshalFrame(ev Event) []byte {
	data, err := json.Marshal(ev)
	if err != nil {
		logger.Warn("SSE: encode event failed; dropping", "event", ev.Name, "err", err)
		return nil
	}
	frame := append(append(make([]byte, 0, len(data)+16), "data: "...), data...)
	return append(frame, '\n', '\n')
}

// encodeEventFrame marshals ev and enforces the maxFrameBytes bound. An
// oversized frame is not enqueued: document-carrying events are replaced by
// their lightweight form (which consumers can refetch over HTTP), anything
// else is dropped — a missing notification is recoverable, an unbounded
// frame pinned per stalled connection is not.
func encodeEventFrame(ev Event) ([]byte, bool) {
	frame := marshalFrame(ev)
	if frame == nil {
		return nil, false
	}
	if len(frame) <= maxFrameBytes {
		return frame, true
	}
	origLen := len(frame)
	if light, ok := lightweightEvent(ev); ok {
		if lf := marshalFrame(light); lf != nil && len(lf) <= maxFrameBytes {
			logger.Warn("SSE: event exceeded frame bound; delivered lightweight form instead",
				"event", ev.Name, "bytes", origLen, "deliveredBytes", len(lf))
			return lf, true
		}
		logger.Warn("SSE: event exceeded frame bound and its lightweight form was unusable; dropped",
			"event", ev.Name, "bytes", origLen)
		return nil, false
	}
	logger.Warn("SSE: event exceeded frame bound; dropped", "event", ev.Name, "bytes", origLen)
	return nil, false
}

// lightweightEvent returns a bounded-size replacement for an oversized
// document-carrying event. Emission sites normally send the summary directly;
// this is the backstop for a site that regresses to emitting the full
// document. TargetUser is preserved so user scoping survives the swap.
func lightweightEvent(ev Event) (Event, bool) {
	if ev.Name != "flow:loaded" {
		return Event{}, false
	}
	var doc *models.FlowDocument
	switch d := ev.Data.(type) {
	case *models.FlowDocument:
		doc = d
	case models.FlowDocument:
		doc = &d
	default:
		return Event{}, false
	}
	return Event{Name: ev.Name, Data: service.FlowLoadedSummaryOf(doc), TargetUser: ev.TargetUser}, true
}

// HandleEvents is the HTTP handler for the SSE endpoint.
// @Summary      Server-sent events stream
// @Description  Long-lived SSE channel for analysis, chat, and collaboration events. Chunks stream incrementally; reconnect with Last-Event-ID to resume.
// @Tags         realtime
// @Produce      text/event-stream
// @Success      200 {string} string "Event stream"
// @Router       /api/events [get]
func (m *EventManager) HandleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		render.Error(w, fmt.Errorf("streaming unsupported"), http.StatusInternalServerError)
		return
	}

	// Per-write deadline machinery (see sseWriteTimeout). The controller is
	// created once; if the underlying writer cannot honor deadlines (a wrapped
	// ResponseWriter without Unwrap), that is logged exactly once and writes
	// proceed undelayed rather than tearing down every connection.
	rc := http.NewResponseController(w)
	deadlineOK := true
	armWriteDeadline := func() {
		if !deadlineOK {
			return
		}
		if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
			logger.Warn("SSE: per-write deadlines unsupported by this connection; stalled clients cannot be reaped by write timeout", "err", err)
			deadlineOK = false
		}
	}

	// Key the connection cap per authenticated user (JWT mode) or per
	// proxy-aware client IP, via the Router-injected clientKey. Falling back to
	// the bare remote IP only when no key func is wired keeps tests and any
	// uninitialised path working.
	key := middleware.ClientIP(r, nil)
	if m.clientKey != nil {
		key = m.clientKey(r)
	}

	// Extract the authenticated user ID for per-user event filtering.
	// sseClientKey returns "user:<id>" (prefixed for connection-limit bucketing);
	// strip the prefix so the stored value matches the bare userID that services
	// pass to EmitTo via auth.ClaimsFromContext. In local mode (no JWT), the key
	// is the bare IP and userID stays "" — all events broadcast to everyone.
	userID := ""
	if m.clientKey != nil {
		rawKey := m.clientKey(r)
		if strings.HasPrefix(rawKey, "user:") {
			userID = rawKey[5:]
		}
	}

	m.clientsMu.Lock()
	if m.sseConnCount[key] >= 10 {
		m.clientsMu.Unlock()
		render.Error(w, fmt.Errorf("too many connections"), http.StatusServiceUnavailable)
		return
	}
	m.sseConnCount[key]++
	// Big enough to absorb bursts (e.g. analysis progress storms) without
	// letting a slow client pin megabytes of buffered events; Emit drops
	// events for a client whose buffer is full.
	ch := make(chan []byte, 512)
	m.clients[ch] = userID
	m.clientsMu.Unlock()

	defer func() {
		m.clientsMu.Lock()
		delete(m.clients, ch)
		m.sseConnCount[key]--
		if m.sseConnCount[key] <= 0 {
			delete(m.sseConnCount, key)
		}
		m.clientsMu.Unlock()
		metrics.SSEClientEnd()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// No-op for the Tauri localhost sidecar, but nginx-style reverse proxies
	// buffer responses by default, which would deliver the whole stream at
	// once instead of incrementally in web deployments.
	w.Header().Set("X-Accel-Buffering", "no")
	// Respect the configured CORS allowlist instead of wildcarding.
	// Vary: Origin is set unconditionally so a caching proxy can't serve a
	// no-ACAO SSE variant to a later allowed-origin request.
	w.Header().Set("Vary", "Origin")
	if origin := r.Header.Get("Origin"); origin != "" && m.allowOrigin != nil && m.allowOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	// Write the status explicitly before flushing. Without this, the first
	// fmt.Fprintf in the event loop triggers an implicit WriteHeader via the
	// otelhttp RespWriterWrapper, and the subsequent Flush triggers it again
	// ("superfluous response.WriteHeader call" warning).
	armWriteDeadline()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	metrics.SSEClientStart()

	// Extract the token JTI and expiry for periodic re-validation. In local
	// mode (no JWT) there is no token, so both checks are skipped.
	jti := ""
	var tokenExp time.Time
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		jti = claims.ID
		if claims.ExpiresAt != nil {
			tokenExp = claims.ExpiresAt.Time
		}
	}

	ctx := r.Context()

	// Heartbeat: keep an otherwise-quiet connection observably alive so the
	// client's read-inactivity watchdog can tell "no events" from "dead
	// socket", and so a write failure here ends the handler instead of the
	// connection lingering until the next real event.
	heartbeat := time.NewTicker(m.heartbeatTick())
	defer heartbeat.Stop()

	// Drop the stream once the access token is blacklisted (logout / explicit
	// revoke). Checked on a slow ticker because it hits the shared blacklist store.
	var revokeC <-chan time.Time
	if m.isRevoked != nil && jti != "" {
		revokeTicker := time.NewTicker(2 * time.Minute)
		defer revokeTicker.Stop()
		revokeC = revokeTicker.C
	}

	// Drop the stream when the access token reaches its expiry, capping a live
	// connection at the access-token TTL rather than letting it outlive the
	// token indefinitely. The frontend reconnects with a freshly-refreshed
	// token (ensureFreshToken), so this won't loop. No DB cost, so fire
	// precisely at expiry instead of polling.
	var expiryC <-chan time.Time
	if !tokenExp.IsZero() {
		expiryTimer := time.NewTimer(time.Until(tokenExp))
		defer expiryTimer.Stop()
		expiryC = expiryTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.shutdownCh:
			return
		case <-revokeC:
			if m.isRevoked(jti) {
				logger.Info("SSE: disconnecting client after token revoked", "jti", jti)
				return
			}
		case <-expiryC:
			logger.Info("SSE: disconnecting client after token expired", "jti", jti)
			return
		case <-heartbeat.C:
			// SSE comment frame: the client parser ignores non-"data: " lines,
			// but any bytes reset its inactivity watchdog.
			armWriteDeadline()
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case frame := <-ch:
			// Frame arrives pre-encoded by deliver (one encode per event, not
			// one per client).
			armWriteDeadline()
			if _, err := w.Write(frame); err != nil {
				// Dead or stalled socket (a deadline-bound write to a client
				// that stopped reading fails here within sseWriteTimeout):
				// return so the goroutine, its channel and its queued frames
				// are released instead of lingering.
				return
			}
			flusher.Flush()
		}
	}
}
