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

// On a declared channel a message of a version or kind this release does not know is counted and dropped before the
// subscriber sees it; a v0 message (no envelope) and one the subscriber must judge for itself (not JSON) pass.
func TestPubSub_DropsWhatThisReleaseDoesNotKnow(t *testing.T) {
	_, client := newMiniRedis(t)
	ps := NewPubSub(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channel := RevocationChannel.Key()

	ready := make(chan struct{}, 1)
	got := make(chan string, 8)
	go func() {
		_ = ps.Subscribe(ctx, channel, func() { ready <- struct{}{} }, func(p []byte) { got <- string(p) })
	}()
	waitFor(t, "onReady", ready)

	unknownBefore := count(RevocationChannel.Name(), ResultUnknown)
	badBefore := count(RevocationChannel.Name(), ResultBad)
	for _, payload := range []string{
		`{"v":9,"jti":"future"}`,                            // a newer version: dropped
		`{"v":1,"kind":"auth.other","jti":"odd"}`,           // another kind (and v1 > the declared v0): dropped
		`{"jti":"old","expires_at":"2100-01-01T00:00:00Z"}`, // v0, the shipped shape: delivered
		`not json`, // undecodable: counted, still delivered for the subscriber to judge
	} {
		if err := ps.Publish(ctx, channel, []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if p := waitFor(t, "the v0 message", got); p != `{"jti":"old","expires_at":"2100-01-01T00:00:00Z"}` {
		t.Fatalf("first delivered payload = %q, want the v0 message", p)
	}
	if p := waitFor(t, "the undecodable message", got); p != "not json" {
		t.Fatalf("second delivered payload = %q", p)
	}
	select {
	case extra := <-got:
		t.Fatalf("a payload this release does not know reached the subscriber: %q", extra)
	case <-time.After(100 * time.Millisecond):
	}
	if d := count(RevocationChannel.Name(), ResultUnknown) - unknownBefore; d != 2 {
		t.Errorf("unknown count grew by %v, want 2", d)
	}
	if d := count(RevocationChannel.Name(), ResultBad) - badBefore; d != 1 {
		t.Errorf("bad count grew by %v, want 1", d)
	}
}

// A channel that declares the kinds it carries is decoded, and its messages counted, by its subscriber, which
// knows them: the layer under it passes every one on, even a kind that is not the declared name's.
var multiKindChannel = Declare(Spec{
	Name: "demo.multikind", Kind: KindPubSub, Pattern: "demo:multikind", Version: 1,
	Kinds: []string{"first", "second"}, OnDown: OnDownOpen, Owner: "internal/redisx",
})

func TestPubSub_ADeclaredKindsChannelPassesEveryKindOn(t *testing.T) {
	_, client := newMiniRedis(t)
	ps := NewPubSub(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channel := multiKindChannel.Key()

	ready := make(chan struct{}, 1)
	got := make(chan string, 8)
	go func() {
		_ = ps.Subscribe(ctx, channel, func() { ready <- struct{}{} }, func(p []byte) { got <- string(p) })
	}()
	waitFor(t, "onReady", ready)
	for _, payload := range []string{`{"v":1,"kind":"first"}`, `{"v":1,"kind":"second"}`, `{"v":7,"kind":"third"}`} {
		if err := ps.Publish(ctx, channel, []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{`{"v":1,"kind":"first"}`, `{"v":1,"kind":"second"}`, `{"v":7,"kind":"third"}`} {
		if p := waitFor(t, want, got); p != want {
			t.Errorf("delivered %q, want %q", p, want)
		}
	}
}
