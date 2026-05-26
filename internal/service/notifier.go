package service

// Notifier defines an interface for sending events to the frontend.
type Notifier interface {
	Emit(name string, data any)
}

// NilNotifier is a no-op implementation of Notifier.
type NilNotifier struct{}

func (n NilNotifier) Emit(name string, data any) {}
