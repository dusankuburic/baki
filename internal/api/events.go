package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/metrics"
)

type Event struct {
	Name string `json:"name"`
	Data any    `json:"data"`
}

// EventManager manages Server-Sent Events (SSE) clients and implements
// service.Notifier to allow internal services to emit events to the frontend.
type EventManager struct {
	clients     map[chan Event]bool
	clientsMu   sync.Mutex
	sseIPCount  map[string]int // per-IP SSE connection counter
	shutdownCh  <-chan struct{}
	allowOrigin func(string) bool // injected by Router; nil = deny all cross-origin
}

func NewEventManager(shutdownCh chan struct{}) *EventManager {
	return &EventManager{
		clients:    make(map[chan Event]bool),
		sseIPCount: make(map[string]int),
		shutdownCh: shutdownCh,
	}
}

// SetOriginChecker injects the router's CORS logic so the SSE endpoint
// respects the same origin allowlist as every other route.
func (m *EventManager) SetOriginChecker(fn func(string) bool) {
	m.allowOrigin = fn
}

// Emit satisfies the service.Notifier interface.
func (m *EventManager) Emit(name string, data any) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	ev := Event{Name: name, Data: data}
	for client := range m.clients {
		select {
		case client <- ev:
		default:
			logger.Warn("SSE client dropped event: send buffer full", "event", name)
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

	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	m.clientsMu.Lock()
	if m.sseIPCount[ip] >= 10 {
		m.clientsMu.Unlock()
		http.Error(w, "Too many connections from your IP", http.StatusServiceUnavailable)
		return
	}
	m.sseIPCount[ip]++
	ch := make(chan Event, 64)
	m.clients[ch] = true
	m.clientsMu.Unlock()

	defer func() {
		m.clientsMu.Lock()
		delete(m.clients, ch)
		m.sseIPCount[ip]--
		if m.sseIPCount[ip] <= 0 {
			delete(m.sseIPCount, ip)
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
