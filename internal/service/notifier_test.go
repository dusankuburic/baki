package service

import (
	"sync"
	"testing"
)

func TestNilNotifier_EmitIsNoOp(t *testing.T) {
	n := NilNotifier{}
	n.Emit("test", "data")
}

func TestGlobalNotifier_SubscribeAndEmit(t *testing.T) {
	n := NewGlobalNotifier()

	var mu sync.Mutex
	var received []struct{ name, data string }
	n.Subscribe(func(name string, data any) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, struct{ name, data string }{name, data.(string)})
	})

	n.Emit("event1", "hello")
	n.Emit("event2", "world")

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 events, got %d", len(received))
	}
	if received[0].name != "event1" || received[0].data != "hello" {
		t.Errorf("unexpected first event: %+v", received[0])
	}
	if received[1].name != "event2" || received[1].data != "world" {
		t.Errorf("unexpected second event: %+v", received[1])
	}
}

func TestGlobalNotifier_MultipleSubscribers(t *testing.T) {
	n := NewGlobalNotifier()

	var count1, count2 int
	n.Subscribe(func(name string, data any) { count1++ })
	n.Subscribe(func(name string, data any) { count2++ })

	n.Emit("event", nil)
	n.Emit("event", nil)

	if count1 != 2 {
		t.Errorf("expected subscriber1 to receive 2 events, got %d", count1)
	}
	if count2 != 2 {
		t.Errorf("expected subscriber2 to receive 2 events, got %d", count2)
	}
}

func TestGlobalNotifier_NilData(t *testing.T) {
	n := NewGlobalNotifier()

	var received any
	n.Subscribe(func(name string, data any) {
		received = data
	})

	n.Emit("event", nil)
	if received != nil {
		t.Errorf("expected nil data, got %v", received)
	}
}
