package testutil

import "sync"

// CountingNotifier implements service.Notifier for tests. It counts all
// Emit calls and captures the last emitted event for assertions.
type CountingNotifier struct {
	mu           sync.Mutex
	count        int
	events       []emittedEvent
	noSubscriber bool
}

type emittedEvent struct {
	Name string
	Data any
}

func (n *CountingNotifier) Emit(name string, data any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.count++
	n.events = append(n.events, emittedEvent{Name: name, Data: data})
}

// EmitTo delegates to Emit — CountingNotifier does not distinguish per-user
// delivery. Tests that need to verify user-scoped delivery should inspect the
// EventManager directly.
func (n *CountingNotifier) EmitTo(userID, name string, data any) {
	n.Emit(name, data)
}

// HasSubscriber reports a connected client unless SetNoSubscriber(true) was
// called; tests use that to drive subscriber-liveness paths (e.g. the chat
// stream watchdog).
func (n *CountingNotifier) HasSubscriber(userID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.noSubscriber
}

func (n *CountingNotifier) SetNoSubscriber(v bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.noSubscriber = v
}

func (n *CountingNotifier) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.count
}

func (n *CountingNotifier) Events() []emittedEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := make([]emittedEvent, len(n.events))
	copy(cp, n.events)
	return cp
}

func (n *CountingNotifier) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.count = 0
	n.events = nil
}
