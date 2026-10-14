package eventbus

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/platform/txhook"
)

// fakeReplicas records what the bus asks it to publish, with the pass the bus ran it in.
type fakeReplicas struct {
	mu   sync.Mutex
	got  []string
	pass []Pass
	boom bool
}

func (f *fakeReplicas) PublishInvalidation(ctx context.Context, key string, scope Scope) {
	f.mu.Lock()
	f.got = append(f.got, key+"{"+scope.String()+"}")
	f.pass = append(f.pass, PassOf(ctx))
	boom := f.boom
	f.mu.Unlock()
	if boom {
		panic("hook failed")
	}
}

func (f *fakeReplicas) published() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// probeInvalidations is the scope function of the probe event: one invalidation of "cache.probe" per ID, none for "".
func probeInvalidations(e probeEvent) []Invalidation {
	if e.ID == "" {
		return nil
	}
	return []Invalidation{{Key: "cache.probe", Scope: NewScope("id", e.ID)}}
}

func replicatedProbe() (*Bus, *fakeReplicas, *log) {
	b := New(nil, time.Second)
	l := &log{}
	r := &fakeReplicas{}
	b.SetReplicas(r)
	Subscribe(b, Sync, "probe-cache", func(ctx context.Context, e probeEvent) { l.add("local:" + passName(ctx) + ":" + e.ID) },
		Replicated(probeInvalidations, "cache.probe"))
	return b, r, l
}

func TestReplicated_PublishedOnceOutsideATransaction(t *testing.T) {
	b, r, l := replicatedProbe()
	b.Publish(context.Background(), probeEvent{ID: "a"})
	if got, want := r.published(), []string{"cache.probe{id=a}"}; !slices.Equal(got, want) {
		t.Errorf("published %v, want %v", got, want)
	}
	if got, want := l.all(), []string{"local:now:a"}; !slices.Equal(got, want) {
		t.Errorf("local delivery %v, want %v (the local subscriber still runs)", got, want)
	}
	if r.pass[0] != Committed {
		t.Errorf("the hook ran in pass %v, want Committed", r.pass[0])
	}
}

func TestReplicated_WaitsForTheCommit(t *testing.T) {
	b, r, _ := replicatedProbe()
	ctx, reg := txhook.With(context.Background())
	b.Publish(ctx, probeEvent{ID: "a"})
	if got := r.published(); len(got) != 0 {
		t.Fatalf("published %v before the commit", got)
	}
	reg.Commit(nil)
	if got, want := r.published(), []string{"cache.probe{id=a}"}; !slices.Equal(got, want) {
		t.Errorf("after the commit published %v, want %v", got, want)
	}
	if r.pass[0] != Committed {
		t.Errorf("the hook ran in pass %v, want Committed", r.pass[0])
	}
}

func TestReplicated_NothingForARolledBackWrite(t *testing.T) {
	b, r, l := replicatedProbe()
	ctx, reg := txhook.With(context.Background())
	b.Publish(ctx, probeEvent{ID: "a"})
	reg.Abort()
	if got := r.published(); len(got) != 0 {
		t.Errorf("published %v for a rolled-back write", got)
	}
	if got, want := l.all(), []string{"local:now:a", "local:rollback:a"}; !slices.Equal(got, want) {
		t.Errorf("local deliveries %v, want %v (the rollback pass is local)", got, want)
	}
}

func TestReplicated_WithoutAHookBehaviourIsUnchanged(t *testing.T) {
	b := New(nil, time.Second)
	l := &log{}
	Subscribe(b, Sync, "probe-cache", func(ctx context.Context, e probeEvent) { l.add("local:" + passName(ctx)) },
		Replicated(probeInvalidations, "cache.probe"))
	ctx, reg := txhook.With(context.Background())
	b.Publish(ctx, probeEvent{ID: "a"})
	reg.Commit(nil)
	if got, want := l.all(), []string{"local:now", "local:commit"}; !slices.Equal(got, want) {
		t.Errorf("deliveries %v, want %v", got, want)
	}
	b.SetReplicas(nil)
	b.Publish(context.Background(), probeEvent{ID: "b"}) // must not panic on a cleared hook
}

func TestReplicated_AnAsyncSubscriberReplicatesAfterTheCommitToo(t *testing.T) {
	b := New(nil, time.Second)
	r := &fakeReplicas{}
	b.SetReplicas(r)
	Subscribe(b, Async, "probe-recheck", func(context.Context, probeEvent) {}, Replicated(probeInvalidations, "cache.probe"))
	ctx, reg := txhook.With(context.Background())
	b.Publish(ctx, probeEvent{ID: "a"})
	if len(r.published()) != 0 {
		t.Fatal("published before the commit")
	}
	reg.Commit(nil)
	if got, want := r.published(), []string{"cache.probe{id=a}"}; !slices.Equal(got, want) {
		t.Errorf("published %v, want %v", got, want)
	}
}

func TestReplicated_IdenticalInvalidationsOfOneEventArePublishedOnce(t *testing.T) {
	b := New(nil, time.Second)
	r := &fakeReplicas{}
	b.SetReplicas(r)
	for _, name := range []string{"first", "second"} {
		Subscribe(b, Sync, name, func(context.Context, probeEvent) {}, Replicated(probeInvalidations, "cache.probe"))
	}
	Subscribe(b, Sync, "third", func(context.Context, probeEvent) {}, Replicated(func(e probeEvent) []Invalidation {
		return []Invalidation{{Key: "cache.probe", Scope: NewScope("id", e.ID)}, {Key: "cache.other"}}
	}, "cache.probe", "cache.other"))
	b.Publish(context.Background(), probeEvent{ID: "a"})
	if got, want := r.published(), []string{"cache.probe{id=a}", "cache.other{}"}; !slices.Equal(got, want) {
		t.Errorf("published %v, want %v", got, want)
	}
}

func TestReplicated_ASubscriberThatSkipsTheEventPublishesNothing(t *testing.T) {
	b, r, _ := replicatedProbe()
	b.Publish(context.Background(), probeEvent{})
	if got := r.published(); len(got) != 0 {
		t.Errorf("published %v for an event the scope function skips", got)
	}
}

func TestReplicated_APanickingHookOrScopeFunctionDoesNotFailThePublisher(t *testing.T) {
	b := New(nil, time.Second)
	r := &fakeReplicas{boom: true}
	b.SetReplicas(r)
	l := &log{}
	Subscribe(b, Sync, "panics", func(context.Context, probeEvent) {},
		Replicated(func(probeEvent) []Invalidation { panic("scope") }, "cache.probe"))
	Subscribe(b, Sync, "hook-panics", func(context.Context, probeEvent) {}, Replicated(probeInvalidations, "cache.probe"))
	Subscribe(b, Sync, "after", func(ctx context.Context, e probeEvent) { l.add("after") }, Local("a probe"))
	b.Publish(context.Background(), probeEvent{ID: "a"})
	if got, want := l.all(), []string{"after"}; !slices.Equal(got, want) {
		t.Errorf("deliveries %v, want %v", got, want)
	}
	if got := r.published(); len(got) != 1 {
		t.Errorf("the hook saw %v, want the one invalidation of the subscriber whose scope function did not panic", got)
	}
}

func TestDeliverRemote_RunsTheApplierAndDoesNotRepublish(t *testing.T) {
	b, r, _ := replicatedProbe()
	var applied []string
	b.OnRemote("cache.probe", func(s Scope) { applied = append(applied, s.String()) })
	if !b.DeliverRemote("cache.probe", NewScope("id", "x")) {
		t.Fatal("DeliverRemote reported an unknown key for a registered applier")
	}
	if want := []string{"id=x"}; !slices.Equal(applied, want) {
		t.Errorf("applied %v, want %v", applied, want)
	}
	if got := r.published(); len(got) != 0 {
		t.Errorf("a remote delivery was published again: %v (it would loop between replicas)", got)
	}
}

func TestDeliverRemote_UnknownKeyIsIgnored(t *testing.T) {
	b := New(nil, time.Second)
	if b.DeliverRemote("cache.from-a-newer-release", nil) {
		t.Error("DeliverRemote reported success for a key nothing registered")
	}
	var nilBus *Bus
	if nilBus.DeliverRemote("cache.probe", nil) {
		t.Error("a nil bus reported success")
	}
}

func TestDeliverRemote_APanickingApplierIsRecovered(t *testing.T) {
	b := New(nil, time.Second)
	b.OnRemote("cache.probe", func(Scope) { panic("boom") })
	b.DeliverRemote("cache.probe", nil) // must not panic
}

func TestOnRemote_RegisteringAKeyTwiceFails(t *testing.T) {
	b := New(nil, time.Second)
	b.OnRemote("cache.probe", func(Scope) {})
	mustPanic(t, "twice", func() { b.OnRemote("cache.probe", func(Scope) {}) })
	mustPanic(t, "nil applier", func() { b.OnRemote("cache.other", nil) })
	mustPanic(t, "bad key", func() { b.OnRemote("Not A Key", func(Scope) {}) })
}

func TestTwoBusesOverALoopbackHook(t *testing.T) {
	// The shape the transport implements: A's hook hands (key, scope) to B's DeliverRemote.
	a, b := New(nil, time.Second), New(nil, time.Second)
	var dropped []string
	b.OnRemote("cache.probe", func(s Scope) { dropped = append(dropped, s["id"]) })
	a.SetReplicas(loopback{to: b})
	Subscribe(a, Sync, "probe-cache", func(context.Context, probeEvent) {}, Replicated(probeInvalidations, "cache.probe"))

	ctx, reg := txhook.With(context.Background())
	a.Publish(ctx, probeEvent{ID: "rolled-back"})
	reg.Abort()
	ctx, reg = txhook.With(context.Background())
	a.Publish(ctx, probeEvent{ID: "committed"})
	reg.Commit(nil)
	if want := []string{"committed"}; !slices.Equal(dropped, want) {
		t.Errorf("the peer dropped %v, want %v", dropped, want)
	}
}

type loopback struct{ to *Bus }

func (l loopback) PublishInvalidation(_ context.Context, key string, scope Scope) {
	l.to.DeliverRemote(key, scope)
}

func TestSubscribers_ListsEveryRegistrationWithItsLocality(t *testing.T) {
	b := New(nil, time.Second)
	Subscribe(b, Sync, "zeta", func(context.Context, probeEvent) {}, Local("a probe, no side effects to share"))
	Subscribe(b, Async, "alpha", func(context.Context, otherEvent) {}, Replicated(func(otherEvent) []Invalidation { return nil }, "cache.b", "cache.a"))
	Subscribe(b, Sync, "bare", func(context.Context, probeEvent) {})

	got := b.Subscribers()
	var lines []string
	for _, s := range got {
		lines = append(lines, s.String())
	}
	want := []string{
		"other | async | committed | alpha | replicated: cache.a, cache.b",
		"probe | sync | immediate+committed+rolled-back | zeta | local: a probe, no side effects to share",
		"probe | sync | immediate+committed+rolled-back | bare | UNCLASSIFIED",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("subscribers:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if un := b.Unclassified(); len(un) != 1 || un[0].Name != "bare" {
		t.Errorf("Unclassified = %v, want only bare", un)
	}
}

func TestSubscribe_LocalityOptionsAreValidated(t *testing.T) {
	b := New(nil, time.Second)
	fn := func(context.Context, probeEvent) {}
	mustPanic(t, "empty reason", func() { Subscribe(b, Sync, "x", fn, Local("  ")) })
	mustPanic(t, "no keys", func() { Subscribe(b, Sync, "x", fn, Replicated(probeInvalidations)) })
	mustPanic(t, "bad key", func() { Subscribe(b, Sync, "x", fn, Replicated(probeInvalidations, "Not A Key")) })
	mustPanic(t, "nil scope function", func() { Subscribe(b, Sync, "x", fn, Replicated[probeEvent](nil, "cache.probe")) })
	mustPanic(t, "both", func() { Subscribe(b, Sync, "x", fn, Local("why"), Replicated(probeInvalidations, "cache.probe")) })
	mustPanic(t, "twice", func() { Subscribe(b, Sync, "x", fn, Local("why"), Local("again")) })
	mustPanic(t, "scope function of another event", func() {
		Subscribe(b, Sync, "x", func(context.Context, otherEvent) {}, Replicated(probeInvalidations, "cache.probe"))
	})
	if got := b.Subscribers(); len(got) != 0 {
		t.Errorf("a refused registration was recorded: %v", got)
	}
}

func TestInvalidations_AnUndeclaredKeyFromTheScopeFunctionIsRefused(t *testing.T) {
	// A scope function may only name the keys its subscription declared, so the golden lists them all.
	b := New(nil, time.Second)
	r := &fakeReplicas{}
	b.SetReplicas(r)
	Subscribe(b, Sync, "sneaky", func(context.Context, probeEvent) {}, Replicated(func(probeEvent) []Invalidation {
		return []Invalidation{{Key: "cache.undeclared"}, {Key: "cache.probe"}}
	}, "cache.probe"))
	b.Publish(context.Background(), probeEvent{ID: "a"})
	if got, want := r.published(), []string{"cache.probe{}"}; !slices.Equal(got, want) {
		t.Errorf("published %v, want only the declared key %v", got, want)
	}
}

func TestReplicatedKeys_AndMissingAppliers(t *testing.T) {
	b := New(nil, time.Second)
	Subscribe(b, Sync, "one", func(context.Context, probeEvent) {}, Replicated(probeInvalidations, "cache.b", "cache.a"))
	Subscribe(b, Async, "two", func(context.Context, otherEvent) {}, Replicated(func(otherEvent) []Invalidation { return nil }, "cache.a"))
	Subscribe(b, Async, "three", func(context.Context, otherEvent) {}, Local("a probe"))
	if got, want := b.ReplicatedKeys(), []string{"cache.a", "cache.b"}; !slices.Equal(got, want) {
		t.Errorf("ReplicatedKeys = %v, want %v", got, want)
	}
	b.OnRemote("cache.a", func(Scope) {})
	if got, want := b.MissingAppliers(), []string{"cache.b"}; !slices.Equal(got, want) {
		t.Errorf("MissingAppliers = %v, want %v", got, want)
	}
}

func TestScope_StringIsCanonical(t *testing.T) {
	a := NewScope("org", "o1", "user", "u1")
	b := Scope{"user": "u1", "org": "o1"}
	if a.String() != b.String() || a.String() != "org=o1,user=u1" {
		t.Errorf("a = %q, b = %q", a.String(), b.String())
	}
	if (Scope)(nil).String() != "" || NewScope().String() != "" {
		t.Error("an empty scope must render empty: it means the whole cache")
	}
	if got := NewScope("scope", "").String(); got != "scope=" {
		t.Errorf("an explicitly empty value must stay visible: %q", got)
	}
	odd := []string{"org"}
	mustPanic(t, "odd pairs", func() { NewScope(odd...) })
}

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", what)
		}
	}()
	fn()
}

// Invalidate is for a change that is not a domain event (an analysis recorded, a credential disabled as a side
// effect): the caller dropped its own entry, the bus tells the other replicas once the write is durable.
func TestInvalidate_PublishedAfterTheCommit(t *testing.T) {
	b, r, _ := replicatedProbe()
	ctx, reg := txhook.With(context.Background())
	b.Invalidate(ctx, "cache.probe", NewScope("id", "a"))
	if got := r.published(); len(got) != 0 {
		t.Fatalf("published %v before the commit", got)
	}
	reg.Commit(nil)
	if got, want := r.published(), []string{"cache.probe{id=a}"}; !slices.Equal(got, want) {
		t.Errorf("after the commit published %v, want %v", got, want)
	}
	if r.pass[0] != Committed {
		t.Errorf("the hook ran in pass %v, want Committed", r.pass[0])
	}
}

func TestInvalidate_AtOnceOutsideATransactionAndNothingOnARollback(t *testing.T) {
	b, r, _ := replicatedProbe()
	b.Invalidate(context.Background(), "cache.probe", nil)
	if got, want := r.published(), []string{"cache.probe{}"}; !slices.Equal(got, want) {
		t.Errorf("published %v, want %v (an empty scope is the whole cache)", got, want)
	}
	r2 := &fakeReplicas{}
	b.SetReplicas(r2)
	ctx, reg := txhook.With(context.Background())
	b.Invalidate(ctx, "cache.probe", NewScope("id", "a"))
	reg.Abort()
	if got := r2.published(); len(got) != 0 {
		t.Errorf("published %v for a rolled-back write", got)
	}
}

func TestInvalidate_OnlyAKeyASubscriberDeclares(t *testing.T) {
	b, r, _ := replicatedProbe()
	b.Invalidate(context.Background(), "cache.unknown", NewScope("id", "a"))
	if got := r.published(); len(got) != 0 {
		t.Errorf("published %v for a key no replicated subscriber declares (the golden would not list it)", got)
	}
}

func TestInvalidate_NilBusAndNoHookAreNoOps(t *testing.T) {
	var nilBus *Bus
	nilBus.Invalidate(context.Background(), "cache.probe", nil)
	b := New(nil, time.Second)
	Subscribe(b, Sync, "probe-cache", func(context.Context, probeEvent) {}, Replicated(probeInvalidations, "cache.probe"))
	b.Invalidate(context.Background(), "cache.probe", nil) // no hook installed
}

func TestInvalidate_APanickingHookDoesNotFailTheCaller(t *testing.T) {
	b, r, _ := replicatedProbe()
	r.boom = true
	b.Invalidate(context.Background(), "cache.probe", NewScope("id", "a"))
}
