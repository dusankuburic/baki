package service

import "sync"

// Notifier defines an interface for sending events to the frontend.
type Notifier interface {
	Emit(name string, data any)
}

// NilNotifier is a no-op implementation of Notifier.
type NilNotifier struct{}

func (n NilNotifier) Emit(name string, data any) {}

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

func (n *GlobalNotifier) Subscribe(s func(name string, data any)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.subscribers = append(n.subscribers, s)
}
