package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource is an in-memory RevocationSource that counts the calls the
// cache makes against it, so a test can assert "no round trip".
type fakeSource struct {
	mu        sync.Mutex
	entries   map[string]time.Time
	lookups   int
	loads     int
	lookupErr error
	loadErr   error
	stopped   bool
	// gate, when set, blocks every LoadActive (after it has been counted in
	// loads) until the channel is closed — a source that is slow to answer.
	gate chan struct{}
}

func newFakeSource() *fakeSource { return &fakeSource{entries: make(map[string]time.Time)} }

func (f *fakeSource) Add(jti string, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[jti] = time.Now().Add(ttl)
}

func (f *fakeSource) AddIfAbsent(jti string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if exp, ok := f.entries[jti]; ok && time.Now().Before(exp) {
		return false, nil
	}
	f.entries[jti] = time.Now().Add(ttl)
	return true, nil
}

func (f *fakeSource) IsRevokedCtx(_ context.Context, jti string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.lookupErr != nil {
		return false, f.lookupErr
	}
	exp, ok := f.entries[jti]
	return ok && time.Now().Before(exp), nil
}

func (f *fakeSource) LoadActive(ctx context.Context) (map[string]time.Time, error) {
	f.mu.Lock()
	f.loads++
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	out := make(map[string]time.Time, len(f.entries))
	now := time.Now()
	for jti, exp := range f.entries {
		if now.Before(exp) {
			out[jti] = exp
		}
	}
	return out, nil
}

func (f *fakeSource) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
}

func (f *fakeSource) counts() (lookups, loads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups, f.loads
}

func (f *fakeSource) setLookupErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupErr = err
}

// gateLoads makes subsequent loads block until the returned release is called.
func (f *fakeSource) gateLoads() (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := make(chan struct{})
	f.gate = gate
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.gate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
}

func (f *fakeSource) setLoadErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadErr = err
}

// fakeBus is an in-memory pub/sub shared by several caches: Publish fans a
// payload out to every live subscription. refuse makes Subscribe fail
// outright; breakAll ends every live subscription with an error, the way a
// dropped Redis connection does.
type fakeBus struct {
	mu     sync.Mutex
	subs   map[chan []byte]chan struct{} // delivery → break signal
	refuse bool
}

func newFakeBus() *fakeBus { return &fakeBus{subs: make(map[chan []byte]chan struct{})} }

func (b *fakeBus) Publish(_ context.Context, _ string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		ch <- payload
	}
	return nil
}

func (b *fakeBus) Subscribe(ctx context.Context, _ string, onReady func(), onMessage func([]byte)) error {
	b.mu.Lock()
	if b.refuse {
		b.mu.Unlock()
		return errors.New("bus: subscribe refused")
	}
	ch, broken := make(chan []byte, 64), make(chan struct{})
	b.subs[ch] = broken
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}()
	onReady()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-broken:
			return errors.New("bus: connection lost")
		case p := <-ch:
			onMessage(p)
		}
	}
}

func (b *fakeBus) breakAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, broken := range b.subs {
		close(broken)
		delete(b.subs, ch)
	}
}

func (b *fakeBus) setRefuse(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuse = v
}

func (b *fakeBus) live() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func newTestCache(t *testing.T, src *fakeSource, bus *fakeBus) *CachedBlacklist {
	t.Helper()
	opts := CachedBlacklistOptions{ReloadInterval: time.Hour}
	if bus != nil {
		opts.Publisher, opts.Subscriber = bus, bus
	}
	bl := NewCachedBlacklist(src, opts)
	bl.minBackoff, bl.maxBackoff = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(bl.Stop)
	return bl
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (bl *CachedBlacklist) size() int {
	bl.mu.RLock()
	defer bl.mu.RUnlock()
	return len(bl.entries)
}

// Add is visible locally at once, before any load, with no lookup against the
// source — and the source was written.
func TestCachedBlacklist_AddIsLocallyVisibleWithoutSourceLookup(t *testing.T) {
	src := newFakeSource()
	bl := newTestCache(t, src, nil) // never started: not ready
	bl.Add("jti-1", time.Minute)
	if !bl.IsRevoked("jti-1") {
		t.Fatal("Add then IsRevoked: want revoked")
	}
	if lookups, _ := src.counts(); lookups != 0 {
		t.Errorf("IsRevoked of a locally known entry made %d source lookups, want 0", lookups)
	}
	if _, ok := src.entries["jti-1"]; !ok {
		t.Error("Add did not write the source")
	}
	// AddIfAbsent the same way.
	added, err := bl.AddIfAbsent("jti-2", time.Minute)
	if err != nil || !added {
		t.Fatalf("AddIfAbsent fresh: (%v, %v), want (true, nil)", added, err)
	}
	if added, _ := bl.AddIfAbsent("jti-2", time.Minute); added {
		t.Error("AddIfAbsent again: want already present")
	}
	if !bl.IsRevoked("jti-2") {
		t.Error("AddIfAbsent then IsRevoked: want revoked")
	}
	if lookups, _ := src.counts(); lookups != 0 {
		t.Errorf("source lookups = %d, want 0", lookups)
	}
}

// Until the first load has succeeded a miss goes to the source; once loaded,
// a single-replica cache answers everything locally, including misses.
func TestCachedBlacklist_BootLoadPopulatesAndMakesMissesLocal(t *testing.T) {
	src := newFakeSource()
	src.Add("seeded", time.Minute)
	bl := newTestCache(t, src, nil)

	if !bl.IsRevoked("seeded") {
		t.Fatal("before load: a seeded entry must still be reported revoked (via the source)")
	}
	if lookups, _ := src.counts(); lookups != 1 {
		t.Fatalf("before load: source lookups = %d, want 1", lookups)
	}

	bl.Start(context.Background())
	eventually(t, "boot load", bl.ready.Load)
	if !bl.lookup("seeded") {
		t.Fatal("boot load did not populate the local set")
	}
	if !bl.IsRevoked("seeded") || bl.IsRevoked("unknown") {
		t.Fatal("after load: wrong answers")
	}
	if lookups, _ := src.counts(); lookups != 1 {
		t.Errorf("after load: source lookups = %d, want still 1 (misses are local now)", lookups)
	}
}

// The boot load retries until the source answers; meanwhile misses still go
// to the source.
func TestCachedBlacklist_BootRetriesUntilLoadSucceeds(t *testing.T) {
	src := newFakeSource()
	src.setLoadErr(errors.New("db down"))
	bl := newTestCache(t, src, nil)
	bl.Start(context.Background())
	eventually(t, "a second load attempt", func() bool { _, loads := src.counts(); return loads >= 2 })
	if bl.ready.Load() {
		t.Fatal("ready despite every load failing")
	}
	bl.IsRevoked("x")
	if lookups, _ := src.counts(); lookups != 1 {
		t.Errorf("not ready: source lookups = %d, want 1", lookups)
	}
	src.setLoadErr(nil)
	eventually(t, "boot load after the source recovers", bl.ready.Load)
}

// A source error during the fallback counts as revoked (fail closed) from
// IsRevoked, and is returned from IsRevokedCtx.
func TestCachedBlacklist_SourceErrorDuringFallbackFailsClosed(t *testing.T) {
	src := newFakeSource()
	src.setLookupErr(errors.New("db down"))
	bl := newTestCache(t, src, nil) // not started: every miss is a fallback
	if !bl.IsRevoked("anything") {
		t.Error("IsRevoked on a source error: want true (fail closed)")
	}
	if _, err := bl.IsRevokedCtx(context.Background(), "anything"); err == nil {
		t.Error("IsRevokedCtx on a source error: want the error")
	}
	// A local hit still wins without touching the broken source.
	bl.Add("known", time.Minute)
	before, _ := src.counts()
	if revoked, err := bl.IsRevokedCtx(context.Background(), "known"); !revoked || err != nil {
		t.Errorf("local hit with a broken source: (%v, %v), want (true, nil)", revoked, err)
	}
	if after, _ := src.counts(); after != before {
		t.Error("a local hit consulted the source")
	}
}

// Expired entries are not reported and are swept by the reload.
func TestCachedBlacklist_ExpiredEntriesAreSwept(t *testing.T) {
	src := newFakeSource()
	bl := newTestCache(t, src, nil)
	bl.Add("gone", -time.Second)
	bl.Add("live", time.Minute)
	if bl.IsRevoked("gone") {
		t.Error("an expired entry reported revoked")
	}
	if bl.size() != 2 {
		t.Fatalf("size before sweep = %d, want 2", bl.size())
	}
	if err := bl.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bl.size() != 1 || !bl.lookup("live") {
		t.Errorf("after load: size = %d (want 1), live=%v (want true)", bl.size(), bl.lookup("live"))
	}
}

// A revocation on one replica reaches the other over the bus without the
// other consulting its source.
func TestCachedBlacklist_RevocationFansOutAcrossReplicas(t *testing.T) {
	bus := newFakeBus()
	srcA, srcB := newFakeSource(), newFakeSource()
	a, b := newTestCache(t, srcA, bus), newTestCache(t, srcB, bus)
	a.Start(context.Background())
	b.Start(context.Background())
	eventually(t, "both replicas subscribed and loaded", func() bool {
		return a.local() && b.local()
	})

	a.Add("logged-out", time.Minute)
	eventually(t, "replica B to learn the revocation", func() bool { return b.lookup("logged-out") })
	if !b.IsRevoked("logged-out") {
		t.Fatal("replica B: want revoked")
	}
	if lookups, _ := srcB.counts(); lookups != 0 {
		t.Errorf("replica B consulted its source %d times, want 0", lookups)
	}
	if _, ok := srcB.entries["logged-out"]; ok {
		t.Error("the fan-out wrote replica B's source; only the revoking replica writes")
	}
}

// With peers configured, a lost subscription makes misses go to the source
// until the subscription is back — after which a full reload catches up.
func TestCachedBlacklist_LostSubscriptionFallsBackToSourceUntilResubscribed(t *testing.T) {
	bus := newFakeBus()
	src := newFakeSource()
	bl := newTestCache(t, src, bus)
	bl.Start(context.Background())
	eventually(t, "subscribed and loaded", bl.local)
	_, loadsBefore := src.counts()

	bus.setRefuse(true)
	bus.breakAll()
	eventually(t, "subscription marked lost", func() bool { return !bl.subscribed.Load() })

	src.Add("revoked-elsewhere", time.Minute) // a peer wrote it while we were deaf
	if !bl.IsRevoked("revoked-elsewhere") {
		t.Fatal("while unsubscribed a miss must be answered by the source")
	}
	if lookups, _ := src.counts(); lookups != 1 {
		t.Errorf("while unsubscribed: source lookups = %d, want 1", lookups)
	}

	bus.setRefuse(false)
	eventually(t, "resubscribed", bl.subscribed.Load)
	eventually(t, "the reload after resubscribing", func() bool {
		_, loads := src.counts()
		return loads > loadsBefore && bl.lookup("revoked-elsewhere")
	})
	if !bl.IsRevoked("revoked-elsewhere") || bl.IsRevoked("nope") {
		t.Error("after resubscribe: wrong answers")
	}
	if lookups, _ := src.counts(); lookups != 1 {
		t.Errorf("after resubscribe: source lookups = %d, want still 1", lookups)
	}
}

// The server's subscribe confirmation alone must not make local misses final:
// a revocation published while this replica was deaf is only in the local set
// once the catch-up load has completed. While that load is still running (a
// slow source), a miss has to keep going to the source — otherwise the token
// revoked on a peer during the outage is accepted here.
func TestCachedBlacklist_ResubscribeIsNotHealthyUntilCatchUpLoadCompletes(t *testing.T) {
	bus := newFakeBus()
	src := newFakeSource()
	bl := newTestCache(t, src, bus)
	bl.Start(context.Background())
	eventually(t, "subscribed and loaded", bl.local)

	bus.setRefuse(true)
	bus.breakAll()
	eventually(t, "subscription marked lost", func() bool { return !bl.subscribed.Load() })
	src.Add("revoked-while-deaf", time.Minute) // a peer's logout we never heard

	// The bus comes back, but the source is slow: the catch-up load starts
	// and blocks.
	_, loadsBefore := src.counts()
	release := src.gateLoads()
	defer release()
	bus.setRefuse(false)
	eventually(t, "the bus-level resubscribe", func() bool { return bus.live() == 1 })
	eventually(t, "the catch-up load to start", func() bool { _, loads := src.counts(); return loads > loadsBefore })

	if bl.subscribed.Load() || bl.local() {
		t.Fatal("marked subscribed/local before the catch-up load completed")
	}
	if !bl.IsRevoked("revoked-while-deaf") {
		t.Fatal("a revocation published during the outage was accepted while the catch-up load was still running")
	}
	if lookups, _ := src.counts(); lookups != 1 {
		t.Errorf("during catch-up: source lookups = %d, want 1 (miss must go to the source)", lookups)
	}

	release()
	eventually(t, "subscribed after the catch-up load", bl.subscribed.Load)
	if !bl.lookup("revoked-while-deaf") {
		t.Fatal("the catch-up load did not bring in the missed revocation")
	}
	if !bl.IsRevoked("revoked-while-deaf") || bl.IsRevoked("nope") {
		t.Error("after catch-up: wrong answers")
	}
	if lookups, _ := src.counts(); lookups != 1 {
		t.Errorf("after catch-up: source lookups = %d, want still 1", lookups)
	}
}

// A catch-up load that fails keeps retrying — and keeps misses on the source —
// until it succeeds.
func TestCachedBlacklist_CatchUpLoadRetriesUntilItSucceeds(t *testing.T) {
	bus := newFakeBus()
	src := newFakeSource()
	bl := newTestCache(t, src, bus)
	bl.Start(context.Background())
	eventually(t, "subscribed and loaded", bl.local)

	src.setLoadErr(errors.New("db down"))
	bus.breakAll()
	eventually(t, "the bus-level resubscribe", func() bool { return bus.live() == 1 })
	_, loadsAtResub := src.counts()
	eventually(t, "retried catch-up loads", func() bool { _, loads := src.counts(); return loads >= loadsAtResub+2 })
	if bl.subscribed.Load() {
		t.Fatal("subscribed while every catch-up load fails")
	}
	src.setLoadErr(nil)
	eventually(t, "subscribed once a load succeeds", bl.subscribed.Load)
}

// Start after Stop is a no-op: nothing it would start could ever be stopped.
func TestCachedBlacklist_StartAfterStopIsANoOp(t *testing.T) {
	bus := newFakeBus()
	src := newFakeSource()
	bl := NewCachedBlacklist(src, CachedBlacklistOptions{Publisher: bus, Subscriber: bus, ReloadInterval: 10 * time.Millisecond})
	bl.minBackoff, bl.maxBackoff = time.Millisecond, time.Millisecond
	bl.Stop()
	bl.Start(context.Background())
	time.Sleep(100 * time.Millisecond)
	_, loads := src.counts()
	if loads != 0 || bus.live() != 0 || bl.ready.Load() {
		t.Fatalf("Start after Stop ran: loads=%d live subscriptions=%d ready=%v; want nothing", loads, bus.live(), bl.ready.Load())
	}
}

// Stop ends the subscription and the loops and stops the source; a cancelled
// Start context does the same.
func TestCachedBlacklist_StopAndContextCancelReleaseEverything(t *testing.T) {
	bus := newFakeBus()
	src := newFakeSource()
	bl := NewCachedBlacklist(src, CachedBlacklistOptions{Publisher: bus, Subscriber: bus, ReloadInterval: time.Hour})
	bl.Start(context.Background())
	eventually(t, "subscribed", bl.subscribed.Load)
	done := make(chan struct{})
	go func() { bl.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return")
	}
	if bus.live() != 0 || !src.stopped {
		t.Errorf("after Stop: live subscriptions = %d (want 0), source stopped = %v (want true)", bus.live(), src.stopped)
	}

	src2 := newFakeSource()
	ctx, cancel := context.WithCancel(context.Background())
	bl2 := NewCachedBlacklist(src2, CachedBlacklistOptions{Publisher: bus, Subscriber: bus, ReloadInterval: time.Hour})
	bl2.Start(ctx)
	eventually(t, "second cache subscribed", bl2.subscribed.Load)
	cancel()
	eventually(t, "the subscription to end on ctx cancel", func() bool { return bus.live() == 0 })
	bl2.Stop()

	// Stop without Start is a no-op that still stops the source.
	src3 := newFakeSource()
	NewCachedBlacklist(src3, CachedBlacklistOptions{}).Stop()
	if !src3.stopped {
		t.Error("Stop without Start did not stop the source")
	}
}

// A malformed message is ignored rather than inserting an empty id.
func TestCachedBlacklist_MalformedMessageIsIgnored(t *testing.T) {
	bl := newTestCache(t, newFakeSource(), nil)
	bl.apply([]byte(`not json`))
	bl.apply([]byte(`{"expires_at":"2099-01-01T00:00:00Z"}`))
	if bl.size() != 0 {
		t.Errorf("malformed messages inserted %d entries", bl.size())
	}
	bl.apply([]byte(`{"jti":"peer","expires_at":"2030-01-01T00:00:00Z"}`))
	if !bl.lookup("peer") {
		t.Error("a well-formed message was not inserted")
	}
}

// A peer-published message is untrusted input: oversized payloads, long ids
// and missing/past expiries are rejected, and a far-future expiry is clamped
// to MaxTTL so the entry is swept once the clamp passes — the local set only
// shrinks by expiry, so an unclamped year-9999 entry would live forever.
func TestCachedBlacklist_PeerMessagesAreBoundedAndClamped(t *testing.T) {
	const maxTTL = 50 * time.Millisecond
	bl := NewCachedBlacklist(newFakeSource(), CachedBlacklistOptions{ReloadInterval: time.Hour, MaxTTL: maxTTL})
	t.Cleanup(bl.Stop)

	rejected := map[string]string{
		"oversized payload": `{"jti":"big","expires_at":"2030-01-01T00:00:00Z","pad":"` + strings.Repeat("x", 1024) + `"}`,
		"long jti":          `{"jti":"` + strings.Repeat("j", 257) + `","expires_at":"2030-01-01T00:00:00Z"}`,
		"zero expiry":       `{"jti":"zero"}`,
		"past expiry":       `{"jti":"past","expires_at":"2000-01-01T00:00:00Z"}`,
	}
	for name, payload := range rejected {
		bl.apply([]byte(payload))
		if bl.size() != 0 {
			t.Fatalf("%s: message was inserted", name)
		}
	}

	bl.apply([]byte(`{"jti":"forever","expires_at":"9999-12-31T00:00:00Z"}`))
	if !bl.lookup("forever") {
		t.Fatal("a clamped far-future entry must still be revoked right now")
	}
	bl.mu.RLock()
	exp := bl.entries["forever"]
	bl.mu.RUnlock()
	if exp.After(time.Now().Add(maxTTL)) {
		t.Fatalf("expiry %v was not clamped to now+%v", exp, maxTTL)
	}
	time.Sleep(2 * maxTTL)
	if bl.lookup("forever") {
		t.Error("clamped entry still revoked after the clamp passed")
	}
	bl.sweep()
	if bl.size() != 0 {
		t.Errorf("clamped entry survived the sweep: %d entries", bl.size())
	}

	// At the limits, accepted.
	bl.apply([]byte(`{"jti":"` + strings.Repeat("j", 256) + `","expires_at":"2030-01-01T00:00:00Z"}`))
	if bl.size() != 1 {
		t.Error("a 256-byte jti was rejected")
	}
}
