package eventbus

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/platform/txhook"
)

type probeEvent struct{ ID string }

func (probeEvent) EventName() string { return "probe" }

type otherEvent struct{}

func (otherEvent) EventName() string { return "other" }

// log collects what subscribers saw, with the pass for sync deliveries.
type log struct {
	mu  sync.Mutex
	got []string
}

func (l *log) add(s string) {
	l.mu.Lock()
	l.got = append(l.got, s)
	l.mu.Unlock()
}

func (l *log) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.got)
}

func passName(ctx context.Context) string {
	return map[Pass]string{Immediate: "now", Committed: "commit", RolledBack: "rollback"}[PassOf(ctx)]
}

func wired() (*Bus, *log) {
	b := New(nil, time.Second)
	l := &log{}
	Subscribe(b, Sync, "sync-probe", func(ctx context.Context, e probeEvent) { l.add("sync:" + passName(ctx) + ":" + e.ID) })
	Subscribe(b, Async, "async-probe", func(ctx context.Context, e probeEvent) { l.add("async:" + e.ID) })
	return b, l
}

func TestPublish_OutsideATransaction(t *testing.T) {
	b, l := wired()
	b.Publish(context.Background(), probeEvent{ID: "a"})
	if got, want := l.all(), []string{"sync:now:a", "async:a"}; !slices.Equal(got, want) {
		t.Errorf("deliveries %v, want %v", got, want)
	}
}

func TestPublish_InsideATransactionThatCommits(t *testing.T) {
	b, l := wired()
	ctx, reg := txhook.With(context.Background())
	b.Publish(ctx, probeEvent{ID: "a"})
	if got, want := l.all(), []string{"sync:now:a"}; !slices.Equal(got, want) {
		t.Fatalf("before the commit: %v, want %v (async waits for the commit)", got, want)
	}
	if !txhook.Published(ctx) {
		t.Error("the open transaction is not marked as having published")
	}
	reg.Commit(nil)
	if got, want := l.all(), []string{"sync:now:a", "sync:commit:a", "async:a"}; !slices.Equal(got, want) {
		t.Errorf("after the commit: %v, want %v", got, want)
	}
	if txhook.Published(ctx) {
		t.Error("a committed transaction still reports uncommitted writes")
	}
}

func TestPublish_InsideATransactionThatRollsBack(t *testing.T) {
	b, l := wired()
	ctx, reg := txhook.With(context.Background())
	b.Publish(ctx, probeEvent{ID: "a"})
	reg.Abort()
	if got, want := l.all(), []string{"sync:now:a", "sync:rollback:a"}; !slices.Equal(got, want) {
		t.Errorf("deliveries %v, want %v (no async delivery for a rolled-back write)", got, want)
	}
}

func TestPublish_OnlyToSubscribersOfTheType(t *testing.T) {
	b, l := wired()
	b.Publish(context.Background(), otherEvent{})
	if got := l.all(); len(got) != 0 {
		t.Errorf("probe subscribers saw %v", got)
	}
}

func TestPublish_APanickingSubscriberDoesNotStopTheOthers(t *testing.T) {
	b := New(nil, time.Second)
	l := &log{}
	Subscribe(b, Sync, "panics", func(context.Context, probeEvent) { panic("boom") })
	Subscribe(b, Sync, "after", func(context.Context, probeEvent) { l.add("after") })
	Subscribe(b, Async, "async-panics", func(context.Context, probeEvent) { panic("boom") })
	Subscribe(b, Async, "async-after", func(context.Context, probeEvent) { l.add("async-after") })
	b.Publish(context.Background(), probeEvent{})
	if got, want := l.all(), []string{"after", "async-after"}; !slices.Equal(got, want) {
		t.Errorf("deliveries %v, want %v", got, want)
	}
}

type stubRunner struct{ accept bool }

func (r stubRunner) Go(fn func()) bool {
	if r.accept {
		fn()
	}
	return r.accept
}

func TestPublish_AsyncDeliveryGetsADetachedBoundedContext(t *testing.T) {
	b := New(stubRunner{accept: true}, 50*time.Millisecond)
	var deadline time.Time
	var hasDeadline bool
	var parentErr error
	Subscribe(b, Async, "probe", func(ctx context.Context, _ probeEvent) {
		deadline, hasDeadline = ctx.Deadline()
		parentErr = ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the request is gone by the time the delivery runs
	b.Publish(ctx, probeEvent{})
	if !hasDeadline || time.Until(deadline) > 50*time.Millisecond {
		t.Errorf("deadline %v (set %v), want the bus timeout", deadline, hasDeadline)
	}
	if parentErr != nil {
		t.Errorf("the delivery inherited the publisher's cancellation: %v", parentErr)
	}
}

func TestPublish_ASaturatedRunnerDropsTheDelivery(t *testing.T) {
	b := New(stubRunner{accept: false}, time.Second)
	ran := false
	Subscribe(b, Async, "probe", func(context.Context, probeEvent) { ran = true })
	b.Publish(context.Background(), probeEvent{})
	if ran {
		t.Error("a delivery the runner refused still ran")
	}
}

func TestPublish_ASaturatedRunnerRunsAnInlineSubscriberInline(t *testing.T) {
	b := New(stubRunner{accept: false}, time.Second)
	var ran []string
	Subscribe(b, Async, "must-deliver", func(context.Context, probeEvent) { ran = append(ran, "inline") }, InlineWhenSaturated())
	Subscribe(b, Async, "droppable", func(context.Context, probeEvent) { ran = append(ran, "dropped?") })
	ctx, reg := txhook.With(context.Background())
	b.Publish(ctx, probeEvent{})
	if len(ran) != 0 {
		t.Fatalf("delivered before the commit: %v", ran)
	}
	reg.Commit(nil)
	if !slices.Equal(ran, []string{"inline"}) {
		t.Errorf("after the commit with a saturated runner: %v, want only the inline subscriber", ran)
	}
}

func TestPublish_NilBusPublishesNothing(t *testing.T) {
	var b *Bus
	b.Publish(context.Background(), probeEvent{})
}

func TestRecorder(t *testing.T) {
	b := New(nil, time.Second)
	rec := NewRecorder(b)
	b.Publish(context.Background(), probeEvent{ID: "a"})
	b.Publish(context.Background(), otherEvent{})
	b.Publish(context.Background(), probeEvent{ID: "b"})
	if got := Recorded[probeEvent](rec); len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("recorded probes %v", got)
	}
	if n := len(rec.Events()); n != 3 {
		t.Errorf("recorded %d events, want 3", n)
	}
	rec.Reset()
	if n := len(rec.Events()); n != 0 {
		t.Errorf("after Reset: %d events", n)
	}
}

func TestGen_RefusesAFillAfterAnInvalidation(t *testing.T) {
	var g Gen[string]
	ctx := context.Background()
	captured := g.Load("u1")
	if !g.FillOK(ctx, "u1", captured) {
		t.Fatal("an undisturbed fill was refused")
	}
	g.Bump("u1")
	if g.FillOK(ctx, "u1", captured) {
		t.Error("a fill read before the invalidation was accepted")
	}
	captured = g.Load("u1")
	g.BumpAll()
	if g.FillOK(ctx, "u1", captured) {
		t.Error("a fill read before BumpAll was accepted")
	}
}

func TestGen_RefusesAFillFromATransactionThatPublished(t *testing.T) {
	var g Gen[string]
	b := New(nil, time.Second)
	ctx, reg := txhook.With(context.Background())
	captured := g.Load("u1")
	if !g.FillOK(ctx, "u1", captured) {
		t.Fatal("a read-only transaction's fill was refused")
	}
	b.Publish(ctx, probeEvent{})
	if g.FillOK(ctx, "u1", captured) {
		t.Error("a fill from a transaction with uncommitted writes was accepted")
	}
	reg.Commit(nil)
	if !g.FillOK(ctx, "u1", captured) {
		t.Error("the fill stays refused after the commit")
	}
}

// TestGen_ConcurrentWriterAndReader replays the race the sync tier and Gen close together: a reader captures the
// generation and reads the old value while a writer's transaction is open, the writer commits, and the reader
// stores afterwards. The writer's committed-pass invalidation must win.
func TestGen_ConcurrentWriterAndReader(t *testing.T) {
	var g Gen[string]
	cache := map[string]string{}
	b := New(nil, time.Second)
	Subscribe(b, Sync, "cache", func(_ context.Context, e probeEvent) {
		g.Bump(e.ID)
		delete(cache, e.ID)
	})

	writer, reg := txhook.With(context.Background())
	b.Publish(writer, probeEvent{ID: "k"}) // immediate pass

	reader := context.Background()
	captured := g.Load("k") // after the immediate pass
	value := "old"          // the writer has not committed: the reader sees the old row

	reg.Commit(nil) // committed pass: bumps again
	if g.FillOK(reader, "k", captured) {
		cache["k"] = value
	}
	if v, ok := cache["k"]; ok {
		t.Errorf("stale %q survived the writer's commit", v)
	}
}
