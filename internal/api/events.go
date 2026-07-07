package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-core/logger"
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

// EventManager manages Server-Sent Events (SSE) clients and implements
// service.Notifier to allow internal services to emit events to the frontend.
type EventManager struct {
	clients      map[chan Event]string // channel → userID ("" in local mode)
	clientsMu    sync.Mutex
	sseConnCount map[string]int // per-client SSE connection counter (keyed by clientKey)
	shutdownCh   <-chan struct{}
	allowOrigin  func(string) bool          // injected by Router; nil = deny all cross-origin
	clientKey    func(*http.Request) string // injected by Router; nil = fall back to remote IP
	isRevoked    func(string) bool          // injected by Router; nil = skip blacklist re-check

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
		clients:      make(map[chan Event]string),
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

// Emit satisfies the service.Notifier interface. It broadcasts to ALL
// connected SSE clients — use only for events that are truly global or in
// local mode where only one client exists.
func (m *EventManager) Emit(name string, data any) {
	m.deliver(Event{Name: name, Data: data})
}

// EmitTo satisfies the service.Notifier interface. It delivers the event
// only to SSE clients associated with userID. In local mode (userID="")
// this behaves identically to Emit. Use EmitTo for any user-scoped event
// (chat, analysis, settings, flow load) to prevent cross-tenant data leaks.
func (m *EventManager) EmitTo(userID, name string, data any) {
	m.deliver(Event{Name: name, Data: data, TargetUser: userID})
}

// HasSubscriber satisfies the service.Notifier interface. It reports whether
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

func (m *EventManager) deliver(ev Event) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	for ch, clientUser := range m.clients {
		// Filter by target user when one is specified.
		if ev.TargetUser != "" && clientUser != ev.TargetUser {
			continue
		}
		select {
		case ch <- ev:
		default:
			logger.Warn("SSE client dropped event: send buffer full", "event", ev.Name)
		}
	}
}

// HandleEvents is the HTTP handler for the SSE endpoint.
func (m *EventManager) HandleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
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
		http.Error(w, "Too many connections", http.StatusServiceUnavailable)
		return
	}
	m.sseConnCount[key]++
	// Big enough to absorb bursts (e.g. analysis progress storms) without
	// letting a slow client pin megabytes of buffered events; Emit drops
	// events for a client whose buffer is full.
	ch := make(chan Event, 512)
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
	if origin := r.Header.Get("Origin"); origin != "" && m.allowOrigin != nil && m.allowOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	// Write the status explicitly before flushing. Without this, the first
	// fmt.Fprintf in the event loop triggers an implicit WriteHeader via the
	// otelhttp RespWriterWrapper, and the subsequent Flush triggers it again
	// ("superfluous response.WriteHeader call" warning).
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
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
