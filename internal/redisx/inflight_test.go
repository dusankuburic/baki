package redisx

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInflightDeclarations(t *testing.T) {
	for _, k := range []*Key{InflightUserKey, InflightOrgKey} {
		s := k.Spec()
		if s.OnDown != OnDownClosed {
			t.Errorf("%s on-down = %v, want closed: the budget ledger refuses rather than guesses", s.Name, s.OnDown)
		}
		if s.TTL < 10*time.Minute {
			t.Errorf("%s TTL = %v, want at least the 10 minute stream cap", s.Name, s.TTL)
		}
		if s.Kind != KindHash || s.Version != 1 {
			t.Errorf("%s = %v v%d, want a v1 hash", s.Name, s.Kind, s.Version)
		}
	}
	if got := MechanismOf(InflightOrgKey.Key("o1")); got != "ai_inflight-org" {
		t.Errorf("mechanism of an org hash = %q", got)
	}
}

// The ledger's own rules, against the keys it writes: a lease per field, a TTL per hash, self skipped, the
// publish in the same round trip as the read, expired fields pruned and unreadable ones left alone.
func TestInflightLedger_PutOthersRemove(t *testing.T) {
	mr, client, _ := newHealth(t)
	clock := time.Now()
	l := NewInflightLedger(client).WithClock(func() time.Time { return clock })
	ctx := context.Background()

	mine := InflightEntry{Field: "f-mine", User: "u1", Org: "o1", Cost: 0.5}
	theirs := InflightEntry{Field: "f-theirs", User: "u2", Org: "o1", Cost: 0.25}
	if err := l.Put(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	// Publish and read in one call: the other's 0.25 is the org's, none of it u1's, and mine is not counted.
	org, user, err := l.Others(ctx, "u1", "o1", mine.Field, &mine)
	if err != nil || org != 0.25 || user != 0 {
		t.Fatalf("Others = (%v, %v, %v), want (0.25, 0, nil)", org, user, err)
	}
	// ...but the publish made it visible to the next reader.
	org, user, _ = l.Others(ctx, "u2", "o1", theirs.Field, nil)
	if org != 0.5 || user != 0 {
		t.Errorf("the other replica sees (%v, %v), want (0.5, 0)", org, user)
	}
	for _, k := range []string{InflightUserKey.Key("u1"), InflightUserKey.Key("u2"), InflightOrgKey.Key("o1")} {
		if ttl := mr.TTL(k); ttl <= 0 || ttl > inflightKeyTTL {
			t.Errorf("%s TTL = %v, want 0 < ttl <= %v", k, ttl, inflightKeyTTL)
		}
	}
	if v := mr.HGet(InflightOrgKey.Key("o1"), "f-mine"); v == "" {
		t.Fatal("the org hash has no entry for the published stream")
	}

	// A personal stream (no org) writes no org hash and sees no org sum.
	solo := InflightEntry{Field: "f-solo", User: "u9", Cost: 0.1}
	if err := l.Put(ctx, solo); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(InflightOrgKey.Key("")) {
		t.Error("a personal stream wrote an org hash")
	}

	// Past its lease an entry stops counting and is pruned; an unreadable one is neither counted nor deleted.
	mr.HSet(InflightOrgKey.Key("o1"), "f-junk", "{{")
	clock = clock.Add(inflightLease + time.Second)
	before := testutil.ToFloat64(ledgerExpired)
	org, _, _ = l.Others(ctx, "u3", "o1", "", nil)
	if org != 0 {
		t.Errorf("after the lease the org still counts %v", org)
	}
	if testutil.ToFloat64(ledgerExpired)-before != 2 {
		t.Errorf("expired counter moved by %v, want 2 (the two org entries)", testutil.ToFloat64(ledgerExpired)-before)
	}
	if keys, _ := mr.HKeys(InflightOrgKey.Key("o1")); fmt.Sprint(keys) != "[f-junk]" {
		t.Errorf("org hash fields after the prune = %v, want only the unreadable one", keys)
	}

	if err := l.Remove(ctx, solo); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(InflightUserKey.Key("u9")) {
		t.Error("Remove left the hash behind")
	}
}

func TestInflightLedger_ErrorsPropagate(t *testing.T) {
	mr, client, _ := newHealth(t)
	l := NewInflightLedger(client)
	mr.Close()
	ctx := context.Background()
	if err := l.Put(ctx, InflightEntry{Field: "f", User: "u"}); err == nil {
		t.Error("Put with Redis down must error: the caller fails closed")
	}
	if _, _, err := l.Others(ctx, "u", "o", "", nil); err == nil {
		t.Error("Others with Redis down must error, not read as empty")
	}
	if got := ErrorClass(errTimeout{}); got != "timeout" {
		t.Errorf("ErrorClass = %q", got)
	}
}

type errTimeout struct{}

func (errTimeout) Error() string { return "i/o timeout" }
