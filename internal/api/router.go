package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"pad-analyzer/internal/manager"
	"sync"
)

type Router struct {
	app       *manager.App
	token     string
	clients   map[chan Event]bool
	clientsMu sync.Mutex
}

type Event struct {
	Name string `json:"name"`
	Data any    `json:"data"`
}

func NewRouter(app *manager.App, token string) *Router {
	return &Router{
		app:     app,
		token:   token,
		clients: make(map[chan Event]bool),
	}
}

func (rt *Router) Emit(name string, data any) {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	ev := Event{Name: name, Data: data}
	for client := range rt.clients {
		select {
		case client <- ev:
		default:
		}
	}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Auth middleware
	token := r.Header.Get("Authorization")
	if token == "" {
		token = "Bearer " + r.URL.Query().Get("token")
	}

	if token != "Bearer "+rt.token {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// SSE endpoint
	if r.URL.Path == "/api/events" {
		rt.handleEvents(w, r)
		return
	}

	// Dispatch other routes
	rt.dispatch(w, r)
}

func (rt *Router) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan Event, 10)
	rt.clientsMu.Lock()
	rt.clients[ch] = true
	rt.clientsMu.Unlock()

	defer func() {
		rt.clientsMu.Lock()
		delete(rt.clients, ch)
		rt.clientsMu.Unlock()
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
