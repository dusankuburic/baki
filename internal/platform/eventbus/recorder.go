package eventbus

import "sync"

// Recorder captures every event published on a bus, in order, whatever its type and whether or not anything
// subscribes to it: a test asserts what a service announced without wiring subscribers.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// NewRecorder returns a recorder of b's events from now on.
func NewRecorder(b *Bus) *Recorder {
	r := &Recorder{}
	b.mu.Lock()
	b.taps = append(b.taps, r.add)
	b.mu.Unlock()
	return r
}

func (r *Recorder) add(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Events returns the events recorded so far.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Reset forgets the events recorded so far.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
}

// Recorded returns the events of type E r recorded, in order.
func Recorded[E Event](r *Recorder) []E {
	var out []E
	for _, e := range r.Events() {
		if v, ok := e.(E); ok {
			out = append(out, v)
		}
	}
	return out
}
