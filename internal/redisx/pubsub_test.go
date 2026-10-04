package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func waitFor[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// A message published after onReady is delivered; cancelling the context ends
// Subscribe with nil.
func TestPubSub_DeliversAfterReadyAndEndsOnCancel(t *testing.T) {
	_, client := newMiniRedis(t)
	ps := NewPubSub(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{}, 1)
	got := make(chan []byte, 8)
	done := make(chan error, 1)
	go func() {
		done <- ps.Subscribe(ctx, "test:chan", func() { ready <- struct{}{} }, func(p []byte) { got <- p })
	}()
	waitFor(t, "onReady", ready)

	if err := ps.Publish(ctx, "test:chan", []byte(`{"jti":"a"}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if p := waitFor(t, "the message", got); string(p) != `{"jti":"a"}` {
		t.Fatalf("payload = %q", p)
	}

	cancel()
	if err := waitFor(t, "Subscribe to return", done); err != nil {
		t.Fatalf("Subscribe after cancel: %v, want nil", err)
	}
}

// Losing the server ends Subscribe with an error — the signal the cache uses
// to mark itself unsubscribed and resubscribe.
func TestPubSub_ServerLossIsAnError(t *testing.T) {
	mr, client := newMiniRedis(t)
	ps := NewPubSub(client)

	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- ps.Subscribe(context.Background(), "test:chan", func() { ready <- struct{}{} }, func([]byte) {})
	}()
	waitFor(t, "onReady", ready)

	mr.Close()
	if err := waitFor(t, "Subscribe to return", done); err == nil {
		t.Fatal("Subscribe returned nil after the server went away; want an error")
	}
}

// With no server at all, Subscribe fails rather than reporting ready.
func TestPubSub_UnreachableServerNeverReportsReady(t *testing.T) {
	mr, client := newMiniRedis(t)
	mr.Close()
	ready := false
	err := NewPubSub(client).Subscribe(context.Background(), "test:chan", func() { ready = true }, func([]byte) {})
	if err == nil || ready {
		t.Fatalf("Subscribe against a dead server: err=%v ready=%v; want an error and not ready", err, ready)
	}
}
