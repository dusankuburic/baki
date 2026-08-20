package service

// EventNotifier defines an interface for sending events to the frontend.
type EventNotifier interface {
	Emit(name string, data any)
	// EmitTo sends an event only to the SSE client(s) associated with userID.
	// In local mode (single client) this behaves identically to Emit.
	EmitTo(userID, name string, data any)
	// HasSubscriber reports whether at least one client that would receive
	// EmitTo(userID, …) is currently connected. Returning false actively
	// signals "the client is gone" and lets services abandon work addressed
	// to it (e.g. cancel an orphaned chat stream), so implementations without
	// a notion of client liveness must return true.
	HasSubscriber(userID string) bool
}

// NilNotifier is a no-op implementation of EventNotifier.
type NilNotifier struct{}

func (n NilNotifier) Emit(name string, data any)           {}
func (n NilNotifier) EmitTo(userID, name string, data any) {}
func (n NilNotifier) HasSubscriber(userID string) bool     { return true }
