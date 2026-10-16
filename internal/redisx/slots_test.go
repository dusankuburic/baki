package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// testSlotsKey is a cap family of this package's tests only.
var testSlotsKey = Declare(Spec{
	Name:    "test.slots",
	Kind:    KindZSet,
	Pattern: "baki:test:slots:{user}",
	TTL:     SlotKeyTTL,
	Version: 1,
	OnDown:  OnDownOpen,
	Owner:   "internal/redisx",
})

func capsCount(outcome string) float64 {
	return testutil.ToFloat64(capsTotal.WithLabelValues(testSlotsKey.Name(), outcome))
}

// Two caps on one Redis are one count: the limit holds across them, per holder, and a release frees a slot for
// either. Every write leaves the key with its TTL.
func TestSlotCap_TheLimitHoldsAcrossCaps(t *testing.T) {
	mr, client, health := newHealth(t)
	a, b := NewSlotCap(client, health, testSlotsKey, 2), NewSlotCap(client, health, testSlotsKey, 2)
	ctx := context.Background()
	r1, ok1 := a.Acquire(ctx, "u1")
	_, ok2 := b.Acquire(ctx, "u1")
	if !ok1 || !ok2 {
		t.Fatalf("the first two slots: %v %v", ok1, ok2)
	}
	refused := capsCount("refused")
	if _, ok := b.Acquire(ctx, "u1"); ok {
		t.Fatal("a third slot was admitted over a limit of 2")
	}
	if got := capsCount("refused") - refused; got != 1 {
		t.Errorf("refused += %v, want 1", got)
	}
	if _, ok := a.Acquire(ctx, "u2"); !ok {
		t.Fatal("another holder was refused")
	}
	if ttl := mr.TTL(testSlotsKey.Key("u1")); ttl <= 0 || ttl > SlotKeyTTL {
		t.Errorf("TTL = %v, want 0 < ttl <= %v", ttl, SlotKeyTTL)
	}
	r1()
	r1() // idempotent
	if a.Held() != 1 {
		t.Errorf("a holds %d, want 1 (u2's)", a.Held())
	}
	if _, ok := b.Acquire(ctx, "u1"); !ok {
		t.Fatal("a released slot was not free for the other cap")
	}
}

// A holder that stops refreshing (its replica died) stops counting once the lease has passed; the next acquire prunes
// it. Nobody deletes it otherwise.
func TestSlotCap_ALapsedLeaseStopsCounting(t *testing.T) {
	mr, client, health := newHealth(t)
	dead := NewSlotCap(client, health, testSlotsKey, 1)
	if _, ok := dead.Acquire(context.Background(), "u1"); !ok {
		t.Fatal("first slot refused")
	}
	mr.SetTime(time.Now().Add(SlotLease + time.Second)) // the server's clock judges the leases
	alive := NewSlotCap(client, health, testSlotsKey, 1)
	if _, ok := alive.Acquire(context.Background(), "u1"); !ok {
		t.Fatal("a lapsed slot still counted")
	}
	members, _ := mr.ZMembers(testSlotsKey.Key("u1"))
	if len(members) != 1 {
		t.Errorf("the set holds %d members after the prune, want 1", len(members))
	}
}

// The leases run on the server's clock, not on the replica's: a replica whose clock is off by any amount writes leases
// that lapse when the server says, so a dead replica's slots stop counting on time (a clock ahead would otherwise keep
// them counting for as long as it is ahead).
func TestSlotCap_LeasesRunOnTheServersClock(t *testing.T) {
	mr, client, health := newHealth(t)
	ctx := context.Background()
	server := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // years away from this process's clock
	mr.SetTime(server)
	if _, ok := NewSlotCap(client, health, testSlotsKey, 1).Acquire(ctx, "u1"); !ok {
		t.Fatal("first slot refused")
	}
	key := testSlotsKey.Key("u1")
	members, _ := mr.ZMembers(key)
	if len(members) != 1 {
		t.Fatalf("members = %v", members)
	}
	if score, _ := mr.ZScore(key, members[0]); int64(score) != server.Add(SlotLease).UnixMilli() {
		t.Errorf("lease ends at %v, want the server's time plus the lease, %v", time.UnixMilli(int64(score)).UTC(), server.Add(SlotLease))
	}
	mr.SetTime(server.Add(SlotLease + time.Second))
	if _, ok := NewSlotCap(client, health, testSlotsKey, 1).Acquire(ctx, "u1"); !ok {
		t.Fatal("a slot whose lease lapsed on the server's clock still counted")
	}
}

// The refresh keeps a held slot counting past its first lease, and does not bring back one released meanwhile.
func TestSlotCap_TheRefreshKeepsHeldSlotsAndOnlyThose(t *testing.T) {
	prev := SlotRefreshEvery
	SlotRefreshEvery = 20 * time.Millisecond
	t.Cleanup(func() { SlotRefreshEvery = prev })
	mr, client, health := newHealth(t)
	server := time.Now()
	mr.SetTime(server)
	c := NewSlotCap(client, health, testSlotsKey, 3)
	keep, _ := c.Acquire(context.Background(), "u1")
	drop, _ := c.Acquire(context.Background(), "u1")
	defer keep()
	drop()
	server = server.Add(time.Minute)
	mr.SetTime(server)
	key := testSlotsKey.Key("u1")
	deadline := time.Now().Add(3 * time.Second)
	for {
		members, _ := mr.ZMembers(key)
		if len(members) == 1 {
			score, _ := mr.ZScore(key, members[0])
			if int64(score) >= server.Add(SlotLease).UnixMilli() {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the held slot's lease was not rewritten, or a released one came back: %v", members)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Fail open: a Redis that does not answer, or one Health calls degraded, admits on the caller's own cap and holds
// nothing; a nil cap and an empty holder admit without asking.
func TestSlotCap_FailsOpen(t *testing.T) {
	mr, client, health := newHealth(t)
	c := NewSlotCap(client, health, testSlotsKey, 1)
	if _, ok := c.Acquire(context.Background(), "u1"); !ok {
		t.Fatal(ok)
	}
	open := capsCount("open")
	mr.Close()
	for i := 0; i < 5; i++ {
		release, ok := c.Acquire(context.Background(), "u1")
		if !ok {
			t.Fatalf("acquire %d with Redis down refused", i+1)
		}
		release()
	}
	if got := capsCount("open") - open; got != 5 {
		t.Errorf("open += %v, want 5", got)
	}
	if !health.Degraded() {
		t.Error("five failed acquires did not degrade Health")
	}
	var none *SlotCap
	if release, ok := none.Acquire(context.Background(), "u1"); !ok {
		t.Error("a nil cap refused")
	} else {
		release()
	}
	if NewSlotCap(nil, nil, testSlotsKey, 1) != nil {
		t.Error("a cap without a client is not nil")
	}
	if _, ok := c.Acquire(context.Background(), ""); !ok {
		t.Error("an empty holder was refused")
	}
}
