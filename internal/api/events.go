package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/metrics"
)

type Event struct {
	Name       string `json:"name"`
	Data       any    `json:"data"`
	TargetUser string `json:"-"` // empty = broadcast to all; non-empty = only this user
}

// EventManager manages Server-Sent Events (SSE) clients and implements
// service.Notifier to allow internal services to emit events to the frontend.
type EventManager struct {
	clients      map[chan Event]string // channel → userID ("" in local mode)
	clientsMu    sync.Mutex
	sseConnCount map[string]int // per-client SSE connection counter (keyed by clientKey)
	shutdownCh   <-chan struct{}
	allowOrigin  func(string) bool          // injected by Router; nil = deny all cross-origin
	clientKey    func(*http.Request) string // injected by Router; nil = fall back to remote IP
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
	// In local mode (no JWT), userID is "" — all events are broadcast.
	userID := ""
	if m.clientKey != nil {
		userID = m.clientKey(r)
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

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.shutdownCh:
			// Server is shutting down — return so http.Server.Shutdown can drain.
			return
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
