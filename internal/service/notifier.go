package service

import "sync"

// Notifier defines an interface for sending events to the frontend.
type Notifier interface {
	Emit(name string, data any)
	// EmitTo sends an event only to the SSE client(s) associated with userID.
	// In local mode (single client) this behaves identically to Emit.
	EmitTo(userID, name string, data any)
}

// NilNotifier is a no-op implementation of Notifier.
type NilNotifier struct{}

func (n NilNotifier) Emit(name string, data any)           {}
func (n NilNotifier) EmitTo(userID, name string, data any) {}

// GlobalNotifier is a concrete implementation of Notifier that allows
// multiple subscribers to listen for emitted events.
type GlobalNotifier struct {
	mu          sync.RWMutex
	subscribers []func(name string, data any)
}

func NewGlobalNotifier() *GlobalNotifier {
	return &GlobalNotifier{}
}

func (n *GlobalNotifier) Emit(name string, data any) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, s := range n.subscribers {
		s(name, data)
	}
}

// EmitTo delegates to Emit — GlobalNotifier is an in-process pub/sub for
// tests; it has no concept of per-user filtering.
func (n *GlobalNotifier) EmitTo(userID, name string, data any) {
	n.Emit(name, data)
}

func (n *GlobalNotifier) Subscribe(s func(name string, data any)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.subscribers = append(n.subscribers, s)
}
